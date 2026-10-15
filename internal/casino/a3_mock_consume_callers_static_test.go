package casino

// A3 SR3-4 (security review): ConsumeLaunchSessionForMockPlay is the MOCK
// vendor's stand-in for the bootstrap CAS. It must be reachable ONLY from the
// play-simulation handlers (registered outside production, MOCK provider only).
// This static scan fails the build if any other non-test code calls it, so a
// production path can never grow a way to consume a session without the vendor
// launch-token bootstrap.

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

const mockConsumeFn = "ConsumeLaunchSessionForMockPlay"

// allowedMockConsumeCallers: file (module-relative, slash form) -> enclosing
// functions allowed to call it.
var allowedMockConsumeCallers = map[string]map[string]bool{
	"internal/httpserver/casino_play_handlers.go": {"runCasinoPlaySimulation": true},
}

// mockConsumeCalls returns "file:func" for every call of the guarded function
// in src (a parsed file), treating both pkg.F(...) and F(...) as calls.
func mockConsumeCalls(fset *token.FileSet, f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name == mockConsumeFn {
					out = append(out, fd.Name.Name)
				}
			case *ast.Ident:
				if fn.Name == mockConsumeFn {
					out = append(out, fd.Name.Name)
				}
			}
			return true
		})
	}
	return out
}

func TestA3_MockConsumeIsOnlyCalledFromThePlaySimulationHandlers(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	seen := map[string]int{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // a file that does not parse cannot be compiled either
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, fn := range mockConsumeCalls(fset, f) {
			if !allowedMockConsumeCallers[rel][fn] {
				t.Errorf("%s calls %s from %s: only the play-simulation handlers may (SR3-4)", rel, mockConsumeFn, fn)
			}
			seen[rel+":"+fn]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Anti-vacuity: the sanctioned call site must actually be found.
	if seen["internal/httpserver/casino_play_handlers.go:runCasinoPlaySimulation"] == 0 {
		t.Fatalf("the scan did not find the sanctioned call site; it is vacuous (seen=%v)", seen)
	}
}

// Planted control: the scan flags a call from any other function.
func TestA3_MockConsumeScanFlagsAPlantedCaller(t *testing.T) {
	const src = `package x
func handler() { _ = casino.ConsumeLaunchSessionForMockPlay(ctx, tx, id, m, "p") }
func bare()    { _ = ConsumeLaunchSessionForMockPlay(ctx, tx, id, m, "p") }
func clean()   { _ = other() }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := mockConsumeCalls(fset, f)
	if len(got) != 2 || got[0] != "handler" || got[1] != "bare" {
		t.Fatalf("expected the selector and bare calls flagged, got %v", got)
	}
}
