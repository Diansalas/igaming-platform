//go:build integration

// RV-PRH-I1 callback-cutover fix round regression tests. These are the
// permanent, committed form of the ledger-finance review's probes
// (docs/plans/payment-readiness/rv-prh-i1-callback-ledger.md, findings
// H1-H4/M1-M5/L1) and the independent code review's overlapping findings
// (docs/plans/payment-readiness/rv-prh-i1-callback-code-review.md, F1/F3/
// F4). Each test name keeps the reviewer's own probe id (P1, P1b, ...) so
// it stays traceable to the finding it regresses.
package payments

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func rvInit(t *testing.T, pool *db.Pool, orch *Orchestrator, f orchFixture, amount int64, key string) InitiateDepositAttemptResult {
	t.Helper()
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	return res
}

func rvCallback(pool *db.Pool, orch *Orchestrator, f orchFixture, providerID string, in InboundCallback) (ReceiveCallbackResult, error) {
	var r ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, providerID, in)
		return err
	})
	return r, err
}

func rvQueryInt(t *testing.T, pool *db.Pool, f orchFixture, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// P1 (H1, CORRECTED): a deposit_reversal event's wire Outcome field is NOT
// a "did this reversal succeed" signal on the current adapter contract -
// MockProvider.HandleCallback requires it to be one of the four enumerated
// values for every event, and it is deliberately reused as a chargeback/
// refund REASON carrier for a reversal (every existing reversal fixture in
// this package sends OutcomeDeclined with a DeclineReason like
// "chargeback"/"chargeback_lost" for a reversal that DID happen and must
// be applied). An earlier version of this fix round misread H1 as "gate
// posting on wireOutcome == succeeded", which broke every genuine
// reversal (TestReceiveCallback_DepositReversalPostsFlow2 and its
// siblings). This test pins the CORRECTED behavior: a reversal posts
// (debits player_cash, stored as outcome=succeeded) regardless of which
// of the four wire Outcome values carries the reason.
func TestRVLF_P1_ReversalPostsRegardlessOfWireOutcomeReasonCarrier(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv1", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv1": p}, MultiWebhookCredentialResolver{"mock-rv1": NewMockWebhookCredentials(p)})

	for i, oc := range []Outcome{OutcomeDeclined, OutcomePending, OutcomeAmbiguous, OutcomeSucceeded} {
		res := rvInit(t, pool, orch, f, 5000, "p1-"+string(oc))
		ref := *res.Attempt.ProviderReference
		if _, err := rvCallback(pool, orch, f, "mock-rv1", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
			t.Fatalf("success callback: %v", err)
		}
		before := cashBalance(t, pool, f)
		revRef := "rev-p1-" + string(oc)
		r, err := rvCallback(pool, orch, f, "mock-rv1", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, revRef, ref, oc, 5000, "EUR", "chargeback", false))
		if err != nil {
			t.Fatalf("case %d (%s) reversal callback: %v", i, oc, err)
		}
		after := cashBalance(t, pool, f)
		var stored string
		_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT outcome FROM payment_provider_events WHERE provider_reference = $1`, revRef).Scan(&stored)
		})
		t.Logf("case %d wire outcome=%s: disposition=%s balance %d -> %d stored_outcome=%q", i, oc, r.Disposition, before, after, stored)
		if after != before-5000 {
			t.Errorf("H1 (corrected): a deposit_reversal carrying wire outcome %q must post (debit 5000), got %d -> %d", oc, before, after)
		}
		if stored != string(OutcomeSucceeded) {
			t.Errorf("H1 (corrected): an applied reversal must be stored as outcome=succeeded regardless of the wire reason-carrier value, got %q", stored)
		}
		if r.Disposition != DispositionApplied {
			t.Errorf("H1 (corrected): a genuine reversal must resolve to disposition=applied, got %s", r.Disposition)
		}
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// P1b (H1, CORRECTED): a reversal for a still-pending (never posted)
// deposit legitimately writes a tombstone regardless of which wire Outcome
// value it carries as its reason - this is Flow 2's intended defense
// against a late-arriving original deposit success being posted after the
// deposit was already reversed, not a bug.
func TestRVLF_P1b_ReversalOfNeverPostedDepositTombstonesRegardlessOfReasonCarrier(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv1b", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv1b": p}, MultiWebhookCredentialResolver{"mock-rv1b": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "p1b")
	ref := *res.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-rv1b", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-p1b", ref, OutcomeDeclined, 0, "", "chargeback_lost", false)); err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv1b", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("late success after tombstone: %v", err)
	}
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	bal := cashBalance(t, pool, f)
	t.Logf("after reversal-precedes-deposit then late success: attempt=%s balance=%d", a.State, bal)
	if bal != 0 || a.State != AttemptDisputed {
		t.Errorf("H1 (corrected): a reversal preceding the original deposit must tombstone it, blocking the late success (state=%s balance=%d)", a.State, bal)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// P2 (H3): an engaged kill switch must never turn a cascadable decline
// callback into an error - the decline still commits; only the cascade
// child is skipped. A success callback under the same switch still
// applies (control).
func TestRVLF_P2_KillSwitch_CascadableDeclineCallback(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv2", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv2": p}, MultiWebhookCredentialResolver{"mock-rv2": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "p2")
	ref := *res.Attempt.ProviderReference
	res2 := rvInit(t, pool, orch, f, 6000, "p2-success")
	ref2 := *res2.Attempt.ProviderReference

	principal := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,$2,$3,'x','tenant_admin','active')`,
			principal, f.tenantID, principal.String()+"@rv.example")
		return err
	}); err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, "*", KillSwitchOperationDeposit, "incident")
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}

	_, err := rvCallback(pool, orch, f, "mock-rv2", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", "provider_unavailable", true))
	if err != nil {
		t.Fatalf("H3: a decline callback for an already-submitted attempt must never fail while a kill switch is engaged (ADR 0095 §10.3): %v", err)
	}
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if a.State != AttemptDeclined {
		t.Errorf("H3: the decline itself must still commit under an engaged switch, got state=%s", a.State)
	}
	var childCount int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 2`, res.Intent.ID).Scan(&childCount)
	})
	if childCount != 0 {
		t.Errorf("H3: a cascade child must never be inserted while the kill switch is engaged, got %d", childCount)
	}

	// Control: a success callback under the same switch still applies -
	// ADR 0095 §10.3 "never stopped: callbacks, QueryStatus polls".
	if _, err := rvCallback(pool, orch, f, "mock-rv2", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref2, "", OutcomeSucceeded, 6000, "EUR", "", false)); err != nil {
		t.Errorf("control: success callback under switch failed: %v", err)
	}
}

// P3 (H4): a T13 success must reject any leftover 'created' cascade
// sibling in the SAME transaction, and a T2 claim of that sibling must be
// refused afterward - never a second real PSP charge.
func TestRVLF_P3_T13RejectsCreatedSibling(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv3", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv3": p}, MultiWebhookCredentialResolver{"mock-rv3": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "p3")
	ref := *res.Attempt.ProviderReference

	if _, err := rvCallback(pool, orch, f, "mock-rv3", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", "provider_unavailable", true)); err != nil {
		t.Fatalf("decline: %v", err)
	}
	var childID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 2`, res.Intent.ID).Scan(&childID)
	}); err != nil {
		t.Fatalf("expected a cascade child: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv3", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("late success (T13): %v", err)
	}
	child := mustGetAttempt(t, pool, f.tenantID, childID)
	t.Logf("after T13: balance=%d child state=%s", cashBalance(t, pool, f), child.State)
	if child.State != AttemptRejected {
		t.Errorf("H4/T13(c): the sibling 'created' attempt must move to 'rejected' (intent_succeeded); got %s", child.State)
	}
	// No second PSP call: a T2 claim of the leftover child must be refused.
	claimErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, childID, "mock-rv3", uuid.New(), "rv", time.Now().Add(time.Minute))
	})
	if claimErr == nil {
		t.Errorf("H4: T2 claim of a cascade child must be refused once its intent has already succeeded - a second provider charge would otherwise follow")
	}
	if bal := cashBalance(t, pool, f); bal != 5000 {
		t.Errorf("H4: exactly one capture must remain posted, got balance=%d", bal)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// P4 (M1): mismatched-amount success evidence on an already-terminal
// (succeeded or declined) attempt must be recorded as an anomaly and
// answered without error - never a CAS-conflict 5xx-redelivery loop.
func TestRVLF_P4_MismatchedSuccessOnTerminalAttempt(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv4", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv4": p}, MultiWebhookCredentialResolver{"mock-rv4": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "p4")
	ref := *res.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-rv4", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("success: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv4", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 4999, "EUR", "", false)); err != nil {
		t.Errorf("M1: §4.4 'succeeded x mismatch' must be an anomaly receipt + 200, not a rollback error: %v", err)
	}
	res2 := rvInit(t, pool, orch, f, 5000, "p4b")
	ref2 := *res2.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-rv4", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref2, "", OutcomeDeclined, 0, "", "insufficient_funds", false)); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv4", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref2, "", OutcomeSucceeded, 1, "EUR", "", false)); err != nil {
		t.Errorf("M1: §4.4 'declined x mismatch' must be an anomaly receipt + 200, not a rollback error: %v", err)
	}
	if b := cashBalance(t, pool, f); b != 5000 {
		t.Fatalf("a mismatched success must never post; balance=%d", b)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// P5 (M2/F1): applied reversal and tombstone receipts must be resolved in
// the same transaction, or they count toward the §6.1 step 5 unapplied-
// receipt cap forever.
func TestRVLF_P5_ReversalReceiptsResolved(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv5", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv5": p}, MultiWebhookCredentialResolver{"mock-rv5": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "p5")
	ref := *res.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-rv5", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("success: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv5", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-p5", ref, OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv5", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-p5-tomb", "unseen-original", OutcomeSucceeded, 100, "EUR", "", false)); err != nil {
		t.Fatalf("tombstone reversal: %v", err)
	}
	var n int
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-rv5", DeferredReceiptCap)
		return err
	})
	unresolvedRev := rvQueryInt(t, pool, f, `SELECT count(*) FROM payment_provider_events WHERE event_type='deposit_reversal' AND resolved_at IS NULL`)
	t.Logf("unapplied count for cap=%d, unresolved reversal receipts=%d", n, unresolvedRev)
	if unresolvedRev != 0 {
		t.Errorf("M2/F1: applied deposit_reversal receipts must be resolved in the same transaction; found %d left resolved_at IS NULL (CountUnappliedReceipts=%d)", unresolvedRev, n)
	}
}

// P6 (L1/F5-M4): a reversal of a T13 second capture must reverse the
// SECOND capture's own ledger transaction, never the first; PAY-REV-1's
// single-reversal-per-original guarantee still holds; the first capture
// remains independently reversible exactly once.
func TestRVLF_P6_ReversalOfSecondCaptureReversesItsOwnTransaction(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("mock-rv6-a", "EUR")
	pb := NewMockProvider("mock-rv6-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv6-a": pa, "mock-rv6-b": pb},
		MultiWebhookCredentialResolver{"mock-rv6-a": NewMockWebhookCredentials(pa), "mock-rv6-b": NewMockWebhookCredentials(pb)})
	amt := int64(MockAmountProviderDeclineCascade)
	res := rvInit(t, pool, orch, f, amt, "p6")
	childRef := *res.Attempt.ProviderReference
	var parentID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 1`, res.Intent.ID).Scan(&parentID)
	}); err != nil {
		t.Fatal(err)
	}
	parentRef := *mustGetAttempt(t, pool, f.tenantID, parentID).ProviderReference

	if _, err := rvCallback(pool, orch, f, "mock-rv6-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("child success: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv6-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, parentRef, "", OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("parent late success (T13): %v", err)
	}
	child := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	parent := mustGetAttempt(t, pool, f.tenantID, parentID)
	if cashBalance(t, pool, f) != 2*amt {
		t.Fatalf("expected two captures")
	}
	// Reverse the SECOND capture (the parent's T13 posting).
	if _, err := rvCallback(pool, orch, f, "mock-rv6-a", pa.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-p6-second", parentRef, OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("reversal of second capture: %v", err)
	}
	var reverses uuid.UUID
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reverses_transaction_id FROM ledger_transactions WHERE provider_tx_id = 'rev-p6-second'`).Scan(&reverses)
	})
	if reverses != *parent.LedgerTransactionID {
		t.Errorf("L1: reversal of the second capture reversed %s; want the parent's own %s (first capture is %s)", reverses, *parent.LedgerTransactionID, *child.LedgerTransactionID)
	}
	// PAY-REV-1: a distinct second reversal of the same original is refused.
	_, err := rvCallback(pool, orch, f, "mock-rv6-a", pa.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-p6-second-b", parentRef, OutcomeSucceeded, amt, "EUR", "", false))
	if !errors.Is(err, ErrDepositAlreadyReversed) {
		t.Errorf("PAY-REV-1 not preserved: %v", err)
	}
	// The first capture remains independently reversible exactly once.
	if _, err := rvCallback(pool, orch, f, "mock-rv6-b", pb.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-p6-first", childRef, OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Errorf("reversal of the first capture: %v", err)
	}
	if b := cashBalance(t, pool, f); b != 0 {
		t.Errorf("balance after both reversals = %d", b)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// P7: out-of-order reversal deliveries before any deposit posted, then two
// late success deliveries - ends disputed/ambiguous with a single
// tombstone, never a double post and never an error on redelivery.
func TestRVLF_P7_ReplayOrderings(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv7", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv7": p}, MultiWebhookCredentialResolver{"mock-rv7": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "p7")
	ref := *res.Attempt.ProviderReference
	for _, rr := range []string{"rev-p7-a", "rev-p7-b", "rev-p7-a"} {
		if _, err := rvCallback(pool, orch, f, "mock-rv7", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, rr, ref, OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
			t.Errorf("tombstone reversal %s: %v", rr, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := rvCallback(pool, orch, f, "mock-rv7", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
			t.Errorf("late success %d: %v", i, err)
		}
	}
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	var status string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, res.Intent.ID).Scan(&status)
	})
	tombs := rvQueryInt(t, pool, f, `SELECT count(*) FROM ledger_transactions WHERE transaction_type='tombstone'`)
	t.Logf("attempt=%s intent=%s balance=%d tombstones=%d", a.State, status, cashBalance(t, pool, f), tombs)
	if a.State != AttemptDisputed || status != string(DepositIntentAmbiguous) || cashBalance(t, pool, f) != 0 || tombs != 1 {
		t.Errorf("unexpected: attempt=%s intent=%s balance=%d tombstones=%d", a.State, status, cashBalance(t, pool, f), tombs)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// racingCallbackProvider delivers the verified success callback for the
// reference it is about to return BEFORE phase C's own T4 commits - the
// ordinary "webhook beats the sync response" race (ADR 0095 §6.4 row 2).
type racingCallbackProvider struct {
	*MockProvider
	onRef func(ref string)
}

func (p *racingCallbackProvider) Deposit(ctx context.Context, req DepositRequest) (DepositResult, error) {
	res, err := p.MockProvider.Deposit(ctx, req)
	if err == nil && res.ProviderReference != "" {
		p.onRef(res.ProviderReference)
	}
	return res, err
}

// P8 (H2): a verified success that races phase C's own T4 transition must
// still be applied by phase C's ApplyDeferredReceiptsForAttempt backstop -
// never left permanently deferred_unresolved.
func TestRVLF_P8_DeferredReceiptAppliedByPhaseC(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	mp := NewMockProvider("mock-rv8", "EUR")
	registerCapability(t, pool, f, mp, 100)
	rp := &racingCallbackProvider{MockProvider: mp}
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv8": rp}, MultiWebhookCredentialResolver{"mock-rv8": NewMockWebhookCredentials(mp)})
	var raceDisp ReceiptDisposition
	var raceErr error
	rp.onRef = func(ref string) {
		r, err := rvCallback(pool, orch, f, "mock-rv8", mp.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
		raceDisp, raceErr = r.Disposition, err
	}
	res := rvInit(t, pool, orch, f, 5000, "p8")
	if raceErr != nil {
		t.Fatalf("race callback: %v", raceErr)
	}
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	unresolved := rvQueryInt(t, pool, f, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = $1 AND resolved_at IS NULL`, *a.ProviderReference)
	t.Logf("race callback disposition=%s; after phase C T4: attempt=%s balance=%d unresolved_receipts=%d", raceDisp, a.State, cashBalance(t, pool, f), unresolved)
	if a.State != AttemptSucceeded {
		t.Errorf("H2: a verified success that raced phase C must be applied by phase C's own T4 backstop (ADR 0095 §6.4); attempt=%s balance=%d unresolved_receipts=%d", a.State, cashBalance(t, pool, f), unresolved)
	}
	if unresolved != 0 {
		t.Errorf("H2: the raced receipt must be resolved once applied, got %d still unresolved", unresolved)
	}
	if bal := cashBalance(t, pool, f); bal != 5000 {
		t.Errorf("H2: the raced success must actually post, balance=%d", bal)
	}
	// N12 (independent code review): the deferred-apply backstop's own
	// projection recompute must run too - the intent's own status column
	// is a SEPARATE write from the attempt's, and a mutant removing
	// recomputeDepositIntentProjection from ApplyDeferredReceiptsForAttempt
	// would leave this assertion the only thing able to catch it (the
	// attempt-state and balance assertions above pass either way).
	var intentStatus string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, res.Intent.ID).Scan(&intentStatus)
	}); err != nil {
		t.Fatal(err)
	}
	if intentStatus != string(DepositIntentSucceeded) {
		t.Errorf("N12: the deferred-apply backstop must also recompute the intent's own projection, got status=%q", intentStatus)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// M4/F4: a reversal callback naming a PAYOUT's provider_reference must be
// rejected as an integrity failure, never tombstoned - a payout attempt
// never has a ledger_transaction_id linking a captured DEPOSIT, so
// checking "never posted" before checking "is this even a deposit" would
// silently tombstone a withdrawal reference.
func TestRVLF_M4_ReversalNamingPayoutReferenceNeverTombstoned(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-m4", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-m4": p}, MultiWebhookCredentialResolver{"mock-m4": NewMockWebhookCredentials(p)})

	payoutRef := "payout-ref-m4"
	payoutAttemptID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		wr := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'submitted',$6)`, wr, f.tenantID, f.brandID, f.playerAccountID, f.walletID, "rv-m4-idem-"+wr.String()); err != nil {
			return err
		}
		_, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: payoutAttemptID, TenantID: f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &wr, AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		if err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, payoutAttemptID, "mock-m4", uuid.New(), "rv-m4", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("seed payout attempt: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, payoutAttemptID, EvidencePlatform, payoutRef, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("mark payout accepted: %v", err)
	}

	_, err := rvCallback(pool, orch, f, "mock-m4", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-m4", payoutRef, OutcomeSucceeded, 5000, "EUR", "", false))
	if !errors.Is(err, ErrDepositReversalIntegrity) {
		t.Errorf("M4/F4: a reversal naming a payout's provider_reference must be rejected as ErrDepositReversalIntegrity, got %v", err)
	}
	var tombs int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE transaction_type='tombstone' AND provider_tx_id = $1`, payoutRef).Scan(&tombs)
	})
	if tombs != 0 {
		t.Errorf("M4/F4: a reversal naming a payout's provider_reference must never write a tombstone, found %d", tombs)
	}
}

// F3/R2 (independent code-reviewer re-review 1): the reviewer's own
// live-sibling probe. A1 declines (cascadable), creating A2. A2 is
// claimed and accepted WHILE A1 is still merely declined (not yet
// succeeded) - so A2 is genuinely live when A1 later gets a late T13
// success. rejectCreatedSiblings finds nothing (A2 is no longer
// 'created'). A2 THEN gets its own, first-ever decline callback
// (cascadable). The bug the reviewer found: finalizeDeclined's sticky
// no-op branch returned the CALLER's stub/stale `intent` argument
// unchanged instead of the ACTUAL resulting status, so
// cascadeEligible(attempt, "", ...) (or a stale non-succeeded status)
// never blocked the cascade, and a THIRD attempt (A3) was wrongly
// created for an intent that had already been credited by A1's T13
// success - a permanent 'created' orphan that can never be claimed
// (the T2 succeeded-sibling guard blocks it) but that the sweeper keeps
// retrying and failing on forever.
func TestRVLF_F3_LiveSiblingDeclineAfterT13NeverCreatesOrphanCascade(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("mock-f3-a", "EUR")
	pb := NewMockProvider("mock-f3-b", "EUR")
	// AcceptAllAmounts: provider b must NOT also see the magic cascade-
	// decline amount as a decline trigger - it needs to accept (Pending)
	// so A2 ends this call genuinely LIVE, not itself already declined.
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f3-a": pa, "mock-f3-b": pb},
		MultiWebhookCredentialResolver{"mock-f3-a": NewMockWebhookCredentials(pa), "mock-f3-b": NewMockWebhookCredentials(pb)})

	amt := int64(MockAmountProviderDeclineCascade)
	// InitiateDepositAttempt's own synchronous cascade loop: A1 (provider
	// a) declines cascadable immediately, and drives A2 (provider b)
	// through phase B/C in the SAME call - provider b's default (non-magic
	// amount) response is OutcomePending, so A2 ends this call already
	// claimed and 'pending' (LIVE), exactly matching the probe's step 2
	// ("A2 is claimed and accepted") with no extra setup needed.
	res := rvInit(t, pool, orch, f, amt, "f3-live-sibling")
	a2Ref := *res.Attempt.ProviderReference
	if res.Attempt.State != AttemptPending {
		t.Fatalf("expected A2 to be live (pending) after the synchronous cascade, got %s", res.Attempt.State)
	}
	var a1ID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 1`, res.Intent.ID).Scan(&a1ID)
	}); err != nil {
		t.Fatal(err)
	}
	a1 := mustGetAttempt(t, pool, f.tenantID, a1ID)
	if a1.State != AttemptDeclined {
		t.Fatalf("expected A1 to be declined, got %s", a1.State)
	}
	a1Ref := *a1.ProviderReference

	// A1's late T13 success - the intent is now succeeded.
	if _, err := rvCallback(pool, orch, f, "mock-f3-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, a1Ref, "", OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("A1 T13 success: %v", err)
	}
	var auditBefore int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'deposit.declined' AND target_id = $1`, res.Intent.ID.String()).Scan(&auditBefore)
	})

	// A2's OWN, first-ever decline - cascadable, would normally create A3.
	if _, err := rvCallback(pool, orch, f, "mock-f3-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, a2Ref, "", OutcomeDeclined, 0, "", "provider_unavailable", true)); err != nil {
		t.Fatalf("A2 decline: %v", err)
	}

	finalA2 := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if finalA2.State != AttemptDeclined {
		t.Errorf("expected A2 to actually decline, got %s", finalA2.State)
	}
	var a3Count int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 3`, res.Intent.ID).Scan(&a3Count)
	}); err != nil {
		t.Fatal(err)
	}
	if a3Count != 0 {
		t.Errorf("F3/R2: no A3 may ever be created for an intent A1 already credited - got %d", a3Count)
	}

	var status, providerID, providerRef string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, provider_id, provider_reference FROM deposit_intents WHERE id = $1`, res.Intent.ID).Scan(&status, &providerID, &providerRef)
	}); err != nil {
		t.Fatal(err)
	}
	if status != string(DepositIntentSucceeded) {
		t.Errorf("F3/R2: intent status must stay 'succeeded', got %q", status)
	}
	if providerID != "mock-f3-a" || providerRef != a1Ref {
		t.Errorf("F3/R2: intent provider_id/provider_reference must stay on A1 (mock-f3-a/%s), got %s/%s", a1Ref, providerID, providerRef)
	}
	var auditAfter int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'deposit.declined' AND target_id = $1`, res.Intent.ID.String()).Scan(&auditAfter)
	})
	if auditAfter != auditBefore {
		t.Errorf("F3/R2: no spurious deposit.declined audit record must be written against an already-succeeded intent (before=%d after=%d)", auditBefore, auditAfter)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// F3/R2, T2 succeeded-sibling claim (kills N14): even if a 'created'
// cascade child DID somehow exist for an already-succeeded intent (e.g. a
// race the guards above did not fully close, or a future code path), the
// T2 claim CAS itself must independently refuse it - the backstop this
// project's H4 fix already added to ClaimCreatedForSubmission.
func TestRVLF_F3_T2ClaimRefusesCreatedSiblingOfSucceededIntent(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-f3-t2", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f3-t2": p}, MultiWebhookCredentialResolver{"mock-f3-t2": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "f3-t2-claim")
	// Succeed the intent's own live attempt first.
	if _, err := rvCallback(pool, orch, f, "mock-f3-t2", p.CallbackPayload(f.tenantID, CallbackEventDeposit, *res.Attempt.ProviderReference, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("success: %v", err)
	}
	// Directly insert a stray 'created' sibling (bypassing the normal
	// cascade path, to isolate the T2 predicate itself from whichever
	// upstream guard would ordinarily have prevented this row from
	// existing at all).
	var strayID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		stray, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &res.Intent.ID, AttemptNo: 2, ExcludedProviderIDs: []string{"mock-f3-t2"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		strayID = stray.ID
		return err
	}); err != nil {
		t.Fatalf("insert stray created sibling: %v", err)
	}
	claimErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, strayID, "mock-f3-t2", uuid.New(), "rv-f3-t2", time.Now().Add(time.Minute))
	})
	if claimErr == nil {
		t.Errorf("F3/R2/N14: T2 must refuse to claim a 'created' sibling once ANY sibling of the same intent has succeeded")
	}
}

// rvEngageWildcardDepositKillSwitch seeds a staff principal and engages a
// tenant-wide ("*"), deposit-scoped kill switch - the shared setup for the
// three H3 kill-switch-site tests below (security review rv-prh-i1-
// killswitch-security.md: receipt.go/drive.go/sweeper.go each insert a
// cascade child and must each be probed separately).
func rvEngageWildcardDepositKillSwitch(t *testing.T, pool *db.Pool, f orchFixture) {
	t.Helper()
	principal := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,$2,$3,'x','tenant_admin','active')`,
			principal, f.tenantID, principal.String()+"@rv.example")
		return err
	}); err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, "*", KillSwitchOperationDeposit, "incident")
		return err
	}); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}
}

// H3 (drive.go site, security review rv-prh-i1-killswitch-security.md):
// applyDepositCallResult's ErrorClassDefiniteDecline branch - "recording a
// synchronous decline for a call already sent" - must commit the decline
// and skip only the cascade child when the switch becomes engaged AFTER
// the provider was already called (T2's own claim CAS is what correctly
// blocks a NEW dispatch while engaged; this is the separate, narrower
// guarantee that a decline for a call already in flight before the switch
// engaged is never rolled back). Calls applyDepositCallResult directly
// (same package) on an attempt already claimed/dispatched BEFORE the
// switch was engaged, exactly the ordering that could occur in production
// between a provider call returning and this transaction committing.
func TestRVLF_H3_DriveGo_SyncCascadableDeclineUnderKillSwitch(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-h3-drive", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-h3-drive": p}, MultiWebhookCredentialResolver{"mock-h3-drive": NewMockWebhookCredentials(p)})

	// Dispatch normally (no switch engaged yet) - lands on 'pending', a
	// live state ApplyDecline accepts, standing in for "submitting" (both
	// are pre-decline live states; the CAS predicate treats them alike).
	res := rvInit(t, pool, orch, f, 5000, "h3-drive")
	attemptID, intentID := res.Attempt.ID, res.Intent.ID

	rvEngageWildcardDepositKillSwitch(t, pool, f)

	capability := ProviderCapability{AdapterCapability: AdapterCapability{ProviderID: "mock-h3-drive"}}
	gr := GateResult[DepositResult]{
		Class: ErrorClassDefiniteDecline,
		Value: DepositResult{Outcome: OutcomeDeclined, ProviderReference: "h3-drive-declined-ref", DeclineReason: "provider_unavailable", Cascadable: true},
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intentID); err != nil {
			return err
		}
		attempt, err := GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, intentID)
		if err != nil {
			return err
		}
		_, _, err = orch.applyDepositCallResult(ctx, tx, intent, attempt, capability, uuid.New(), gr, EvidenceSync, false)
		return err
	})
	if err != nil {
		t.Fatalf("H3 (drive.go): a synchronous cascadable decline must never fail while the kill switch is engaged: %v", err)
	}
	final := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if final.State != AttemptDeclined {
		t.Errorf("H3 (drive.go): the decline itself must still commit, got state=%s", final.State)
	}
	var childCount int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 2`, intentID).Scan(&childCount)
	})
	if childCount != 0 {
		t.Errorf("H3 (drive.go): no cascade child may be inserted while the kill switch is engaged, got %d", childCount)
	}
	var skipped int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payment.cascade_skipped_kill_switch' AND target_id = $1`, attemptID.String()).Scan(&skipped)
	})
	if skipped != 1 {
		t.Errorf("H3 (drive.go): the skipped cascade must be labelled with an audit record, got %d", skipped)
	}
}

// H3 (sweeper.go site, security review rv-prh-i1-killswitch-security.md):
// a QueryStatus poll that resolves to a cascadable decline must commit the
// decline and skip only the cascade child while the switch is engaged -
// today (pre-fix) this rolled back and re-polled forever instead.
func TestRVLF_H3_SweeperGo_PollCascadableDeclineUnderKillSwitch(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-h3-sweep", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-h3-sweep": p}, MultiWebhookCredentialResolver{"mock-h3-sweep": NewMockWebhookCredentials(p)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "h3-sweep",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	ref := *res.Attempt.ProviderReference
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	p.Resolve(ref, OutcomeDeclined, "provider_unavailable", true)

	rvEngageWildcardDepositKillSwitch(t, pool, f)

	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats.Errors) != 0 {
		t.Fatalf("H3 (sweeper.go): a cascadable decline poll must never error while the kill switch is engaged: %v", stats.Errors)
	}
	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptDeclined {
		t.Errorf("H3 (sweeper.go): the decline itself must still commit, got state=%s", final.State)
	}
	var childCount int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 2`, res.Intent.ID).Scan(&childCount)
	})
	if childCount != 0 {
		t.Errorf("H3 (sweeper.go): no cascade child may be inserted while the kill switch is engaged, got %d", childCount)
	}
	var skipped int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payment.cascade_skipped_kill_switch' AND target_id = $1`, res.Attempt.ID.String()).Scan(&skipped)
	})
	if skipped != 1 {
		t.Errorf("H3 (sweeper.go): the skipped cascade must be labelled with an audit record, got %d", skipped)
	}
}

// R4(a)/(c) (ledger-finance payout re-review rv-prh-i1-payout-ledger.md):
// a payout decline delivered through the receipt path (ApplyReceiptEvidence
// resolving to a PAYOUT attempt, EventType "payout") must release the hold
// via withdrawal.Fail in the SAME transaction as the attempt's own decline
// - never decline the attempt while stranding the hold - and must not
// violate payment_provider_events_check1 (an earlier version of this fix
// only populated decline_stage/cascadable for EventType=="deposit").
func TestRVLF_R4a_PayoutDeclineViaReceiptPathReleasesHold(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	p := NewMockProvider("mock-r4a", "EUR")
	registerCapability(t, pool, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4a": p}, MultiWebhookCredentialResolver{"mock-r4a": NewMockWebhookCredentials(p)})

	wr, attempt := notSentPayoutAttempt(t, pool, orch, f, 5000, "r4a")
	// notSentPayoutAttempt leaves the attempt 'created' (NotSent, never
	// reached the provider) - claim it to 'submitting' as the real
	// dispatch path would before any evidence arrives.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, attempt.ID, "mock-r4a", uuid.New(), "rv-r4a", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim for submission: %v", err)
	}
	ref := "r4a-payout-declined-ref"
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, attempt.ID, EvidencePlatform, ref, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("mark accepted: %v", err)
	}

	disp, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-r4a", ReceiptEvidence{
		EventType: "payout", ProviderReference: ref, Outcome: OutcomeDeclined,
		DeclineReason: "provider_unavailable", Cascadable: false,
	})
	if err != nil {
		t.Fatalf("R4(a)/(c): a payout decline via the receipt path must not error (CHECK violation or otherwise): %v", err)
	}
	if disp != DispositionApplied {
		t.Errorf("expected disposition=applied, got %s", disp)
	}

	final := mustGetAttempt(t, pool, f.tenantID, attempt.ID)
	if final.State != AttemptDeclined {
		t.Errorf("R4(a): the payout attempt must decline, got %s", final.State)
	}
	var wrState string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&wrState)
	}); err != nil {
		t.Fatal(err)
	}
	if wrState != "failed" {
		t.Errorf("R4(a): the withdrawal request's hold must be released via withdrawal.Fail (state=failed), got %q - a stranded hold", wrState)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// R4(d): a payout SUCCESS delivered through the receipt path must settle
// the withdrawal (withdrawal.Complete) via the SAME applyPayoutSuccess
// function payout dispatch's own success path uses, with the amount/asset
// cross-check applied uniformly (§4.4's existing "mismatched" precondition
// covers both operations).
func TestRVLF_R4d_PayoutSuccessViaReceiptPathSettles(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	p := NewMockProvider("mock-r4d", "EUR")
	registerCapability(t, pool, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4d": p}, MultiWebhookCredentialResolver{"mock-r4d": NewMockWebhookCredentials(p)})

	wr, attempt := notSentPayoutAttempt(t, pool, orch, f, 5000, "r4d")
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, attempt.ID, "mock-r4d", uuid.New(), "rv-r4d", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim for submission: %v", err)
	}
	ref := "r4d-payout-success-ref"
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, attempt.ID, EvidencePlatform, ref, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("mark accepted: %v", err)
	}

	disp, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-r4d", ReceiptEvidence{
		EventType: "payout", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("R4(d): a payout success via the receipt path must not error: %v", err)
	}
	if disp != DispositionApplied {
		t.Errorf("expected disposition=applied, got %s", disp)
	}
	final := mustGetAttempt(t, pool, f.tenantID, attempt.ID)
	if final.State != AttemptSucceeded {
		t.Errorf("R4(d): the payout attempt must succeed, got %s", final.State)
	}
	var wrState string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&wrState)
	}); err != nil {
		t.Fatal(err)
	}
	if wrState != "completed" {
		t.Errorf("R4(d): the withdrawal request must be completed via withdrawal.Complete, got %q", wrState)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// R4(b): a "deposit"-typed event naming a PAYOUT attempt's own
// provider_reference (or the reverse - a "payout"-typed event naming a
// DEPOSIT attempt's reference) must be an anomaly with NO effect on
// either state machine.
func TestRVLF_R4b_EventTypeOperationMismatchIsAnomaly(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	p := NewMockProvider("mock-r4b", "EUR")
	registerCapability(t, pool, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4b": p}, MultiWebhookCredentialResolver{"mock-r4b": NewMockWebhookCredentials(p)})

	// A payout attempt, submitting, with its own provider_reference.
	_, payoutAttempt := notSentPayoutAttempt(t, pool, orch, f, 5000, "r4b-payout")
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, payoutAttempt.ID, "mock-r4b", uuid.New(), "rv-r4b", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim payout for submission: %v", err)
	}
	payoutRef := "r4b-payout-ref"
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, payoutAttempt.ID, EvidencePlatform, payoutRef, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("mark payout accepted: %v", err)
	}

	// A deposit attempt with its own provider_reference.
	res := rvInit(t, pool, orch, f.orchFixture, 5000, "r4b-deposit")
	depositRef := *res.Attempt.ProviderReference

	// A "deposit"-typed event naming the PAYOUT's reference.
	disp, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-r4b", ReceiptEvidence{
		EventType: string(CallbackEventDeposit), ProviderReference: payoutRef, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("deposit-typed event naming a payout reference: %v", err)
	}
	if disp != DispositionAnomaly {
		t.Errorf("R4(b): a deposit-typed event naming a payout reference must be an anomaly, got %s", disp)
	}
	if final := mustGetAttempt(t, pool, f.tenantID, payoutAttempt.ID); final.State != AttemptSubmitting && final.State != AttemptPending {
		t.Errorf("R4(b): the payout attempt must be untouched, got %s", final.State)
	}

	// A "payout"-typed event naming the DEPOSIT's reference.
	disp2, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-r4b", ReceiptEvidence{
		EventType: "payout", ProviderReference: depositRef, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("payout-typed event naming a deposit reference: %v", err)
	}
	if disp2 != DispositionAnomaly {
		t.Errorf("R4(b): a payout-typed event naming a deposit reference must be an anomaly, got %s", disp2)
	}
	final2 := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final2.State == AttemptSucceeded {
		t.Errorf("R4(b): the deposit attempt must never be marked succeeded by a mislabeled payout event")
	}
}

// rvApplyReceipt calls ApplyReceiptEvidence directly inside a fresh
// tenant-scoped transaction - the receipt path's own entry point,
// independent of any live HTTP route (R4's payout branches, like F1's
// EventType=="payout" cross-check, exercise ApplyReceiptEvidence directly
// since no live webhook route yet produces a "payout"-typed CallbackEvent).
func rvApplyReceipt(pool *db.Pool, orch *Orchestrator, tenantID uuid.UUID, providerID string, ev ReceiptEvidence) (ReceiptDisposition, error) {
	var disp ReceiptDisposition
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disp, err = ApplyReceiptEvidence(ctx, tx, orch, tenantID, providerID, ev)
		return err
	})
	return disp, err
}

// L2 (ledger-finance review): backfillCascadeAttemptsForIntent (receive_
// bridge_integration_test.go) builds a real two-attempt cascade shape -
// this test drives a genuine T13 second capture (a callback naming the
// FIRST, declined attempt's own reference with real success evidence)
// while the cascade CHILD is still sitting 'created', proving H4's
// sibling-rejection fires through the bridge's own multi-attempt shape,
// not only through a live-provider-driven cascade.
func TestRVLF_L2_BridgeCascadeThenT13SecondCapture(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-l2", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-l2": p}, MultiWebhookCredentialResolver{"mock-l2": NewMockWebhookCredentials(p)})

	var intent DepositIntent
	var first, second PaymentAttempt
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "l2-cascade-t13",
		})
		if err != nil {
			return err
		}
		first, second, err = backfillCascadeAttemptsForIntent(ctx, tx, intent, "mock-l2")
		return err
	})
	if err != nil {
		t.Fatalf("bridge setup: %v", err)
	}
	if second.State != AttemptCreated || second.AttemptNo != 2 {
		t.Fatalf("expected a cascade child left 'created' (attempt_no=2), got state=%s attempt_no=%d", second.State, second.AttemptNo)
	}

	firstRef := *mustGetAttempt(t, pool, f.tenantID, first.ID).ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-l2", p.CallbackPayload(f.tenantID, CallbackEventDeposit, firstRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("T13 late success on the first (declined) attempt: %v", err)
	}

	finalFirst := mustGetAttempt(t, pool, f.tenantID, first.ID)
	if finalFirst.State != AttemptSucceeded {
		t.Errorf("L2/T13: the declined first attempt must succeed on genuine T13 evidence, got %s", finalFirst.State)
	}
	finalSecond := mustGetAttempt(t, pool, f.tenantID, second.ID)
	if finalSecond.State != AttemptRejected {
		t.Errorf("L2/H4: the cascade child left 'created' must be rejected (intent_succeeded) once T13 lands, got %s", finalSecond.State)
	}
	if bal := cashBalance(t, pool, f); bal != 5000 {
		t.Errorf("L2/T13: exactly one capture must post, got balance=%d", bal)
	}
	var status string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, intent.ID).Scan(&status)
	}); err != nil {
		t.Fatal(err)
	}
	if status != string(DepositIntentSucceeded) {
		t.Errorf("expected intent status succeeded, got %q", status)
	}
	assertLedgerBalanced(t, pool, f.tenantID)

	// The cascade child, now rejected, must refuse a T2 claim - no second
	// PSP call (H4's own guard, exercised here through the bridge shape).
	claimErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intent.ID); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, second.ID, "mock-l2", uuid.New(), "rv-l2", time.Now().Add(time.Minute))
	})
	if claimErr == nil {
		t.Errorf("L2/H4: a rejected cascade child must never be claimable for submission")
	}
}

// L4 (ledger-finance review): a tombstone receipt for a resolved-but-
// never-posted original attempt now backfills amount/asset_code from
// that attempt's own declared values when the wire reversal payload
// omits them, instead of storing 0/"" even though the true values were
// already known.
func TestRVLF_L4_TombstoneReceiptBackfillsAmountAssetFromOriginalAttempt(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-l4", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-l4": p}, MultiWebhookCredentialResolver{"mock-l4": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "l4")
	ref := *res.Attempt.ProviderReference

	// A reversal naming a still-pending (never posted) original, with the
	// wire payload omitting amount/asset entirely (amount=0, asset="") -
	// the "reverse the whole deposit" shape some PSPs use.
	if _, err := rvCallback(pool, orch, f, "mock-l4", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-l4", ref, OutcomeSucceeded, 0, "", "", false)); err != nil {
		t.Fatalf("reversal (tombstone branch): %v", err)
	}

	var storedAmount int64
	var storedAsset string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT amount, asset_code FROM payment_provider_events WHERE provider_reference = $1`, "rev-l4").Scan(&storedAmount, &storedAsset)
	}); err != nil {
		t.Fatalf("query stored receipt: %v", err)
	}
	if storedAmount != 5000 {
		t.Errorf("L4: expected the tombstone receipt's amount backfilled from the original attempt (5000), got %d", storedAmount)
	}
	if storedAsset != "EUR" {
		t.Errorf("L4: expected the tombstone receipt's asset_code backfilled from the original attempt (EUR), got %q", storedAsset)
	}
}

// F2 (code review): an oversized (>64 byte) vendor decline_reason must
// never reach the payment_attempts/payment_provider_events CHECK'd
// decline_reason column raw - it must be replaced with the bounded
// sentinel, never error, and never be silently truncated mid-UTF-8.
func TestRVLF_F2_OversizeDeclineReasonBounded(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-f2", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f2": p}, MultiWebhookCredentialResolver{"mock-f2": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "f2")
	ref := *res.Attempt.ProviderReference

	oversize := strings.Repeat("x", 83)

	if _, err := rvCallback(pool, orch, f, "mock-f2", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", oversize, true)); err != nil {
		t.Fatalf("F2: an oversize decline_reason must never error (CHECK violation/redelivery loop): %v", err)
	}
	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptDeclined {
		t.Fatalf("expected declined, got %s", final.State)
	}
	if final.DeclineReason == nil || *final.DeclineReason == oversize {
		t.Errorf("F2: the raw 83-byte vendor decline_reason must never be persisted verbatim, got %v", final.DeclineReason)
	}
	if final.DeclineReason != nil && len(*final.DeclineReason) > 64 {
		t.Errorf("F2: stored decline_reason must fit the 64-byte CHECK, got %d bytes: %q", len(*final.DeclineReason), *final.DeclineReason)
	}
	// F2 follow-up (independent code review): the raw text must never be
	// silently discarded with NO record at all - a redacted audit entry
	// (byte length + hash prefix, never the raw text) must exist.
	var auditCount int64
	var meta []byte
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), (array_agg(metadata))[1] FROM audit_log WHERE action = 'payment.decline_reason_bounded'`).Scan(&auditCount, &meta)
	}); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("F2: expected exactly one payment.decline_reason_bounded audit record, got %d", auditCount)
	}
	if strings.Contains(string(meta), oversize) {
		t.Errorf("F2: the audit record must never contain the raw vendor text, got %s", meta)
	}
	if !strings.Contains(string(meta), `"original_byte_length": 83`) && !strings.Contains(string(meta), `"original_byte_length":83`) {
		t.Errorf("F2: expected the audit record to carry the original byte length (83), got %s", meta)
	}
}

// F2, multibyte (independent code review R1): an 80-byte, 40-rune
// multibyte decline reason must bound identically to the ASCII case -
// never truncated mid-UTF-8 (which would itself be invalid UTF-8), never
// left raw.
func TestRVLF_F2_MultibyteOversizeDeclineReasonBounded(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-f2mb", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f2mb": p}, MultiWebhookCredentialResolver{"mock-f2mb": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "f2mb")
	ref := *res.Attempt.ProviderReference

	// "é" is 2 bytes in UTF-8; 40 runes * 2 bytes = 80 bytes, 40 runes.
	oversize := strings.Repeat("é", 40)
	if len(oversize) != 80 {
		t.Fatalf("fixture byte length %d, want 80", len(oversize))
	}

	if _, err := rvCallback(pool, orch, f, "mock-f2mb", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", oversize, true)); err != nil {
		t.Fatalf("F2 multibyte: an oversize multibyte decline_reason must never error: %v", err)
	}
	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptDeclined {
		t.Fatalf("expected declined, got %s", final.State)
	}
	if final.DeclineReason == nil || len(*final.DeclineReason) > 64 || !utf8.ValidString(*final.DeclineReason) {
		t.Errorf("F2 multibyte: stored decline_reason must be <=64 bytes and valid UTF-8, got %v", final.DeclineReason)
	}
}

// F2, drive.go's own synchronous-decline path (independent code review
// R1's "one on each of the drive/sweeper paths" requirement). Calls
// applyDepositCallResult directly (same pattern as the H3 drive.go test)
// so an arbitrary oversized DeclineReason can be injected regardless of
// MockProvider's own fixed, short decline-reason vocabulary.
func TestRVLF_F2_DriveGo_OversizeDeclineReasonBounded(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-f2-drive", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f2-drive": p}, MultiWebhookCredentialResolver{"mock-f2-drive": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "f2-drive")
	attemptID, intentID := res.Attempt.ID, res.Intent.ID
	oversize := strings.Repeat("y", 83)

	capability := ProviderCapability{AdapterCapability: AdapterCapability{ProviderID: "mock-f2-drive"}}
	gr := GateResult[DepositResult]{
		Class: ErrorClassDefiniteDecline,
		Value: DepositResult{Outcome: OutcomeDeclined, ProviderReference: "f2-drive-declined-ref", DeclineReason: oversize, Cascadable: false},
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intentID); err != nil {
			return err
		}
		attempt, err := GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, intentID)
		if err != nil {
			return err
		}
		_, _, err = orch.applyDepositCallResult(ctx, tx, intent, attempt, capability, uuid.New(), gr, EvidenceSync, false)
		return err
	})
	if err != nil {
		t.Fatalf("F2 (drive.go): an oversize decline_reason must never error: %v", err)
	}
	final := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if final.State != AttemptDeclined {
		t.Fatalf("expected declined, got %s", final.State)
	}
	if final.DeclineReason == nil || len(*final.DeclineReason) > 64 {
		t.Errorf("F2 (drive.go): stored decline_reason must fit the 64-byte CHECK, got %v", final.DeclineReason)
	}
}

// F2, sweeper.go's own QueryStatus-decline path.
func TestRVLF_F2_SweeperGo_OversizeDeclineReasonBounded(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-f2-sweep", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f2-sweep": p}, MultiWebhookCredentialResolver{"mock-f2-sweep": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "f2-sweep")
	ref := *res.Attempt.ProviderReference
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	oversize := strings.Repeat("z", 83)
	p.Resolve(ref, OutcomeDeclined, oversize, false)

	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats.Errors) != 0 {
		t.Fatalf("F2 (sweeper.go): an oversize decline_reason must never error: %v", stats.Errors)
	}
	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptDeclined {
		t.Fatalf("expected declined, got %s", final.State)
	}
	if final.DeclineReason == nil || len(*final.DeclineReason) > 64 {
		t.Errorf("F2 (sweeper.go): stored decline_reason must fit the 64-byte CHECK, got %v", final.DeclineReason)
	}
}

// N4b (independent code review re-review 1): the precondition-anomaly
// branch (ResolveAttemptForEvidence's cross-provider case) must resolve
// its own receipt, or it counts toward the deferred-receipt cap forever.
func TestRVLF_N4b_PreconditionAnomalyReceiptResolved(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("mock-n4b-a", "EUR")
	pb := NewMockProvider("mock-n4b-b", "EUR")
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-n4b-a": pa, "mock-n4b-b": pb},
		MultiWebhookCredentialResolver{"mock-n4b-a": NewMockWebhookCredentials(pa), "mock-n4b-b": NewMockWebhookCredentials(pb)})

	res := rvInit(t, pool, orch, f, 5000, "n4b")
	// A genuinely verified "mock-n4b-b" event naming attempt A's own
	// merchant reference is a cross-provider collision: A is bound to
	// "mock-n4b-a", never "mock-n4b-b".
	disp, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-n4b-b", ReceiptEvidence{
		EventType: string(CallbackEventDeposit), ProviderReference: "n4b-unrelated-ref", MerchantReference: res.Attempt.ID.String(), Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("cross-provider anomaly delivery: %v", err)
	}
	if disp != DispositionAnomaly {
		t.Errorf("expected disposition=anomaly, got %s", disp)
	}
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-n4b-b", DeferredReceiptCap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("N4b: the precondition-anomaly receipt must be resolved, CountUnappliedReceipts=%d", n)
	}
}

// N5b: the §4.4 precondition-2 reference-conflict branch must also
// resolve its own receipt.
func TestRVLF_N5b_ReferenceConflictAnomalyReceiptResolved(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-n5b", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-n5b": p}, MultiWebhookCredentialResolver{"mock-n5b": NewMockWebhookCredentials(p)})

	resA := rvInit(t, pool, orch, f, 1000, "n5b-a")
	resB := rvInit(t, pool, orch, f, 2000, "n5b-b")
	refA := *resA.Attempt.ProviderReference

	// Names B's own merchant reference but A's provider_reference - B
	// resolves via merchant match, but refA already belongs to A, not B.
	disp, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-n5b", ReceiptEvidence{
		EventType: string(CallbackEventDeposit), MerchantReference: resB.Attempt.ID.String(), ProviderReference: refA, Outcome: OutcomeSucceeded, Amount: 2000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("reference-conflict anomaly delivery: %v", err)
	}
	if disp != DispositionAnomaly {
		t.Errorf("expected disposition=anomaly, got %s", disp)
	}
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-n5b", DeferredReceiptCap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("N5b: the reference-conflict anomaly receipt must be resolved, CountUnappliedReceipts=%d", n)
	}
}

// N5c: the R4(b) cross-operation anomaly branch must also resolve its own
// receipt (TestRVLF_R4b only asserts the disposition, not the cap).
func TestRVLF_N5c_CrossOperationAnomalyReceiptResolved(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	p := NewMockProvider("mock-n5c", "EUR")
	registerCapability(t, pool, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-n5c": p}, MultiWebhookCredentialResolver{"mock-n5c": NewMockWebhookCredentials(p)})

	_, payoutAttempt := notSentPayoutAttempt(t, pool, orch, f, 5000, "n5c-payout")
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, payoutAttempt.ID, "mock-n5c", uuid.New(), "rv-n5c", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim payout: %v", err)
	}
	payoutRef := "n5c-payout-ref"
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, payoutAttempt.ID, EvidencePlatform, payoutRef, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("mark accepted: %v", err)
	}

	disp, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-n5c", ReceiptEvidence{
		EventType: string(CallbackEventDeposit), ProviderReference: payoutRef, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("cross-operation anomaly delivery: %v", err)
	}
	if disp != DispositionAnomaly {
		t.Errorf("expected disposition=anomaly, got %s", disp)
	}
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-n5c", DeferredReceiptCap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("N5c: the cross-operation anomaly receipt must be resolved, CountUnappliedReceipts=%d", n)
	}
}

// N1: ApplyDeferredReceiptsForAttempt's event_type='deposit' filter must
// actually matter - a stored (even if hypothetically unresolved, e.g. a
// pre-M2 legacy row) deposit_reversal receipt sharing an attempt's
// (provider_id, provider_reference) must never be picked up and replayed
// as deposit evidence.
func TestRVLF_N1_DeferredApplyNeverReplaysAReversalReceiptAsDeposit(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-n1", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-n1": p}, MultiWebhookCredentialResolver{"mock-n1": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "n1")
	ref := *res.Attempt.ProviderReference

	// Directly insert an UNRESOLVED deposit_reversal receipt for this
	// SAME (provider_id, provider_reference) - simulating a row that
	// predates M2's resolve-on-insert fix, or any future writer that
	// leaves one unresolved. Its stored outcome is 'succeeded' (the
	// normalized shape every real reversal receipt has).
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_provider_events (id, tenant_id, provider_id, event_type, provider_reference, outcome, amount, asset_code, event_fingerprint, disposition_at_receipt)
			 VALUES (gen_random_uuid(), $1, $2, 'deposit_reversal', $3, 'succeeded', 5000, 'EUR', $4, 'applied')`,
			f.tenantID, "mock-n1", ref, []byte("n1-fingerprint-0000000000000001"))
		return err
	}); err != nil {
		t.Fatalf("seed unresolved reversal receipt: %v", err)
	}

	// Directly invoke the backstop under the intent's own lock, exactly as
	// its real callers do - if the event_type filter is missing, the
	// reversal row above would be picked up and replayed as deposit
	// evidence.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		_, err := ApplyDeferredReceiptsForAttempt(ctx, tx, orch, res.Attempt)
		return err
	}); err != nil {
		t.Fatalf("ApplyDeferredReceiptsForAttempt: %v", err)
	}
	var stillUnresolved int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = $1 AND event_type = 'deposit_reversal' AND resolved_at IS NULL`, ref).Scan(&stillUnresolved)
	}); err != nil {
		t.Fatal(err)
	}
	if stillUnresolved != 1 {
		t.Errorf("N1: a deposit_reversal receipt sharing this attempt's reference must never be picked up by the deposit-only deferred-apply backstop, got %d still unresolved", stillUnresolved)
	}
}

// A7-TOMB-1 (architect review): two IDENTICAL deliveries of the same
// tombstone-branch reversal event (naming an original that was never
// posted), fired concurrently, must never deadlock (40P01) and must
// produce exactly one effect (one tombstone, both deliveries resolved -
// the second as a byte-identical-fingerprint duplicate).
func TestRVLF_A7Tomb1_ConcurrentIdenticalTombstoneReversalsNoDeadlockExactlyOneEffect(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-a7tomb", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-a7tomb": p}, MultiWebhookCredentialResolver{"mock-a7tomb": NewMockWebhookCredentials(p)})

	// A never-posted original: rvInit puts the attempt in 'pending' with a
	// real provider_reference, but it has no ledger_transaction_id yet -
	// exactly the tombstone branch's own precondition.
	res := rvInit(t, pool, orch, f, 5000, "a7tomb")
	originalRef := *res.Attempt.ProviderReference
	payload := p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "a7tomb-reversal-ref", originalRef, OutcomeSucceeded, 5000, "EUR", "", false)

	var wg sync.WaitGroup
	results := make([]struct {
		disp ReceiveCallbackResult
		err  error
	}, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i].disp, results[i].err = rvCallback(pool, orch, f, "mock-a7tomb", payload)
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			msg := r.err.Error()
			if strings.Contains(msg, "40P01") || strings.Contains(msg, "deadlock detected") {
				t.Fatalf("A7-TOMB-1: delivery %d deadlocked: %v", i, r.err)
			}
			t.Fatalf("A7-TOMB-1: delivery %d unexpected error: %v", i, r.err)
		}
	}
	dispositions := map[ReceiptDisposition]int{}
	for _, r := range results {
		dispositions[r.disp.Disposition]++
	}
	if dispositions[DispositionApplied] != 1 || dispositions[DispositionDuplicateEffect] != 1 {
		t.Errorf("A7-TOMB-1: expected exactly one applied + one duplicate_effect across the two concurrent identical deliveries, got %v", dispositions)
	}

	var tombs int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE transaction_type = 'tombstone' AND provider_tx_id = $1`, originalRef).Scan(&tombs)
	}); err != nil {
		t.Fatal(err)
	}
	if tombs != 1 {
		t.Errorf("A7-TOMB-1: expected exactly one tombstone, got %d", tombs)
	}
	var receiptCount int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = 'a7tomb-reversal-ref'`).Scan(&receiptCount)
	}); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 {
		t.Errorf("A7-TOMB-1: expected exactly one receipt row (deduped on fingerprint), got %d", receiptCount)
	}
}
