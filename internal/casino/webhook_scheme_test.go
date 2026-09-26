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
