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

func TestInitiateDepositAttempt_AmbiguousOutcome_T6(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-v2-d", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-v2-d": provider}, MultiWebhookCredentialResolver{"mock-psp-v2-d": NewMockWebhookCredentials(provider)})

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

	var reason string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'decline_reason' FROM audit_log WHERE action = 'deposit.declined' AND target_id = $1`,
			res.Intent.ID.String(),
		).Scan(&reason)
	}); err != nil {
		t.Fatalf("read decline audit: %v", err)
	}
	if reason != "kill_switch" {
		t.Fatalf("expected decline_reason = kill_switch, got %q", reason)
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
