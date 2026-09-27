// Security review RV-PRH-I2 C3: the outbound-credential resolver must be
// chosen by the ADAPTER's own kind, never by "is any mock wired anywhere".
// These are unit tests (no database) for OutboundKindSplitResolver's own
// routing logic, independent of cmd/platform-api's wiring plumbing (which
// has its own TestCasinoOutboundResolver_FollowsWiring).
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// fakeRealResolver is a minimal, non-MOCK OutboundCredentialResolver stand-
// in - it never carries SyntheticComponent(), so the kind split must treat
// its provider id as "real", not "synthetic".
type fakeRealResolver struct{ called int }

func (f *fakeRealResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	f.called++
	return providercred.OutboundCredential{TenantID: tenantID, Domain: "casino", ProviderID: providerID}, nil
}

func TestOutboundKindSplitResolver_SyntheticAdapterUsesMockOnly(t *testing.T) {
	adapters := map[string]CasinoProvider{
		"mock-casino": NewMockCasinoProvider("mock-casino", "EUR"), // carries SyntheticComponent()
	}
	mock := NewMockOutboundResolver()
	real := &fakeRealResolver{}
	r := NewOutboundKindSplitResolver(adapters, mock, real)

	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), nil, tenantID, "mock-casino")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Domain != "casino" || cred.ProviderID != "mock-casino" || cred.TenantID != tenantID {
		t.Fatalf("expected the MOCK's own synthetic credential, got %+v", cred)
	}
	if real.called != 0 {
		t.Fatalf("expected the real resolver never called for a synthetic adapter, got %d calls", real.called)
	}
}

// realCasinoAdapter is a non-synthetic CasinoProvider stand-in. It embeds
// the CasinoProvider INTERFACE (not the *MockCasinoProvider concrete type)
// deliberately: embedding the concrete type would promote EVERY one of its
// methods, including SyntheticComponent() - which is not part of the
// CasinoProvider interface - defeating the point of this fixture.
// Embedding the interface promotes only the interface's own method set, so
// this type genuinely does not implement syntheticCasinoAdapter, exactly
// like a real adapter type that never declares the marker.
type realCasinoAdapter struct{ CasinoProvider }

func TestOutboundKindSplitResolver_NonSyntheticAdapterUsesRealOnly(t *testing.T) {
	inner := NewMockCasinoProvider("real-casino", "EUR")
	adapters := map[string]CasinoProvider{"real-casino": realCasinoAdapter{inner}}
	mock := NewMockOutboundResolver()
	real := &fakeRealResolver{}
	r := NewOutboundKindSplitResolver(adapters, mock, real)

	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), nil, tenantID, "real-casino")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.ProviderID != "real-casino" {
		t.Fatalf("expected the real resolver's credential, got %+v", cred)
	}
	if real.called != 1 {
		t.Fatalf("expected the real resolver called exactly once, got %d", real.called)
	}
}

func TestOutboundKindSplitResolver_UnregisteredProviderFailsClosed(t *testing.T) {
	adapters := map[string]CasinoProvider{"mock-casino": NewMockCasinoProvider("mock-casino", "EUR")}
	r := NewOutboundKindSplitResolver(adapters, NewMockOutboundResolver(), &fakeRealResolver{})

	_, err := r.Resolve(context.Background(), nil, uuid.New(), "never-registered")
	if !errors.Is(err, providercred.ErrOutboundCredentialUnavailable) {
		t.Fatalf("expected ErrOutboundCredentialUnavailable for an unregistered provider id, got %v", err)
	}
}

func TestOutboundKindSplitResolver_BothNilYieldsTrueNilInterface(t *testing.T) {
	r := NewOutboundKindSplitResolver(map[string]CasinoProvider{"mock-casino": NewMockCasinoProvider("mock-casino", "EUR")}, nil, nil)
	if r != nil {
		t.Fatalf("expected a true nil interface when both mock and real are nil, got %T", r)
	}
}

func TestOutboundKindSplitResolver_SyntheticAdapterWithNilMockFailsClosed(t *testing.T) {
	adapters := map[string]CasinoProvider{"mock-casino": NewMockCasinoProvider("mock-casino", "EUR")}
	real := &fakeRealResolver{}
	r := NewOutboundKindSplitResolver(adapters, nil, real)

	_, err := r.Resolve(context.Background(), nil, uuid.New(), "mock-casino")
	if !errors.Is(err, providercred.ErrOutboundCredentialUnavailable) {
		t.Fatalf("expected ErrOutboundCredentialUnavailable for a synthetic adapter with no mock resolver wired, got %v", err)
	}
	if real.called != 0 {
		t.Fatalf("expected the real resolver never called for a synthetic adapter even when mock is nil (no fallback), got %d calls", real.called)
	}
}
