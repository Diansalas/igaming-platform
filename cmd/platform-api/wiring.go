package main

import (
	"log/slog"

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

	// CasinoOutboundResolver wires the casino MOCK outbound-credential
	// resolver LaunchGame's phase B uses (ADR 0095 §15.1, PROV-OUTBOUND-
	// CRED-1; security review RV-PRH-I2 C3). Chosen by ADAPTER KIND, not
	// merely "is this true" - casinoOutboundCredentials() routes it through
	// casino.NewOutboundKindSplitResolver, which serves it only to a
	// registered SYNTHETIC casino adapter. When false, a launch against a
	// synthetic adapter fails closed ("no outbound credential resolver
	// configured") rather than falling back to the real, unconfigured
	// resolver.
	CasinoOutboundResolver bool

	// KYCOutboundResolver wires the KYC MOCK outbound-credential resolver
	// CreateVerification/SubmitVerification's phase B uses (ADR 0095
	// §15.2/§15.3, PROV-OUTBOUND-CRED-1) - CasinoOutboundResolver's KYC
	// twin. Chosen by ADAPTER KIND via kycOutboundCredentials()'s
	// kyc.NewOutboundKindSplitResolver, which serves it only to a
	// registered SYNTHETIC kyc adapter. When false, a call against a
	// synthetic adapter fails closed rather than falling back to the real,
	// unconfigured resolver.
	KYCOutboundResolver bool

	// PaymentsOutboundResolver wires the payments MOCK outbound-credential
	// resolver InitiateDepositAttempt/DispatchWithdraw's phase B uses (ADR
	// 0095 §9.1/§11, PROV-OUTBOUND-CRED-1, phase 2 orchestrator wiring) -
	// CasinoOutboundResolver's payments twin. Chosen by ADAPTER KIND via
	// paymentsOutboundCredentials()'s payments.NewOutboundKindSplitResolver,
	// which serves it only to a registered SYNTHETIC payments adapter.
	// When false, a call against a synthetic adapter fails closed (T5,
	// never a provider call without credentials) rather than falling back
	// to the real, unconfigured resolver.
	PaymentsOutboundResolver bool

	// GeolocationCountry is the country the MOCK static Locator
	// (geolocation.StaticLocator) reports for every request (A8): the
	// operator-set MOCK_GEOLOCATION_COUNTRY, honoured ONLY when mock
	// components are enabled (test support on, non-production guard
	// environment). Empty (owner decision 3) means the gate's Locator is the
	// production-eligible KYC-verified-residence source instead
	// (marketEvidenceLocator). It is never derived from a request.
	GeolocationCountry string
}

// testSupportRoutesGate is the SINGLE function every test-support seam in
// this binary derives from (U-6, defence in depth over ADR 0085 §1): the
// four httpserver.Deps route-enable flags (CasinoPlaySimulationEnabled,
// PaymentsMockSettlementEnabled, AccountActivationTestSupportEnabled,
// SportsbookSettlementSimulationEnabled, set in main.go) and
// mockProviderWiring (which in turn sets Deps.KYCWebhookEnabled and every
// MOCK resolver). test_support_gate_test.go pins, by AST, that no other
// derivation exists.
//
// It is true only when BOTH:
//   - cfg.TestSupportRoutesEnabled() (ADR 0085's two-layer gate:
//     Environment != "production" AND TEST_SUPPORT_ENDPOINTS_ENABLED), and
//   - cfg.GuardEnvironment() is one of the explicit non-production values
//     "development" or "staging" - an ALLOW-list, not "!= production", so
//     GuardEnvironment()=="production" (which includes a MISSING APP_ENV)
//     and any value config.Load() would itself have rejected (an explicit
//     empty APP_ENV, "test", a typo) are all off even for a Config that
//     was constructed without going through Load().
//
// config.Load() already refuses production + test support, and the
// synthetic-component guard (refuseSyntheticInProduction) already refuses
// a missing APP_ENV before any route is built, so for a Load()-produced
// Config this changes nothing reachable today: explicit development/
// staging with the flag on keep the documented ADR 0085 behaviour. It
// exists so that the route flags no longer depend on those earlier,
// separately-maintained checks running first.
func testSupportRoutesGate(cfg config.Config) bool {
	switch cfg.GuardEnvironment() {
	case "development", "staging":
		return cfg.TestSupportRoutesEnabled()
	default:
		return false
	}
}

// mockProviderWiring is a pure function of cfg - no I/O, no globals - so it
// can be unit-tested for every {production, missing APP_ENV, staging,
// development} x {flag on, off} combination.
//
// Stage 10.3 gate-W1 fix round (security S-4, code review #2): on top of
// TestSupportRoutesEnabled() it also requires GuardEnvironment() to be an
// explicit non-production value (via testSupportRoutesGate, U-6), so a
// MISSING APP_ENV (which TestSupportRoutesEnabled alone reads as
// "development") can never wire a MOCK resolver or the KYC mock.
// Explicit staging/development keep the TestSupportRoutesEnabled
// semantics unchanged.
func mockProviderWiring(cfg config.Config) mockWiring {
	testSupport := testSupportRoutesGate(cfg)
	geolocationCountry := ""
	if testSupport {
		geolocationCountry = cfg.MockGeolocationCountry
	}
	return mockWiring{
		GeolocationCountry:       geolocationCountry,
		PaymentsWebhookResolver:  testSupport,
		KYCWebhookEnabled:        testSupport,
		CasinoWebhookResolver:    testSupport,
		CasinoOutboundResolver:   testSupport,
		KYCOutboundResolver:      testSupport,
		PaymentsOutboundResolver: testSupport,
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
// here except the orchestrator itself. logger receives the matched-key_id
// line (W2A-SEC-2); nil means slog.Default().
func kycOrchestrator(w mockWiring, b providerBundle, logger *slog.Logger) *kyc.Orchestrator {
	if !w.KYCWebhookEnabled || b.KYC == nil {
		return nil
	}
	o := kyc.NewOrchestrator(b.kycAdapters(), b.kycOrchestratorResolver())
	o.SetWebhookLogger(logger)
	return o
}
