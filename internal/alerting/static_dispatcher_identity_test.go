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
	"strings"
	"testing"
)

var staticAllowedDispatcherIdentityFiles = map[string]bool{
	"dispatcher.go":         true,
	"dispatcher_actions.go": true,
	"fallback.go":           true,
}

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

func TestStatic_ServiceAlertDispatcherConfinedToDispatcherAndFallback(t *testing.T) {
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
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "db" || sel.Sel.Name != "ServiceAlertDispatcher" {
				return true
			}
			if !staticAllowedDispatcherIdentityFiles[filepath.Base(name)] {
				t.Errorf("%s: db.ServiceAlertDispatcher referenced outside the allowed dispatcher/fallback files (AL-10)", name)
			}
			return true
		})
	}
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
