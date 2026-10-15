package marketgate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SR8-3 code scans. The market guard is mandatory at three orchestrator entry
// points; these tests walk every non-test Go file of the module and fail if
//
//   - a production caller of LaunchGame, InitiateDepositAttempt or
//     sportsbook.PlaceBet does not pass a params composite literal with a
//     MarketGuard key whose value is not nil (a variable params value is refused
//     too: the scan cannot prove it), or
//   - any production code outside this package references the named test-only
//     "ungated" values.

func productionGoFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", ".claude", "vendor", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(path)] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 100 {
		t.Fatalf("scan found only %d production files - wrong module root?", len(out))
	}
	return out
}

// guardedCalls: callee selector name -> params composite type name.
var guardedCalls = map[string]string{
	"LaunchGame":             "LaunchGameParams",
	"InitiateDepositAttempt": "InitiateDepositParams",
	"PlaceBet":               "PlaceBetParams",
}

func TestEveryProductionCallerOfAGuardedEntryPointPassesARealGuard(t *testing.T) {
	seen := map[string]int{}
	for path, f := range productionGoFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			typeName, guarded := guardedCalls[sel.Sel.Name]
			if !guarded {
				return true
			}
			// PlaceBet is only guarded as sportsbook.PlaceBet; the method names are unique to their packages.
			if sel.Sel.Name == "PlaceBet" {
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "sportsbook" {
					return true
				}
			}
			seen[sel.Sel.Name]++
			var lit *ast.CompositeLit
			for _, a := range call.Args {
				if cl, ok := a.(*ast.CompositeLit); ok && litTypeName(cl) == typeName {
					lit = cl
				}
			}
			if lit == nil {
				t.Errorf("%s: call to %s must pass a %s composite literal so the guard can be verified", path, sel.Sel.Name, typeName)
				return true
			}
			ok = false
			for _, el := range lit.Elts {
				kv, isKV := el.(*ast.KeyValueExpr)
				if !isKV {
					continue
				}
				if k, isID := kv.Key.(*ast.Ident); isID && k.Name == "MarketGuard" {
					if v, isID := kv.Value.(*ast.Ident); isID && v.Name == "nil" {
						continue
					}
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: call to %s passes no non-nil MarketGuard (SR8-3: an empty guard refuses)", path, sel.Sel.Name)
			}
			return true
		})
	}
	for name := range guardedCalls {
		if seen[name] == 0 {
			t.Errorf("the scan found no production caller of %s - the entry point moved, update the scan", name)
		}
	}
}

func litTypeName(cl *ast.CompositeLit) string {
	switch tt := cl.Type.(type) {
	case *ast.SelectorExpr:
		return tt.Sel.Name
	case *ast.Ident:
		return tt.Name
	}
	return ""
}

func TestProductionCodeNeverUsesTheUngatedSentinels(t *testing.T) {
	for path, f := range productionGoFiles(t) {
		if strings.HasPrefix(path, "../../internal/marketgate/") || path == "../../internal/casino/market_exposure.go" {
			continue // the definitions themselves
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && (id.Name == "UngatedForTests" || id.Name == "WithUngatedMarketOpenForTests") {
				t.Errorf("%s: production code must not reference %s", path, id.Name)
			}
			return true
		})
	}
	// The definitions exist where this test says they do.
	if _, err := os.Stat("../casino/market_exposure.go"); err != nil {
		t.Fatalf("casino/market_exposure.go moved: %v", err)
	}
}
