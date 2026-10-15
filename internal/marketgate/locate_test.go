package marketgate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/geolocation"
)

type fakeLocator struct {
	res   geolocation.SignalResult
	err   error
	panic bool
	block bool
	seen  geolocation.SignalRequest
}

func (f *fakeLocator) GetSignal(ctx context.Context, req geolocation.SignalRequest) (geolocation.SignalResult, error) {
	f.seen = req
	if f.panic {
		panic("vendor exploded")
	}
	if f.block {
		<-ctx.Done()
		return geolocation.SignalResult{Outcome: geolocation.SignalResolved, CountryCode: "MT"}, nil
	}
	return f.res, f.err
}

func resolved(cc string) geolocation.SignalResult {
	return geolocation.SignalResult{Outcome: geolocation.SignalResolved, CountryCode: cc}
}

func TestLocate_OnlyAValidResolvedCountryIsKnown(t *testing.T) {
	cases := []struct {
		name string
		loc  geolocation.Locator
		want bool
	}{
		{"resolved valid", &fakeLocator{res: resolved("MT")}, true},
		{"nil locator", nil, false},
		{"go error", &fakeLocator{err: errors.New("boom"), res: resolved("MT")}, false},
		{"inconclusive", &fakeLocator{res: geolocation.SignalResult{Outcome: geolocation.SignalInconclusive, CountryCode: "MT"}}, false},
		{"unavailable", &fakeLocator{res: geolocation.SignalResult{Outcome: geolocation.SignalUnavailable, CountryCode: "MT"}}, false},
		{"vendor error outcome", &fakeLocator{res: geolocation.SignalResult{Outcome: geolocation.SignalError, CountryCode: "MT"}}, false},
		{"empty outcome", &fakeLocator{res: geolocation.SignalResult{CountryCode: "MT"}}, false},
		{"lowercase country not coerced", &fakeLocator{res: resolved("mt")}, false},
		{"unassigned code", &fakeLocator{res: resolved("ZZ")}, false},
		{"three letters", &fakeLocator{res: resolved("MLT")}, false},
		{"empty country", &fakeLocator{res: resolved("")}, false},
		{"panic", &fakeLocator{panic: true}, false},
		{"default static locator", geolocation.NewStaticLocator(""), false},
		{"configured static locator", geolocation.NewStaticLocator("MT"), true},
		{"mock provider unconfigured (errors)", geolocation.NewMockLocationProvider("m"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g *Gate
			if tc.loc == nil {
				g = New(nil)
			} else {
				g = New(tc.loc)
			}
			if got := g.Locate(context.Background(), LocateRequest{TenantID: uuid.New(), BrandID: uuid.New()}).Known(); got != tc.want {
				t.Fatalf("Known() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLocate_NilGateIsUnknown(t *testing.T) {
	var g *Gate
	if g.Locate(context.Background(), LocateRequest{}).Known() {
		t.Fatal("a nil gate must report unknown evidence")
	}
}

func TestLocate_TimeoutIsUnknown(t *testing.T) {
	g := New(&fakeLocator{block: true})
	g.timeout = 20 * time.Millisecond
	start := time.Now()
	if g.Locate(context.Background(), LocateRequest{}).Known() {
		t.Fatal("a timed-out lookup must be unknown even if the vendor answers late")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Locate did not honour its timeout")
	}
}

func TestLocate_PassesOnlyServerDerivedRequestFields(t *testing.T) {
	f := &fakeLocator{res: resolved("MT")}
	tid, bid, pid := uuid.New(), uuid.New(), uuid.New()
	New(f).Locate(context.Background(), LocateRequest{TenantID: tid, BrandID: bid, PlayerAccountID: pid, RequestID: "r1", IPAddress: "203.0.113.5"})
	if f.seen.TenantID != tid || f.seen.BrandID != bid || f.seen.PlayerAccountID != pid || f.seen.RequestID != "r1" || f.seen.IPAddress != "203.0.113.5" {
		t.Fatalf("locator did not receive the request context: %+v", f.seen)
	}
}

func TestEvidence_ZeroValueIsUnknown(t *testing.T) {
	if (Evidence{}).Known() {
		t.Fatal("the zero Evidence must be unknown")
	}
}

func TestOpCodes_AreTheSeededVocabulary(t *testing.T) {
	op, prod, ok := opCodes(OpRegistration)
	if !ok || op != "registration" || prod != nil {
		t.Fatalf("registration: %q %v %v", op, prod, ok)
	}
	op, prod, ok = opCodes(OpDeposit)
	if !ok || op != "deposit" || prod != nil {
		t.Fatalf("deposit: %q %v %v", op, prod, ok)
	}
	op, prod, ok = opCodes(OpLaunch)
	if !ok || op != "wagering" || prod == nil || *prod != "casino" {
		t.Fatalf("launch: %q %v %v", op, prod, ok)
	}
	if _, _, ok := opCodes(Operation("withdrawal")); ok {
		t.Fatal("an operation outside the gated set must not map")
	}
}
