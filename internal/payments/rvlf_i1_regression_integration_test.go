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
	"testing"
	"time"

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

// P1 (H1): a deposit_reversal whose wire outcome is declined/pending/
// ambiguous must never debit player_cash, and must never be stored as a
// claimed outcome='succeeded'.
func TestRVLF_P1_NonSucceededReversalOutcomes(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv1", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv1": p}, MultiWebhookCredentialResolver{"mock-rv1": NewMockWebhookCredentials(p)})

	for i, oc := range []Outcome{OutcomeDeclined, OutcomePending, OutcomeAmbiguous} {
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
		if after != before {
			t.Errorf("H1: a deposit_reversal with wire outcome %q debited player_cash (%d -> %d), stored outcome=%q", oc, before, after, stored)
		}
		if stored == string(OutcomeSucceeded) {
			t.Errorf("H1: a deposit_reversal with wire outcome %q must never be stored as outcome=%q", oc, OutcomeSucceeded)
		}
		if r.Disposition == DispositionApplied {
			t.Errorf("H1: a non-succeeded reversal wire outcome must never resolve to disposition=applied, got %s", r.Disposition)
		}
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// P1b (H1): a declined reversal for a still-pending (never posted) deposit
// must NOT write a tombstone - the genuine later success still posts.
func TestRVLF_P1b_DeclinedReversalDoesNotTombstonePendingDeposit(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-rv1b", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-rv1b": p}, MultiWebhookCredentialResolver{"mock-rv1b": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "p1b")
	ref := *res.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-rv1b", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "rev-p1b", ref, OutcomeDeclined, 0, "", "chargeback_lost", false)); err != nil {
		t.Fatalf("declined reversal: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-rv1b", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("genuine success: %v", err)
	}
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	bal := cashBalance(t, pool, f)
	t.Logf("after declined reversal then genuine success: attempt=%s balance=%d", a.State, bal)
	if bal != 5000 || a.State != AttemptSucceeded {
		t.Errorf("H1: a DECLINED reversal must never tombstone a live deposit; the genuine success must still be credited (state=%s balance=%d)", a.State, bal)
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
			WithdrawalRequestID: &wr, AttemptNo: 1, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
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

// F3: a T13 success on one sibling must never be overwritten by a later
// decline on ANOTHER sibling of the same intent - the intent's status,
// provider_id and provider_reference all stay on the succeeded attempt,
// and no spurious "deposit.declined" audit record is written against an
// already-succeeded intent.
func TestRVLF_F3_SucceededInputNeverRegressedByLaterDeclineOnAnotherSibling(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("mock-f3-a", "EUR")
	pb := NewMockProvider("mock-f3-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f3-a": pa, "mock-f3-b": pb},
		MultiWebhookCredentialResolver{"mock-f3-a": NewMockWebhookCredentials(pa), "mock-f3-b": NewMockWebhookCredentials(pb)})
	amt := int64(MockAmountProviderDeclineCascade)
	res := rvInit(t, pool, orch, f, amt, "f3")
	childRef := *res.Attempt.ProviderReference
	var parentID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 1`, res.Intent.ID).Scan(&parentID)
	}); err != nil {
		t.Fatal(err)
	}
	parentRef := *mustGetAttempt(t, pool, f.tenantID, parentID).ProviderReference

	// The cascade CHILD succeeds first - the intent is now succeeded, with
	// provider_reference pointing at the child.
	if _, err := rvCallback(pool, orch, f, "mock-f3-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, amt, "EUR", "", false)); err != nil {
		t.Fatalf("child success: %v", err)
	}
	var auditBefore int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'deposit.declined' AND target_id = $1`, res.Intent.ID.String()).Scan(&auditBefore)
	})

	// A late DECLINE on the PARENT sibling must never regress the intent.
	if _, err := rvCallback(pool, orch, f, "mock-f3-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, parentRef, "", OutcomeDeclined, 0, "", "provider_unavailable", false)); err != nil {
		t.Fatalf("late decline on succeeded sibling: %v", err)
	}

	var status, providerID, providerRef string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, provider_id, provider_reference FROM deposit_intents WHERE id = $1`, res.Intent.ID).Scan(&status, &providerID, &providerRef)
	}); err != nil {
		t.Fatal(err)
	}
	if status != string(DepositIntentSucceeded) {
		t.Errorf("F3: intent status must stay 'succeeded', got %q", status)
	}
	if providerID != "mock-f3-b" || providerRef != childRef {
		t.Errorf("F3: intent provider_id/provider_reference must stay on the succeeded attempt (mock-f3-b/%s), got %s/%s", childRef, providerID, providerRef)
	}
	var auditAfter int64
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'deposit.declined' AND target_id = $1`, res.Intent.ID.String()).Scan(&auditAfter)
	})
	if auditAfter != auditBefore {
		t.Errorf("F3: no spurious deposit.declined audit record must be written against an already-succeeded intent (before=%d after=%d)", auditBefore, auditAfter)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}
