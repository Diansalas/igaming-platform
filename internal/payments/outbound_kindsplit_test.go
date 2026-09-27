// RV-PRH-I1 kill-switch phase 2 code review C1: the outbound-credential
// resolver must be chosen by the ADAPTER's own kind, never by "is any mock
// wired anywhere". These are unit tests (no database) for
// OutboundKindSplitResolver's own routing logic, independent of
// cmd/platform-api's wiring plumbing (which has its own
// TestPaymentsOutboundResolver_FollowsWiring) - ported from
// internal/casino/outbound_kindsplit_test.go and internal/kyc's identical
// twin, using DISTINCT mock and real resolvers so the choice is
// observable (the review's own finding: the pre-existing
// TestOutboundKindSplitResolver_UnregisteredProviderFailsClosed_NoAdapterCall
// used MockCredentialResolver{} as BOTH mock and real, so it could not
// tell the two apart - M1b, "everything goes to mock", survived).
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// fakeRealPaymentResolver is a minimal, non-MOCK OutboundCredentialResolver
// stand-in - it never carries SyntheticComponent(), so the kind split must
// treat its provider id as "real", not "synthetic". Distinct from
// MockCredentialResolver's own credential shape so a test can tell, by
// inspecting the returned credential's HandleID, which resolver actually
// answered.
type fakeRealPaymentResolver struct{ called int }

func (f *fakeRealPaymentResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	f.called++
	return providercred.OutboundCredential{TenantID: tenantID, Domain: "payments", ProviderID: providerID, HandleID: uuid.New(), KeyID: "real-key"}, nil
}

func TestOutboundKindSplitResolver_SyntheticAdapterUsesMockOnly(t *testing.T) {
	adapters := map[string]PaymentProvider{
		"mock-payments": NewMockProvider("mock-payments", "EUR"), // carries SyntheticComponent()
	}
	mock := MockCredentialResolver{}
	real := &fakeRealPaymentResolver{}
	r := NewOutboundKindSplitResolver(adapters, mock, real)

	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), nil, tenantID, "mock-payments")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Domain != "payments" || cred.ProviderID != "mock-payments" || cred.TenantID != tenantID {
		t.Fatalf("expected the MOCK's own synthetic credential, got %+v", cred)
	}
	if cred.KeyID == "real-key" {
		t.Fatal("expected the MOCK's own credential shape, not the real resolver's")
	}
	if real.called != 0 {
		t.Fatalf("expected the real resolver never called for a synthetic adapter, got %d calls", real.called)
	}
}

// realPaymentAdapter is a non-synthetic PaymentProvider stand-in. It embeds
// the PaymentProvider INTERFACE (not the *MockProvider concrete type)
// deliberately: embedding the concrete type would promote EVERY one of its
// methods, including SyntheticComponent() - which is not part of the
// PaymentProvider interface - defeating the point of this fixture.
// Embedding the interface promotes only the interface's own method set, so
// this type genuinely does not implement syntheticPaymentAdapter, exactly
// like a real adapter type that never declares the marker.
type realPaymentAdapter struct{ PaymentProvider }

func TestOutboundKindSplitResolver_NonSyntheticAdapterUsesRealOnly(t *testing.T) {
	inner := NewMockProvider("real-payments", "EUR")
	adapters := map[string]PaymentProvider{"real-payments": realPaymentAdapter{inner}}
	mock := MockCredentialResolver{}
	real := &fakeRealPaymentResolver{}
	r := NewOutboundKindSplitResolver(adapters, mock, real)

	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), nil, tenantID, "real-payments")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.ProviderID != "real-payments" {
		t.Fatalf("expected the real resolver's credential, got %+v", cred)
	}
	if cred.KeyID != "real-key" {
		t.Fatal("expected the real resolver's own credential shape, not the MOCK's")
	}
	if real.called != 1 {
		t.Fatalf("expected the real resolver called exactly once, got %d", real.called)
	}
}

func TestOutboundKindSplitResolver_UnregisteredProviderFailsClosed(t *testing.T) {
	adapters := map[string]PaymentProvider{"mock-payments": NewMockProvider("mock-payments", "EUR")}
	r := NewOutboundKindSplitResolver(adapters, MockCredentialResolver{}, &fakeRealPaymentResolver{})

	_, err := r.Resolve(context.Background(), nil, uuid.New(), "never-registered")
	if !errors.Is(err, providercred.ErrOutboundCredentialUnavailable) {
		t.Fatalf("expected ErrOutboundCredentialUnavailable for an unregistered provider id, got %v", err)
	}
}

func TestOutboundKindSplitResolver_BothNilYieldsTrueNilInterface(t *testing.T) {
	r := NewOutboundKindSplitResolver(map[string]PaymentProvider{"mock-payments": NewMockProvider("mock-payments", "EUR")}, nil, nil)
	if r != nil {
		t.Fatalf("expected a true nil interface when both mock and real are nil, got %T", r)
	}
}

func TestOutboundKindSplitResolver_SyntheticAdapterWithNilMockFailsClosed(t *testing.T) {
	adapters := map[string]PaymentProvider{"mock-payments": NewMockProvider("mock-payments", "EUR")}
	real := &fakeRealPaymentResolver{}
	r := NewOutboundKindSplitResolver(adapters, nil, real)

	_, err := r.Resolve(context.Background(), nil, uuid.New(), "mock-payments")
	if !errors.Is(err, providercred.ErrOutboundCredentialUnavailable) {
		t.Fatalf("expected ErrOutboundCredentialUnavailable for a synthetic adapter with no mock resolver wired, got %v", err)
	}
	if real.called != 0 {
		t.Fatalf("expected the real resolver never called for a synthetic adapter even when mock is nil (no fallback), got %d calls", real.called)
	}
}
