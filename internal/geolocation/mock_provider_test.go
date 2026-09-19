package geolocation

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// Compile-time assertion that *MockLocationProvider satisfies
// LocationProvider - mirrors the identical convention used across
// internal/kyc and other provider-abstraction packages.
var _ LocationProvider = (*MockLocationProvider)(nil)

func TestMockLocationProvider_GetSignal_ReturnsConfiguredResultForConfiguredKey(t *testing.T) {
	p := NewMockLocationProvider("mock")
	playerID := uuid.New()
	configured := SignalResult{
		Outcome:           SignalResolved,
		CountryCode:       "DE",
		ProviderReference: "ref-123",
		Reason:            "resolved_by_ip",
	}
	p.SetSignal(playerID.String(), configured)

	got, err := p.GetSignal(context.Background(), SignalRequest{PlayerAccountID: playerID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != configured {
		t.Fatalf("expected %+v, got %+v", configured, got)
	}
}

func TestMockLocationProvider_GetSignal_UnconfiguredKeyReturnsError(t *testing.T) {
	p := NewMockLocationProvider("mock")
	_, err := p.GetSignal(context.Background(), SignalRequest{PlayerAccountID: uuid.New()})
	if err == nil {
		t.Fatal("expected an error for an unconfigured key")
	}
}

func TestMockLocationProvider_SetUnavailable(t *testing.T) {
	p := NewMockLocationProvider("mock")
	playerID := uuid.New()
	p.SetSignal(playerID.String(), SignalResult{Outcome: SignalResolved, CountryCode: "FR"})

	p.SetUnavailable(true)

	if err := p.HealthStatus(context.Background()); err == nil {
		t.Fatal("expected HealthStatus to report an error while unavailable")
	}
	if _, err := p.GetSignal(context.Background(), SignalRequest{PlayerAccountID: playerID}); err == nil {
		t.Fatal("expected GetSignal to fail while unavailable, even for a configured key")
	}

	p.SetUnavailable(false)

	if err := p.HealthStatus(context.Background()); err != nil {
		t.Fatalf("expected HealthStatus to succeed once available again, got %v", err)
	}
	got, err := p.GetSignal(context.Background(), SignalRequest{PlayerAccountID: playerID})
	if err != nil {
		t.Fatalf("expected GetSignal to succeed once available again, got %v", err)
	}
	if got.CountryCode != "FR" {
		t.Fatalf("expected restored configured result, got %+v", got)
	}
}

func TestMockLocationProvider_GetCapabilities_IsStableAndDeterministic(t *testing.T) {
	p := NewMockLocationProvider("mock")
	first := p.GetCapabilities()
	second := p.GetCapabilities()
	if first != second {
		t.Fatalf("expected GetCapabilities to be deterministic, got %+v then %+v", first, second)
	}
	if !first.SupportsCountry {
		t.Error("expected SupportsCountry to be true")
	}
	if first.SupportsSubdivision {
		t.Error("expected SupportsSubdivision to be false for every conceivable Phase-B provider")
	}
	if first.SupportsVPNDetection {
		t.Error("expected the mock to declare no VPN-detection capability")
	}
}

func TestMockLocationProvider_HealthStatus_RespectsUnavailableFlag(t *testing.T) {
	p := NewMockLocationProvider("mock")
	if err := p.HealthStatus(context.Background()); err != nil {
		t.Fatalf("expected a fresh provider to be healthy, got %v", err)
	}
	p.SetUnavailable(true)
	if err := p.HealthStatus(context.Background()); err == nil {
		t.Fatal("expected HealthStatus to report an error while unavailable")
	}
	p.SetUnavailable(false)
	if err := p.HealthStatus(context.Background()); err != nil {
		t.Fatalf("expected HealthStatus to succeed once available again, got %v", err)
	}
}

func TestMockLocationProvider_ID_ReturnsConfiguredID(t *testing.T) {
	p := NewMockLocationProvider("mock-geo-1")
	if p.ID() != "mock-geo-1" {
		t.Fatalf("expected ID() to return the configured id, got %q", p.ID())
	}
}

// TestSignalResult_FieldsAreExactlyTheFourDocumented is a deliberate
// regression guard (Stage 4I Phase B directive): SignalResult must never
// grow a coordinate, city, ISP, ASN, or postal-code field, and must never
// carry a raw vendor payload. If a future well-meaning contributor adds a
// field, this test fails and forces an explicit, reviewed decision rather
// than a silent expansion of what this boundary type can carry.
func TestSignalResult_FieldsAreExactlyTheFourDocumented(t *testing.T) {
	want := []struct {
		name string
		typ  reflect.Kind
	}{
		{"Outcome", reflect.String},
		{"CountryCode", reflect.String},
		{"ProviderReference", reflect.String},
		{"Reason", reflect.String},
	}

	typ := reflect.TypeOf(SignalResult{})
	if typ.NumField() != len(want) {
		t.Fatalf("expected SignalResult to have exactly %d fields, got %d: %v", len(want), typ.NumField(), fieldNames(typ))
	}
	for i, w := range want {
		f := typ.Field(i)
		if f.Name != w.name {
			t.Fatalf("expected field %d to be named %q, got %q (full field list: %v)", i, w.name, f.Name, fieldNames(typ))
		}
		if f.Type.Kind() != w.typ {
			t.Fatalf("expected field %q to have kind %v, got %v", f.Name, w.typ, f.Type.Kind())
		}
	}
}

func fieldNames(typ reflect.Type) []string {
	names := make([]string, typ.NumField())
	for i := range names {
		names[i] = typ.Field(i).Name
	}
	return names
}
