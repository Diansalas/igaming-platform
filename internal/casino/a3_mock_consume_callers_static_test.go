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

// mockConsumeRefs returns the enclosing function name (or "<package level>")
// of EVERY reference to the guarded identifier in f - calls, and also any other
// use such as assigning the function to a variable or passing it as a value
// (SR3B-1). The function's own declaration is not a reference.
func mockConsumeRefs(fset *token.FileSet, f *ast.File) []string {
	var out []string
	scan := func(root ast.Node, enclosing string) {
		ast.Inspect(root, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == mockConsumeFn {
				out = append(out, enclosing)
			}
			return true
		})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == mockConsumeFn {
				continue // the definition itself
			}
			if d.Body != nil {
				scan(d.Body, d.Name.Name)
			}
		default:
			scan(d, "<package level>")
		}
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
		for _, fn := range mockConsumeRefs(fset, f) {
			if !allowedMockConsumeCallers[rel][fn] {
				t.Errorf("%s references %s from %s: only the play-simulation handlers may (SR3-4/SR3B-1)", rel, mockConsumeFn, fn)
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

// Planted control: the scan flags a call, a selector call, and a function-value
// reference (assignment, argument, package-level var) from any other place, and
// does not flag the definition or unrelated code.
func TestA3_MockConsumeScanFlagsAPlantedReference(t *testing.T) {
	const src = `package x
var pkgLevel = casino.ConsumeLaunchSessionForMockPlay
func handler() { _ = casino.ConsumeLaunchSessionForMockPlay(ctx, tx, id, m) }
func bare()    { _ = ConsumeLaunchSessionForMockPlay(ctx, tx, id, m) }
func alias()   { f := casino.ConsumeLaunchSessionForMockPlay; _ = f }
func passed()  { use(ConsumeLaunchSessionForMockPlay) }
func clean()   { _ = other() }
func ConsumeLaunchSessionForMockPlay(ctx context.Context) error { return nil }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := mockConsumeRefs(fset, f)
	want := []string{"<package level>", "handler", "bare", "alias", "passed"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}
