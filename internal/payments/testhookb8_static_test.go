// B8: testHookAfterDispatchStatusPreRead (sweeper_resolution_only.go) is a test-only
// interleaving seam. Like testHookBeforeReferenceConflictRecheck it must never be assigned
// by non-test code in this package (go/ast only, same convention as
// testhookrefconflict_static_test.go).
package payments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatic_B8InterleavingHookNeverAssignedInNonTestCode(t *testing.T) {
	const name = "testHookAfterDispatchStatusPreRead"
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	scanned, declared := 0, false
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if flHookAssignsToName(x, name) {
					t.Errorf("%s assigns %s in non-test code", fset.Position(x.Pos()), name)
				}
			case *ast.ValueSpec:
				for _, id := range x.Names {
					if id.Name == name {
						declared = true
					}
				}
			}
			return true
		})
	}
	if scanned < 20 || !declared {
		t.Fatalf("vacuous scan: scanned=%d declared=%v", scanned, declared)
	}
}
