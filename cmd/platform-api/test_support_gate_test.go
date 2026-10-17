// U-6 (provider-independent closure): the test-support route-enable flags
// are derived through ONE function, testSupportRoutesGate (wiring.go), which
// can never return true when cfg.GuardEnvironment() == "production"
// (including a missing APP_ENV). These tests pin the behaviour over a table
// of APP_ENV values, and pin by AST that main.go's httpserver.Deps literal
// derives every route-enable flag from that function and nowhere else.
package main

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/httpserver"
)

// gateEnvKeys are cleared before each config.Load() row so an ambient CI
// variable cannot change the outcome (mirrors internal/config's clearEnv).
var gateEnvKeys = []string{
	"APP_ENV", "HTTP_ADDR", "DATABASE_URL", "DATABASE_MAX_CONNS", "JWT_SIGNING_SECRET", "OTEL_SERVICE_NAME", "OTEL_EXPORTER",
	"CORS_ALLOWED_ORIGINS", "TEST_SUPPORT_ENDPOINTS_ENABLED", "MOCK_GEOLOCATION_COUNTRY", "CASINO_SESSION_MAX_LIFETIME",
	"PROVIDER_CREDENTIAL_FINGERPRINT_KEY", "SECRETSTORE_BACKENDS", "SECRETSTORE_DEVFILE_ROOT", "JWT_PREVIOUS_SECRET",
	"AWS_SECRETSMANAGER_REGION", "AWS_REGION", "ACTOR_PROOF_KEYS", "ACTOR_PROOF_ACTIVE_KID",
	"PAYOUT_INSTRUMENT_KEYS", "PAYOUT_INSTRUMENT_ACTIVE_KID", "PAYOUT_INSTRUMENT_FP_KEYS", "PAYOUT_INSTRUMENT_FP_ACTIVE_KID",
	"STAFF_MFA_FACTOR_KEYS", "STAFF_MFA_FACTOR_ACTIVE_KID", "STAFF_MFA_MAC_KEYS", "STAFF_MFA_MAC_ACTIVE_KID", "STAFF_MFA_ISSUER",
	"STAFF_ACCESS_TOKEN_TTL_SECONDS", "STAFF_REFRESH_IDLE_TTL_SECONDS", "STAFF_SESSION_ABSOLUTE_TTL_SECONDS",
	"STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS", "STAFF_STEP_UP_FINANCIAL_MAX_AGE_SECONDS",
}

// TestTestSupportRoutesGate_AppEnvTable drives every APP_ENV value through
// BOTH paths a Config can take:
//
//   - config.Load() (the real startup path) with TEST_SUPPORT_ENDPOINTS_ENABLED
//     =true: either Load refuses to start (no routes can exist), or the gate
//     equals wantLoaded; and
//   - a Config constructed directly with that raw Environment value (as if
//     Load's own validation were bypassed or regressed): the gate must STILL
//     be off for every value that is not an explicit "development"/"staging".
//
// The documented ADR 0085 dev/test behaviour (explicit development or
// staging plus the flag) stays ON; the flag off is always OFF.
func TestTestSupportRoutesGate_AppEnvTable(t *testing.T) {
	cases := []struct {
		name          string
		set           bool   // APP_ENV present in the environment
		value         string // its value when set
		wantLoadErr   bool
		wantLoaded    bool // gate after a successful Load, flag on
		wantDirectOn  bool // gate for a hand-built Config, flag on
		guardIsProdOK bool // GuardEnvironment() == "production" for the hand-built Config
	}{
		{name: "unset", set: false, wantLoadErr: false, wantLoaded: false, wantDirectOn: false, guardIsProdOK: true},
		{name: "empty", set: true, value: "", wantLoadErr: true, wantDirectOn: false},
		{name: "development", set: true, value: "development", wantLoaded: true, wantDirectOn: true},
		{name: "test", set: true, value: "test", wantLoadErr: true, wantDirectOn: false},
		{name: "staging", set: true, value: "staging", wantLoaded: true, wantDirectOn: true},
		{name: "production", set: true, value: "production", wantLoadErr: true, wantDirectOn: false, guardIsProdOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range gateEnvKeys {
				t.Setenv(k, "")
				_ = os.Unsetenv(k)
			}
			t.Setenv("DATABASE_URL", "postgres://localhost/u6-gate-test")
			t.Setenv("JWT_SIGNING_SECRET", "u6-gate-test-secret-at-least-32-characters")
			t.Setenv("TEST_SUPPORT_ENDPOINTS_ENABLED", "true")
			if tc.set {
				t.Setenv("APP_ENV", tc.value)
			}

			cfg, err := config.Load()
			switch {
			case tc.wantLoadErr && err == nil:
				t.Fatalf("config.Load() accepted APP_ENV=%q with test support on; want a startup refusal", tc.value)
			case !tc.wantLoadErr && err != nil:
				t.Fatalf("config.Load(): unexpected error: %v", err)
			case err == nil:
				if got := testSupportRoutesGate(cfg); got != tc.wantLoaded {
					t.Errorf("after Load: testSupportRoutesGate = %v, want %v", got, tc.wantLoaded)
				}
				if cfg.GuardEnvironment() == "production" && testSupportRoutesGate(cfg) {
					t.Error("after Load: gate on while GuardEnvironment()==production")
				}
			}

			// Hand-built Config: defence in depth independent of Load().
			raw := "development" // resolveAppEnv's default for an absent APP_ENV
			if tc.set {
				raw = tc.value
			}
			for _, flag := range []bool{true, false} {
				direct := config.Config{Environment: raw, EnvironmentExplicit: tc.set, TestSupportEndpointsEnabled: flag}
				want := flag && tc.wantDirectOn
				if got := testSupportRoutesGate(direct); got != want {
					t.Errorf("direct Config{Environment:%q, explicit:%v, flag:%v}: gate=%v, want %v", raw, tc.set, flag, got, want)
				}
				if (direct.GuardEnvironment() == "production") != tc.guardIsProdOK {
					t.Errorf("direct Config{Environment:%q, explicit:%v}: GuardEnvironment()=%q, table expected production=%v", raw, tc.set, direct.GuardEnvironment(), tc.guardIsProdOK)
				}
				if direct.GuardEnvironment() == "production" && testSupportRoutesGate(direct) {
					t.Errorf("direct Config{Environment:%q}: gate on while GuardEnvironment()==production", raw)
				}
				// mockProviderWiring must move in lockstep with the gate.
				w := mockProviderWiring(direct)
				for name, v := range map[string]bool{
					"KYCWebhookEnabled": w.KYCWebhookEnabled, "PaymentsWebhookResolver": w.PaymentsWebhookResolver,
					"CasinoWebhookResolver": w.CasinoWebhookResolver, "CasinoOutboundResolver": w.CasinoOutboundResolver,
					"KYCOutboundResolver": w.KYCOutboundResolver, "PaymentsOutboundResolver": w.PaymentsOutboundResolver,
				} {
					if v != want {
						t.Errorf("direct Config{Environment:%q, explicit:%v, flag:%v}: mockWiring.%s=%v, want %v", raw, tc.set, flag, name, v, want)
					}
				}
			}
		})
	}
}

// depsBoolFlagDerivation is the exact, reviewed classification of every
// bool field of httpserver.Deps and the ONLY expression main.go may assign
// to it. A new bool field on Deps fails TestDepsRouteFlags_DerivedFromSingleGate
// until it is classified here (a route-enable flag that should be
// test-support-gated must be "testSupportRoutesGate(cfg)").
var depsBoolFlagDerivation = map[string]string{
	// The four ADR 0085 test-support route flags: the single gate.
	"CasinoPlaySimulationEnabled":           "testSupportRoutesGate(cfg)",
	"PaymentsMockSettlementEnabled":         "testSupportRoutesGate(cfg)",
	"AccountActivationTestSupportEnabled":   "testSupportRoutesGate(cfg)",
	"SportsbookSettlementSimulationEnabled": "testSupportRoutesGate(cfg)",
	// KYC mock webhook route: from mockProviderWiring, which itself is
	// pinned below to derive from testSupportRoutesGate.
	"KYCWebhookEnabled": "wiring.KYCWebhookEnabled",
	// Not test-support: the production sportsbook feature itself.
	"SportsbookEnabled": "true",
	// Not a route flag: alert routing mode.
	"AlertRoutingProduction": `cfg.GuardEnvironment() == "production"`,
}

func parseMainPkgFile(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return fset, f
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	_ = printer.Fprint(&b, fset, e)
	return b.String()
}

// TestDepsRouteFlags_DerivedFromSingleGate is the U-6 static pin.
func TestDepsRouteFlags_DerivedFromSingleGate(t *testing.T) {
	// 1. Every bool field of Deps is classified (and no stale entry).
	dt := reflect.TypeOf(httpserver.Deps{})
	boolFields := map[string]bool{}
	for i := 0; i < dt.NumField(); i++ {
		if dt.Field(i).Type.Kind() == reflect.Bool {
			boolFields[dt.Field(i).Name] = true
			if _, ok := depsBoolFlagDerivation[dt.Field(i).Name]; !ok {
				t.Errorf("httpserver.Deps.%s is a new bool field not classified in depsBoolFlagDerivation - if it enables a test-support route it must be testSupportRoutesGate(cfg)", dt.Field(i).Name)
			}
		}
	}
	for k := range depsBoolFlagDerivation {
		if !boolFields[k] {
			t.Errorf("stale depsBoolFlagDerivation entry %q: no such bool field on httpserver.Deps", k)
		}
	}

	// 2. main.go's httpserver.Deps literal(s) assign exactly the classified expression.
	fset, mainFile := parseMainPkgFile(t, "main.go")
	literals := 0
	ast.Inspect(mainFile, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Deps" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "httpserver" {
			return true
		}
		literals++
		seen := map[string]bool{}
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				t.Errorf("httpserver.Deps literal uses a positional element at %s", fset.Position(el.Pos()))
				continue
			}
			key := kv.Key.(*ast.Ident).Name
			want, classified := depsBoolFlagDerivation[key]
			if !classified {
				continue
			}
			seen[key] = true
			if got := exprString(fset, kv.Value); got != want {
				t.Errorf("httpserver.Deps.%s = %s at %s; must be exactly %s", key, got, fset.Position(kv.Pos()), want)
			}
		}
		for k, want := range depsBoolFlagDerivation {
			if strings.HasPrefix(want, "testSupportRoutesGate") && !seen[k] {
				t.Errorf("httpserver.Deps literal at %s does not set %s from testSupportRoutesGate(cfg)", fset.Position(cl.Pos()), k)
			}
		}
		return true
	})
	if literals != 1 {
		t.Fatalf("expected exactly one httpserver.Deps literal in main.go, found %d", literals)
	}

	// 3. `wiring` is mockProviderWiring(cfg), and mockProviderWiring's
	//    testSupport is testSupportRoutesGate(cfg).
	foundWiring := false
	ast.Inspect(mainFile, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == "wiring" {
			foundWiring = true
			if got := exprString(fset, as.Rhs[0]); got != "mockProviderWiring(cfg)" {
				t.Errorf("main.go: wiring := %s, want mockProviderWiring(cfg)", got)
			}
		}
		return true
	})
	if !foundWiring {
		t.Error("main.go: no `wiring := mockProviderWiring(cfg)` assignment found")
	}

	// 4. Across every non-test file of the package, cfg.TestSupportRoutesEnabled()
	//    is called ONLY inside testSupportRoutesGate, and mockProviderWiring
	//    calls testSupportRoutesGate.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	gateBodyOK, wiringUsesGate := false, false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fs, f := parseMainPkgFile(t, name)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "TestSupportRoutesEnabled" {
						if fn.Name.Name == "testSupportRoutesGate" {
							gateBodyOK = true
						} else {
							t.Errorf("%s: %s calls TestSupportRoutesEnabled() directly; derive from testSupportRoutesGate(cfg) instead", fs.Position(call.Pos()), fn.Name.Name)
						}
					}
				case *ast.Ident:
					if fun.Name == "testSupportRoutesGate" && fn.Name.Name == "mockProviderWiring" {
						wiringUsesGate = true
					}
				}
				return true
			})
		}
	}
	if !gateBodyOK {
		t.Error("testSupportRoutesGate no longer calls cfg.TestSupportRoutesEnabled()")
	}
	if !wiringUsesGate {
		t.Error("mockProviderWiring does not derive from testSupportRoutesGate")
	}
}
