//go:build integration

// PRH-I1 step (b): InitiateDepositAttempt (deposit_v2.go), the
// provider-call gate (gate.go) and the contract additions (contract.go),
// exercised end to end against a PRIVATE scratch database migrated all
// the way up (including migration 0101), rather than the shared
// TEST_DATABASE_URL - which is not guaranteed to have 0101 applied in
// every checkout/CI run this step lands in, and per the "avoid
// contention with other agents" instruction, schema-dependent tests use
// their own scratch instance via TEST_ADMIN_DATABASE_URL, exactly like
// migration_0101_integration_test.go's own harness.
package payments

import (
	"context"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// depositV2ScratchPool creates a fresh scratch database migrated all the
// way up through migration 0101 (tolerating the transient 0100 gap - see
// migration_0101_integration_test.go's own doc comment) and returns a
// connected *db.Pool, exactly like testPool(t) but on private storage.
func depositV2ScratchPool(t *testing.T) *db.Pool {
	t.Helper()
	v := migration0101Version(t)
	pool, _ := migration0101Scratch(t, "m0101v2_", v)
	// PRH-I1 kill switch (migration 0105): every claim-path CAS function
	// (ClaimCreatedForSubmission/InsertSubmittingAttempt/ResubmitAmbiguous)
	// now evaluates the kill-switch predicate unconditionally, so any
	// scratch database exercising them needs that table too, not just 0101's.
	if _, err := pool.MigrateUp(context.Background(), realMigrationsDir(t)); err != nil {
		t.Fatalf("migrate up to latest: %v", err)
	}
	// SIGNED-ACTOR-PROOF (ADR 0110, migration 0120): provision the per-process
	// random signing key into this scratch database and install the issuer.
	prooftest.InstallVia(t, pool.Raw(), t.Name()+"/"+uuid.NewString())
	return pool
}

// txscopeCapturingProvider wraps a *MockProvider and records, for every
// Deposit call, whether ctx was marked as holding a pooled transaction
// (txscope.Held) at the moment the call happened - the direct,
// deterministic proof of INV-IO-1 for this step's own call site (no
// adapter-method call runs while a transaction is held).
type txscopeCapturingProvider struct {
	*MockProvider
	mu       sync.Mutex
	heldSeen []bool
}

func (p *txscopeCapturingProvider) Deposit(ctx context.Context, req DepositRequest) (DepositResult, error) {
	p.mu.Lock()
	p.heldSeen = append(p.heldSeen, txscope.Held(ctx))
	p.mu.Unlock()
	return p.MockProvider.Deposit(ctx, req)
}

func TestInitiateDepositAttempt_INV_IO_1_NoTxHeldDuringProviderCall(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	inner := NewMockProvider("mock-psp-v2-a", "EUR")
	provider := &txscopeCapturingProvider{MockProvider: inner}
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-a": provider}, MultiWebhookCredentialResolver{"mock-psp-v2-a": NewMockWebhookCredentials(inner)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "v2-txscope",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if !res.AttemptCreated {
		t.Fatalf("expected an attempt to be created")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.heldSeen) != 1 {
		t.Fatalf("expected exactly 1 Deposit call, got %d", len(provider.heldSeen))
	}
	if provider.heldSeen[0] {
		t.Fatalf("INV-IO-1 violated: Deposit was called while ctx was marked as holding a pooled transaction")
	}
}

func TestInitiateDepositAttempt_PendingOutcome_T4(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-v2-b", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-b": provider}, MultiWebhookCredentialResolver{"mock-psp-v2-b": NewMockWebhookCredentials(provider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "v2-pending",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptPending {
		t.Fatalf("expected attempt state pending, got %s", res.Attempt.State)
	}
	if res.Attempt.ProviderReference == nil || *res.Attempt.ProviderReference == "" {
		t.Fatalf("expected a provider_reference to be set by T4")
	}
	if res.Intent.Status != DepositIntentPending {
		t.Fatalf("expected intent status pending, got %s", res.Intent.Status)
	}
}

func TestInitiateDepositAttempt_DeclinedOutcome_T8(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-v2-c", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-c": provider}, MultiWebhookCredentialResolver{"mock-psp-v2-c": NewMockWebhookCredentials(provider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountPlayerDeclineNoCascade, PaymentMethod: "card", IdempotencyKey: "v2-declined",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptDeclined {
		t.Fatalf("expected attempt state declined, got %s", res.Attempt.State)
	}
	if res.Attempt.Cascadable == nil || *res.Attempt.Cascadable {
		t.Fatalf("expected cascadable=false for this decline")
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected intent status declined, got %s", res.Intent.Status)
	}
}

// TestInitiateDepositAttempt_NonCascadableDeclineNeverCascadesToAvailableFallback
// closes a genuine coverage gap found while mutation-testing drive.go's
// cascadeEligible (PROV-OUTBOUND-CRED-1-LEGACY-PATH/E2 follow-up,
// coordinator-requested mutant run): TestInitiateDepositAttempt_
// DeclinedOutcome_T8 above registers only ONE provider, so a mutant that
// removes cascadeEligible's own `if !evidenceCascadable { return false }`
// guard is NOT observable there - with no second provider to route to,
// the outcome (declined) is identical whether or not the guard runs. This
// test registers a SECOND, accepting provider so the guard's own effect
// is actually observable: a non-cascadable decline must stay declined
// even though a fallback that would have accepted the deposit is sitting
// right there, unused.
func TestInitiateDepositAttempt_NonCascadableDeclineNeverCascadesToAvailableFallback(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	declining := newSpyProvider(NewMockProvider("mock-psp-v2-nc-a", "EUR"))
	fallback := newSpyProvider(NewMockProvider("mock-psp-v2-nc-b", "EUR"))
	fallback.AcceptAllAmounts = true
	registerCapability(t, pool, f, declining, 10)
	registerCapability(t, pool, f, fallback, 20)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-nc-a": declining, "mock-psp-v2-nc-b": fallback}, MultiWebhookCredentialResolver{"mock-psp-v2-nc-a": NewMockWebhookCredentials(declining.MockProvider), "mock-psp-v2-nc-b": NewMockWebhookCredentials(fallback.MockProvider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountPlayerDeclineNoCascade, PaymentMethod: "card", IdempotencyKey: "v2-nc-fallback",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptDeclined {
		t.Fatalf("expected attempt state declined, got %s", res.Attempt.State)
	}
	if res.Attempt.Cascadable == nil || *res.Attempt.Cascadable {
		t.Fatalf("expected cascadable=false for this decline")
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected intent status declined, got %s", res.Intent.Status)
	}
	if got := fallback.DepositCallCount(); got != 0 {
		t.Fatalf("a non-cascadable decline must never cascade, but the available fallback's Deposit was called %d times", got)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("expected no ledger effect, got balance %d", balance)
	}
}

// TestInitiateDepositAttempt_AmbiguousOutcome_T6 also closes
// PROV-OUTBOUND-CRED-1-LEGACY-PATH (E2)'s own migration of the deleted
// legacy chain's TestInitiateDeposit_AmbiguousOutcomeCallsQueryStatus
// BeforeNotCascading: that test's subject (a synchronous, in-transaction
// provider.QueryStatus call, made before ever finalizing an ambiguous
// deposit ambiguous or cascading it) is the exact QueryStatus-in-tx
// anti-pattern ADR 0095 removes (see receive_bridge_integration_test.go's
// own doc comment on the identical, already-adapted callback-path case,
// TestReceiveCallback_AmbiguousCallbackResolvedViaQueryStatus_NotCascaded).
// InitiateDepositAttempt (deposit_v2.go) never calls QueryStatus at all -
// an ambiguous synchronous Deposit outcome is left `ambiguous` for a
// later sweeper/T17 touch, never resolved inline. This test therefore
// asserts the STRICT OPPOSITE of the deleted test's own assertion (zero
// QueryStatus calls, not exactly one) while keeping every one of its
// other guarantees (no cascade to a second provider, no ledger effect) -
// an equally strict, never weaker, replacement, not a silent drop.
func TestInitiateDepositAttempt_AmbiguousOutcome_T6(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := newSpyProvider(NewMockProvider("mock-psp-v2-d", "EUR"))
	fallback := newSpyProvider(NewMockProvider("mock-psp-v2-d-fallback", "EUR"))
	fallback.AcceptAllAmounts = true
	registerCapability(t, pool, f, provider, 10)
	registerCapability(t, pool, f, fallback, 20)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-d": provider, "mock-psp-v2-d-fallback": fallback}, MultiWebhookCredentialResolver{"mock-psp-v2-d": NewMockWebhookCredentials(provider.MockProvider), "mock-psp-v2-d-fallback": NewMockWebhookCredentials(fallback.MockProvider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountAmbiguous, PaymentMethod: "card", IdempotencyKey: "v2-ambiguous",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptAmbiguous {
		t.Fatalf("expected attempt state ambiguous, got %s", res.Attempt.State)
	}
	if !res.Attempt.EverPossiblySent {
		t.Fatalf("ambiguous must set ever_possibly_sent=true")
	}
	if res.Intent.Status != DepositIntentAmbiguous {
		t.Fatalf("expected intent status ambiguous, got %s", res.Intent.Status)
	}
	if got := provider.QueryStatusCallCount(); got != 0 {
		t.Fatalf("expected zero synchronous QueryStatus calls (resolution belongs to the sweeper, not phase B), got %d", got)
	}
	if got := fallback.DepositCallCount(); got != 0 {
		t.Fatalf("an ambiguous outcome must never auto-cascade to another provider, but the fallback's Deposit was called %d times", got)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("an ambiguous outcome must post nothing, got balance %d", balance)
	}
}

// depositKillSwitchStaffPrincipal is killSwitchStaffPrincipal's (payout_
// dispatch_fixround_test.go) exact twin for a deposit-scoped test in this
// file - a genuine, active tenant_admin staff_users row, since
// payment_kill_switch_session() requires one, not an arbitrary uuid.
func depositKillSwitchStaffPrincipal(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, $2, $3, 'x', 'tenant_admin', 'active')`,
			id, tenantID, "ks-deposit-staff-"+id.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("create staff principal: %v", err)
	}
	return id
}

// TestInitiateDepositAttempt_KillSwitchEngaged_DeclinesCleanly_T3 is the
// Phase 2 orchestrator wiring item: a T1+T2 kill-switch refusal at
// InsertSubmittingAttempt must become a clean, terminal T3 decline (never
// a 500), with the "kill_switch" label living only in deposit.declined's
// own audit metadata - the response DTO never surfaces decline_reason, so
// the player-facing outcome is generic.
func TestInitiateDepositAttempt_KillSwitchEngaged_DeclinesCleanly_T3(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	inner := NewMockProvider("mock-psp-v2-ks", "EUR")
	provider := newSpyProvider(inner)
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-ks": provider}, MultiWebhookCredentialResolver{"mock-psp-v2-ks": NewMockWebhookCredentials(inner)})

	principal := depositKillSwitchStaffPrincipal(t, pool, f.tenantID)
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, "mock-psp-v2-ks", KillSwitchOperationDeposit, "phase2-test")
		return err
	}); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "v2-kill-switch",
	})
	if err != nil {
		t.Fatalf("expected a clean decline, not an error: %v", err)
	}
	if res.AttemptCreated {
		t.Fatal("expected no attempt to be created while the kill switch is engaged")
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected intent status declined (T3), got %s", res.Intent.Status)
	}
	if provider.DepositCallCount() != 0 {
		t.Fatalf("expected 0 provider.Deposit calls while the kill switch was engaged, got %d", provider.DepositCallCount())
	}

	var reason, auditProviderID string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'decline_reason', coalesce(metadata->>'provider_id', '') FROM audit_log WHERE action = 'deposit.declined' AND target_id = $1`,
			res.Intent.ID.String(),
		).Scan(&reason, &auditProviderID)
	}); err != nil {
		t.Fatalf("read decline audit: %v", err)
	}
	if reason != "kill_switch" {
		t.Fatalf("expected decline_reason = kill_switch, got %q", reason)
	}
	// C5 (RV-PRH-I1 kill-switch phase 2 code review): the audit row (and
	// the intent's own provider_id column) must show WHICH provider's
	// switch fired.
	if auditProviderID != "mock-psp-v2-ks" {
		t.Fatalf("expected the decline audit to carry provider_id, got %q", auditProviderID)
	}
	if res.Intent.ProviderID == nil || *res.Intent.ProviderID != "mock-psp-v2-ks" {
		t.Fatalf("expected the declined intent's own provider_id column to be set, got %v", res.Intent.ProviderID)
	}
}

// TestDriveCreatedAttemptCascade_KillSwitchOnFallbackProvider_DeclinesCleanly_T3
// is KS-DEP-T2-T3-1 (architect review rv-prh-i1-killswitch-phase2-
// architect.md, §"New finding"): a kill switch scoped to the FALLBACK
// provider only (not the original, cascadable-declining one) makes the
// cascade's own T2 claim (ClaimCreatedForSubmission, driven from
// driveCreatedAttempt) match zero rows. Before this fix, that surfaced as
// a plain Go error from InitiateDepositAttempt - the player's synchronous
// request failed outright, the cascade attempt was stuck in 'created' and
// the intent stuck 'pending' forever (no sweeper is wired in
// cmd/platform-api to ever pick it back up). This must instead be a
// clean, terminal T3 decline: no error, the cascade attempt rejected with
// reason "kill_switch", the intent finalized declined, and no provider
// call ever made against the switched-off fallback.
func TestDriveCreatedAttemptCascade_KillSwitchOnFallbackProvider_DeclinesCleanly_T3(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	decliningInner := NewMockProvider("mock-ks-t2t3-a", "EUR")
	declining := newSpyProvider(decliningInner)
	fallbackInner := NewMockProvider("mock-ks-t2t3-b", "EUR")
	fallbackInner.AcceptAllAmounts = true
	fallback := newSpyProvider(fallbackInner)
	registerCapability(t, pool, f, declining, 10)
	registerCapability(t, pool, f, fallback, 20)
	orch := NewOrchestrator(
		map[string]PaymentProvider{"mock-ks-t2t3-a": declining, "mock-ks-t2t3-b": fallback},
		MultiWebhookCredentialResolver{"mock-ks-t2t3-a": NewMockWebhookCredentials(decliningInner), "mock-ks-t2t3-b": NewMockWebhookCredentials(fallbackInner)},
	)

	// Engage the switch on the FALLBACK provider only - the original,
	// cascadably-declining provider is untouched, so routing still
	// proceeds past it into the cascade exactly as it would without any
	// switch engaged.
	principal := depositKillSwitchStaffPrincipal(t, pool, f.tenantID)
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, "mock-ks-t2t3-b", KillSwitchOperationDeposit, "ks-dep-t2-t3-1-test")
		return err
	}); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountProviderDeclineCascade, PaymentMethod: "card", IdempotencyKey: "ks-t2-t3-1",
	})
	if err != nil {
		t.Fatalf("expected a clean decline, not an error: %v", err)
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected intent status declined (T3), got %s", res.Intent.Status)
	}
	if declining.DepositCallCount() != 1 {
		t.Fatalf("expected exactly 1 Deposit call to the original (cascadably-declining) provider, got %d", declining.DepositCallCount())
	}
	if fallback.DepositCallCount() != 0 {
		t.Fatalf("expected 0 Deposit calls to the switched-off fallback provider, got %d", fallback.DepositCallCount())
	}

	// The cascade child (attempt_no=2) must be terminally rejected, never
	// stuck in 'created'.
	var cascadeState, terminalReason string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT state, coalesce(terminal_reason, '') FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 2`,
			res.Intent.ID,
		).Scan(&cascadeState, &terminalReason)
	}); err != nil {
		t.Fatalf("read cascade attempt: %v", err)
	}
	if cascadeState != string(AttemptRejected) {
		t.Fatalf("expected the cascade attempt to be rejected (never stuck in created), got %s", cascadeState)
	}
	if terminalReason != "kill_switch" {
		t.Fatalf("expected terminal_reason = kill_switch, got %q", terminalReason)
	}

	// Audited, with the provider whose switch actually fired.
	var auditProviderID string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'provider_id' FROM audit_log WHERE action = 'payments.cascade_rejected_kill_switch' AND target_type = 'payment_attempt'`,
		).Scan(&auditProviderID)
	}); err != nil {
		t.Fatalf("read cascade kill-switch audit: %v", err)
	}
	if auditProviderID != "mock-ks-t2t3-b" {
		t.Fatalf("expected the cascade kill-switch audit to name the fallback provider, got %q", auditProviderID)
	}
}

// mismatchedDomainResolver deliberately returns a credential whose Domain
// never matches what the gate expects, so the gate's own binding check
// (step 4, S95-C8(b)) refuses the call with ErrorClassNotSent - exercising
// T5 (submitting->created) without needing a real credential store.
type mismatchedDomainResolver struct{ calls int32 }

func (r *mismatchedDomainResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	atomic.AddInt32(&r.calls, 1)
	return providercred.OutboundCredential{TenantID: tenantID, ProviderID: providerID, Domain: "wrong-domain"}, nil
}

func TestInitiateDepositAttempt_CredentialBindingMismatch_T5_NoCall(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-v2-e", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-e": provider}, MultiWebhookCredentialResolver{"mock-psp-v2-e": NewMockWebhookCredentials(provider)})

	resolver := &mismatchedDomainResolver{}
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, resolver, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "v2-cred-mismatch",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptCreated {
		t.Fatalf("expected attempt to revert to created (T5) on a credential binding mismatch, got %s", res.Attempt.State)
	}
	if res.Attempt.EverPossiblySent {
		t.Fatalf("a NotSent refusal on a first send must leave ever_possibly_sent=false (INV-IO-9)")
	}
	if atomic.LoadInt32(&resolver.calls) != 1 {
		t.Fatalf("expected the resolver to be called exactly once, got %d", resolver.calls)
	}
	// The MOCK adapter's own Deposit must never have been called.
	if provider.AttemptCount() != 0 {
		t.Fatalf("expected zero provider calls, got %d", provider.AttemptCount())
	}
}

func TestDenyingKYCGate_DeniesBeforeAnyAttempt(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-v2-f", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-f": provider}, MultiWebhookCredentialResolver{"mock-psp-v2-f": NewMockWebhookCredentials(provider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, denyingKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "v2-kyc-deny",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.AttemptCreated {
		t.Fatalf("a KYC deny must never create an attempt")
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected intent status declined on KYC deny, got %s", res.Intent.Status)
	}
	if provider.AttemptCount() != 0 {
		t.Fatalf("expected zero provider calls on a KYC deny, got %d", provider.AttemptCount())
	}
}

type denyingKYCGate struct{}

func (denyingKYCGate) EvaluateDeposit(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, string) (bool, string, error) {
	return false, "kyc_test_deny", nil
}

func TestRedactedReason_StripsURLQueryAndUserinfo(t *testing.T) {
	u, _ := url.Parse("https://psp.invalid/charge?api_key=SECRET&card=4111")
	u.User = url.UserPassword("user", "hunter2")
	uerr := &url.Error{Op: "Post", URL: u.String(), Err: context.DeadlineExceeded}
	got := redactedReason(uerr)
	if got != "Post: timeout" {
		t.Fatalf("expected a redacted, allow-listed reason, got %q", got)
	}
}

func TestCallProvider_PanicIsMappedToAmbiguous_NeverNotSent(t *testing.T) {
	claimToken := uuid.New()
	in := callProviderInput{
		TenantID: uuid.New(), ProviderID: "mock-psp-panic",
		AttemptState: AttemptSubmitting, ClaimToken: claimToken, ExpectedClaim: claimToken,
		Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
	}
	gr := callProvider(context.Background(), nil, MockCredentialResolver{}, in, func(context.Context, CallContext) (DepositResult, ErrorClass, error) {
		panic("simulated adapter panic")
	})
	if gr.Class != ErrorClassAmbiguous {
		t.Fatalf("expected a recovered panic to classify as Ambiguous, got %s", gr.Class)
	}
	if gr.Err == nil {
		t.Fatalf("expected a non-nil error describing the recovered panic")
	}
}

// alwaysFailingOutboundResolver simulates a credential-resolution outage
// (a transient store failure, a resolver timeout) - never a provider call
// without credentials.
type alwaysFailingOutboundResolver struct{ calls int32 }

func (r *alwaysFailingOutboundResolver) Resolve(context.Context, providercred.TenantTxRunner, uuid.UUID, string) (providercred.OutboundCredential, error) {
	atomic.AddInt32(&r.calls, 1)
	return providercred.OutboundCredential{}, providercred.ErrOutboundCredentialUnavailable
}

// TestCallProvider_CredentialResolutionOutage_MapsToNotSent_NeverCallsAdapter
// is PROV-OUTBOUND-CRED-1 phase 2 orchestrator wiring's required T5
// mapping: a credential-resolution failure (an outage, a timeout, an
// unregistered/expired/revoked handle) is ALWAYS ErrorClassNotSent, and
// the adapter function itself is never invoked - the call provably never
// reached the provider.
func TestCallProvider_CredentialResolutionOutage_MapsToNotSent_NeverCallsAdapter(t *testing.T) {
	claimToken := uuid.New()
	in := callProviderInput{
		TenantID: uuid.New(), ProviderID: "mock-psp-outage",
		AttemptState: AttemptSubmitting, ClaimToken: claimToken, ExpectedClaim: claimToken,
		Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
	}
	resolver := &alwaysFailingOutboundResolver{}
	var adapterCalls int32
	gr := callProvider(context.Background(), nil, resolver, in, func(context.Context, CallContext) (DepositResult, ErrorClass, error) {
		atomic.AddInt32(&adapterCalls, 1)
		return DepositResult{}, ErrorClassSucceeded, nil
	})
	if gr.Class != ErrorClassNotSent {
		t.Fatalf("expected a credential-resolution outage to map to NotSent (T5), got %s", gr.Class)
	}
	if gr.Attempted {
		t.Fatal("expected Attempted=false: a credential-resolution failure is a pre-flight refusal, not a call")
	}
	if atomic.LoadInt32(&adapterCalls) != 0 {
		t.Fatal("expected the adapter function to never be invoked when credential resolution fails")
	}
	if atomic.LoadInt32(&resolver.calls) != 1 {
		t.Fatalf("expected exactly one resolver call, got %d", resolver.calls)
	}
}

// TestOutboundKindSplitResolver_UnregisteredProviderFailsClosed_NoAdapterCall
// proves the SAME T5 guarantee end to end through the actual kind-split
// resolver InitiateDepositAttempt/DispatchWithdraw use in production
// (cmd/platform-api's paymentsOutboundCredentials()): a provider id never
// registered in the adapters map the resolver was built from is refused,
// never routed to either the mock or the real resolver, and never reaches
// an adapter call.
func TestOutboundKindSplitResolver_UnregisteredProviderFailsClosed_NoAdapterCall(t *testing.T) {
	mock := NewMockProvider("kind-split-registered", "EUR")
	resolver := NewOutboundKindSplitResolver(map[string]PaymentProvider{"kind-split-registered": mock}, MockCredentialResolver{}, MockCredentialResolver{})

	claimToken := uuid.New()
	in := callProviderInput{
		TenantID: uuid.New(), ProviderID: "kind-split-unregistered",
		AttemptState: AttemptSubmitting, ClaimToken: claimToken, ExpectedClaim: claimToken,
		Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
	}
	var adapterCalls int32
	gr := callProvider(context.Background(), nil, resolver, in, func(context.Context, CallContext) (DepositResult, ErrorClass, error) {
		atomic.AddInt32(&adapterCalls, 1)
		return DepositResult{}, ErrorClassSucceeded, nil
	})
	if gr.Class != ErrorClassNotSent {
		t.Fatalf("expected an unregistered provider id to map to NotSent (T5), got %s", gr.Class)
	}
	if atomic.LoadInt32(&adapterCalls) != 0 {
		t.Fatal("expected the adapter function to never be invoked for an unregistered provider id")
	}
}

// wrongTenantResolver returns a correctly-SHAPED credential (right
// provider, right domain) but for a DIFFERENT tenant than the one it was
// asked to resolve for - simulating a future caching/derived-token
// resolver bug, never today's real or MOCK resolver.
type wrongTenantResolver struct{ wrongTenant uuid.UUID }

func (r wrongTenantResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, _ uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	return providercred.OutboundCredential{TenantID: r.wrongTenant, ProviderID: providerID, Domain: "payments"}, nil
}

// TestCallProvider_CredentialForWrongTenant_RefusedByBindingCheck is
// RV-PRH-I1 kill-switch phase 2 review P2-L1's required test: the gate's
// own step-4 binding check (S95-C8(b)) must independently refuse a
// credential resolved for the WRONG tenant, even though nothing in this
// codebase's real or MOCK resolver can produce one today - this is the
// gate's own defence-in-depth layer, not merely documentation of an
// already-impossible case.
func TestCallProvider_CredentialForWrongTenant_RefusedByBindingCheck(t *testing.T) {
	claimToken := uuid.New()
	requestedTenant := uuid.New()
	in := callProviderInput{
		TenantID: requestedTenant, ProviderID: "mock-psp-wrong-tenant",
		AttemptState: AttemptSubmitting, ClaimToken: claimToken, ExpectedClaim: claimToken,
		Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
	}
	resolver := wrongTenantResolver{wrongTenant: uuid.New()}
	var adapterCalls int32
	gr := callProvider(context.Background(), nil, resolver, in, func(context.Context, CallContext) (DepositResult, ErrorClass, error) {
		atomic.AddInt32(&adapterCalls, 1)
		return DepositResult{}, ErrorClassSucceeded, nil
	})
	if gr.Class != ErrorClassNotSent {
		t.Fatalf("expected a wrong-tenant credential to map to NotSent (binding refusal), got %s", gr.Class)
	}
	if atomic.LoadInt32(&adapterCalls) != 0 {
		t.Fatal("expected the adapter function to never be invoked for a wrong-tenant credential")
	}
}

// TestCallProvider_NilResolver_RefusesCleanly_NeverPanics is RV-PRH-I1
// kill-switch phase 2 security review P2-L3's required test:
// NewOutboundKindSplitResolver returns a true nil interface when nothing
// is wired, and calling Resolve on a nil interface would otherwise panic
// (resolver.Resolve runs before step 6's safeCall, so a panic there is
// NOT recovered) - callProvider's own step 0 must refuse cleanly with
// NotSent instead.
func TestCallProvider_NilResolver_RefusesCleanly_NeverPanics(t *testing.T) {
	claimToken := uuid.New()
	in := callProviderInput{
		TenantID: uuid.New(), ProviderID: "mock-psp-nil-resolver",
		AttemptState: AttemptSubmitting, ClaimToken: claimToken, ExpectedClaim: claimToken,
		Domain: "payments", Manifest: OperationManifest{CallTimeout: time.Second},
	}
	var adapterCalls int32
	gr := callProvider(context.Background(), nil, nil, in, func(context.Context, CallContext) (DepositResult, ErrorClass, error) {
		atomic.AddInt32(&adapterCalls, 1)
		return DepositResult{}, ErrorClassSucceeded, nil
	})
	if gr.Class != ErrorClassNotSent {
		t.Fatalf("expected a nil resolver to map to NotSent, got %s", gr.Class)
	}
	if atomic.LoadInt32(&adapterCalls) != 0 {
		t.Fatal("expected the adapter function to never be invoked with a nil resolver")
	}
}
