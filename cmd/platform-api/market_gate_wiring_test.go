package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// A8 / owner decision 3: the operating-market gate's Locator is, by default, the
// production-eligible KYC-verified-residence source. The Synthetic static MOCK is
// wired only when an explicit development/staging country is configured, and the
// MOCK-ADAPTER-PROD-1 guard refuses it in production like every other mock. A
// production process with no Locator at all is refused as well.

func geolocationRegistrations(regs []providerkind.Registration) []providerkind.Registration {
	var out []providerkind.Registration
	for _, r := range regs {
		if r.Domain == "geolocation" {
			out = append(out, r)
		}
	}
	return out
}

func TestBuildRegistrations_RegistersTheGeolocationLocator(t *testing.T) {
	cfg := baseConfig(t, "development", true)
	withCountry := allOnWiring
	withCountry.GeolocationCountry = "MT"
	for name, tc := range map[string]struct {
		wiring        mockWiring
		wantSynthetic bool
	}{
		"all off (default KYC source)":   {mockWiring{}, false},
		"all on (default KYC source)":    {allOnWiring, false},
		"explicit mock country (static)": {withCountry, true},
	} {
		b := buildProviderBundle(tc.wiring)
		got := geolocationRegistrations(buildRegistrations(cfg, b))
		if len(got) != 1 || got[0].Name != "locator" || got[0].Component != any(b.Geolocation) {
			t.Fatalf("%s: want exactly the bundle's Locator registered under geolocation/locator, got %+v", name, got)
		}
		_, synthetic := got[0].Component.(providerkind.Synthetic)
		if synthetic != tc.wantSynthetic {
			t.Fatalf("%s: synthetic=%v, want %v", name, synthetic, tc.wantSynthetic)
		}
		_, isKYC := got[0].Component.(*marketgate.KYCResidenceLocator)
		if isKYC == tc.wantSynthetic {
			t.Fatalf("%s: KYC residence source = %v, want %v", name, isKYC, !tc.wantSynthetic)
		}
	}
}

// The existing production-refusal guard now PASSES when the KYC residence
// source is wired and REFUSES otherwise: a Synthetic static Locator, a missing
// Locator, or a second one.
func TestRefuseSyntheticInProduction_KYCResidenceSourcePassesOtherwiseRefused(t *testing.T) {
	prod := baseConfig(t, "production", true)
	kycReg := providerkind.Registration{Domain: "geolocation", Name: "locator", Component: marketgate.NewKYCResidenceLocator(nil)}

	// Both startup checks main() runs, in order.
	both := func(cfg config.Config, regs []providerkind.Registration) error {
		if err := refuseSyntheticInProduction(cfg, regs); err != nil {
			return err
		}
		return requireMarketEvidenceInProduction(cfg, regs)
	}

	if err := both(prod, []providerkind.Registration{kycReg}); err != nil {
		t.Fatalf("production with the KYC residence source must pass: %v", err)
	}
	// Through the real bundle: the production-shaped bundle (no mock country) registers it.
	b := buildProviderBundle(mockProviderWiring(prod))
	if only := geolocationRegistrations(buildRegistrations(prod, b)); both(prod, only) != nil {
		t.Fatal("the production bundle's locator registration must pass the guard")
	}

	for name, regs := range map[string][]providerkind.Registration{
		"no registration":   nil,
		"nil component":     {{Domain: "geolocation", Name: "locator", Component: nil}},
		"two registrations": {kycReg, kycReg},
		"synthetic static":  {{Domain: "geolocation", Name: "locator", Component: geolocation.NewStaticLocator("MT")}},
		"other name only":   {{Domain: "geolocation", Name: "other", Component: marketgate.NewKYCResidenceLocator(nil)}},
	} {
		if err := both(prod, regs); err == nil {
			t.Fatalf("%s: production must refuse", name)
		}
	}
	// Non-production environments do not require it.
	for _, env := range []string{"staging", "development"} {
		if err := both(baseConfig(t, env, true), nil); err != nil {
			t.Fatalf("%s: must not require a locator: %v", env, err)
		}
	}
}

// MOCK_GEOLOCATION_COUNTRY is optional: unset, every environment (staging and
// development included) gets the KYC residence source; production can never
// get the static one (config refuses the variable and mockProviderWiring
// ignores it).
func TestMarketEvidenceLocator_KYCByDefaultStaticOnlyWithExplicitCountry(t *testing.T) {
	if _, ok := marketEvidenceLocator(mockWiring{}).(*marketgate.KYCResidenceLocator); !ok {
		t.Fatal("default must be the KYC residence source")
	}
	if _, ok := marketEvidenceLocator(mockWiring{GeolocationCountry: "MT"}).(*geolocation.StaticLocator); !ok {
		t.Fatal("an explicit country selects the static MOCK")
	}
	prod := baseConfig(t, "production", true)
	prod.TestSupportEndpointsEnabled = true
	prod.MockGeolocationCountry = "MT" // cannot happen via config.Load; wiring must still ignore it
	if _, ok := marketEvidenceLocator(mockProviderWiring(prod)).(*marketgate.KYCResidenceLocator); !ok {
		t.Fatal("production wiring must never select the static locator")
	}
}

func TestBindMarketEvidence_BindsOnlyTheKYCSourceAndNeedsAPool(t *testing.T) {
	b := buildProviderBundle(mockWiring{})
	k := b.Geolocation.(*marketgate.KYCResidenceLocator)
	if k.Bound() {
		t.Fatal("the bundle's locator is unbound until the pool exists")
	}
	if err := bindMarketEvidence(b, nil); err == nil {
		t.Fatal("binding a nil pool must fail (startup would refuse)")
	}
	if err := bindMarketEvidence(buildProviderBundle(mockWiring{GeolocationCountry: "MT"}), nil); err != nil {
		t.Fatalf("the static MOCK needs no pool: %v", err)
	}
}

// SRME-8: in production the Locator must be the KYC source and bound; elsewhere nothing is required.
func TestRequireMarketEvidenceBound(t *testing.T) {
	prod := baseConfig(t, "production", true)
	unbound := buildProviderBundle(mockWiring{})
	if err := requireMarketEvidenceBound(prod, unbound); err == nil {
		t.Fatal("production with an unbound KYC locator must refuse")
	}
	k := unbound.Geolocation.(*marketgate.KYCResidenceLocator)
	if err := k.Bind(nil); err == nil {
		t.Fatal("nil pool must not bind")
	}
	if err := requireMarketEvidenceBound(baseConfig(t, "staging", true), unbound); err != nil {
		t.Fatalf("staging must not require it: %v", err)
	}
	static := buildProviderBundle(mockWiring{GeolocationCountry: "MT"})
	if err := requireMarketEvidenceBound(prod, static); err == nil {
		t.Fatal("production with the static MOCK must refuse")
	}
	if err := requireMarketEvidenceBound(prod, providerBundle{}); err == nil {
		t.Fatal("production with no locator must refuse")
	}
}

// main.go must bind the pool to the market-evidence source right after the
// database connects, or the gate has no evidence (fail closed, but unusable).
func TestMain_BindsTheMarketEvidenceSourceToThePool(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "bindMarketEvidence(providers, pool)") {
		t.Fatal("main.go must call bindMarketEvidence(providers, pool) after db.Connect")
	}
}

func TestRefuseSyntheticInProduction_RefusesTheMockLocatorByName(t *testing.T) {
	// The static MOCK is selected only by an explicit development/staging country.
	b := buildProviderBundle(mockWiring{GeolocationCountry: "MT"})
	only := geolocationRegistrations(buildRegistrations(baseConfig(t, "production", true), b))
	for _, env := range []struct {
		name     string
		cfgEnv   string
		explicit bool
		wantErr  bool
	}{
		{"production", "production", true, true},
		{"missing APP_ENV", "development", false, true},
		{"staging", "staging", true, false},
		{"development", "development", true, false},
	} {
		err := refuseSyntheticInProduction(baseConfig(t, env.cfgEnv, env.explicit), only)
		if env.wantErr {
			if err == nil || !strings.Contains(err.Error(), "geolocation/locator") {
				t.Fatalf("%s: want a refusal naming geolocation/locator, got %v", env.name, err)
			}
		} else if err != nil {
			t.Fatalf("%s: unexpected refusal: %v", env.name, err)
		}
	}
}

func TestMockProviderWiring_GeolocationCountryOnlyWhenMocksAreEnabled(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		explicit    bool
		testSupport bool
		want        string
	}{
		{"development with test support", "development", true, true, "MT"},
		{"staging with test support", "staging", true, true, "MT"},
		{"development without test support", "development", true, false, ""},
		{"production", "production", true, false, ""},
		{"missing APP_ENV is production-like", "development", false, true, ""},
	}
	for _, tc := range cases {
		cfg := baseConfig(t, tc.environment, tc.explicit)
		cfg.TestSupportEndpointsEnabled = tc.testSupport
		cfg.MockGeolocationCountry = "MT"
		if got := mockProviderWiring(cfg).GeolocationCountry; got != tc.want {
			t.Fatalf("%s: GeolocationCountry = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// With no configured country the bundle's Locator reports UNKNOWN, and the
// gate built over it therefore has no usable evidence: fail closed.
func TestBundleLocator_DefaultsToUnknownEvidence(t *testing.T) {
	b := buildProviderBundle(mockWiring{})
	res, err := b.Geolocation.GetSignal(context.Background(), geolocation.SignalRequest{IPAddress: "203.0.113.1"})
	if err != nil || res.Outcome == geolocation.SignalResolved {
		t.Fatalf("default bundle Locator must report unknown, got %+v err=%v", res, err)
	}
	if marketgate.New(b.Geolocation).Locate(context.Background(), marketgate.LocateRequest{}).Known() {
		t.Fatal("the gate must have no evidence under the default bundle Locator")
	}
	configured := buildProviderBundle(mockWiring{GeolocationCountry: "MT"})
	if !marketgate.New(configured.Geolocation).Locate(context.Background(), marketgate.LocateRequest{}).Known() {
		t.Fatal("a configured mock country must produce evidence")
	}
}

// main.go must put a marketgate.New(...) into the httpserver.Deps literal:
// Deps.MarketGate nil makes the register, deposit and launch handlers fail
// closed (503), so omitting it would silently disable those routes rather
// than leave them ungated - but the omission must still be caught here.
func TestMain_WiresTheMarketGateIntoDeps(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "MarketGate" {
			return true
		}
		call, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "marketgate" && sel.Sel.Name == "New" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("main.go must set httpserver.Deps.MarketGate to marketgate.New(<bundle Locator>)")
	}
}
