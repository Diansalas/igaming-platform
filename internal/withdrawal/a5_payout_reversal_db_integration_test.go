//go:build integration

// A5 / WITHDRAWAL-REVERSAL-1: the DATABASE side of migration 0162, driven as the table owner (no role is
// exempt from the 0132/0162 triggers) and, for the Go primitives, through RecordPayoutReversalDecision /
// ReverseCompletedPayout. Every refusal names its pinned SQLSTATE:
//
//	WD026 E11 without two distinct-Person 'reverse' decisions on ONE payload digest
//	WD022 E11 column discipline (the reversal link set exactly once, the release link unchanged)
//	WD020 / WD021 a reversed row is terminal; no edge leaves it
//	WD035 check A: the reversal link must be this request's exact withdrawal_reversed posting
//	WD040 B-i: a withdrawal_reversed transaction can never touch a hold account
//	WD044 a withdrawal_reversed posting nobody links at COMMIT
//	23505 one reversal per completion (ledger.ErrWithdrawalReversalAlreadyExists); one reverse vote per
//	      approver; 23514 the 'reverse' reason_code shape
//
// plus migration 0162 up -> down -> up on a scratch database (whole-schema snapshot) and the down refusal
// (WD097) while a reversal row exists.
package withdrawal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

var a5Hash = strings.Repeat("ab", 32)

// a5Completed creates a request and walks it to completed through the Go writers (completion provider
// "a5psp", completion reference ref).
func a5Completed(t *testing.T, f fixture, amount int64, ref string) WithdrawalRequest {
	t.Helper()
	return a5Walk(t, f, amount, ref, true)
}

func a5Walk(t *testing.T, f fixture, amount int64, ref string, complete bool) WithdrawalRequest {
	t.Helper()
	pool := testPool(t)
	wr := mustRequestWithdrawal(t, pool, f, amount, "a5-"+uuid.NewString())
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))
	approver := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Approve(ctx, tx, wr.ID, approver, false, nil, alwaysEligible, auth.StepUpAssertion{})
		return err
	})
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MarkSubmitted(ctx, tx, wr.ID, "a5psp", "out-"+ref) })
	if complete {
		mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return Complete(ctx, tx, wr.ID, "a5psp", ref) })
	}
	var got WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = GetByID(ctx, tx, wr.ID)
		return err
	})
	if want := map[bool]State{true: StateCompleted, false: StateSubmitted}[complete]; got.State != want {
		t.Fatalf("setup: %s, want %s", got.State, want)
	}
	return got
}

func a5Payload(wr WithdrawalRequest, ref string) PayoutReversal {
	return PayoutReversal{ProviderID: "a5psp", OriginalProviderReference: ref, ReturnReference: "ret-" + ref,
		Amount: wr.Amount, AssetCode: wr.AssetCode, EvidenceRefHash: a5Hash}
}

// a5Vote inserts a human 'reverse' decision row directly (the 0034/0162 governance trigger still runs).
func a5Vote(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, staff uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals
		(tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code,
		 threshold_amount_at_decision, request_amount_at_decision)
		VALUES ($1, $2, $3, false, 'reverse', $4, 0, $5)`, wr.TenantID, wr.ID, staff, reason, wr.Amount)
	return err
}

// a5PostReversal posts the pinned withdrawal_reversed shape for wr (Dr psp_clearing / Cr player_cash).
func a5PostReversal(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, key, returnRef string, amount int64) (uuid.UUID, error) {
	accts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode})
	if err != nil {
		return uuid.Nil, err
	}
	pid := "a5psp"
	res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: wr.TenantID, TransactionType: ledger.TxWithdrawalReversed, IdempotencyKey: key,
		ProviderID: &pid, ProviderTxID: &returnRef, CorrelationID: wr.ID, ReversesTransactionID: wr.ReleaseLedgerTransactionID,
		Entries: []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: amount}},
	})
	return res.TransactionID, err
}

func a5Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func a5CreatePersonStaff(t *testing.T, tenantID, person uuid.UUID) uuid.UUID {
	t.Helper()
	pool := testPool(t)
	id := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1) ON CONFLICT DO NOTHING`, person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mustRunTx(t, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'x', 'finance', 'active', $4)`, id, tenantID, id.String()+"@example.com", person)
		return err
	})
	return id
}

// E11 at the database: the governed shape commits; every weaker shape is refused with its code.
func TestA5DB_E11_GovernanceFloorAndShape(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	ref := "a5db-" + uuid.NewString()[:8]
	reason := ReversalReasonPrefix + strings.Repeat("ab", 32)
	other := ReversalReasonPrefix + strings.Repeat("cd", 32)
	s1, s2 := mustCreateApprover(t, pool, f.tenantID), mustCreateApprover(t, pool, f.tenantID)
	samePerson := uuid.New()
	l1, l2 := a5CreatePersonStaff(t, f.tenantID, samePerson), a5CreatePersonStaff(t, f.tenantID, samePerson)

	attempt := func(name, want string, votes func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error,
		act func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error) {
		t.Run(name, func(t *testing.T) {
			wr := a5Completed(t, f, 100, ref+"-"+strings.ReplaceAll(name, " ", "-"))
			err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if err := votes(ctx, tx, wr); err != nil {
					return err
				}
				return act(ctx, tx, wr)
			})
			if want == "" {
				if err != nil {
					t.Fatalf("the governed shape must commit: %v", err)
				}
				return
			}
			// "WD044/WD035": the reversal posting's commit trigger fires first (deferred triggers run in event
			// order: the posting precedes the UPDATE) and carries check A's WD035 inside its message.
			code, inner, _ := strings.Cut(want, "/")
			if got := a5Code(err); got != code || (inner != "" && !strings.Contains(err.Error(), inner+":")) {
				t.Fatalf("want SQLSTATE %s, got %q (%v)", want, got, err)
			}
		})
	}
	twoPersons := func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		if err := a5Vote(ctx, tx, wr, s1, reason); err != nil {
			return err
		}
		return a5Vote(ctx, tx, wr, s2, reason)
	}
	e11 := func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		id, err := a5PostReversal(ctx, tx, wr, wr.ID.String()+":reversed", "ret-"+wr.ID.String(), wr.Amount)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, id, wr.ID)
		return err
	}
	attempt("governed shape", "", twoPersons, e11)
	attempt("one decision", "WD026", func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		return a5Vote(ctx, tx, wr, s1, reason)
	}, e11)
	attempt("no decision", "WD026", func(context.Context, pgx.Tx, WithdrawalRequest) error { return nil }, e11)
	attempt("two logins one person", "WD026", func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		if err := a5Vote(ctx, tx, wr, l1, reason); err != nil {
			return err
		}
		return a5Vote(ctx, tx, wr, l2, reason)
	}, e11)
	attempt("two persons two payloads", "WD026", func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		if err := a5Vote(ctx, tx, wr, s1, reason); err != nil {
			return err
		}
		return a5Vote(ctx, tx, wr, s2, other)
	}, e11)
	attempt("E11 without the reversal link", "WD022", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed' WHERE id = $1`, wr.ID)
		return err
	})
	attempt("E11 re-pointing the release link", "WD022", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		id, err := a5PostReversal(ctx, tx, wr, wr.ID.String()+":reversed", "ret-"+wr.ID.String(), wr.Amount)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1, release_ledger_transaction_id = $1 WHERE id = $2`, id, wr.ID)
		return err
	})
	attempt("reversal link on a completed row without E11", "WD020", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET reversal_ledger_transaction_id = release_ledger_transaction_id WHERE id = $1`, wr.ID)
		return err
	})
	attempt("E11 linking the completion as the reversal", "WD035", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = release_ledger_transaction_id WHERE id = $1`, wr.ID)
		return err
	})
	attempt("E11 with a reversal of the wrong amount", "WD044/WD035", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		id, err := a5PostReversal(ctx, tx, wr, wr.ID.String()+":reversed", "ret-"+wr.ID.String(), wr.Amount+1)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, id, wr.ID)
		return err
	})
	attempt("E11 with the wrong reversal key", "WD044/WD035", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		id, err := a5PostReversal(ctx, tx, wr, wr.ID.String()+":rev", "ret-"+wr.ID.String(), wr.Amount)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, id, wr.ID)
		return err
	})
	attempt("E11 with a reversal that reverses another transaction", "WD044/WD035", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		other := wr
		other.ReleaseLedgerTransactionID = wr.HoldLedgerTransactionID // reverses the hold, not the completion
		id, err := a5PostReversal(ctx, tx, other, wr.ID.String()+":reversed", "ret-"+wr.ID.String(), wr.Amount)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, id, wr.ID)
		return err
	})
	attempt("standalone withdrawal_reversed nobody links", "WD044", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		_, err := a5PostReversal(ctx, tx, wr, wr.ID.String()+":reversed", "ret-"+wr.ID.String(), wr.Amount)
		return err
	})
	attempt("withdrawal_reversed touching the hold", "WD040", twoPersons, func(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
		accts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
			ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
			ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode})
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: wr.TenantID, TransactionType: ledger.TxWithdrawalReversed,
			IdempotencyKey: wr.ID.String() + ":reversed", CorrelationID: wr.ID, ReversesTransactionID: wr.ReleaseLedgerTransactionID,
			Entries: []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: wr.Amount},
				{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: wr.Amount}}})
		return err
	})
}

// A reversed row is terminal: nothing leaves it, not even updated_at. A second reversal of the same
// completion is refused by the unique index even under a fresh key.
func TestA5DB_ReversedIsTerminal_OneReversalPerCompletion(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	ref := "a5db-term-" + uuid.NewString()[:8]
	wr := a5Completed(t, f, 100, ref)
	p := a5Payload(wr, ref)
	s1, s2 := mustCreateApprover(t, pool, f.tenantID), mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := a5Vote(ctx, tx, wr, s1, p.ReasonCode(wr.ID)); err != nil {
			return err
		}
		if err := a5Vote(ctx, tx, wr, s2, p.ReasonCode(wr.ID)); err != nil {
			return err
		}
		_, err := ReverseCompletedPayout(ctx, tx, wr.ID, p)
		return err
	})
	for name, set := range map[string]string{
		"back to completed": `state = 'completed'`, "to failed": `state = 'failed'`, "touch updated_at": `updated_at = now()`,
		"clear the reversal link": `reversal_ledger_transaction_id = NULL`,
	} {
		err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET `+set+` WHERE id = $1`, wr.ID)
			return err
		})
		if got := a5Code(err); got != "WD020" {
			t.Fatalf("%s: want WD020, got %q (%v)", name, got, err)
		}
	}
	// A second reversal of the same completion under a FRESH key: the 0162 index refuses it at insert.
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a5PostReversal(ctx, tx, wr, wr.ID.String()+":second", "ret2-"+ref, wr.Amount)
		return err
	})
	if !errors.Is(err, ledger.ErrWithdrawalReversalAlreadyExists) {
		t.Fatalf("want ErrWithdrawalReversalAlreadyExists, got %v", err)
	}
	// No edge reaches reversed except E11: a submitted request cannot jump there (WD021).
	sub := a5Walk(t, f, 100, ref+"-sub", false)
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed' WHERE id = $1`, sub.ID)
		return err
	})
	if got := a5Code(err); got != "WD021" {
		t.Fatalf("submitted -> reversed: want WD021, got %q (%v)", got, err)
	}
	// A non-terminal row never carries a reversal link (WD022, before check A's WD035 at commit).
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET reversal_ledger_transaction_id = hold_ledger_transaction_id WHERE id = $1`, sub.ID)
		return err
	})
	if got := a5Code(err); got != "WD022" {
		t.Fatalf("a reversal link on a submitted row: want WD022, got %q (%v)", got, err)
	}
	// ReverseCompletedPayout again: a state conflict, nothing posted.
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReverseCompletedPayout(ctx, tx, wr.ID, p)
		return err
	})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("want ErrStateConflict, got %v", err)
	}
}

// The 'reverse' decision row: its reason_code shape, one vote per approver, never automated, never the
// beneficiary. The review uniqueness (approve/reject) is unchanged; an approver of the payout may vote on
// its reversal (the 0026 UNIQUE was split per decision class).
func TestA5DB_ReverseDecisionRow_Guards(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	ref := "a5db-row-" + uuid.NewString()[:8]
	wr := a5Completed(t, f, 100, ref)
	reason := ReversalReasonPrefix + strings.Repeat("0f", 32)
	s1 := mustCreateApprover(t, pool, f.tenantID)
	cases := []struct {
		name, want string
		fn         func(ctx context.Context, tx pgx.Tx) error
	}{
		{"bad reason shape", "23514", func(ctx context.Context, tx pgx.Tx) error { return a5Vote(ctx, tx, wr, s1, "payout_reversal:XYZ") }},
		{"reason on an approve row looks like a reversal", "23514", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, decision, reason_code,
				threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, 'approve', $4, 0, 1)`, wr.TenantID, wr.ID, s1, reason)
			return err
		}},
		{"automated reverse by an unknown principal", "P0001", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code,
				threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, true, 'reverse', $4, 0, 1)`, wr.TenantID, wr.ID, uuid.New(), reason)
			return err
		}},
		{"beneficiary reverse", "P0001", func(ctx context.Context, tx pgx.Tx) error {
			return a5Vote(ctx, tx, wr, a5CreatePersonStaff(t, f.tenantID, f.personID), reason)
		}},
		{"second vote by the same approver", "23505", func(ctx context.Context, tx pgx.Tx) error {
			if err := a5Vote(ctx, tx, wr, s1, reason); err != nil {
				return err
			}
			return a5Vote(ctx, tx, wr, s1, ReversalReasonPrefix+strings.Repeat("1e", 32))
		}},
	}
	for _, c := range cases {
		err := runTx(pool, f.tenantID, c.fn)
		if got := a5Code(err); got != c.want {
			t.Fatalf("%s: want %s, got %q (%v)", c.name, c.want, got, err)
		}
	}
	// The payout's own approver (already holds an 'approve' row on this request) may also vote 'reverse'.
	var approver uuid.UUID
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT approver_principal_id FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'approve'`, wr.ID).Scan(&approver)
	})
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return a5Vote(ctx, tx, wr, approver, reason) })
}

// The Go decision path: payload bound to the digest, distinct Persons counted, Ready at the floor.
func TestA5DB_RecordDecision_CountsDistinctPersonsPerPayload(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	ref := "a5db-go-" + uuid.NewString()[:8]
	wr := a5Completed(t, f, 100, ref)
	p := a5Payload(wr, ref)
	no := func(uuid.UUID) (bool, error) { return false, nil }
	dec := func(staff uuid.UUID, q PayoutReversal) (ReversalDecision, error) {
		var d ReversalDecision
		err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			d, err = RecordPayoutReversalDecision(ctx, tx, wr.ID, staff, q, no, alwaysEligible, auth.StepUpAssertion{})
			return err
		})
		return d, err
	}
	if _, err := dec(mustCreateApprover(t, pool, f.tenantID), PayoutReversal{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("an empty payload: %v", err)
	}
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordPayoutReversalDecision(ctx, tx, wr.ID, uuid.New(), p, nil, alwaysEligible, auth.StepUpAssertion{})
		return err
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil beneficiary check: %v", err)
	}
	d1, err := dec(mustCreateApprover(t, pool, f.tenantID), p)
	if err != nil || d1.Ready || d1.Counted != 1 || d1.Required != MinReversalApprovers {
		t.Fatalf("first: %+v %v", d1, err)
	}
	d2, err := dec(mustCreateApprover(t, pool, f.tenantID), p)
	if err != nil || !d2.Ready || d2.Counted != 2 {
		t.Fatalf("second: %+v %v", d2, err)
	}
	// ReverseCompletedPayout re-checks the payload against the completion it reverses, under the L2 lock: a
	// payload naming another original reference is refused even when two Persons agreed on a valid one (the
	// database floor WD026 proves two Persons agreed on SOME payload; the Go layer binds it to the posting).
	wrong := p
	wrong.OriginalProviderReference = "a5db-not-the-completion"
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReverseCompletedPayout(ctx, tx, wr.ID, wrong)
		return err
	})
	if !errors.Is(err, ErrReversalPayloadMismatch) {
		t.Fatalf("a payload not matching the completion: %v, want ErrReversalPayloadMismatch", err)
	}
	// The digest binds every payload field.
	base := p.Digest(wr.ID)
	for name, mut := range map[string]func(*PayoutReversal){
		"provider": func(q *PayoutReversal) { q.ProviderID = "x" }, "original": func(q *PayoutReversal) { q.OriginalProviderReference = "x" },
		"return": func(q *PayoutReversal) { q.ReturnReference = "x" }, "amount": func(q *PayoutReversal) { q.Amount++ },
		"asset": func(q *PayoutReversal) { q.AssetCode = "USD" }, "evidence": func(q *PayoutReversal) { q.EvidenceRefHash = strings.Repeat("0", 64) },
	} {
		q := p
		mut(&q)
		if q.Digest(wr.ID) == base {
			t.Fatalf("the digest ignores %s", name)
		}
	}
	if p.Digest(uuid.New()) == base {
		t.Fatal("the digest ignores the request id")
	}
}
