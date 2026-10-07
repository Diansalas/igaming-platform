package reconciliation

// R3-TEST-GAPS-1 (security D-2): listNonActiveTenants is a variable only so a
// test can force the listing to fail. Production code must never reassign it
// (a reassignment would silently blind the observation sweep). It may be
// assigned only in its own declaration and in _test.go files (which this scan
// does not read).

import (
	"go/ast"
	"go/token"
	"testing"
)

func TestR3_Static_ListNonActiveTenantsAssignedOnlyInDeclaration(t *testing.T) {
	const name = "listNonActiveTenants"
	declared := 0
	for file, f := range r3ProductionFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for _, id := range x.Names {
					if id.Name == name {
						declared++
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
						t.Errorf("%s assigns %s: only its declaration (and _test.go files) may", file, name)
					}
				}
			case *ast.IncDecStmt:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == name {
					t.Errorf("%s modifies %s", file, name)
				}
			case *ast.UnaryExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == name && x.Op == token.AND {
					t.Errorf("%s takes the address of %s", file, name)
				}
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if id, ok := e.(*ast.Ident); ok && id != nil && id.Name == name {
						t.Errorf("%s ranges into %s", file, name)
					}
				}
			}
			return true
		})
	}
	// Vacuity control: exactly one declaration is seen.
	if declared != 1 {
		t.Fatalf("expected exactly one declaration of %s in production code, saw %d (scan is vacuous or the var moved)", name, declared)
	}
}
