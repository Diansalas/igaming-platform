// Security review RV-PRH-I2 KYC C2: the outbound-credential resolver must
// be chosen by the ADAPTER's own kind, never by "is any mock wired
// anywhere" - a mutant making OutboundKindSplitResolver.Resolve always
// prefer the mock resolver survived the full `internal/kyc` and
// `cmd/platform-api` suites because no test exercised this routing logic
// directly. These are unit tests (no database) for
// OutboundKindSplitResolver's own routing logic, independent of
// cmd/platform-api's wiring plumbing (which has its own
// TestKYCOutboundResolver_FollowsWiring) - mirrors
// internal/casino/outbound_kindsplit_test.go exactly.
package kyc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// fakeRealKYCResolver is a minimal, non-MOCK OutboundCredentialResolver
// stand-in - it never carries SyntheticComponent(), so the kind split must
// treat its provider id as "real", not "synthetic".
type fakeRealKYCResolver struct{ called int }

func (f *fakeRealKYCResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	f.called++
	return providercred.OutboundCredential{TenantID: tenantID, Domain: "kyc", ProviderID: providerID}, nil
}

func TestKYCOutboundKindSplitResolver_SyntheticAdapterUsesMockOnly(t *testing.T) {
	adapters := map[string]KYCProvider{
		"mock": NewMockKYCProvider(), // carries SyntheticComponent()
	}
	mock := NewMockOutboundResolver()
	real := &fakeRealKYCResolver{}
	r := NewOutboundKindSplitResolver(adapters, mock, real)

	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), nil, tenantID, "mock")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Domain != "kyc" || cred.ProviderID != "mock" || cred.TenantID != tenantID {
		t.Fatalf("expected the MOCK's own synthetic credential, got %+v", cred)
	}
	if real.called != 0 {
		t.Fatalf("expected the real resolver never called for a synthetic adapter, got %d calls", real.called)
	}
}

// realKYCAdapter is a non-synthetic KYCProvider stand-in. It embeds the
// KYCProvider INTERFACE (not the *MockKYCProvider concrete type)
// deliberately: embedding the concrete type would promote EVERY one of its
// methods, including SyntheticComponent() - which is not part of the
// KYCProvider interface - defeating the point of this fixture. Embedding
// the interface promotes only the interface's own method set, so this type
// genuinely does not implement syntheticKYCAdapter, exactly like a real
// adapter type that never declares the marker.
type realKYCAdapter struct{ KYCProvider }

func TestKYCOutboundKindSplitResolver_NonSyntheticAdapterUsesRealOnly(t *testing.T) {
	inner := NewMockKYCProvider()
	adapters := map[string]KYCProvider{"real-kyc": realKYCAdapter{inner}}
	mock := NewMockOutboundResolver()
	real := &fakeRealKYCResolver{}
	r := NewOutboundKindSplitResolver(adapters, mock, real)

	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), nil, tenantID, "real-kyc")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.ProviderID != "real-kyc" {
		t.Fatalf("expected the real resolver's credential, got %+v", cred)
	}
	if real.called != 1 {
		t.Fatalf("expected the real resolver called exactly once, got %d", real.called)
	}
}

func TestKYCOutboundKindSplitResolver_UnregisteredProviderFailsClosed(t *testing.T) {
	adapters := map[string]KYCProvider{"mock": NewMockKYCProvider()}
	r := NewOutboundKindSplitResolver(adapters, NewMockOutboundResolver(), &fakeRealKYCResolver{})

	_, err := r.Resolve(context.Background(), nil, uuid.New(), "never-registered")
	if !errors.Is(err, providercred.ErrOutboundCredentialUnavailable) {
		t.Fatalf("expected ErrOutboundCredentialUnavailable for an unregistered provider id, got %v", err)
	}
}

func TestKYCOutboundKindSplitResolver_BothNilYieldsTrueNilInterface(t *testing.T) {
	r := NewOutboundKindSplitResolver(map[string]KYCProvider{"mock": NewMockKYCProvider()}, nil, nil)
	if r != nil {
		t.Fatalf("expected a true nil interface when both mock and real are nil, got %T", r)
	}
}

func TestKYCOutboundKindSplitResolver_SyntheticAdapterWithNilMockFailsClosed(t *testing.T) {
	adapters := map[string]KYCProvider{"mock": NewMockKYCProvider()}
	real := &fakeRealKYCResolver{}
	r := NewOutboundKindSplitResolver(adapters, nil, real)

	_, err := r.Resolve(context.Background(), nil, uuid.New(), "mock")
	if !errors.Is(err, providercred.ErrOutboundCredentialUnavailable) {
		t.Fatalf("expected ErrOutboundCredentialUnavailable for a synthetic adapter with no mock resolver wired, got %v", err)
	}
	if real.called != 0 {
		t.Fatalf("expected the real resolver never called for a synthetic adapter even when mock is nil (no fallback), got %d calls", real.called)
	}
}
