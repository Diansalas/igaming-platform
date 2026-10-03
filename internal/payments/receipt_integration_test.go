//go:build integration

// PRH-I1 step (d): the receipt resolution path (receipt.go), proven in
// isolation - these tests call ApplyReceiptEvidence/ApplyDeferredReceiptsForAttempt
// directly; nothing here goes through the live webhook route or
// ReceiveVerifiedCallback (which are NOT modified by this step).
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestReceipt_ResolveByProviderReference_AppliesSuccess_LedgerBalanced(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-rc-a", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-rc-a": provider}, MultiWebhookCredentialResolver{"mock-psp-rc-a": NewMockWebhookCredentials(provider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "rc-success",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	ref := *res.Attempt.ProviderReference

	var disposition ReceiptDisposition
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disposition, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, "mock-psp-rc-a", ReceiptEvidence{
			EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
		})
		return err
	}); err != nil {
		t.Fatalf("ApplyReceiptEvidence: %v", err)
	}
	if disposition != DispositionApplied {
		t.Fatalf("expected applied, got %s", disposition)
	}

	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptSucceeded || final.LedgerTransactionID == nil {
		t.Fatalf("expected succeeded with a ledger link, got state=%s ledger=%v", final.State, final.LedgerTransactionID)
	}
	assertLedgerBalanced(t, pool, f.tenantID)

	// Duplicate delivery of the SAME event: one receipt, no double post,
	// disposition duplicate_effect.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disposition, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, "mock-psp-rc-a", ReceiptEvidence{
			EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
		})
		return err
	}); err != nil {
		t.Fatalf("ApplyReceiptEvidence (duplicate): %v", err)
	}
	if disposition != DispositionDuplicateEffect {
		t.Fatalf("expected duplicate_effect on redelivery, got %s", disposition)
	}
	assertLedgerBalanced(t, pool, f.tenantID)

	var receiptCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2`, f.tenantID, ref).Scan(&receiptCount)
	}); err != nil || receiptCount != 1 {
		t.Fatalf("expected exactly 1 receipt row across both deliveries, got %d (err=%v)", receiptCount, err)
	}
}

// TestReceipt_T13SecondCapture_ThroughApplyReceiptEvidence_LedgerBalanced was
// removed (ADR 0095 §28, ledger-finance ruling §5 item 1, INV-DEP-1 /
// PAY-DOUBLE-CREDIT-1 Financial Hardening FH-3): it asserted that a T13
// second capture POSTS its own, distinct ledger transaction - superseded
// by INV-DEP-1, under which a second capture is disputed
// (multiple_success_for_intent, T13d) and NEVER posted. Replaced by
// TestINVDEP1_Inverted_T13SecondCapture_BecomesDisputed_NoSecondPosting
// (internal/payments/inv_dep1_matrix_integration_test.go), which keeps the
// identical fixture shape and asserts the new, inverted behaviour.

func TestReceipt_MerchantReferenceCrossProvider_IsAnomaly_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	providerA := NewMockProvider("mock-psp-rc-b1", "EUR")
	providerB := NewMockProvider("mock-psp-rc-b2", "EUR")
	registerCapability(t, pool, f, providerA, 100)
	registerCapability(t, pool, f, providerB, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-rc-b1": providerA, "mock-psp-rc-b2": providerB},
		MultiWebhookCredentialResolver{"mock-psp-rc-b1": NewMockWebhookCredentials(providerA), "mock-psp-rc-b2": NewMockWebhookCredentials(providerB)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "rc-cross-provider",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.ProviderID == nil {
		t.Fatalf("expected a routed provider")
	}
	merchantRef := res.Attempt.MerchantReference

	// A verified event from the OTHER provider, naming this attempt's
	// merchant reference (INV-IO-14: never resolved cross-provider).
	otherProvider := "mock-psp-rc-b1"
	if *res.Attempt.ProviderID == otherProvider {
		otherProvider = "mock-psp-rc-b2"
	}
	var disposition ReceiptDisposition
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disposition, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, otherProvider, ReceiptEvidence{
			EventType: "deposit", ProviderReference: "forged-ref", MerchantReference: merchantRef,
			Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
		})
		return err
	}); err != nil {
		t.Fatalf("ApplyReceiptEvidence: %v", err)
	}
	if disposition != DispositionAnomaly {
		t.Fatalf("expected anomaly for a cross-provider merchant-reference match, got %s", disposition)
	}

	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State == AttemptSucceeded {
		t.Fatalf("a cross-provider anomaly must never change the attempt's state")
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

func TestReceipt_Unresolved_DeferredThenAppliedOnceReferenceKnown(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-rc-c", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-rc-c": &refLessAmbiguousProvider{provider}}, MultiWebhookCredentialResolver{"mock-psp-rc-c": NewMockWebhookCredentials(provider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountAmbiguous, PaymentMethod: "card", IdempotencyKey: "rc-deferred",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptAmbiguous {
		t.Fatalf("expected ambiguous (no reference known yet), got %s", res.Attempt.State)
	}

	// A callback arrives naming a reference the platform does not know
	// yet (raced phase C, no merchant-reference echo): deferred_unresolved.
	unknownRef := "future-ref-" + uuid.NewString()
	var disposition ReceiptDisposition
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disposition, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, "mock-psp-rc-c", ReceiptEvidence{
			EventType: "deposit", ProviderReference: unknownRef, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR",
		})
		return err
	}); err != nil {
		t.Fatalf("ApplyReceiptEvidence (deferred): %v", err)
	}
	if disposition != DispositionDeferredUnresolved {
		t.Fatalf("expected deferred_unresolved, got %s", disposition)
	}
	assertLedgerBalanced(t, pool, f.tenantID)

	// Now the attempt learns that reference (simulating T9/T4 - it must
	// be the SAME reference the deferred receipt named).
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, res.Attempt.ID, EvidenceQueryStatus, unknownRef, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("MarkAccepted: %v", err)
	}

	attemptNow := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	var applied int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *attemptNow.DepositIntentID); err != nil {
			return err
		}
		var err error
		applied, err = ApplyDeferredReceiptsForAttempt(ctx, tx, orch, attemptNow)
		return err
	}); err != nil {
		t.Fatalf("ApplyDeferredReceiptsForAttempt: %v", err)
	}
	if applied != 1 {
		t.Fatalf("expected exactly 1 deferred receipt applied, got %d", applied)
	}

	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptSucceeded {
		t.Fatalf("expected the deferred receipt to converge the attempt to succeeded, got %s", final.State)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

func TestReceipt_DeferredReceiptCap_ExceededReturnsErrorNothingWritten(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-rc-d", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-rc-d": provider}, MultiWebhookCredentialResolver{"mock-psp-rc-d": NewMockWebhookCredentials(provider)})

	// Pre-fill the cap with distinct unresolved receipts for this
	// (tenant, provider) using a tiny cap so the test is fast.
	const cap = 3
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < cap; i++ {
			if _, _, err := insertReceiptDeduped(ctx, tx, f.tenantID, "mock-psp-rc-d", ReceiptEvidence{
				EventType: "deposit", ProviderReference: uuid.NewString(), Outcome: OutcomeAmbiguous,
			}, DispositionDeferredUnresolved); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed unresolved receipts: %v", err)
	}

	var countBefore int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		countBefore, err = CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-psp-rc-d", cap)
		return err
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if countBefore != cap {
		t.Fatalf("expected exactly %d unresolved receipts seeded, got %d", cap, countBefore)
	}

	var probeCount int
	var probeErr error
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// A direct probe at the cap: with limit=cap-1, count should
		// already exceed it, which is exactly the condition
		// ApplyReceiptEvidence checks before ever inserting a new
		// deferred receipt.
		var err error
		probeCount, err = CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-psp-rc-d", cap-1)
		return err
	}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probeCount <= cap-1 {
		t.Fatalf("expected the probe to report more than the cap-1 limit, got %d", probeCount)
	}

	// End to end: a brand-new unresolved event, with the cap set below
	// what is already seeded, must be refused with nothing new written.
	var disposition ReceiptDisposition
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		n, err := CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-psp-rc-d", cap-1)
		if err != nil {
			return err
		}
		if n > cap-1 {
			return ErrDeferredReceiptCapExceeded
		}
		disposition, probeErr = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, "mock-psp-rc-d", ReceiptEvidence{
			EventType: "deposit", ProviderReference: uuid.NewString(), Outcome: OutcomeAmbiguous,
		})
		return probeErr
	})
	if err == nil || err != ErrDeferredReceiptCapExceeded {
		t.Fatalf("expected ErrDeferredReceiptCapExceeded, got %v (disposition=%v)", err, disposition)
	}

	var countAfter int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		countAfter, err = CountUnappliedReceipts(ctx, tx, f.tenantID, "mock-psp-rc-d", 1000)
		return err
	}); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if countAfter != cap {
		t.Fatalf("expected the cap-exceeded path to write nothing new: before=%d after=%d", cap, countAfter)
	}
}

func TestReceipt_DeferredReceipt_PredatesSubmission_NeverApplied(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-rc-e", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-rc-e": &refLessAmbiguousProvider{provider}}, MultiWebhookCredentialResolver{"mock-psp-rc-e": NewMockWebhookCredentials(provider)})

	ref := "predates-ref-" + uuid.NewString()

	// received_at is DB-clock-authoritative and append-only (the guard
	// trigger forbids ever changing it after insert - by design, so a
	// receipt's arrival time can never be spoofed) - so instead of
	// backdating a receipt, this test gets a genuine "receipt predates
	// submission" ordering the realistic way: the receipt for `ref`
	// arrives and is deferred FIRST, and only afterwards does a deposit
	// attempt come to exist and get (synthetically, via MarkAccepted)
	// assigned that same already-received reference. Its
	// first_submitted_at - set at attempt-claim time, after the receipt
	// was already durably received - is therefore genuinely later than
	// the receipt's received_at.
	var receiptID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := insertReceiptDeduped(ctx, tx, f.tenantID, "mock-psp-rc-e", ReceiptEvidence{
			EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR",
		}, DispositionDeferredUnresolved)
		receiptID = id
		return err
	}); err != nil {
		t.Fatalf("seed predating receipt: %v", err)
	}

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountAmbiguous, PaymentMethod: "card", IdempotencyKey: "rc-predates",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, res.Attempt.ID, EvidenceQueryStatus, ref, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("MarkAccepted: %v", err)
	}

	attemptNow := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if attemptNow.FirstSubmittedAt == nil {
		t.Fatalf("expected first_submitted_at to be set")
	}

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *attemptNow.DepositIntentID); err != nil {
			return err
		}
		_, err := ApplyDeferredReceiptsForAttempt(ctx, tx, orch, attemptNow)
		return err
	}); err != nil {
		t.Fatalf("ApplyDeferredReceiptsForAttempt: %v", err)
	}

	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State == AttemptSucceeded {
		t.Fatalf("a receipt predating first_submitted_at must never be applied")
	}

	var resolution *string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolution FROM payment_provider_events WHERE id = $1`, receiptID).Scan(&resolution)
	}); err != nil || resolution == nil || *resolution != string(ResolutionAnomalyPredatesSubmission) {
		t.Fatalf("expected resolution=anomaly_predates_submission, got %v (err=%v)", resolution, err)
	}
}
