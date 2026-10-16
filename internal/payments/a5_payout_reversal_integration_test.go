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

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/capability"
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
	// A payload that differs ONLY in its evidence hash (same references) is a different payload too.
	evid := p
	evid.EvidenceRefHash = strings.Repeat("0e", 32)
	s5 := e.a5Staff(t)
	if r := e.a5MustDecide(t, s5, wr.ID, evid, PayoutReversalRecorded); r.Counted != 1 {
		t.Fatalf("an evidence-only difference counts on its own: %+v", r)
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
	// Same references as the executed reversal, but a payload that never reached the quorum: not a replay.
	if _, err := e.a5Decide(s5, wr.ID, evid); !errors.Is(err, withdrawal.ErrStateConflict) {
		t.Fatalf("the evidence-only payload's voter after execution: %v, want ErrStateConflict", err)
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

// a5K3 is the tombstone-branch environment: the K3 world (finance staff f1..f4 WITH in-force
// payment_force_resolve:request|approve grants, financeNoGrant WITHOUT) seen through the rbEnv helpers.
func a5K3(t *testing.T) (rbEnv, *k3World) {
	t.Helper()
	w := newK3World(t, k3Opts{base: 1})
	return rbEnv{pool: w.pool, f: w.f, orch: w.orch, pid: w.provider}, w
}

// Reversal seen before the original (tombstone rule): the provider reports the return while the
// platform still has the payout `submitted` (attempt pending). Fix round SR5-1 / LF-A5-1: each decider
// must hold payment_force_resolve:approve (Go and WD027). The quorum writes the tombstone on the
// original reference, parks the attempt, releases the hold to the player and raises the P1. A late
// success for that reference then posts nothing but writes its own audit row and a DISTINCT P1; the
// ledger refuses any posting on the reference with the typed ErrProviderReferenceTaken.
func TestA5_PayoutReversal_BeforeCompletion_TombstoneParkRelease(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	ref := *a.ProviderReference
	p := e.a5Payload(ref)
	cashBefore := e.a5Cash(t)
	// A decider without the force-resolve grant cannot take the tombstone branch at all.
	if _, err := e.a5Decide(w.financeNoGrant.ID, wr.ID, p); !errors.Is(err, withdrawal.ErrReversalCapabilityRequired) {
		t.Fatalf("no grant: %v, want ErrReversalCapabilityRequired", err)
	}
	e.a5MustDecide(t, w.f1.ID, wr.ID, p, PayoutReversalRecorded)
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptPending {
		t.Fatal("one decision must not park")
	}
	r := e.a5MustDecide(t, w.f2.ID, wr.ID, p, PayoutReversalTombstoned)
	if r.ParkedAttemptID == nil || *r.ParkedAttemptID != a.ID || r.LedgerTransactionID == nil {
		t.Fatalf("%+v", r)
	}
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != TerminalReasonTombstonePrecedesSuccess {
		t.Fatalf("attempt must be parked with the tombstone reason: %s %v", got.State, got.TerminalReason)
	}
	wNow := fpReqState(t, e.pool, e.f, wr.ID)
	if wNow.State != withdrawal.StateFailed || wNow.ReleaseLedgerTransactionID == nil {
		t.Fatalf("the hold must be released (E10): %s", wNow.State)
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
	// LF-A5-1: a late success for the original reference posts nothing, keeps the request failed, writes
	// ONE audit row (redelivery: no second row) and raises a DISTINCT P1.
	txBefore := fpLedgerTx(t, e.pool, e.f)
	for i := 0; i < 2; i++ {
		e.b12Callback(t, a, OutcomeSucceeded, ref, 500)
	}
	if fpLedgerTx(t, e.pool, e.f) != txBefore || fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateFailed {
		t.Fatal("a late success after the tombstone must post nothing")
	}
	if n := e.b12AuditCount(t, "payments.payout_success_after_reversal_tombstone", a.ID); n != 1 {
		t.Fatalf("late-success audit rows = %d, want 1 (once per new receipt)", n)
	}
	disc := map[string]int{}
	for _, row := range e.b12Rows(t) {
		disc[row.Discriminator] = row.Occurrences
	}
	lateDisc := "payout_attempt:" + a.ID.String() + ":reason:" + alertReasonPayoutSuccessAfterReversalTombstone
	if len(disc) != 2 || disc[lateDisc] < 1 {
		t.Fatalf("want the park P1 AND a distinct late-success P1, got %v", disc)
	}
	// The status-poll source reaches the same cell: posts nothing, one more audit row (every poll is a new
	// observation), the same distinct P1 counted again.
	e.r8StatusSuccess(t, wr, mustGetAttempt(t, e.pool, e.f.tenantID, a.ID), ref, 500, "EUR")
	if fpLedgerTx(t, e.pool, e.f) != txBefore || fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateFailed {
		t.Fatal("a polled success after the tombstone must post nothing")
	}
	if n := e.b12AuditCount(t, "payments.payout_success_after_reversal_tombstone", a.ID); n != 2 {
		t.Fatalf("late-success audit rows after a poll = %d, want 2", n)
	}
	occ := 0
	for _, row := range e.b12Rows(t) {
		if row.Discriminator == lateDisc {
			occ = row.Occurrences
		}
	}
	if occ <= disc[lateDisc] {
		t.Fatalf("the poll must raise the late-success P1 again: %d -> %d", disc[lateDisc], occ)
	}
	// The third source: a success that lost its CAS to the park (applyPayoutLateEvidence, the attempt
	// re-read as disputed) reaches the same cell.
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		return applyPayoutLateEvidence(ctx, tx, a, EvidenceCallback, "late_success_after_terminal", wr.ID, OutcomeSucceeded, ref)
	})
	if err != nil {
		t.Fatalf("late evidence after the tombstone: %v", err)
	}
	pending.Flush(context.Background())
	if n := e.b12AuditCount(t, "payments.payout_success_after_reversal_tombstone", a.ID); n != 3 {
		t.Fatalf("late-success audit rows after a lost CAS = %d, want 3", n)
	}
	if fpLedgerTx(t, e.pool, e.f) != txBefore {
		t.Fatal("a late success after the tombstone must post nothing")
	}
	// The ledger refuses the original reference: (tenant, provider_id, provider_tx_id) UNIQUE, typed.
	err = e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
			VALUES ($1, $2, 'withdrawal_completed', $3, $4, $5, $6)`, uuid.New(), e.f.tenantID, e.pid+":"+ref, e.pid, ref, wr.ID)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "idx_ledger_transactions_tenant_provider_tx" {
		t.Fatalf("a completion on the tombstoned reference must be refused by the provider unique index, got %v", err)
	}
	err = e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, e.f.tenantID, &e.f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		clr, err := ledger.GetOrCreateAccount(ctx, tx, e.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		pid, rr := e.pid, ref
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: e.f.tenantID, TransactionType: ledger.TxDeposit,
			IdempotencyKey: "a5-late-" + uuid.NewString(), ProviderID: &pid, ProviderTxID: &rr, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{{LedgerAccountID: clr, Direction: ledger.Debit, Amount: 500}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 500}}})
		return err
	})
	if !errors.Is(err, ledger.ErrProviderReferenceTaken) {
		t.Fatalf("ledger.Post on a held provider reference: %v, want ErrProviderReferenceTaken", err)
	}
	w.assertInvariants()
}

// PAYOUT-AMOUNT-DISPUTE-1 x reversal: a success reporting a different amount parks the payout (no
// posting, hold kept); when the provider then returns the payout, the governed reversal (force-resolve
// grant holders) releases the hold through the tombstone branch (the attempt is already parked: no second
// park). A park whose reference the platform never held (a payout park stays reference-less,
// ledger-finance C1) cannot be tombstoned: the reversal is refused (the M4 unbound-park exit applies).
func TestA5_PayoutReversal_AmountDisputeThenReturn(t *testing.T) {
	e, w := a5K3(t)
	t.Run("reference never held: refused", func(t *testing.T) {
		wr, a := e.claim(t, "a5-aad-unbound")
		e.b12Callback(t, a, OutcomeSucceeded, "a5-ref-aad-unbound", 499)
		if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptDisputed || got.ProviderReference != nil {
			t.Fatalf("setup: an unbound park: %s %v", got.State, got.ProviderReference)
		}
		if _, err := e.a5Decide(w.f1.ID, wr.ID, e.a5Payload("a5-ref-aad-unbound")); !errors.Is(err, withdrawal.ErrReversalPayloadMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	wr, a := w.payout(500)
	ref := *a.ProviderReference
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
	e.a5MustDecide(t, w.f1.ID, wr.ID, p, PayoutReversalRecorded)
	r := e.a5MustDecide(t, w.f2.ID, wr.ID, p, PayoutReversalTombstoned)
	if r.ParkedAttemptID == nil || *r.ParkedAttemptID != a.ID {
		t.Fatalf("%+v", r)
	}
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); *got.TerminalReason != TerminalReasonCallbackAmountAssetMismatch {
		t.Fatalf("the original park reason is kept: %v", got.TerminalReason)
	}
	if wNow := fpReqState(t, e.pool, e.f, wr.ID); wNow.State != withdrawal.StateFailed {
		t.Fatalf("hold released, got %s", wNow.State)
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
	w.assertInvariants()
}

// SR5-3: a return reference the platform already holds for the provider (here another payout's own
// reference) is refused before any vote is recorded, so it can never occupy that payout's reference.
func TestA5_PayoutReversal_ReturnReferenceAlreadyHeld_Refused(t *testing.T) {
	e, w := a5K3(t)
	_, other := w.payout(300) // its reference is held by its attempt and its withdrawal
	wr, _ := e.a5Completed(t, "a5-rr", "a5-ref-rr")
	p := e.a5Payload("a5-ref-rr")
	p.ReturnReference = *other.ProviderReference
	if _, err := e.a5Decide(w.f1.ID, wr.ID, p); !errors.Is(err, withdrawal.ErrReversalReturnReferenceInUse) {
		t.Fatalf("got %v, want ErrReversalReturnReferenceInUse", err)
	}
	// Each source on its own: a reference held only by a payment attempt (a pending deposit of the same
	// provider), and one held only by a ledger posting (a tombstone with an unrelated correlation).
	_, depRef := pendingDeposit(t, e.pool, w.orch, w.f.orchFixture, "a5-rr-dep-"+uuid.NewString()[:8], 700)
	ledgerOnly := "a5-rr-ledger-" + uuid.NewString()[:8]
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		pid := e.pid
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: e.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:" + pid + ":" + ledgerOnly, ProviderID: &pid, ProviderTxID: &ledgerOnly, CorrelationID: uuid.New()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, held := range []string{depRef, ledgerOnly} {
		p.ReturnReference = held
		if _, err := e.a5Decide(w.f1.ID, wr.ID, p); !errors.Is(err, withdrawal.ErrReversalReturnReferenceInUse) {
			t.Fatalf("%s: got %v, want ErrReversalReturnReferenceInUse", held, err)
		}
	}
	// Its own original reference (held by its completion) is refused the same way.
	p.ReturnReference = "a5-ref-rr-x"
	q := p
	q.ReturnReference = "a5-ref-rr" // == the completion's provider_tx_id is already refused by Validate (return != original)
	if _, err := e.a5Decide(w.f1.ID, wr.ID, q); !errors.Is(err, withdrawal.ErrInvalidInput) {
		t.Fatalf("got %v", err)
	}
	if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id=$1 AND decision='reverse'`, wr.ID); n != 0 {
		t.Fatalf("a refused reference recorded %d decisions", n)
	}
	e.a5MustDecide(t, w.f1.ID, wr.ID, p, PayoutReversalRecorded)
}

// SR5-1: on the tombstone branch a vote counts only while its voter STILL holds payment_force_resolve:approve.
// f1 votes, f1's grant is revoked, f2's vote alone does not execute (the Go count re-checks the grant at
// execution; WD027 restates it); f3 completes the quorum.
func TestA5_PayoutReversal_TombstoneVoterGrantRevoked_NoLongerCounts(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	p := e.a5Payload(*a.ProviderReference)
	e.a5MustDecide(t, w.f1.ID, wr.ID, p, PayoutReversalRecorded)
	w.revokeGrant(w.f1.ID, capability.CapabilityPaymentForceResolveApprove)
	if r := e.a5MustDecide(t, w.f2.ID, wr.ID, p, PayoutReversalRecorded); r.Counted != 1 {
		t.Fatalf("a voter whose grant was revoked must not count: %+v", r)
	}
	if fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateSubmitted {
		t.Fatal("no tombstone with one grant holder")
	}
	e.a5MustDecide(t, w.f3.ID, wr.ID, p, PayoutReversalTombstoned)
	w.assertInvariants()
}

// WD027 at the database, past the Go gates: one grant holder's recorded decision plus a raw decision row of a
// finance Person WITHOUT the grant (same digest) is one counted Person, so a tombstone correlated to the
// request is refused (SQLSTATE WD027) even in the deciders' principal scope.
func TestA5_PayoutReversal_WD027_OneGrantHolderIsNotAQuorum(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	ref := *a.ProviderReference
	e.a5MustDecide(t, w.f1.ID, wr.ID, e.a5Payload(ref), PayoutReversalRecorded)
	err := e.pool.WithPrincipalScope(context.Background(), e.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		var reason string
		if err := tx.QueryRow(ctx, `SELECT reason_code FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'reverse'`, wr.ID).Scan(&reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval,
			decision, reason_code, threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, false, 'reverse', $4, 0, 500)`,
			e.f.tenantID, wr.ID, w.financeNoGrant.ID, reason); err != nil {
			return err
		}
		pid := e.pid
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: e.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:" + pid + ":" + ref, ProviderID: &pid, ProviderTxID: &ref, CorrelationID: wr.ID})
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "WD027" || !strings.Contains(pgErr.Message, "(found 1)") {
		t.Fatalf("one grant holder + one non-holder: want WD027 with one counted Person, got %v", err)
	}
}

// SR5-4 / LF-A5-3: a vote counts only while its voter is ACTIVE staff. The first voter is suspended, so
// the second vote alone does not execute (Go count); a third active Person completes the quorum.
func TestA5_PayoutReversal_SuspendedVoterNoLongerCounts(t *testing.T) {
	e := newRBEnv(t, "mock-a5-susp")
	wr, _ := e.a5Completed(t, "a5-su", "a5-ref-su")
	p := e.a5Payload("a5-ref-su")
	s1 := e.a5Staff(t)
	e.a5MustDecide(t, s1, wr.ID, p, PayoutReversalRecorded)
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, s1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalRecorded); r.Counted != 1 {
		t.Fatalf("the suspended voter must not count: %+v", r)
	}
	if fpReqState(t, e.pool, e.f, wr.ID).State != withdrawal.StateCompleted {
		t.Fatal("no execution with one active Person")
	}
	e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalExecuted)
}

// The four-eyes floor is unconditional: a withdrawal policy asking for ONE approval still needs two
// distinct Persons for a reversal (ADR 0100 s3.4 interim (b); the database floor WD026 agrees).
func TestA5_PayoutReversal_FloorOfTwoEvenWhenThePolicyAsksForOne(t *testing.T) {
	e := newRBEnv(t, "mock-a5-floor")
	wr, _ := e.a5Completed(t, "a5-fl", "a5-ref-fl")
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			VALUES ($1, 'EUR', 1000000, 1, now() - interval '1 minute')`, e.f.tenantID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p := e.a5Payload("a5-ref-fl")
	if r := e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalRecorded); r.Required != withdrawal.MinReversalApprovers {
		t.Fatalf("required = %d, want the floor %d", r.Required, withdrawal.MinReversalApprovers)
	}
	e.a5MustDecide(t, e.a5Staff(t), wr.ID, p, PayoutReversalExecuted)
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
