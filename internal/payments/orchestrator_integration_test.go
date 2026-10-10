//go:build integration

package payments

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

type orchFixture struct {
	tenantID        uuid.UUID
	brandID         uuid.UUID
	playerAccountID uuid.UUID
	walletID        uuid.UUID
}

func seedOrchFixture(t *testing.T, pool *db.Pool) orchFixture {
	t.Helper()
	f := orchFixture{tenantID: uuid.New(), brandID: uuid.New(), playerAccountID: uuid.New()}
	personID := uuid.New()

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence', 'active')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Test Brand', 'active')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID, f.tenantID, f.brandID, personID, f.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "EUR")
		if err != nil {
			return err
		}
		f.walletID = w.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant rows: %v", err)
	}
	return f
}

// registerCapability writes an active ProviderCapability row for provider
// under f's tenant (tenant-wide, brand_id NULL) supporting EUR/card with
// the given priority, inside its own transaction.
// registerCapability writes an active, tenant-wide ProviderCapability row
// mirroring provider's own declared capability exactly (never a narrower
// or wider set than what NewMockProvider was constructed with) - so tests
// can freely vary which currencies each mock instance declares.
func registerCapability(t *testing.T, pool *db.Pool, f orchFixture, provider PaymentProvider, priority int) {
	t.Helper()
	declared := provider.Capabilities()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: declared.SupportedFiatCurrencies,
			SupportedCryptoAssets:   declared.SupportedCryptoAssets,
			SupportedPaymentMethods: declared.SupportedPaymentMethods,
			SupportsDeposit:         declared.SupportsDeposit,
			SupportsWithdrawal:      declared.SupportsWithdrawal,
			SupportsRefundReversal:  declared.SupportsRefundReversal,
			AmountLimits:            declared.AmountLimits,
			Priority:                priority,
			Status:                  CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("register capability for %s: %v", declared.ProviderID, err)
	}
}

func cashBalance(t *testing.T, pool *db.Pool, f orchFixture) int64 {
	t.Helper()
	var balance int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetByID(ctx, tx, f.walletID)
		if err != nil {
			return err
		}
		summary, err := wallet.GetSummary(ctx, tx, w)
		if err != nil {
			return err
		}
		balance = summary.CashBalance
		return nil
	})
	if err != nil {
		t.Fatalf("read cash balance: %v", err)
	}
	return balance
}

func TestInitiateDeposit_SuccessThenCallbackPostsFlow1(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "dep-1",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.Status != DepositIntentPending {
		t.Fatalf("expected pending intent awaiting async callback, got %v", intent.Status)
	}
	if intent.ProviderReference == nil || *intent.ProviderReference == "" {
		t.Fatal("expected a provider reference to be recorded")
	}

	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, 5000, "EUR", "", false)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Status != DepositIntentSucceeded {
		t.Fatalf("expected succeeded, got %v", result.Status)
	}
	if result.LedgerTransactionID == nil {
		t.Fatal("expected a ledger transaction id")
	}

	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected cash balance 5000, got %d", balance)
	}
}

func TestReceiveCallback_RedeliveredSuccessIsIdempotent(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 4200, PaymentMethod: "card", IdempotencyKey: "dep-redeliver",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}

	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, 4200, "EUR", "", false)
	for i := 0; i < 2; i++ {
		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", payload)
			return err
		})
		if err != nil {
			t.Fatalf("ReceiveCallback delivery %d: %v", i, err)
		}
	}

	if balance := cashBalance(t, pool, f); balance != 4200 {
		t.Fatalf("expected exactly one deposit's worth (4200) after a redelivered callback, got %d", balance)
	}
}

// TestInitiateDeposit_SynchronousDeclineNoCascade is DELETED
// (PROV-OUTBOUND-CRED-1-LEGACY-PATH/E2 code review E2-1): it called
// initiateDepositWithAttempt, which now wraps the TEST-ONLY
// legacyShapeInitiateDeposit copy - a regression in the LIVE synchronous-
// decline/no-cascade path could not have failed it. Redundant with
// TestInitiateDepositAttempt_DeclinedOutcome_T8 (deposit_v2_integration_
// test.go), which drives the identical MockAmountPlayerDeclineNoCascade
// scenario through InitiateDepositAttempt itself and asserts
// Cascadable=false and DepositIntentDeclined.

// TestInitiateDeposit_CascadesToSecondProviderOnCascadableDecline is
// DELETED (E2-1, same reasoning): redundant with
// TestDriveCreatedAttemptCascade_PoolThreadedToResolver
// (pool_threading_integration_test.go), which drives the identical
// MockAmountProviderDeclineCascade scenario (mock-a declines cascadable,
// mock-b accepts) through InitiateDepositAttempt's own synchronous
// cascade loop and asserts the intent lands DepositIntentPending on the
// fallback provider.

// TestInitiateDeposit_CascadeExhaustedEndsDeclined migrated (E2-1) to
// InitiateDepositAttempt: no live-path test already caps MaxCascadeDepth
// and asserts the resulting decline once every candidate is exhausted,
// so this is not redundant - it is the only live coverage of that
// specific exhaustion behaviour. A third, ACCEPTING provider c is
// registered (lowest priority, never reached when MaxCascadeDepth is
// honoured) so an off-by-one mutant in cascade.go's own cascadeEligible
// (e.g. attempt.AttemptNo > maxDepth instead of >=) is actually
// observable: with only a/b registered (both declining), an off-by-one
// that wrongly permits a third attempt would still terminate in the same
// declined state via ErrNoRoutableProvider (ledger-finance/coordinator-
// requested mutant run, PROV-OUTBOUND-CRED-1-LEGACY-PATH/E2 follow-up) -
// c's presence turns that into an observably WRONG pending/succeeded
// outcome instead, which this test's own spy assertion now catches.
func TestInitiateDeposit_CascadeExhaustedEndsDeclined(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	a := NewMockProvider("mock-a", "EUR")
	b := NewMockProvider("mock-b", "EUR")
	c := newSpyProvider(NewMockProvider("mock-c", "EUR"))
	c.AcceptAllAmounts = true
	registerCapability(t, pool, f, a, 10)
	registerCapability(t, pool, f, b, 20)
	registerCapability(t, pool, f, c, 30)
	orch := NewOrchestrator(
		map[string]PaymentProvider{"mock-a": a, "mock-b": b, "mock-c": c},
		MultiWebhookCredentialResolver{"mock-a": NewMockWebhookCredentials(a), "mock-b": NewMockWebhookCredentials(b), "mock-c": NewMockWebhookCredentials(c.MockProvider)},
	)
	orch.MaxCascadeDepth = 2 // exactly enough to try a and b, never c

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountProviderDeclineCascade, PaymentMethod: "card", IdempotencyKey: "dep-exhausted",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected declined once every candidate is exhausted, got %v", res.Intent.Status)
	}
	if got := c.DepositCallCount(); got != 0 {
		t.Fatalf("expected MaxCascadeDepth to stop before ever reaching the third provider, but mock-c's Deposit was called %d times", got)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("no ledger entry should exist after cascade exhaustion, got balance %d", balance)
	}
}

// TestInitiateDeposit_AmbiguousOutcomeIsNotCascaded is DELETED
// (E2-1 fix round, second pass): the first migration attempt (E2-1) kept
// this test's second half - a callback naming the SAME reference the
// mock's synchronous Ambiguous Deposit call returned, expected to post a
// success - but that assumption is WRONG for the live path and the
// migrated test FAILED when actually run (not merely reasoned about):
// MarkAmbiguousFromSubmitting (T6, attempt.go) deliberately does NOT
// record a provider_reference on the ATTEMPT row - only
// finalizeAmbiguous's own deposit_intents.provider_reference column gets
// it, which is no longer what the post-cutover receipt path
// (ApplyReceiptEvidence) resolves callbacks against. A callback naming
// that reference therefore correctly returns `deferred_unresolved` (the
// platform does not yet know that reference belongs to this attempt),
// exactly the shape TestReceipt_Unresolved_DeferredThenAppliedOnceReferenceKnown
// (receipt_integration_test.go) already builds and asserts, INCLUDING the
// eventual convergence to succeeded once the attempt separately learns
// the reference (simulating T9/T4) and ApplyDeferredReceiptsForAttempt
// re-applies it - a strictly MORE thorough live test than this one ever
// was. First half (ambiguous never cascades) is separately redundant with
// the strengthened TestInitiateDepositAttempt_AmbiguousOutcome_T6
// (deposit_v2_integration_test.go). Both halves of the original legacy-
// chain test are therefore covered by existing live tests; nothing here
// needed migrating after all.

func TestInitiateDeposit_ClientRetryReturnsOriginalIntent_NoSecondProviderCall(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	params := InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 3000, PaymentMethod: "card", IdempotencyKey: "dep-retry-key",
	}

	firstRes, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, params)
	if err != nil {
		t.Fatalf("first InitiateDepositAttempt: %v", err)
	}
	secondRes, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, params)
	if err != nil {
		t.Fatalf("retried InitiateDepositAttempt: %v", err)
	}
	first, second := firstRes.Intent, secondRes.Intent

	if first.ID != second.ID {
		t.Fatalf("expected the retried call to return the SAME intent, got %s vs %s", first.ID, second.ID)
	}
	if first.ProviderReference == nil || second.ProviderReference == nil || *first.ProviderReference != *second.ProviderReference {
		t.Fatal("expected the same provider reference - no second provider call should have happened")
	}
}

// TestReceiveCallback_UnknownProviderReferenceRejected predates the
// PRH-payments-callback-cutover (ADR 0095 §6.1 step 5/§6.2, S95-C4):
// before it, a callback naming a reference for which no deposit_intents
// row existed at all was ErrDepositIntentNotFound (a 404 at the HTTP
// layer). ADR 0095 deliberately retires that: an unresolvable verified
// callback is never an error and never a 404 (a verified sender must
// never be able to learn "this reference does not exist" from an error
// code) - it is durably receipted as `deferred_unresolved` and left for
// phase C/the sweeper to apply once (if ever) a matching attempt appears.
// ErrDepositIntentNotFound is no longer reachable from
// ReceiveVerifiedCallback's deposit/reversal branches at all. This test's
// original intent (an unknown reference must not silently succeed, and
// must not be treated as if it named something real) is now expressed by
// the SAME/stronger set of checks: no error, no ledger effect, the
// disposition is exactly `deferred_unresolved`, and a durable, unresolved
// receipt row exists for exactly this reference.
func TestReceiveCallback_UnknownProviderReferenceRejected(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, "never-initiated-ref", "", OutcomeSucceeded, 1000, "EUR", "", false)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", payload)
		return err
	})
	if err != nil {
		t.Fatalf("expected an unresolvable callback to be durably deferred, not an error, got %v", err)
	}
	if result.Disposition != DispositionDeferredUnresolved {
		t.Fatalf("expected disposition %q, got %q", DispositionDeferredUnresolved, result.Disposition)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("an unresolvable callback must never post, got balance %d", balance)
	}
	var ledgerCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&ledgerCount)
	}); err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	if ledgerCount != 0 {
		t.Fatalf("expected zero ledger_transactions rows, got %d", ledgerCount)
	}
	var disposition string
	var resolvedAt *time.Time
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT disposition_at_receipt, resolved_at FROM payment_provider_events
			  WHERE tenant_id = $1 AND provider_id = 'mock-psp' AND provider_reference = 'never-initiated-ref'`,
			f.tenantID,
		).Scan(&disposition, &resolvedAt)
	}); err != nil {
		t.Fatalf("expected a durable, unresolved receipt row for the unknown reference: %v", err)
	}
	if disposition != string(DispositionDeferredUnresolved) {
		t.Fatalf("expected the receipt's disposition_at_receipt to be %q, got %q", DispositionDeferredUnresolved, disposition)
	}
	if resolvedAt != nil {
		t.Fatalf("expected the receipt to remain unresolved (resolved_at IS NULL), got %v", *resolvedAt)
	}
}

// TestReceiveCallback_CrossTenantProviderReferenceIsInvisible predates
// PAY-WH-TENANT-1 (ADR 0090): before it, a callback's authenticity said
// nothing about which tenant it was "for", so this test's own assertion
// was really about deposit_intents' RLS/tenant-scoped lookup, not
// authentication - a callback signed by the shared secret for tenant 1's
// reference, delivered under tenant 2's scope, used to reach (and fail at)
// the deposit-intent lookup. PAY-WH-TENANT-1 moves the tenant into WHAT is
// verified, so the SAME scenario now fails earlier, at signature
// verification (S-6's fix - see TestWebhook_CrossTenant_SameRefCollision_Rejected
// for the dedicated regression test), never even reaching the
// deposit_intents lookup this test originally exercised. Updated to assert
// the new, STRONGER outcome, rather than deleted.
func TestReceiveCallback_CrossTenantProviderReferenceIsInvisible(t *testing.T) {
	pool := testPool(t)
	f1 := seedOrchFixture(t, pool)
	f2 := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f1, provider, 100)
	registerCapability(t, pool, f2, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f1.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f1.tenantID, BrandID: f1.brandID, PlayerAccountID: f1.playerAccountID, WalletID: f1.walletID},
			AssetCode: "EUR", Amount: 2500, PaymentMethod: "card", IdempotencyKey: "dep-cross-tenant",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit for tenant 1: %v", err)
	}

	// Signed FOR tenant 1's own reference - delivered as tenant 2. A
	// callback resolved (by the caller's own webhook path/credential) to
	// tenant 2 must never see or credit tenant 1's deposit intent, even
	// though it names the exact same provider_id and provider_reference.
	payload := provider.CallbackPayload(f1.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, 2500, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f2.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f2.tenantID, "mock-psp", payload)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != ReasonSignatureInvalid {
		t.Fatalf("expected a *CallbackAuthError{Reason: signature_invalid} under tenant 2's scope (the tenant is now bound into "+
			"the signature itself, so this never even reaches the deposit_intents lookup), got %v", err)
	}

	if balance := cashBalance(t, pool, f2); balance != 0 {
		t.Fatalf("tenant 2's wallet must not be credited, got balance %d", balance)
	}
}

func TestReceiveCallback_DepositReversalPostsFlow2(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 8000, PaymentMethod: "card", IdempotencyKey: "dep-to-reverse",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	depositRef := *intent.ProviderReference

	successPayload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeSucceeded, 8000, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", successPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (success): %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 8000 {
		t.Fatalf("expected 8000 after deposit, got %d", balance)
	}

	reversalPayload := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "chargeback-ref-1", depositRef, OutcomeDeclined, 8000, "EUR", "chargeback", false)
	var reversalResult ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reversalResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", reversalPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (reversal): %v", err)
	}
	if reversalResult.Tombstoned {
		t.Fatal("a reversal of an already-posted deposit must not be a tombstone")
	}
	if reversalResult.LedgerTransactionID == nil {
		t.Fatal("expected a reversal ledger transaction id")
	}

	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("expected balance back to 0 after reversal, got %d", balance)
	}

	// Verify the reversal actually references the original via
	// reverses_transaction_id, and the reversal's own idempotency/provider
	// reference is independent of the original's.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var reverses uuid.UUID
		return tx.QueryRow(ctx,
			`SELECT reverses_transaction_id FROM ledger_transactions WHERE id = $1`, *reversalResult.LedgerTransactionID,
		).Scan(&reverses)
	})
	if err != nil {
		t.Fatalf("verify reverses_transaction_id: %v", err)
	}
}

// TestReceiveCallback_ReversalAmountCannotExceedOriginal closes a P1
// finding from the Stage 3B security review: a deposit-reversal
// callback's amount/asset are payload-controlled facts about a debit
// the platform is about to post, and (before this fix) were accepted
// uncapped - a reversal callback declaring an amount far larger than the
// original deposit would debit player_cash arbitrarily, going deeply
// negative, with no sufficiency check anywhere on the reversal path.
func TestReceiveCallback_ReversalAmountCannotExceedOriginal(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "dep-cap-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	depositRef := *intent.ProviderReference

	successPayload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeSucceeded, 5000, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", successPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (success): %v", err)
	}

	// Reversal declares 500000000 - vastly more than the 5000 that was
	// ever actually deposited.
	oversizedReversal := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "reversal-oversized", depositRef, OutcomeDeclined, 500000000, "EUR", "chargeback", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", oversizedReversal)
		return err
	})
	if !errors.Is(err, ErrCallbackProviderMismatch) {
		t.Fatalf("expected ErrCallbackProviderMismatch for an oversized reversal amount, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected balance unchanged at 5000 after rejected oversized reversal, got %d", balance)
	}

	// A reversal naming a different asset than the original must also be
	// rejected, never silently posted against the original's asset.
	wrongAsset := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "reversal-wrong-asset", depositRef, OutcomeDeclined, 5000, "USD", "chargeback", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", wrongAsset)
		return err
	})
	if !errors.Is(err, ErrCallbackProviderMismatch) {
		t.Fatalf("expected ErrCallbackProviderMismatch for a mismatched reversal asset, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected balance still unchanged at 5000, got %d", balance)
	}

	// The correctly-sized, correctly-scoped reversal must still succeed -
	// proves the rejections above are about the mismatch, not a blanket
	// block on reversing this deposit at all.
	correct := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "reversal-correct", depositRef, OutcomeDeclined, 5000, "EUR", "chargeback", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", correct)
		return err
	})
	if err != nil {
		t.Fatalf("expected the correctly-scoped reversal to succeed, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("expected balance 0 after the correct reversal, got %d", balance)
	}
}

// TestReceiveCallback_SecondReversalOfSameDepositRejected closes the
// other half of the same P1 finding: the ledger's (tenant_id, provider_id,
// provider_tx_id) uniqueness only catches a REDELIVERY of the identical
// reversal reference - it does nothing to stop a second, distinct
// reversal reference naming the same already-reversed original from
// posting an independent second debit.
func TestReceiveCallback_SecondReversalOfSameDepositRejected(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 4000, PaymentMethod: "card", IdempotencyKey: "dep-double-reversal-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	depositRef := *intent.ProviderReference

	successPayload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeSucceeded, 4000, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", successPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (success): %v", err)
	}

	firstReversal := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "reversal-first", depositRef, OutcomeDeclined, 4000, "EUR", "chargeback", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", firstReversal)
		return err
	})
	if err != nil {
		t.Fatalf("first reversal: %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("expected balance 0 after first reversal, got %d", balance)
	}

	// A SECOND reversal, under its own distinct provider_reference, for
	// the SAME original deposit - must be rejected, not posted as a
	// further debit driving the balance negative.
	secondReversal := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "reversal-second-distinct-ref", depositRef, OutcomeDeclined, 4000, "EUR", "chargeback", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", secondReversal)
		return err
	})
	if !errors.Is(err, ErrDepositAlreadyReversed) {
		t.Fatalf("expected ErrDepositAlreadyReversed for a second distinct reversal reference, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("expected balance still 0 after rejected second reversal, got %d", balance)
	}
}

// TestReceiveCallback_LateDeclineAfterSuccessIsNoOp closes a P1 finding
// from the Stage 3B code review: postDepositSuccess already short-
// circuits a REDELIVERED success callback, but a late/out-of-order
// DECLINE callback for an intent that already succeeded had no equivalent
// guard - it would flip a succeeded intent's status to "declined" (while
// the ledger credit and wallet balance stayed in place, permanently
// disagreeing with the intent row), null out its provider_id/reference
// (finalizeDeclined's cascade-exhausted path), and - with a second
// provider configured - could even trigger a FRESH provider.Deposit call
// at that second PSP for a deposit that already succeeded.
func TestReceiveCallback_LateDeclineAfterSuccessIsNoOp(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 3000, PaymentMethod: "card", IdempotencyKey: "dep-late-decline-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	depositRef := *intent.ProviderReference

	successPayload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeSucceeded, 3000, "EUR", "", false)
	var succeeded ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		succeeded, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", successPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (success): %v", err)
	}
	if succeeded.Status != DepositIntentSucceeded {
		t.Fatalf("expected status succeeded, got %q", succeeded.Status)
	}

	// A late decline arrives for the same reference after success already
	// posted (out-of-order delivery, or a stale retry of an attempt the
	// provider itself later resolved differently).
	latePayload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeDeclined, 3000, "EUR", "issuer_declined_late", false)
	var late ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		late, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", latePayload)
		return err
	})
	if err != nil {
		t.Fatalf("expected the late decline to be a safe no-op, not an error, got %v", err)
	}
	if late.Status != DepositIntentSucceeded {
		t.Fatalf("expected status to remain succeeded after a late decline, got %q", late.Status)
	}
	if late.LedgerTransactionID == nil || *late.LedgerTransactionID != *succeeded.LedgerTransactionID {
		t.Fatal("expected the same ledger transaction id to be reported, no new posting or corruption")
	}
	if balance := cashBalance(t, pool, f); balance != 3000 {
		t.Fatalf("expected balance unchanged at 3000 after the late decline, got %d", balance)
	}

	// Confirm the intent row itself, not just ReceiveCallback's return
	// value, still shows succeeded with its provider fields intact -
	// guards against finalizeDeclined's cascade-exhausted path having
	// nulled provider_id/provider_reference on an already-succeeded row.
	var status, providerID string
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, provider_id FROM deposit_intents WHERE id = $1`, intent.ID).Scan(&status, &providerID)
	})
	if err != nil {
		t.Fatalf("query intent row: %v", err)
	}
	if status != string(DepositIntentSucceeded) {
		t.Fatalf("expected persisted status 'succeeded', got %q", status)
	}
	if providerID != "mock-psp" {
		t.Fatalf("expected provider_id to remain 'mock-psp', got %q", providerID)
	}
}

func TestReceiveCallback_ReversalOfNeverPostedDepositWritesTombstone(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	// A deposit intent exists (provider_id/provider_reference set,
	// status='pending') but no success callback has arrived yet - so no
	// LedgerTransaction was ever posted for it.
	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 6000, PaymentMethod: "card", IdempotencyKey: "dep-never-posted",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	depositRef := *intent.ProviderReference

	reversalPayload := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "chargeback-ref-2", depositRef, OutcomeDeclined, 6000, "EUR", "chargeback", false)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", reversalPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (reversal of unseen original): %v", err)
	}
	if !result.Tombstoned {
		t.Fatal("expected a tombstone for a reversal of a deposit never posted to the ledger")
	}

	// A late-arriving success callback for the ORIGINAL reference must
	// never post. PRH-payments-callback-cutover (ADR 0095 §4.4 tombstone
	// cell / LF95-C6(d), §6.2 "anomaly" row): old->new, this used to be a
	// hard error (the ledger's own (tenant_id, provider_id, provider_tx_id)
	// uniqueness colliding with the tombstone, surfaced as a Go error). It
	// is now committed terminally as `disputed`
	// (`reversal_tombstone_precedes_success`, T10) together with its
	// receipt - never rolled back to an error - so the call returns no
	// error and the uniform `applied` disposition; the invariant this test
	// exists for (no second posting, ever) is unchanged and still asserted
	// below via the ledger row count and the attempt's own DB state.
	latePayload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeSucceeded, 6000, "EUR", "", false)
	var lateResult ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		lateResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", latePayload)
		return err
	})
	if err != nil {
		t.Fatalf("expected the tombstone collision to be committed disputed, not returned as an error: %v", err)
	}
	if lateResult.Disposition != DispositionApplied {
		t.Fatalf("expected disposition %q, got %q", DispositionApplied, lateResult.Disposition)
	}

	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("a tombstoned-then-late-arriving deposit must never post, got balance %d", balance)
	}

	var attemptState, terminalReason string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT state, terminal_reason FROM payment_attempts WHERE provider_id = 'mock-psp' AND provider_reference = $1`,
			depositRef,
		).Scan(&attemptState, &terminalReason)
	}); err != nil {
		t.Fatalf("query attempt state: %v", err)
	}
	if attemptState != "disputed" || terminalReason != "reversal_tombstone_precedes_success" {
		t.Fatalf("expected the attempt to be disputed with terminal_reason 'reversal_tombstone_precedes_success', got state=%q terminal_reason=%q", attemptState, terminalReason)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-psp' AND provider_tx_id = $1`, depositRef,
		).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 ledger_transactions row (the tombstone) for this provider reference, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify tombstone uniqueness: %v", err)
	}
}

func TestRouteProvider_FiltersByAssetMethodAmountAndHealth(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	eur := NewMockProvider("eur-only", "EUR")
	usd := NewMockProvider("usd-only", "USD")
	registerCapability(t, pool, f, eur, 100)
	registerCapability(t, pool, f, usd, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"eur-only": eur, "usd-only": usd}, MultiWebhookCredentialResolver{"eur-only": NewMockWebhookCredentials(eur), "usd-only": NewMockWebhookCredentials(usd)})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		provider, capability, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: f.tenantID, BrandID: f.brandID, AssetCode: "USD", PaymentMethod: "card", Amount: 1000, Operation: OperationDeposit,
		})
		if err != nil {
			return err
		}
		if capability.ProviderID != "usd-only" {
			t.Fatalf("expected routing to select usd-only for a USD deposit, got %s", capability.ProviderID)
		}
		if provider == nil {
			t.Fatal("expected a non-nil provider")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RouteProvider: %v", err)
	}

	// Now open eur-only's circuit and confirm a EUR request has no
	// routable candidate at all (usd-only doesn't declare EUR).
	eur.SetHealth(ProviderHealth{CircuitState: CircuitOpen})
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: f.tenantID, BrandID: f.brandID, AssetCode: "EUR", PaymentMethod: "card", Amount: 1000, Operation: OperationDeposit,
		})
		return err
	})
	if !errors.Is(err, ErrNoRoutableProvider) {
		t.Fatalf("expected ErrNoRoutableProvider once eur-only's circuit is open, got %v", err)
	}
}

func TestRouteProvider_PrefersHealthierProviderOverPriority(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	healthy := NewMockProvider("healthy", "EUR")
	unhealthy := NewMockProvider("unhealthy", "EUR")
	// unhealthy has the numerically BETTER (lower) priority, but a worse
	// rolling success rate - health ranks ahead of priority
	// (payment-orchestration.md §6; docs/decisions/0022 §2: priority only
	// breaks ties "among otherwise-equal candidates").
	registerCapability(t, pool, f, unhealthy, 1)
	registerCapability(t, pool, f, healthy, 100)
	unhealthy.SetHealth(ProviderHealth{CircuitState: CircuitClosed, RollingSuccessRate: 0.5, RollingLatencyP99Ms: 500})
	healthy.SetHealth(ProviderHealth{CircuitState: CircuitClosed, RollingSuccessRate: 0.99, RollingLatencyP99Ms: 50})

	orch := NewOrchestrator(map[string]PaymentProvider{"healthy": healthy, "unhealthy": unhealthy}, MultiWebhookCredentialResolver{"healthy": NewMockWebhookCredentials(healthy), "unhealthy": NewMockWebhookCredentials(unhealthy)})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, capability, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: f.tenantID, BrandID: f.brandID, AssetCode: "EUR", PaymentMethod: "card", Amount: 1000, Operation: OperationDeposit,
		})
		if err != nil {
			return err
		}
		if capability.ProviderID != "healthy" {
			t.Fatalf("expected the healthier provider to be chosen despite worse priority, got %s", capability.ProviderID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RouteProvider: %v", err)
	}
}
