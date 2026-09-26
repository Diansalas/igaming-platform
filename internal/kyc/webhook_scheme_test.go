package kyc

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

// TestMockKYCProvider_WebhookSchemeConformance: the adapter's own scheme
// passes SC1-SC13.
func TestMockKYCProvider_WebhookSchemeConformance(t *testing.T) {
	f, ok := webhookauthtest.MockSchemeFixture(NewMockKYCProvider().WebhookScheme())
	if !ok {
		t.Fatal("the KYC mock must expose the platform MOCK scheme")
	}
	webhookauthtest.RunSchemeConformance(t, f)
}

// noTimestampScheme declares a real scheme without a signed timestamp
// (ADR 0022 §3 point 10 violation).
type noTimestampScheme struct{ webhookauth.VerificationScheme }

func (noTimestampScheme) Name() string { return "vendor-nots-v1" }
func (noTimestampScheme) Properties() webhookauth.SchemeProperties {
	return webhookauth.SchemeProperties{Binding: webhookauth.BindingPerMerchantKey, KeySelection: webhookauth.KeyFromHeader,
		Replay: webhookauth.ReplayIdempotencyOnly}
}

type noTimestampAdapter struct{ *MockKYCProvider }

func (a noTimestampAdapter) WebhookScheme() webhookauth.VerificationScheme {
	return noTimestampScheme{a.MockKYCProvider.WebhookScheme()}
}

func TestNewOrchestrator_BadWebhookSchemeFailsStartup(t *testing.T) {
	for name, providers := range map[string]map[string]KYCProvider{
		"real scheme without timestamp": {"vendor": noTimestampAdapter{NewMockKYCProvider()}},
		"nil adapter":                   {"mock": nil},
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
	if _, ok := NewOrchestrator(map[string]KYCProvider{"mock": NewMockKYCProvider()}, nil).WebhookScheme("mock"); !ok {
		t.Fatal("the registered adapter's scheme must be selectable by provider id")
	}
}
