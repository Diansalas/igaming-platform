package admission

// Import boundary guard (ADR 0097 §20 architect condition AC1):
// internal/admission is a stdlib-only leaf, and the only allowed importer
// under internal/ or cmd/ is internal/httpserver (and its own tests).
// Mirrors the style of the existing internal/db raw-SQL guard tests: a
// pure AST/text scan, no I/O beyond reading source files, run as part of
// the normal test suite so a violation fails CI, not just review.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func admissionRepoRoot(t *testing.T) string {
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

// TestAdmissionPackageIsStdlibOnly enforces the "stdlib only" half of AC1:
// no file in this package (test files included, since a test helper that
// pulls in a dependency would still defeat the leaf-package guarantee for
// anyone vendoring/building this package in isolation) imports anything
// with a dot in its first path segment (the standard heuristic for "this
// is not a standard-library import path": every stdlib import path's
// first segment is a bare word - "net/http", "sync" - while module paths
// contain a domain, e.g. "github.com/...").
func TestAdmissionPackageIsStdlibOnly(t *testing.T) {
	root := admissionRepoRoot(t)
	dir := filepath.Join(root, "internal", "admission")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			first := strings.SplitN(p, "/", 2)[0]
			if strings.Contains(first, ".") {
				t.Errorf("%s: internal/admission must be stdlib-only, found import %q", e.Name(), p)
			}
		}
	}
}

// TestAdmissionPackageOnlyImportedByHTTPServer enforces AC1's other half:
// no package under internal/ or cmd/ other than internal/httpserver may
// import internal/admission. Domain packages (webhookauth, payments,
// casino, kyc, ledger, db, identity, config) must stay unaware of the
// transport-layer admission control.
func TestAdmissionPackageOnlyImportedByHTTPServer(t *testing.T) {
	root := admissionRepoRoot(t)
	const target = `"github.com/Diansalas/igaming-platform/internal/admission"`
	allowedDirs := map[string]bool{
		filepath.Join(root, "internal", "httpserver"): true,
		filepath.Join(root, "internal", "admission"):  true, // itself/tests
	}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.Contains(string(src), target) {
				return nil
			}
			dir := filepath.Dir(path)
			if allowedDirs[dir] {
				return nil
			}
			t.Errorf("%s imports internal/admission but is not internal/httpserver (AC1)", path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
