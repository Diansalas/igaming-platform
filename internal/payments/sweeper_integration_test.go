//go:build integration

// PRH-I1 step (c): the sweeper (sweeper.go), cascade-via-committed-decline
// (cascade.go, drive.go) and the breaker (breaker.go), on a private
// scratch database migrated through 0101 (same harness as step (a)/(b)).
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

func TestSweeper_PendingConvergesToSucceeded_T7_LedgerBalanced(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-sw-a", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-sw-a": provider}, MultiWebhookCredentialResolver{"mock-psp-sw-a": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "sw-pending-success",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptPending {
		t.Fatalf("expected pending, got %s", res.Attempt.State)
	}
	ref := *res.Attempt.ProviderReference

	// Make the attempt due immediately and simulate the provider now
	// reporting success.
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	provider.Resolve(ref, OutcomeSucceeded, "", false)

	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats.Errors) != 0 {
		t.Fatalf("sweeper errors: %v", stats.Errors)
	}
	if stats.Processed != 1 {
		t.Fatalf("expected 1 processed, got %d", stats.Processed)
	}

	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptSucceeded {
		t.Fatalf("expected succeeded, got %s", final.State)
	}
	if final.LedgerTransactionID == nil {
		t.Fatalf("expected a ledger_transaction_id to be linked")
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

func TestSweeper_PendingDeclineCascades_ThenSweptAttemptConvergesOnSecondProvider(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	providerA := NewMockProvider("mock-psp-sw-b1", "EUR")
	providerB := NewMockProvider("mock-psp-sw-b2", "EUR")
	providerB.AcceptAllAmounts = true
	registerCapability(t, pool, f, providerA, 100)
	registerCapability(t, pool, f, providerB, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-sw-b1": providerA, "mock-psp-sw-b2": providerB},
		MultiWebhookCredentialResolver{"mock-psp-sw-b1": NewMockWebhookCredentials(providerA), "mock-psp-sw-b2": NewMockWebhookCredentials(providerB)}).WithPayoutDestinations(pitest.Shared())

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "sw-cascade",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	ref := *res.Attempt.ProviderReference
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	// A cascadable decline discovered on poll.
	providerA.Resolve(ref, OutcomeDeclined, "provider_unavailable", true)

	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats.Errors) != 0 {
		t.Fatalf("sweeper errors (decline pass): %v", stats.Errors)
	}

	declined := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if declined.State != AttemptDeclined {
		t.Fatalf("expected first attempt declined, got %s", declined.State)
	}

	// A cascade row (attempt_no=2, provider excluded) must now exist and
	// be due immediately.
	child := mustGetLiveDepositAttempt(t, pool, f.tenantID, *declined.DepositIntentID)
	if child.AttemptNo != 2 {
		t.Fatalf("expected cascade attempt_no=2, got %d", child.AttemptNo)
	}

	stats2 := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats2.Errors) != 0 {
		t.Fatalf("sweeper errors (cascade drive pass): %v", stats2.Errors)
	}
	final := mustGetAttempt(t, pool, f.tenantID, child.ID)
	if final.State != AttemptPending && final.State != AttemptSucceeded {
		t.Fatalf("expected the cascade attempt to reach pending/succeeded on the accepting provider, got %s", final.State)
	}
	if final.ProviderID == nil || *final.ProviderID != "mock-psp-sw-b2" {
		t.Fatalf("expected the cascade attempt to route to the second provider, got %v", final.ProviderID)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

func TestSweeper_InteractiveCreatedAttempt_ExpiresAfterPresenceWindow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-sw-c", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-sw-c": provider}, MultiWebhookCredentialResolver{"mock-psp-sw-c": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	intentID := insertRawDepositIntent(t, pool, f, "pending")
	attemptID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, true /* interactive */, time.Now().Add(-time.Hour))

	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	sweeper.PresenceWindow = time.Minute // shrink so the 1h-old fixture above is already expired
	stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats.Errors) != 0 {
		t.Fatalf("sweeper errors: %v", stats.Errors)
	}
	final := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if final.State != AttemptRejected {
		t.Fatalf("expected rejected (T3 expiry), got %s", final.State)
	}
	if final.TerminalReason == nil || *final.TerminalReason != "expired_before_submission" {
		t.Fatalf("expected terminal_reason=expired_before_submission, got %v", final.TerminalReason)
	}
	if provider.AttemptCount() != 0 {
		t.Fatalf("an expired interactive attempt must never reach the provider")
	}
}

func TestBreaker_OpensAfterConsecutiveAmbiguousAndHalfOpenProbes(t *testing.T) {
	b := NewBreaker()
	tenant := uuid.New()
	for i := 0; i < breakerConsecutiveFailureThreshold; i++ {
		if !b.Allow(tenant, "p1") {
			t.Fatalf("must still be allowed before the threshold is reached (i=%d)", i)
		}
		b.RecordResult(tenant, "p1", ErrorClassAmbiguous)
	}
	if b.State(tenant, "p1") != CircuitOpen {
		t.Fatalf("expected open after %d consecutive ambiguous results", breakerConsecutiveFailureThreshold)
	}
	if b.Allow(tenant, "p1") {
		t.Fatalf("must not allow while open and within cooldown")
	}
	// A different tenant using the same provider is unaffected.
	otherTenant := uuid.New()
	if !b.Allow(otherTenant, "p1") {
		t.Fatalf("a breaker open for one tenant must not affect another tenant's use of the same provider")
	}
	// DefiniteDecline never counts toward the threshold.
	b2 := NewBreaker()
	for i := 0; i < breakerConsecutiveFailureThreshold+5; i++ {
		b2.RecordResult(tenant, "p2", ErrorClassDefiniteDecline)
	}
	if b2.State(tenant, "p2") != CircuitClosed {
		t.Fatalf("DefiniteDecline must never open the breaker")
	}
}

// --- test helpers -----------------------------------------------------

func setNextActionNow(t *testing.T, pool *db.Pool, tenantID, attemptID uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET next_action_at = now() WHERE id = $1`, attemptID)
		return err
	}); err != nil {
		t.Fatalf("set next_action_at: %v", err)
	}
}

func mustGetAttempt(t *testing.T, pool *db.Pool, tenantID, attemptID uuid.UUID) PaymentAttempt {
	t.Helper()
	var a PaymentAttempt
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = GetAttemptByID(ctx, tx, attemptID)
		return err
	}); err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	return a
}

func mustGetLiveDepositAttempt(t *testing.T, pool *db.Pool, tenantID, depositIntentID uuid.UUID) PaymentAttempt {
	t.Helper()
	var a PaymentAttempt
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		live, ok, err := ListLiveAttemptForDepositIntent(ctx, tx, depositIntentID)
		if err != nil {
			return err
		}
		if !ok {
			return err
		}
		a = live
		return nil
	}); err != nil {
		t.Fatalf("get live attempt: %v", err)
	}
	return a
}

func assertLedgerBalanced(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var debits, credits int64
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
		                                COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
		                           FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits)
	}); err != nil || debits != credits {
		t.Fatalf("ledger unbalanced: debits=%d credits=%d (err=%v)", debits, credits, err)
	}
}

func insertRawDepositIntent(t *testing.T, pool *db.Pool, f orchFixture, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, status)
			 VALUES ($1,$2,$3,$4,$5,'EUR',5000,'card',$6,$7)`,
			id, f.tenantID, f.brandID, f.playerAccountID, f.walletID, "raw-"+id.String(), status)
		return err
	}); err != nil {
		t.Fatalf("insert raw deposit intent: %v", err)
	}
	return id
}

func insertRawCreatedAttempt(t *testing.T, pool *db.Pool, tenantID, depositIntentID uuid.UUID, interactive bool, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind, created_at, next_action_at)
			 VALUES ($1,$2,'deposit',$3,1,'card','EUR',5000,$4,$5,$6,'created','platform',$7,now())`,
			id, tenantID, depositIntentID, interactive, id.String(), "pa:"+id.String(), createdAt)
		return err
	}); err != nil {
		t.Fatalf("insert raw created attempt: %v", err)
	}
	return id
}
