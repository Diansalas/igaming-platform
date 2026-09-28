package alerting

// This static test enforces two ADR 0102 invariants that must hold no
// matter how the package evolves, mirroring internal/txscope's own
// go/ast-based static-guard convention (no_provider_call_in_tx_closure_
// static_test.go): pure go/ast + go/parser, no go/types, no build.
//
//  1. AL-10/Q1/static test (§5, §6.3): db.ServiceAlertDispatcher is
//     referenced ONLY inside this package's dispatcher*.go and
//     fallback.go - never by a business-facing file (alert.go, raise.go,
//     guarded.go, detached.go, scope.go, kind.go). This is what makes
//     "business code borrowing the dispatcher identity" structurally
//     impossible, not merely disciplined.
//  2. AL-14 (security Part 1): an Alert{Kind: KindAlertingRaiseFailed...}
//     composite literal is constructed ONLY inside fallback.go - nothing
//     else in the package may build a raise_failed alert.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var staticAllowedRaiseFailedConstructorFiles = map[string]bool{
	"fallback.go": true,
}

// staticAllowedPendingConstructorFiles: LF F4's "no Pending spans two
// transactions" guarantee rests on there being exactly one place a
// *Pending value is ever constructed - InTx (guarded.go). If a future
// change added a second `&Pending{...}` construction site, it could
// trivially defeat the one-collector-per-transaction invariant no matter
// how carefully InTx itself is written.
var staticAllowedPendingConstructorFiles = map[string]bool{
	"guarded.go": true,
}

func staticListPackageFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/alerting dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	return files
}

// staticRepoRoot mirrors internal/txscope's own identical helper: walk up
// from the working directory to the module root (the directory
// containing go.mod), so this test works regardless of which directory
// `go test` is invoked from.
func staticRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// staticWalkNonTestGoFiles visits every non-test .go file under root.
func staticWalkNonTestGoFiles(t *testing.T, root string, visit func(path string, src []byte)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendor/node_modules/.git and any other VCS-ish or
			// dependency directory that could otherwise slow this test
			// down or produce noise unrelated to this repo's own code.
			// Like the go tool, also skip every dot- and underscore-prefixed
			// directory and testdata: they are never part of the module's
			// packages, and .claude/worktrees/ holds other checkouts of this
			// same repo whose copies of the allowed files would otherwise
			// be misread as violations.
			base := d.Name()
			if path != root && (base == "vendor" || base == "node_modules" || base == "testdata" ||
				strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		visit(path, src)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// staticAllowedDispatcherIdentityPaths: security IC-3. internal/db/
// platform_service.go is where db.ServiceAlertDispatcher is DEFINED (a
// bare identifier declaration, not a "reference" in the sense this test
// cares about, but included explicitly so the scan does not need special
// casing for the declaration site); the three alerting files are the only
// ones permitted to actually USE the identity.
var staticAllowedDispatcherIdentityPaths = map[string]bool{
	filepath.Join("internal", "db", "platform_service.go"):         true,
	filepath.Join("internal", "alerting", "dispatcher.go"):         true,
	filepath.Join("internal", "alerting", "dispatcher_actions.go"): true,
	filepath.Join("internal", "alerting", "fallback.go"):           true,
}

// TestStatic_ServiceAlertDispatcherConfinedToDispatcherAndFallback is
// security IC-3, widened from a single-package scan to the WHOLE
// repository (every non-test .go file): db.ServiceAlertDispatcher must
// never be referenced by ANY business-facing code anywhere, not merely
// within internal/alerting - and neither may a caller sidestep the named
// constant by writing db.PlatformService("alert_dispatcher") directly
// (a raw string conversion to the same type, which internal/db's own
// WithPlatformService would accept identically to the real constant,
// since Go constants of a defined string type compare equal to an
// equivalent literal conversion), nor by ANY OTHER route that reaches the
// same identity value: IR-2 additionally flags
//   - a bare `"alert_dispatcher"` string literal anywhere (a slice/map
//     key, a struct field, a test fixture masquerading as production
//     code, etc. - not only inside a db.PlatformService(...) call), and
//   - any string literal that both contains `set_config(` and mentions
//     `app.platform_service_id` (a raw SQL statement setting this GUC
//     directly, bypassing db.WithPlatformService entirely - the only
//     sanctioned way to set it).
func TestStatic_ServiceAlertDispatcherConfinedToDispatcherAndFallback(t *testing.T) {
	root := staticRepoRoot(t)
	staticWalkNonTestGoFiles(t, root, func(path string, src []byte) {
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			t.Fatalf("relativize %s: %v", path, relErr)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		allowed := staticAllowedDispatcherIdentityPaths[rel]
		ast.Inspect(file, func(n ast.Node) bool {
			// db.ServiceAlertDispatcher (a SelectorExpr on package "db").
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkgIdent, ok := sel.X.(*ast.Ident); ok && pkgIdent.Name == "db" && sel.Sel.Name == "ServiceAlertDispatcher" {
					if !allowed {
						t.Errorf("%s: db.ServiceAlertDispatcher referenced outside the allowed dispatcher/fallback files (AL-10/IC-3)", rel)
					}
				}
			}
			// Any string literal anywhere - covers db.PlatformService(
			// "alert_dispatcher") equally well as a bare literal, plus the
			// IR-2 raw set_config(...) case.
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if value == "alert_dispatcher" && !allowed {
					t.Errorf("%s: \"alert_dispatcher\" string literal referenced outside the allowed dispatcher/fallback files (AL-10/IC-3)", rel)
				}
				if strings.Contains(value, "set_config(") && strings.Contains(value, "app.platform_service_id") && !allowed {
					t.Errorf("%s: a raw set_config(...) SQL literal mentions app.platform_service_id outside the allowed dispatcher/fallback files (AL-10/IC-3) - use db.WithPlatformService instead", rel)
				}
			}
			return true
		})
	})
}

func TestStatic_RaiseFailedConstructedOnlyInFallback(t *testing.T) {
	for _, name := range staticListPackageFiles(t) {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			comp, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typeIdent, ok := comp.Type.(*ast.Ident)
			if !ok || typeIdent.Name != "Alert" {
				return true
			}
			for _, elt := range comp.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				keyIdent, ok := kv.Key.(*ast.Ident)
				if !ok || keyIdent.Name != "Kind" {
					continue
				}
				valSel, ok := kv.Value.(*ast.Ident)
				if !ok || valSel.Name != "KindAlertingRaiseFailed" {
					continue
				}
				if !staticAllowedRaiseFailedConstructorFiles[filepath.Base(name)] {
					t.Errorf("%s: Alert{Kind: KindAlertingRaiseFailed} constructed outside fallback.go (AL-14)", name)
				}
			}
			return true
		})
	}
}

func TestStatic_PendingConstructedOnlyByInTx(t *testing.T) {
	for _, name := range staticListPackageFiles(t) {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			unary, ok := n.(*ast.UnaryExpr)
			if !ok || unary.Op != token.AND {
				return true
			}
			comp, ok := unary.X.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := comp.Type.(*ast.Ident)
			if !ok || ident.Name != "Pending" {
				return true
			}
			if !staticAllowedPendingConstructorFiles[filepath.Base(name)] {
				t.Errorf("%s: &Pending{...} constructed outside guarded.go's InTx (LF F4)", name)
			}
			return true
		})
	}
}
