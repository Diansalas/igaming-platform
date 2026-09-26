package payments

import (
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

// TestMockProvider_WebhookSchemeConformance: the adapter's own scheme (the
// one the orchestrator and preamble actually use) passes SC1-SC13.
func TestMockProvider_WebhookSchemeConformance(t *testing.T) {
	f, ok := webhookauthtest.MockSchemeFixture(NewMockProvider("mock-psp").WebhookScheme())
	if !ok {
		t.Fatal("the payments mock must expose the platform MOCK scheme")
	}
	webhookauthtest.RunSchemeConformance(t, f)
}

// badDeclarationScheme wraps a valid scheme but declares a non-permitted
// Properties() (a real scheme with a 1-hour MaxSkew, above the cap).
type badDeclarationScheme struct{ webhookauth.VerificationScheme }

func (badDeclarationScheme) Name() string { return "vendor-bad-v1" }
func (badDeclarationScheme) Properties() webhookauth.SchemeProperties {
	return webhookauth.SchemeProperties{Binding: webhookauth.BindingPerMerchantKey, KeySelection: webhookauth.KeyFromHeader,
		SignedTimestamp: true, MaxSkew: time.Hour, Replay: webhookauth.ReplayTimestampWindow}
}

type badSchemeAdapter struct{ *MockProvider }

func (a badSchemeAdapter) WebhookScheme() webhookauth.VerificationScheme {
	return badDeclarationScheme{a.MockProvider.WebhookScheme()}
}

// TestNewOrchestrator_BadWebhookSchemeFailsStartup (security C11): a bad
// declaration, a nil adapter, or a provider id outside the webhook charset
// makes construction panic, so the process refuses to start.
func TestNewOrchestrator_BadWebhookSchemeFailsStartup(t *testing.T) {
	for name, providers := range map[string]map[string]PaymentProvider{
		"unlisted real scheme with MaxSkew above cap": {"vendor-bad": badSchemeAdapter{NewMockProvider("vendor-bad")}},
		"nil adapter":     {"mock-psp": nil},
		"bad provider id": {"Bad_ID": NewMockProvider("Bad_ID")},
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
	if o := NewOrchestrator(map[string]PaymentProvider{"mock-psp": NewMockProvider("mock-psp")}, nil); o == nil {
		t.Fatal("a valid registry must construct")
	}
	if _, ok := NewOrchestrator(map[string]PaymentProvider{"mock-psp": NewMockProvider("mock-psp")}, nil).WebhookScheme("mock-psp"); !ok {
		t.Fatal("the registered adapter's scheme must be selectable by provider id")
	}
}

// unmarkedPaymentsAdapter wraps the MOCK behind the PaymentProvider
// interface only, so it does NOT carry the MOCK's SyntheticComponent
// marker - the shape of a real adapter reusing the platform MOCK scheme.
type unmarkedPaymentsAdapter struct{ PaymentProvider }

// crossDomainSchemeAdapter is a synthetic adapter returning the casino MOCK.
type crossDomainSchemeAdapter struct{ *MockProvider }

func (crossDomainSchemeAdapter) WebhookScheme() webhookauth.VerificationScheme {
	return webhookauth.CasinoScheme().VerificationScheme()
}

// TestNewOrchestrator_MockSchemeOnlyFromSyntheticAdapter is security S-1 at
// the payments orchestrator: the payments MOCK scheme from an adapter that
// is not itself a synthetic component, or another domain's MOCK scheme,
// makes construction panic (startup fails).
func TestNewOrchestrator_MockSchemeOnlyFromSyntheticAdapter(t *testing.T) {
	for name, providers := range map[string]map[string]PaymentProvider{
		"payments MOCK scheme on an unmarked adapter": {"vendor-x": unmarkedPaymentsAdapter{NewMockProvider("vendor-x")}},
		"casino MOCK scheme on a synthetic adapter":   {"mock-psp": crossDomainSchemeAdapter{NewMockProvider("mock-psp")}},
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
