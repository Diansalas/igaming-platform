package main

import (
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// mockWiring says which MOCK provider components this deployment wires
// (Stage 10.2, ADR 0091; architect review §3, ruling J9 / PAYWH-GATE-1).
// It is derived ONLY from cfg.TestSupportRoutesEnabled() (ADR 0085 §1), the
// single fail-closed two-layer gate every test-support seam already uses:
// production is structurally off (config.Load refuses the flag there, and
// TestSupportRoutesEnabled is false for production regardless).
//
// Later Stage 10.2 steps extend this struct (KYC mock provider/resolver/
// route, casino mock webhook resolver) so every mock-wiring decision comes
// from this one, unit-tested value.
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
}

// mockProviderWiring is a pure function of cfg - no I/O, no globals - so it
// can be unit-tested for every {production, non-production} x {flag on,
// off} combination.
func mockProviderWiring(cfg config.Config) mockWiring {
	testSupport := cfg.TestSupportRoutesEnabled()
	return mockWiring{
		PaymentsWebhookResolver: testSupport,
		KYCWebhookEnabled:       testSupport,
	}
}

// kycWebhookResolver returns the KYC Orchestrator's single injected
// webhook credential resolver for w: the MOCK resolver bound to mock when
// w enables it, otherwise a true nil interface (never a typed nil), so
// the Orchestrator's own nil-resolver branch fails every callback closed
// as ReasonNoResolver.
func kycWebhookResolver(w mockWiring, mock *kyc.MockKYCProvider) webhookauth.Resolver {
	if !w.KYCWebhookEnabled || mock == nil {
		return nil
	}
	return kyc.NewMockWebhookCredentials(mock)
}

// kycOrchestrator builds the KYC Orchestrator for w (design §B3): a nil
// *kyc.Orchestrator entirely - not merely a nil resolver - when w disables
// KYC, so player self-service KYC (POST /v1/me/kyc/verifications) is
// itself unavailable (503) in production/test-support-off, matching the
// design's disclosed consequence that player-initiated KYC needs the MOCK
// provider this stage ships. mock is still constructed by the caller and
// passed in unconditionally purely so callers that need a *MockKYCProvider
// reference for other purposes have one; this function is what decides
// whether it is ever wired into anything reachable.
func kycOrchestrator(w mockWiring, mock *kyc.MockKYCProvider) *kyc.Orchestrator {
	if !w.KYCWebhookEnabled || mock == nil {
		return nil
	}
	return kyc.NewOrchestrator(map[string]kyc.KYCProvider{mock.ID(): mock}, kycWebhookResolver(w, mock))
}

// paymentsWebhookResolver returns the payments Orchestrator's single
// injected webhook credential resolver for w: the MOCK resolver bound to
// mock when w enables it, otherwise a true nil interface (never a typed
// nil or an empty map), so the Orchestrator's own nil-resolver branch
// fails every callback closed as ReasonNoResolver.
func paymentsWebhookResolver(w mockWiring, mock *payments.MockProvider) payments.WebhookCredentialResolver {
	if !w.PaymentsWebhookResolver || mock == nil {
		return nil
	}
	return payments.MultiWebhookCredentialResolver{
		mock.Capabilities().ProviderID: payments.NewMockWebhookCredentials(mock),
	}
}
