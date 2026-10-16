//go:build integration

// A5 / WITHDRAWAL-REVERSAL-1 (migration 0162): the governed payout reversal, end to end through
// DecidePayoutReversal on a scratch database migrated to head, with MOCK providers. Cases: normal
// (four-eyes then the compensating posting), duplicate / replay, concurrency, partial failure, the
// reversal-before-completion tombstone, a parked amount dispute that the provider returns, an
// in-flight attempt, and the authorization refusals (unlinked, inactive, beneficiary, one Person with
// two logins, step-up, another tenant, payload mismatch).
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/stepup"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const a5EvidenceHash = "3b7a3c1f6f0b5e3a9e4c2d1b0a99887766554433221100ffeeddccbbaa998877"

// a5Checks is the transaction-bound eligibility and beneficiary pair built from the DATABASE, like the
// HTTP layer's (staff row FOR SHARE; Person of the staff vs the withdrawing player's Person).
func a5Checks(requestID uuid.UUID) func(ctx context.Context, tx pgx.Tx) (withdrawal.BeneficiaryCheck, withdrawal.ApproverEligibility) {
	return func(ctx context.Context, tx pgx.Tx) (withdrawal.BeneficiaryCheck, withdrawal.ApproverEligibility) {
		benef := func(staffID uuid.UUID) (bool, error) {
			var same bool
			err := tx.QueryRow(ctx, `SELECT COALESCE(su.person_id = pa.person_id, false)
				FROM staff_users su, withdrawal_requests w JOIN player_accounts pa ON pa.id = w.player_account_id
				WHERE su.id = $1 AND w.id = $2`, staffID, requestID).Scan(&same)
			return same, err
		}
		elig := func(staffID uuid.UUID) (bool, bool, error) {
			var linked, active bool
			err := tx.QueryRow(ctx, `SELECT person_id IS NOT NULL, status = 'active' FROM staff_users WHERE id = $1 FOR SHARE`, staffID).Scan(&linked, &active)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, false, nil
			}
			return linked, active, err
		}
		return benef, elig
	}
}

func (e rbEnv) a5Staff(t *testing.T) uuid.UUID {
	t.Helper()
	return k3MkStaff(t, e.pool, e.f.tenantID, "finance", uuid.New()).ID
}

func (e rbEnv) a5Payload(ref string) withdrawal.PayoutReversal {
	return withdrawal.PayoutReversal{ProviderID: e.pid, OriginalProviderReference: ref, ReturnReference: "ret-" + ref,
		Amount: 500, AssetCode: "EUR", EvidenceRefHash: a5EvidenceHash}
}

func (e rbEnv) a5Decide(staff, requestID uuid.UUID, p withdrawal.PayoutReversal) (PayoutReversalResult, error) {
	return DecidePayoutReversal(context.Background(), e.pool, e.f.tenantID, requestID, PayoutReversalInput{
		ApproverID: staff, Payload: p, Checks: a5Checks(requestID),
	})
}

func (e rbEnv) a5MustDecide(t *testing.T, staff, requestID uuid.UUID, p withdrawal.PayoutReversal, want PayoutReversalOutcome) PayoutReversalResult {
	t.Helper()
	res, err := e.a5Decide(staff, requestID, p)
	if err != nil {
		t.Fatalf("DecidePayoutReversal: %v", err)
	}
	if res.Outcome != want {
		t.Fatalf("outcome = %s, want %s (%+v)", res.Outcome, want, res)
	}
	return res
}

// a5Completed: a payout settled by a matching success callback (attempt succeeded, request completed).
func (e rbEnv) a5Completed(t *testing.T, key, ref string) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	t.Helper()
	wr, a := e.claim(t, key)
	e.b12Callback(t, a, OutcomeSucceeded, ref, 500)
	if got := fpReqState(t, e.pool, e.f, wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("setup: want completed, got %s", got.State)
	}
	return fpReqState(t, e.pool, e.f, wr.ID), mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
}

func (e rbEnv) a5Cash(t *testing.T) int64 {
	t.Helper()
	var bal int64
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		id, err := ledger.GetOrCreateAccount(ctx, tx, e.f.tenantID, &e.f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		b, err := ledger.GetProjectedBalance(ctx, tx, id)
		bal = b.Signed()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return bal
}

func (e rbEnv) a5Count(t *testing.T, q string, args ...any) int {
	t.Helper()
	return fpCount(t, e.pool, e.f.tenantID, q, args...)
}

func (e rbEnv) a5ReversalLink(t *testing.T, id uuid.UUID) *uuid.UUID {
	t.Helper()
	var link *uuid.UUID
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		link, err = withdrawal.ReversalLink(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return link
}

// Normal: one decision records only; the second distinct Person executes the compensating entry (Dr
// psp_clearing / Cr player_cash, same asset, same wallet), E11, audits; the completion is untouched.
func TestA5_PayoutReversal_Normal_FourEyesThenCompensatingEntry(t *testing.T) {
	e := newRBEnv(t, "mock-a5-normal")
	wr, a := e.a5Completed(t, "a5-n", "a5-ref-n")
	p := e.a5Payload("a5-ref-n")
	cashBefore, txBefore := e.a5Cash(t), fpLedgerTx(t, e.pool, e.f)
	s1, s2 := e.a5Staff(t), e.a5Staff(t)

	r1 := e.a5MustDecide(t, s1, wr.ID, p, PayoutReversalRecorded)
	if r1.Counted != 1 || r1.Required != 2 || r1.LedgerTransactionID != nil {
		t.Fatalf("first decision: %+v", r1)
	}
	if fpLedgerTx(t, e.pool, e.f) != txBefore || fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateCompleted {
		t.Fatal("one decision must not move money or state")
	}
	r2 := e.a5MustDecide(t, s2, wr.ID, p, PayoutReversalExecuted)
	if r2.Counted != 2 || r2.LedgerTransactionID == nil {
		t.Fatalf("second decision: %+v", r2)
	}
	got := fpReqState(t, e.pool, e.f, wr.ID)
	if got.State != withdrawal.StateReversed || *got.ReleaseLedgerTransactionID != *wr.ReleaseLedgerTransactionID {
		t.Fatalf("E11: state=%s release=%v (completion %v must stay the release link)", got.State, got.ReleaseLedgerTransactionID, wr.ReleaseLedgerTransactionID)
	}
	if link := e.a5ReversalLink(t, wr.ID); link == nil || *link != *r2.LedgerTransactionID {
		t.Fatalf("reversal link %v, want %v", link, r2.LedgerTransactionID)
	}
	if d := e.a5Cash(t) - cashBefore; d != 500 {
		t.Fatalf("player_cash must be credited back the payout (500 EUR), delta %d", d)
	}
	if n := fpLedgerTx(t, e.pool, e.f); n != txBefore+1 {
		t.Fatalf("exactly one new ledger transaction, %d -> %d", txBefore, n)
	}
	// The exact posting shape.
	if n := e.a5Count(t, `SELECT count(*) FROM ledger_transactions t
		WHERE t.id = $1 AND t.transaction_type = 'withdrawal_reversed' AND t.idempotency_key = $2 AND t.correlation_id = $3
		  AND t.reverses_transaction_id = $4 AND t.provider_id = $5 AND t.provider_tx_id = $6
		  AND (SELECT count(*) FROM ledger_entries e WHERE e.ledger_transaction_id = t.id) = 2
		  AND EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		              WHERE e.ledger_transaction_id = t.id AND e.direction = 'debit' AND la.account_type = 'psp_clearing' AND e.amount = 500 AND e.asset_code = 'EUR')
		  AND EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		              WHERE e.ledger_transaction_id = t.id AND e.direction = 'credit' AND la.account_type = 'player_cash' AND e.wallet_id = $7 AND e.amount = 500 AND e.asset_code = 'EUR')`,
		*r2.LedgerTransactionID, wr.ID.String()+":reversed", wr.ID, *wr.ReleaseLedgerTransactionID, e.pid, "ret-a5-ref-n", e.f.walletID); n != 1 {
		t.Fatal("the reversal is not the pinned withdrawal_reversed shape")
	}
	// The payout attempt keeps its truth: it did succeed.
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("the payout attempt stays succeeded, got %s", got.State)
	}
	// Audit: two decision rows (staff actors) and one execution row.
	if n := e.a5Count(t, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='withdrawal.payout_reversal_decision_recorded' AND target_id=$2 AND actor_type='staff'`, e.f.tenantID, wr.ID.String()); n != 2 {
		t.Fatalf("decision audits = %d, want 2", n)
	}
	if n := e.a5Count(t, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='withdrawal.reversed' AND target_id=$2
		AND metadata->>'state_before' = 'completed' AND metadata->>'state_after' = 'reversed'`, e.f.tenantID, wr.ID.String()); n != 1 {
		t.Fatalf("execution audit = %d, want 1", n)
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, e.pool, e.f.tenantID)
}

// Duplicates and replays: the same approver twice before the quorum is refused (one vote per person);
// a different payload does not count with the first; after execution the executing call's retry is a
// replay (nothing recorded, same transaction id) and a newcomer is a state conflict.
func TestA5_PayoutReversal_DuplicatesReplaysAndPayloadBinding(t *testing.T) {
	e := newRBEnv(t, "mock-a5-dup")
	wr, _ := e.a5Completed(t, "a5-d", "a5-ref-d")
	p := e.a5Payload("a5-ref-d")
	other := p
	other.ReturnReference = "ret-a5-ref-d-other"
	s1, s2, s3, s4 := e.a5Staff(t), e.a5Staff(t), e.a5Staff(t), e.a5Staff(t)

	e.a5MustDecide(t, s1, wr.ID, p, PayoutReversalRecorded)
	if _, err := e.a5Decide(s1, wr.ID, p); !errors.Is(err, withdrawal.ErrDuplicateApproval) {
		t.Fatalf("same approver twice: %v, want ErrDuplicateApproval", err)
	}
	// A second Person agreeing to a DIFFERENT payload does not complete the first payload's quorum.
	if r := e.a5MustDecide(t, s2, wr.ID, other, PayoutReversalRecorded); r.Counted != 1 {
		t.Fatalf("a different payload counts on its own: %+v", r)
	}
	txBefore := fpLedgerTx(t, e.pool, e.f)
	r := e.a5MustDecide(t, s3, wr.ID, p, PayoutReversalExecuted)
	if fpLedgerTx(t, e.pool, e.f) != txBefore+1 {
		t.Fatal("one posting")
	}
	// Retry of the executing call (lost response): a replay with the same posting, nothing new.
	rr := e.a5MustDecide(t, s3, wr.ID, p, PayoutReversalReplay)
	if rr.LedgerTransactionID == nil || *rr.LedgerTransactionID != *r.LedgerTransactionID || fpLedgerTx(t, e.pool, e.f) != txBefore+1 {
		t.Fatalf("replay must return the executed posting and post nothing: %+v", rr)
	}
	if _, err := e.a5Decide(s4, wr.ID, p); !errors.Is(err, withdrawal.ErrStateConflict) {
		t.Fatalf("a newcomer after execution: %v, want ErrStateConflict", err)
	}
	if _, err := e.a5Decide(s2, wr.ID, other); !errors.Is(err, withdrawal.ErrStateConflict) {
		t.Fatalf("the other payload's voter after execution: %v, want ErrStateConflict", err)
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
}

// Concurrency: eight distinct Persons decide at once. Exactly one reversal posts; every call either
// records, executes once, replays nothing, or meets the terminal state; no deadlock.
func TestA5_PayoutReversal_ConcurrentDecisions_ExactlyOnePosting(t *testing.T) {
	e := newRBEnv(t, "mock-a5-race")
	for round := 0; round < 3; round++ {
		key, ref := fmt.Sprintf("a5-r-%d", round), fmt.Sprintf("a5-ref-r-%d", round)
		wr, _ := e.a5Completed(t, key, ref)
		p := e.a5Payload(ref)
		const n = 8
		staff := make([]uuid.UUID, n)
		for i := range staff {
			staff[i] = e.a5Staff(t)
		}
		txBefore, cashBefore := fpLedgerTx(t, e.pool, e.f), e.a5Cash(t)
		var wg sync.WaitGroup
		outs := make([]PayoutReversalOutcome, n)
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				r, err := e.a5Decide(staff[i], wr.ID, p)
				outs[i], errs[i] = r.Outcome, err
			}(i)
		}
		close(start)
		wg.Wait()
		executed := 0
		for i := range outs {
			switch {
			case errs[i] == nil && outs[i] == PayoutReversalExecuted:
				executed++
			case errs[i] == nil && outs[i] == PayoutReversalRecorded:
			case errors.Is(errs[i], withdrawal.ErrStateConflict):
			default:
				t.Fatalf("round %d call %d: outcome %s err %v", round, i, outs[i], errs[i])
			}
		}
		if executed != 1 {
			t.Fatalf("round %d: exactly one call executes, got %d (%v)", round, executed, outs)
		}
		if got := fpLedgerTx(t, e.pool, e.f); got != txBefore+1 {
			t.Fatalf("round %d: exactly one posting, %d -> %d", round, txBefore, got)
		}
		if d := e.a5Cash(t) - cashBefore; d != 500 {
			t.Fatalf("round %d: credited exactly once, delta %d", round, d)
		}
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, e.pool, e.f.tenantID)
}

// Partial failure: the final approval's transaction fails after the posting (the caller's audit hook
// errors). Nothing survives: no decision row of that approver, no posting, state completed. The retry
// then executes cleanly.
func TestA5_PayoutReversal_PartialFailure_RollsBackEverything(t *testing.T) {
	e := newRBEnv(t, "mock-a5-partial")
	wr, _ := e.a5Completed(t, "a5-p", "a5-ref-p")
	p := e.a5Payload("a5-ref-p")
	s1, s2 := e.a5Staff(t), e.a5Staff(t)
	e.a5MustDecide(t, s1, wr.ID, p, PayoutReversalRecorded)
	txBefore, cashBefore := fpLedgerTx(t, e.pool, e.f), e.a5Cash(t)

	boom := errors.New("injected failure after the posting")
	sawExecuted := false
	_, err := DecidePayoutReversal(context.Background(), e.pool, e.f.tenantID, wr.ID, PayoutReversalInput{
		ApproverID: s2, Payload: p, Checks: a5Checks(wr.ID),
		Audit: func(ctx context.Context, tx pgx.Tx, res PayoutReversalResult) error {
			sawExecuted = res.Outcome == PayoutReversalExecuted
			return boom
		},
	})
	if !errors.Is(err, boom) || !sawExecuted {
		t.Fatalf("the failure must surface after the execution step: err=%v executed=%v", err, sawExecuted)
	}
	if got := fpReqState(t, e.pool, e.f, wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("state must roll back to completed, got %s", got.State)
	}
	if fpLedgerTx(t, e.pool, e.f) != txBefore || e.a5Cash(t) != cashBefore {
		t.Fatal("the posting must roll back")
	}
	if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id=$1 AND approver_principal_id=$2`, wr.ID, s2); n != 0 {
		t.Fatalf("the failed approver's decision must roll back, %d rows", n)
	}
	e.a5MustDecide(t, s2, wr.ID, p, PayoutReversalExecuted)
	loAssertBalanced(t, e.pool, e.f.tenantID)
}

// Authorization: every refusal records nothing.
func TestA5_PayoutReversal_AuthorizationRefusals(t *testing.T) {
	e := newRBEnv(t, "mock-a5-authz")
	wr, _ := e.a5Completed(t, "a5-z", "a5-ref-z")
	p := e.a5Payload("a5-ref-z")
	noRows := func(t *testing.T) {
		t.Helper()
		if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id=$1 AND decision='reverse'`, wr.ID); n != 0 {
			t.Fatalf("a refused call recorded %d decisions", n)
		}
	}
	t.Run("unlinked staff", func(t *testing.T) {
		// A finance role requires a linked Person in the database (SF014); a support login may be unlinked.
		s := uuid.New()
		if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status)
				VALUES ($1, $2, $3, 'x', 'support', 'active')`, s, e.f.tenantID, "a5-unlinked-"+s.String()+"@test.invalid")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.a5Decide(s, wr.ID, p); !errors.Is(err, withdrawal.ErrApproverNotLinked) {
			t.Fatalf("got %v", err)
		}
		noRows(t)
	})
	t.Run("unknown principal", func(t *testing.T) {
		if _, err := e.a5Decide(uuid.New(), wr.ID, p); !errors.Is(err, withdrawal.ErrApproverNotLinked) {
			t.Fatalf("got %v", err)
		}
		noRows(t)
	})
	t.Run("suspended staff", func(t *testing.T) {
		s := e.a5Staff(t)
		if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, s)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.a5Decide(s, wr.ID, p); !errors.Is(err, withdrawal.ErrApproverInactive) {
			t.Fatalf("got %v", err)
		}
		noRows(t)
	})
	t.Run("beneficiary", func(t *testing.T) {
		s := k3MkStaff(t, e.pool, e.f.tenantID, "finance", e.f.personID).ID
		if _, err := e.a5Decide(s, wr.ID, p); !errors.Is(err, withdrawal.ErrSelfApproval) {
			t.Fatalf("got %v", err)
		}
		noRows(t)
	})
	t.Run("payload mismatch", func(t *testing.T) {
		s := e.a5Staff(t)
		for name, mut := range map[string]func(*withdrawal.PayoutReversal){
			"amount":            func(q *withdrawal.PayoutReversal) { q.Amount = 499 },
			"asset":             func(q *withdrawal.PayoutReversal) { q.AssetCode = "USD" },
			"provider":          func(q *withdrawal.PayoutReversal) { q.ProviderID = "mock-other" },
			"original ref":      func(q *withdrawal.PayoutReversal) { q.OriginalProviderReference = "a5-not-the-ref" },
			"return equals ref": func(q *withdrawal.PayoutReversal) { q.ReturnReference = q.OriginalProviderReference },
			"bad evidence hash": func(q *withdrawal.PayoutReversal) { q.EvidenceRefHash = strings.ToUpper(a5EvidenceHash) },
		} {
			q := p
			mut(&q)
			_, err := e.a5Decide(s, wr.ID, q)
			if !errors.Is(err, withdrawal.ErrReversalPayloadMismatch) && !errors.Is(err, withdrawal.ErrInvalidInput) {
				t.Fatalf("%s: got %v", name, err)
			}
		}
		noRows(t)
	})
	t.Run("another tenant", func(t *testing.T) {
		of, oorch, _ := fpOrch(t, e.pool, "mock-a5-authz-other")
		o := rbEnv{pool: e.pool, f: of, orch: oorch, pid: "mock-a5-authz-other"}
		s := o.a5Staff(t)
		_, err := DecidePayoutReversal(context.Background(), e.pool, o.f.tenantID, wr.ID, PayoutReversalInput{ApproverID: s, Payload: p, Checks: a5Checks(wr.ID)})
		if !errors.Is(err, withdrawal.ErrNotFound) {
			t.Fatalf("another tenant's session must not see the request: %v", err)
		}
		noRows(t)
	})
	t.Run("one Person, two logins", func(t *testing.T) {
		person := uuid.New()
		a := k3MkStaff(t, e.pool, e.f.tenantID, "finance", person).ID
		b := k3MkStaff(t, e.pool, e.f.tenantID, "finance", person).ID
		e.a5MustDecide(t, a, wr.ID, p, PayoutReversalRecorded)
		if r := e.a5MustDecide(t, b, wr.ID, p, PayoutReversalRecorded); r.Counted != 1 {
			t.Fatalf("two logins of one Person count once: %+v", r)
		}
		if fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateCompleted {
			t.Fatal("one Person must never execute a reversal")
		}
	})
	t.Run("missing check closures", func(t *testing.T) {
		if _, err := DecidePayoutReversal(context.Background(), e.pool, e.f.tenantID, wr.ID, PayoutReversalInput{ApproverID: e.a5Staff(t), Payload: p}); !errors.Is(err, withdrawal.ErrInvalidInput) {
			t.Fatalf("got %v", err)
		}
	})
}

// The withdrawal policy's require_step_up applies to every reversal decision at any amount.
func TestA5_PayoutReversal_StepUpPolicy(t *testing.T) {
	e := newRBEnv(t, "mock-a5-stepup")
	wr, _ := e.a5Completed(t, "a5-s", "a5-ref-s")
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, require_step_up, effective_from)
			VALUES ($1, 'EUR', 1000000, 3, true, now() - interval '1 minute')`, e.f.tenantID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p := e.a5Payload("a5-ref-s")
	s1 := e.a5Staff(t)
	if _, err := e.a5Decide(s1, wr.ID, p); !errors.Is(err, withdrawal.ErrStepUpRequired) {
		t.Fatalf("no step-up: %v", err)
	}
	stale := stepup.Assertion{Subject: s1, SessionID: uuid.New(), MFAAt: time.Now().Add(-10 * time.Minute), Class: stepup.Financial}
	if _, err := DecidePayoutReversal(context.Background(), e.pool, e.f.tenantID, wr.ID, PayoutReversalInput{ApproverID: s1, Payload: p, Checks: a5Checks(wr.ID), StepUp: stale}); !errors.Is(err, withdrawal.ErrStepUpRequired) {
		t.Fatalf("stale step-up: %v", err)
	}
	decide := func(s uuid.UUID) PayoutReversalResult {
		fresh := stepup.Assertion{Subject: s, SessionID: uuid.New(), MFAAt: time.Now(), Class: stepup.Financial}
		r, err := DecidePayoutReversal(context.Background(), e.pool, e.f.tenantID, wr.ID, PayoutReversalInput{ApproverID: s, Payload: p, Checks: a5Checks(wr.ID), StepUp: fresh})
		if err != nil {
			t.Fatalf("fresh step-up: %v", err)
		}
		return r
	}
	// The policy asks for 3 approvals: max(2, 3).
	if r := decide(s1); r.Outcome != PayoutReversalRecorded || r.Required != 3 {
		t.Fatalf("%+v", r)
	}
	if r := decide(e.a5Staff(t)); r.Outcome != PayoutReversalRecorded {
		t.Fatalf("%+v", r)
	}
	if r := decide(e.a5Staff(t)); r.Outcome != PayoutReversalExecuted {
		t.Fatalf("%+v", r)
	}
}

// Reversal seen before the original (tombstone rule): the provider reports the return while the
// platform still has the payout `submitted` (attempt pending). The quorum writes the tombstone on the
// original reference, parks the attempt, releases the hold to the player and raises the P1. A late
// success for that reference then posts nothing, and the ledger refuses any posting on it.
func TestA5_PayoutReversal_BeforeCompletion_TombstoneParkRelease(t *testing.T) {
	e := newRBEnv(t, "mock-a5-tomb")
	ref := "a5-ref-t"
	wr, a := e.seedBound(t, "a5-t", ref)
	if a.State != AttemptPending {
		t.Fatalf("setup: want pending, got %s", a.State)
	}
	p := e.a5Payload(ref)
	cashBefore := e.a5Cash(t)
	e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalRecorded)
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptPending {
		t.Fatal("one decision must not park")
	}
	r := e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalTombstoned)
	if r.ParkedAttemptID == nil || *r.ParkedAttemptID != a.ID || r.LedgerTransactionID == nil {
		t.Fatalf("%+v", r)
	}
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != TerminalReasonTombstonePrecedesSuccess {
		t.Fatalf("attempt must be parked with the tombstone reason: %s %v", got.State, got.TerminalReason)
	}
	w := fpReqState(t, e.pool, e.f, wr.ID)
	if w.State != withdrawal.StateFailed || w.ReleaseLedgerTransactionID == nil {
		t.Fatalf("the hold must be released (E10): %s", w.State)
	}
	if d := e.a5Cash(t) - cashBefore; d != 500 {
		t.Fatalf("player_cash back by the hold, delta %d", d)
	}
	if n := e.a5Count(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1 AND transaction_type='tombstone' AND provider_id=$2 AND provider_tx_id=$3 AND correlation_id=$4`,
		e.f.tenantID, e.pid, ref, wr.ID); n != 1 {
		t.Fatalf("one tombstone on (provider, original reference), got %d", n)
	}
	e.b12AssertOneAlert(t, a.ID, TerminalReasonTombstonePrecedesSuccess, ref)
	if n := e.a5Count(t, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_reversal_tombstoned' AND target_id=$2`, e.f.tenantID, wr.ID.String()); n != 1 {
		t.Fatalf("tombstone audit = %d", n)
	}
	// A late success for the original reference: nothing posts, the request stays failed.
	txBefore := fpLedgerTx(t, e.pool, e.f)
	e.b12Callback(t, a, OutcomeSucceeded, ref, 500)
	if fpLedgerTx(t, e.pool, e.f) != txBefore || fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateFailed {
		t.Fatal("a late success after the tombstone must post nothing")
	}
	// The ledger itself refuses the original reference: (tenant, provider_id, provider_tx_id) UNIQUE.
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
			VALUES ($1, $2, 'withdrawal_completed', $3, $4, $5, $6)`, uuid.New(), e.f.tenantID, e.pid+":"+ref, e.pid, ref, wr.ID)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "idx_ledger_transactions_tenant_provider_tx" {
		t.Fatalf("a completion on the tombstoned reference must be refused by the provider unique index, got %v", err)
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, e.pool, e.f.tenantID)
}

// PAYOUT-AMOUNT-DISPUTE-1 x reversal: a success reporting a different amount parks the payout (no
// posting, hold kept); when the provider then returns the payout, the governed reversal releases the
// hold through the tombstone branch (the attempt is already parked: no second park). A park whose
// reference the platform never held (a payout park stays reference-less, ledger-finance C1) cannot be
// tombstoned: the reversal is refused and the existing unbound-park exit (M4 "not paid") applies.
func TestA5_PayoutReversal_AmountDisputeThenReturn(t *testing.T) {
	e := newRBEnv(t, "mock-a5-aad")
	t.Run("reference never held: refused", func(t *testing.T) {
		wr, a := e.claim(t, "a5-aad-unbound")
		e.b12Callback(t, a, OutcomeSucceeded, "a5-ref-aad-unbound", 499)
		if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptDisputed || got.ProviderReference != nil {
			t.Fatalf("setup: an unbound park: %s %v", got.State, got.ProviderReference)
		}
		if _, err := e.a5Decide(e.a5Staff(t), wr.ID, e.a5Payload("a5-ref-aad-unbound")); !errors.Is(err, withdrawal.ErrReversalPayloadMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	ref := "a5-ref-aad"
	wr, a := e.seedBound(t, "a5-aad", ref)
	txBefore := fpLedgerTx(t, e.pool, e.f)
	e.b12Callback(t, a, OutcomeSucceeded, ref, 499)
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || *got.TerminalReason != TerminalReasonCallbackAmountAssetMismatch {
		t.Fatalf("setup: the mismatch must park: %s %v", got.State, got.TerminalReason)
	}
	if fpLedgerTx(t, e.pool, e.f) != txBefore || fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateSubmitted {
		t.Fatal("a mismatched amount must never post")
	}
	p := e.a5Payload(ref)
	e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalRecorded)
	r := e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalTombstoned)
	if r.ParkedAttemptID == nil || *r.ParkedAttemptID != a.ID {
		t.Fatalf("%+v", r)
	}
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); *got.TerminalReason != TerminalReasonCallbackAmountAssetMismatch {
		t.Fatalf("the original park reason is kept: %v", got.TerminalReason)
	}
	if w := fpReqState(t, e.pool, e.f, wr.ID); w.State != withdrawal.StateFailed {
		t.Fatalf("hold released, got %s", w.State)
	}
	// Only the original mismatch alert of THIS attempt: no second alert for an attempt already parked.
	n := 0
	for _, row := range e.b12Rows(t) {
		if strings.Contains(row.Discriminator, a.ID.String()) {
			n++
			if row.Discriminator != "payout_attempt:"+a.ID.String()+":reason:"+TerminalReasonCallbackAmountAssetMismatch {
				t.Fatalf("unexpected alert %s", row.Discriminator)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want exactly the original mismatch alert, got %d", n)
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
}

// An attempt that may be dispatching right now refuses the reversal (nothing recorded).
func TestA5_PayoutReversal_AttemptInFlight_Refused(t *testing.T) {
	e := newRBEnv(t, "mock-a5-flight")
	wr, _ := e.claim(t, "a5-f")
	// The request is submitted with the attempt `submitting` and no reference: no payload can even match.
	p := e.a5Payload("a5-ref-f")
	if _, err := e.a5Decide(e.a5Staff(t), wr.ID, p); !errors.Is(err, ErrPayoutReversalAttemptInFlight) {
		t.Fatalf("got %v", err)
	}
	if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id=$1 AND decision='reverse'`, wr.ID); n != 0 {
		t.Fatalf("recorded %d", n)
	}
}
