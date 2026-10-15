package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// A8: the operating-market gate's Locator is a Synthetic component, is
// registered with the MOCK-ADAPTER-PROD-1 guard, and the guard refuses it in
// production exactly like every other mock. The gate requires location
// evidence for EVERY decision, so there is no configuration in which a
// MOCK Locator may run in production.

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
	for name, wiring := range map[string]mockWiring{"all off": {}, "all on": allOnWiring} {
		b := buildProviderBundle(wiring)
		got := geolocationRegistrations(buildRegistrations(cfg, b))
		if len(got) != 1 || got[0].Name != "locator" || got[0].Component != any(b.Geolocation) {
			t.Fatalf("%s: want exactly the bundle's Locator registered under geolocation/locator, got %+v", name, got)
		}
		if _, ok := got[0].Component.(providerkind.Synthetic); !ok {
			t.Fatalf("%s: the registered Locator must be Synthetic", name)
		}
	}
}

func TestRefuseSyntheticInProduction_RefusesTheMockLocatorByName(t *testing.T) {
	b := buildProviderBundle(mockWiring{})
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
