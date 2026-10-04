// Stage 10.3 gate-W1 fix round, security S-4 point 2 / code review #2:
// "main's wiring equals the bundle". buildRegistrations can only prove that
// every providerBundle field reaches the synthetic guard; this test proves
// the other half - that no non-test file in cmd/platform-api other than
// registrations.go constructs a provider component at all, so everything
// main() wires must have come from the bundle.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowedProviderCallsOutsideRegistrations are the only provider-package
// functions a non-test file other than registrations.go may call: the
// three orchestrator constructors (they wrap bundle components and
// construct none) and the startup catalogue sync's two steps (SB-
// CATALOGUE-IO-1: FetchCatalogue reads the bundle's sportsbook provider
// with no transaction held, SyncCatalogue then upserts the already-fetched
// result inside one). Anything else - a New* constructor, a conversion
// to a provider type, any other package-level function - is refused, as
// is every composite literal of a provider-package type.
var allowedProviderCallsOutsideRegistrations = map[string]bool{
	"payments.NewOrchestrator":  true,
	"payments.RunSweeperLoop":   true, // PRH-2 H: runs an already-built Sweeper, constructs nothing
	"casino.NewOrchestrator":    true,
	"kyc.NewOrchestrator":       true,
	"sportsbook.SyncCatalogue":  true,
	"sportsbook.FetchCatalogue": true,
}

// providerConstructionsIn returns one description per call to a
// provider-package function outside allowedProviderCallsOutsideRegistrations,
// and per composite literal whose type is a provider-package type, in file.
// The provider packages are forbiddenProviderImports (no_mock_binaries_test.go).
func providerConstructionsIn(fset *token.FileSet, file *ast.File) []string {
	forbidden := make(map[string]bool, len(forbiddenProviderImports))
	for _, p := range forbiddenProviderImports {
		forbidden[p] = true
	}
	// local import name -> canonical package name (last path element).
	aliases := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !forbidden[path] {
			continue
		}
		canonical := path[strings.LastIndex(path, "/")+1:]
		local := canonical
		if imp.Name != nil {
			local = imp.Name.Name
		}
		aliases[local] = canonical
	}
	if len(aliases) == 0 {
		return nil
	}
	providerSel := func(e ast.Expr) (string, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		canonical, ok := aliases[id.Name]
		if !ok {
			return "", false
		}
		return canonical + "." + sel.Sel.Name, true
	}

	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			fun := x.Fun
			if idx, ok := fun.(*ast.IndexExpr); ok { // generic instantiation pkg.F[T](...)
				fun = idx.X
			}
			if name, ok := providerSel(fun); ok && !allowedProviderCallsOutsideRegistrations[name] {
				found = append(found, fset.Position(x.Pos()).String()+": call "+name)
			}
		case *ast.CompositeLit:
			if name, ok := providerSel(x.Type); ok {
				found = append(found, fset.Position(x.Pos()).String()+": composite literal "+name+"{}")
			}
		}
		return true
	})
	return found
}

// TestMain_ConstructsNoProviderComponentOutsideRegistrations scans every
// non-test .go file of this package except registrations.go.
func TestMain_ConstructsNoProviderComponentOutsideRegistrations(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var scanned []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "registrations.go" {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned = append(scanned, name)
		for _, f := range providerConstructionsIn(fset, file) {
			t.Errorf("%s - security S-4: construct provider components only in registrations.go (buildProviderBundle), so they reach the synthetic guard", f)
		}
	}
	sort.Strings(scanned)
	// Non-vacuity: the files that wire orchestrators must actually be scanned.
	for _, want := range []string{"main.go", "wiring.go"} {
		i := sort.SearchStrings(scanned, want)
		if i >= len(scanned) || scanned[i] != want {
			t.Fatalf("expected %s to be scanned, scanned %v", want, scanned)
		}
	}
}

// TestProviderConstructionsIn_CatchesConstructionOutsideBundle is the
// negative control: the same helper reports a mock constructor, a MOCK
// resolver composite literal (under an import alias), a provider-type
// conversion and a generic constructor, and does NOT report the allowed
// orchestrator constructors or a map literal of provider interface types.
func TestProviderConstructionsIn_CatchesConstructionOutsideBundle(t *testing.T) {
	const src = `package main

import (
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	wa "github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func wire(b providerBundle) {
	p := payments.NewMockProvider("mock-payments", "EUR")
	r := wa.MockResolver{Label: "x"}
	m := payments.MultiWebhookCredentialResolver(nil)
	s := wa.MustAdapterSchemeSet[kyc.KYCProvider]("kyc", nil)
	_ = payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock-payments": b.Payments}, nil)
	_ = kyc.NewOrchestrator(map[string]kyc.KYCProvider{}, nil)
	_, _, _, _ = p, r, m, s
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := providerConstructionsIn(fset, file)
	want := []string{
		"call payments.NewMockProvider",
		"composite literal webhookauth.MockResolver{}",
		"call payments.MultiWebhookCredentialResolver",
		"call webhookauth.MustAdapterSchemeSet",
	}
	if len(found) != len(want) {
		t.Fatalf("expected %d findings, got %d: %v", len(want), len(found), found)
	}
	for i, w := range want {
		if !strings.HasSuffix(found[i], w) {
			t.Errorf("finding %d = %q, want suffix %q", i, found[i], w)
		}
	}
}
