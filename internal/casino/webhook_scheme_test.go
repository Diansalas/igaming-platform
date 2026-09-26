package casino

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

// TestMockCasinoProvider_WebhookSchemeConformance: the adapter's own scheme
// passes SC1-SC13.
func TestMockCasinoProvider_WebhookSchemeConformance(t *testing.T) {
	f, ok := webhookauthtest.MockSchemeFixture(NewMockCasinoProvider("mock-casino", "EUR").WebhookScheme())
	if !ok {
		t.Fatal("the casino mock must expose the platform MOCK scheme")
	}
	webhookauthtest.RunSchemeConformance(t, f)
}

// fakeSyntheticScheme claims Synthetic without being the platform MOCK
// scheme - a real adapter trying to opt out of the timestamp rule and the
// conformance gate.
type fakeSyntheticScheme struct{ webhookauth.VerificationScheme }

func (fakeSyntheticScheme) Name() string { return "vendor-claims-mock-v1" }

type fakeSyntheticAdapter struct{ *MockCasinoProvider }

func (a fakeSyntheticAdapter) WebhookScheme() webhookauth.VerificationScheme {
	return fakeSyntheticScheme{a.MockCasinoProvider.WebhookScheme()}
}

func TestNewOrchestrator_BadWebhookSchemeFailsStartup(t *testing.T) {
	for name, providers := range map[string]map[string]CasinoProvider{
		"non-MOCK scheme declaring Synthetic": {"vendor": fakeSyntheticAdapter{NewMockCasinoProvider("vendor", "EUR")}},
		"nil adapter":                         {"mock-casino": nil},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewOrchestrator must refuse (panic) so startup fails")
				}
			}()
			NewOrchestrator(providers, nil)
		})
	}
	if _, ok := NewOrchestrator(map[string]CasinoProvider{"mock-casino": NewMockCasinoProvider("mock-casino", "EUR")}, nil).WebhookScheme("mock-casino"); !ok {
		t.Fatal("the registered adapter's scheme must be selectable by provider id")
	}
}

// unmarkedCasinoAdapter wraps the MOCK behind the CasinoProvider interface
// only, so it does NOT carry the MOCK's SyntheticComponent marker - the
// shape of a real adapter reusing the platform MOCK scheme. Mirrors
// internal/payments.unmarkedPaymentsAdapter and
// internal/kyc.unmarkedKYCAdapter exactly (Stage 10.3-W1 fix round B).
type unmarkedCasinoAdapter struct{ CasinoProvider }

// crossDomainCasinoSchemeAdapter is a synthetic adapter (embeds the
// concrete *MockCasinoProvider, so SyntheticComponent is promoted)
// returning another domain's canonical MOCK scheme (payments') instead of
// casino's own - a non-canonical MOCK scheme from casino's point of view.
type crossDomainCasinoSchemeAdapter struct{ *MockCasinoProvider }

func (crossDomainCasinoSchemeAdapter) WebhookScheme() webhookauth.VerificationScheme {
	return webhookauth.PaymentsScheme().VerificationScheme()
}

// TestNewOrchestrator_MockSchemeOnlyFromSyntheticAdapter is security S-1 at
// the casino orchestrator (Stage 10.3-W1 fix round B, mirroring the
// identical payments/kyc tests): mustCasinoSchemeSet, routed through
// webhookauth.MustAdapterSchemeSet, must refuse
//   - a Synthetic (casino MOCK) scheme returned by an adapter that does not
//     itself implement SyntheticComponent() - "casino MOCK scheme on an
//     unmarked adapter" below;
//   - a non-canonical MOCK scheme (another domain's canonical MOCK,
//     payments') even from an adapter that IS a synthetic component -
//     "payments MOCK scheme on a synthetic adapter" below.
//
// If mustCasinoSchemeSet were ever reverted to the plain
// webhookauth.MustSchemeSet (pre fix-round-B), this test goes red: neither
// case is rejected by MustSchemeSet alone, since it never inspects the
// registering adapter's own type for SyntheticComponent().
func TestNewOrchestrator_MockSchemeOnlyFromSyntheticAdapter(t *testing.T) {
	for name, providers := range map[string]map[string]CasinoProvider{
		"casino MOCK scheme on an unmarked adapter":   {"vendor-x": unmarkedCasinoAdapter{NewMockCasinoProvider("vendor-x", "EUR")}},
		"payments MOCK scheme on a synthetic adapter": {"mock-casino": crossDomainCasinoSchemeAdapter{NewMockCasinoProvider("mock-casino", "EUR")}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewOrchestrator must refuse (panic) so startup fails")
				}
			}()
			NewOrchestrator(providers, nil)
		})
	}
}
