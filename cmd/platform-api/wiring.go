package main

import (
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// mockWiring says which MOCK provider components this deployment wires
// (Stage 10.2, ADR 0091; architect review §3, ruling J9 / PAYWH-GATE-1).
// It is derived from cfg.TestSupportRoutesEnabled() (ADR 0085 §1), the
// single fail-closed two-layer gate every test-support seam already uses,
// AND cfg.GuardEnvironment() (see mockProviderWiring): production and a
// missing APP_ENV are structurally off.
type mockWiring struct {
	// PaymentsWebhookResolver wires the payments MOCK webhook credential
	// resolver. When false the payments Orchestrator gets a nil resolver,
	// so every payments webhook fails closed with a uniform 401
	// (reason no_resolver) - the real resolver is NOT IMPLEMENTED.
	PaymentsWebhookResolver bool

	// KYCWebhookEnabled is Stage 10.2's KYC-WH-1 fix (ADR 0091, architect
	// ruling R5/J7): the KYC mock provider, its webhook credential
	// resolver, and the webhook ROUTE ITSELF are all wired from this one
	// field, so route registration and credential resolution can never
	// diverge (K11) - unlike payments (whose webhook route stays
	// registered unconditionally per the design's environment matrix),
	// the KYC webhook route is ABSENT (404) when this is false, since
	// there is no legitimate reason to expose a public callback route for
	// a provider that was never wired.
	KYCWebhookEnabled bool

	// CasinoWebhookResolver wires the casino MOCK webhook credential
	// resolver (Stage 10.2, CAS-WH-TENANT-1, ADR 0091, design §C7).
	// When false the casino Orchestrator gets a nil resolver, so every
	// casino webhook fails closed with a uniform 401 (reason
	// no_resolver) - no real aggregator exists, so this is correct, not
	// merely a placeholder. The mock adapter itself stays registered for
	// catalogue and launch (MOCK-ADAPTER-PROD-1, out of this scope).
	CasinoWebhookResolver bool
}

// mockProviderWiring is a pure function of cfg - no I/O, no globals - so it
// can be unit-tested for every {production, missing APP_ENV, staging,
// development} x {flag on, off} combination.
//
// Stage 10.3 gate-W1 fix round (security S-4, code review #2): on top of
// TestSupportRoutesEnabled() it also requires GuardEnvironment() !=
// "production", so a MISSING APP_ENV (which TestSupportRoutesEnabled alone
// reads as "development") can never wire a MOCK resolver or the KYC mock.
// Explicit staging/development keep the TestSupportRoutesEnabled
// semantics unchanged.
func mockProviderWiring(cfg config.Config) mockWiring {
	testSupport := cfg.TestSupportRoutesEnabled() && cfg.GuardEnvironment() != "production"
	return mockWiring{
		PaymentsWebhookResolver: testSupport,
		KYCWebhookEnabled:       testSupport,
		CasinoWebhookResolver:   testSupport,
	}
}

// kycOrchestrator builds the KYC Orchestrator for w (design §B3): a nil
// *kyc.Orchestrator entirely - not merely a nil resolver - when w disables
// KYC, so player self-service KYC (POST /v1/me/kyc/verifications) is
// itself unavailable (503) in production/test-support-off, matching the
// design's disclosed consequence that player-initiated KYC needs the MOCK
// provider this stage ships. b.KYC is nil whenever w disables KYC
// (buildProviderBundle constructs it only when w.KYCWebhookEnabled, ADR
// 0085 amendment: "absent", not merely unwired) - this function's own nil
// check is defence in depth, not the sole gate. The adapter and resolver
// are the bundle's own instances (security S-4): nothing is constructed
// here except the orchestrator itself.
func kycOrchestrator(w mockWiring, b providerBundle) *kyc.Orchestrator {
	if !w.KYCWebhookEnabled || b.KYC == nil {
		return nil
	}
	return kyc.NewOrchestrator(b.kycAdapters(), b.kycOrchestratorResolver())
}
