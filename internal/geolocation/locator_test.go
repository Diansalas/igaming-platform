package geolocation

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestStaticLocator_DefaultIsUnknown(t *testing.T) {
	l := NewStaticLocator("")
	got, err := l.GetSignal(context.Background(), SignalRequest{PlayerAccountID: uuid.New(), IPAddress: "203.0.113.9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome == SignalResolved || got.CountryCode != "" {
		t.Fatalf("default static locator must report unknown, got %+v", got)
	}
}

func TestStaticLocator_ConfiguredCountryIgnoresRequest(t *testing.T) {
	l := NewStaticLocator("MT")
	a, _ := l.GetSignal(context.Background(), SignalRequest{IPAddress: "198.51.100.1"})
	b, _ := l.GetSignal(context.Background(), SignalRequest{IPAddress: "203.0.113.200", PlayerAccountID: uuid.New()})
	if a.Outcome != SignalResolved || a.CountryCode != "MT" || a != b {
		t.Fatalf("static answer must not depend on the request: %+v vs %+v", a, b)
	}
	l.SetCountry("")
	c, _ := l.GetSignal(context.Background(), SignalRequest{})
	if c.Outcome == SignalResolved {
		t.Fatalf("SetCountry(\"\") must restore unknown, got %+v", c)
	}
}

func TestStaticLocator_IsSyntheticMarker(t *testing.T) {
	var v any = NewStaticLocator("")
	if _, ok := v.(interface{ SyntheticComponent() }); !ok {
		t.Fatal("StaticLocator must carry the providerkind.Synthetic marker so the production guard refuses it")
	}
}

func TestMockLocationProvider_IsALocator(t *testing.T) {
	var _ Locator = NewMockLocationProvider("m")
}
