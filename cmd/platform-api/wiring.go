package main

import (
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/payments"
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
}

// mockProviderWiring is a pure function of cfg - no I/O, no globals - so it
// can be unit-tested for every {production, non-production} x {flag on,
// off} combination.
func mockProviderWiring(cfg config.Config) mockWiring {
	testSupport := cfg.TestSupportRoutesEnabled()
	return mockWiring{
		PaymentsWebhookResolver: testSupport,
	}
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
