package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// wiringOffConfigs are the configurations that must wire NO MOCK webhook
// resolver and no KYC mock: production (flag on or off), an explicit
// staging deployment with test support off, and - Stage 10.3 gate-W1 fix
// round, security S-4 - a MISSING APP_ENV (EnvironmentExplicit false) even
// with the test-support flag on.
var wiringOffConfigs = []config.Config{
	{Environment: "production", EnvironmentExplicit: true, TestSupportEndpointsEnabled: true},
	{Environment: "production", EnvironmentExplicit: true, TestSupportEndpointsEnabled: false},
	{Environment: "staging", EnvironmentExplicit: true, TestSupportEndpointsEnabled: false},
	{Environment: "development", EnvironmentExplicit: false, TestSupportEndpointsEnabled: true},
}

// wiringOnConfig is an explicit staging deployment with test support on.
var wiringOnConfig = config.Config{Environment: "staging", EnvironmentExplicit: true, TestSupportEndpointsEnabled: true}

// TestMockProviderWiring_Matrix is PAYWH-GATE-1 (ruling J9) / K11's
// payments leg: mock wiring follows TestSupportRoutesEnabled() for an
// explicit APP_ENV, and is always off when GuardEnvironment() is
// production - including a MISSING APP_ENV (security S-4, code review #2).
func TestMockProviderWiring_Matrix(t *testing.T) {
	cases := []struct {
		env          string
		explicit     bool
		flag         bool
		wantPayments bool
	}{
		// config.Load refuses production+flag, but the wiring must fail
		// closed on its own too (defence in depth, ADR 0085 layer 2).
		{"production", true, true, false},
		{"production", true, false, false},
		{"staging", true, true, true},
		{"staging", true, false, false},
		{"development", true, true, true},
		{"development", true, false, false},
		// APP_ENV missing: Load defaults Environment to "development", so
		// TestSupportRoutesEnabled() alone would be true - GuardEnvironment
		// folds it into production, so nothing is wired.
		{"development", false, true, false},
		{"development", false, false, false},
	}
	for _, c := range cases {
		cfg := config.Config{Environment: c.env, EnvironmentExplicit: c.explicit, TestSupportEndpointsEnabled: c.flag}
		got := mockProviderWiring(cfg)
		if got.PaymentsWebhookResolver != c.wantPayments {
			t.Errorf("env=%s explicit=%v flag=%v: PaymentsWebhookResolver=%v, want %v", c.env, c.explicit, c.flag, got.PaymentsWebhookResolver, c.wantPayments)
		}
		want := cfg.TestSupportRoutesEnabled() && cfg.GuardEnvironment() != "production"
		if got.PaymentsWebhookResolver != want {
			t.Errorf("env=%s explicit=%v flag=%v: wiring diverges from TestSupportRoutesEnabled() && GuardEnvironment() != production", c.env, c.explicit, c.flag)
		}
		// KYC-WH-1 (Stage 10.2, ADR 0091, ruling J7/K11): the KYC leg must
		// move in lockstep - route registration and resolver/orchestrator
		// wiring come from this SAME field, so they cannot diverge.
		if got.KYCWebhookEnabled != want {
			t.Errorf("env=%s explicit=%v flag=%v: KYCWebhookEnabled diverges", c.env, c.explicit, c.flag)
		}
		// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091, ruling C10/design §C7).
		if got.CasinoWebhookResolver != c.wantPayments {
			t.Errorf("env=%s explicit=%v flag=%v: CasinoWebhookResolver=%v, want %v", c.env, c.explicit, c.flag, got.CasinoWebhookResolver, c.wantPayments)
		}
	}
}

// TestKYCOrchestrator_FollowsWiring is K11's KYC leg, built through the
// bundle exactly as main() builds it: every wiring-off configuration yields
// no KYC mock, no KYC resolver and a nil *kyc.Orchestrator entirely (not
// merely a nil resolver); an explicit staging deployment with test support
// on yields a working Orchestrator whose resolver verifies the mock's own
// callbacks.
func TestKYCOrchestrator_FollowsWiring(t *testing.T) {
	for _, cfg := range wiringOffConfigs {
		w := mockProviderWiring(cfg)
		b := buildProviderBundle(w)
		if b.KYC != nil || b.KYCWebhookResolver != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected no KYC mock and no KYC resolver in the bundle", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled)
		}
		if orch := kycOrchestrator(w, b, nil); orch != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected a nil *kyc.Orchestrator, got %v", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled, orch)
		}
		if r := b.kycOrchestratorResolver(); r != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected a nil resolver interface, got %T", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled, r)
		}
	}
	// Defence in depth: a wiring value that enables KYC with a bundle that
	// has no KYC mock still yields no orchestrator.
	if orch := kycOrchestrator(mockWiring{KYCWebhookEnabled: true}, providerBundle{}, nil); orch != nil {
		t.Fatal("expected a nil *kyc.Orchestrator for a bundle without a KYC mock")
	}

	w := mockProviderWiring(wiringOnConfig)
	b := buildProviderBundle(w)
	orch := kycOrchestrator(w, b, nil)
	if orch == nil {
		t.Fatal("expected a working *kyc.Orchestrator with test support on")
	}
	if _, ok := orch.Provider("mock"); !ok {
		t.Fatal("expected the mock provider to be registered under its own id")
	}

	mock := b.KYC
	tenantID := uuid.New()
	in := mock.CallbackPayload(tenantID, "some-ref", kyc.ProviderApproved, "x")
	resolver := b.kycOrchestratorResolver()
	cred, err := resolveKey(resolver, tenantID, "mock", webhookauth.MockKeyID)
	if err != nil || cred.TenantID != tenantID || cred.ProviderID != "mock" {
		t.Fatalf("MOCK resolver must resolve its own provider, got %v / %v", cred, err)
	}
	if _, err := mock.HandleCallback(context.Background(), in, cred); err != nil {
		t.Fatalf("mock callback must verify under the wired resolver, got %v", err)
	}
	if _, err := resolveKey(resolver, tenantID, "other", webhookauth.MockKeyID); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("MOCK resolver must fail closed for another provider, got %v", err)
	}
}

// TestCasinoWebhookResolver_FollowsWiring is CAS-WH-TENANT-1's casino leg:
// a TRUE nil interface when off (so the Orchestrator's nil-resolver branch
// rejects every callback as no_resolver -> uniform 401, design §C7/C10),
// and a working MOCK resolver bound to the bundle's mock adapter when on.
func TestCasinoWebhookResolver_FollowsWiring(t *testing.T) {
	for _, cfg := range wiringOffConfigs {
		b := buildProviderBundle(mockProviderWiring(cfg))
		if b.CasinoWebhookResolver != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected no casino MOCK resolver in the bundle", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled)
		}
		if r := b.casinoOrchestratorResolver(); r != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected a nil resolver interface, got %T", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled, r)
		}
	}

	b := buildProviderBundle(mockProviderWiring(wiringOnConfig))
	r := b.casinoOrchestratorResolver()
	if r == nil {
		t.Fatal("expected the MOCK resolver with test support on")
	}
	mock := b.Casino
	tenantID := uuid.New()
	cred, err := resolveKey(r, tenantID, "mock-casino", "mock-v1")
	if err != nil || cred.TenantID != tenantID || cred.ProviderID != "mock-casino" {
		t.Fatalf("MOCK resolver must resolve its own provider, got %v / %v", cred, err)
	}
	if _, err := resolveKey(r, tenantID, "other", "mock-v1"); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("MOCK resolver must fail closed for another provider, got %v", err)
	}
	// And a callback signed by the mock verifies under exactly this
	// resolver's credential.
	in := mock.CallbackPayload(tenantID, casino.CallbackEventBet, "bet-wiring-1", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
	if _, err := mock.HandleCallback(context.Background(), in, cred); err != nil {
		t.Fatalf("mock callback must verify under the wired resolver, got %v", err)
	}
}

// TestCasinoOutboundResolver_FollowsWiring is security review RV-PRH-I2
// C3's fix, casino's OUTBOUND twin of TestCasinoWebhookResolver_
// FollowsWiring: a TRUE nil interface when off, and a working MOCK
// resolver - reached ONLY via casinoOutboundCredentials()'s kind split, not
// merely because b.CasinoOutboundResolver is set - when on. The kind
// split's own routing-by-adapter-identity logic is unit-tested directly in
// internal/casino (TestOutboundKindSplitResolver_*); this test only proves
// the wiring itself reaches the mock adapter's own provider id.
func TestCasinoOutboundResolver_FollowsWiring(t *testing.T) {
	for _, cfg := range wiringOffConfigs {
		b := buildProviderBundle(mockProviderWiring(cfg))
		if b.CasinoOutboundResolver != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected no casino outbound MOCK resolver in the bundle", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled)
		}
		if r := b.casinoOutboundCredentials(); r != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected a nil outbound resolver interface, got %T", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled, r)
		}
	}

	b := buildProviderBundle(mockProviderWiring(wiringOnConfig))
	r := b.casinoOutboundCredentials()
	if r == nil {
		t.Fatal("expected the outbound MOCK resolver with test support on")
	}
	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), nil, tenantID, "mock-casino")
	if err != nil || cred.TenantID != tenantID || cred.ProviderID != "mock-casino" || cred.Domain != "casino" {
		t.Fatalf("outbound MOCK resolver must resolve its own registered synthetic provider, got %v / %v", cred, err)
	}
	if _, err := r.Resolve(context.Background(), nil, tenantID, "other-provider"); err == nil {
		t.Fatal("outbound MOCK resolver must fail closed for an unregistered provider id")
	}
}

// TestPaymentsWebhookResolver_FollowsWiring proves the resolver actually
// injected follows the wiring value: a TRUE nil interface when off (so the
// Orchestrator's nil-resolver branch rejects every callback as
// no_resolver -> uniform 401, covered end-to-end by
// internal/httpserver TestWebhook_AuthFailureLogging_AllowListOnly and
// internal/payments' nil-resolver tenant-binding test), and a working
// MOCK resolver bound to the bundle's mock adapter when on.
func TestPaymentsWebhookResolver_FollowsWiring(t *testing.T) {
	for _, cfg := range wiringOffConfigs {
		b := buildProviderBundle(mockProviderWiring(cfg))
		if b.PaymentsWebhookResolver != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected no payments MOCK resolver in the bundle", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled)
		}
		if r := b.paymentsOrchestratorResolver(); r != nil {
			t.Fatalf("env=%s explicit=%v flag=%v: expected a nil resolver interface, got %T", cfg.Environment, cfg.EnvironmentExplicit, cfg.TestSupportEndpointsEnabled, r)
		}
	}

	b := buildProviderBundle(mockProviderWiring(wiringOnConfig))
	r := b.paymentsOrchestratorResolver()
	if r == nil {
		t.Fatal("expected the MOCK resolver with test support on")
	}
	mock := b.Payments
	tenantID := uuid.New()
	cred, err := resolveKey(r, tenantID, "mock-payments", "mock-v1")
	if err != nil || cred.TenantID != tenantID || cred.ProviderID != "mock-payments" {
		t.Fatalf("MOCK resolver must resolve its own provider, got %v / %v", cred, err)
	}
	if _, err := resolveKey(r, tenantID, "other", "mock-v1"); !errors.Is(err, payments.ErrWebhookCredentialUnavailable) {
		t.Fatalf("MOCK resolver must fail closed for another provider, got %v", err)
	}
	// And a callback signed by the mock verifies under exactly this
	// resolver's credential.
	in := mock.CallbackPayload(tenantID, payments.CallbackEventDeposit, "ref", "", payments.OutcomeSucceeded, 100, "EUR", "", false)
	if _, err := mock.HandleCallback(context.Background(), in, cred); err != nil {
		t.Fatalf("mock callback must verify under the wired resolver, got %v", err)
	}
}

// TestBundleAdapters_ProviderIDsUnchanged pins the provider ids main()
// registers (previously literals in main.go): the payments and casino
// mock adapters must keep distinct ids (Stage 9.3 finding) and match what
// every tenant capability row and test fixture uses.
func TestBundleAdapters_ProviderIDsUnchanged(t *testing.T) {
	b := buildProviderBundle(mockProviderWiring(wiringOnConfig))
	if _, ok := b.paymentsAdapters()["mock-payments"]; !ok || len(b.paymentsAdapters()) != 1 {
		t.Fatalf("payments adapters = %v, want exactly mock-payments", b.paymentsAdapters())
	}
	if _, ok := b.casinoAdapters()["mock-casino"]; !ok || len(b.casinoAdapters()) != 1 {
		t.Fatalf("casino adapters = %v, want exactly mock-casino", b.casinoAdapters())
	}
	if _, ok := b.kycAdapters()["mock"]; !ok || len(b.kycAdapters()) != 1 {
		t.Fatalf("kyc adapters = %v, want exactly mock", b.kycAdapters())
	}
	if n := len(buildProviderBundle(mockWiring{}).kycAdapters()); n != 0 {
		t.Fatalf("kyc adapters with KYC unwired = %d, want 0", n)
	}
}

// resolveKey resolves one KeyFromHeader credential through the composed
// orchestrator resolver (ADR 0093 §4 signature; a MOCK resolver ignores
// the nil tx).
func resolveKey(r webhookauth.Resolver, tenantID uuid.UUID, providerID, keyID string) (webhookauth.Credential, error) {
	set, err := r.Resolve(context.Background(), nil, tenantID, providerID, keyID, webhookauth.KeyFromHeader)
	return set.Active, err
}
