// Security review F-L3 (rv-fh3-security.md, 81dd4b7): testHookBeforeReference
// ConflictRecheck (receipt.go) is a package-level func() var that exists
// ONLY as a test-only interleaving seam for code-review C1's deliberate-
// race test (rvlf_i1_regression_integration_test.go). The reviewer's own
// ruling is that it cannot be set in production builds today (only an
// unexported func-value var, assignable only from within package payments,
// and -ldflags -X cannot set a func value) - but that ruling depends on NO
// non-test file in this package ever assigning it. This is a permanent,
// static guard for that residual: it fails the build the moment any
// non-_test.go file in this package assigns testHookBeforeReferenceConflict
// Recheck, matching this codebase's established static-guard style
// (internal/txscope's no_provider_call_in_tx_closure_static_test.go,
// internal/ledger's lockorder_static_test.go).
//
// Deliberately pure go/ast + go/parser (no go/types, no build), matching
// the same established convention those guards use.
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

const testHookBeforeReferenceConflictRecheckName = "testHookBeforeReferenceConflictRecheck"

// testHookAnyPrefix generalises the F-L3 guard (LF round-2 delta review C-1):
// EVERY package-level `testHook*` interleaving seam in this package (e.g.
// testHookAfterDeferredSelect, testHookAfterDispatchStatusPreRead, the
// manual-resolution hooks) belongs to the same "test-only seam" category the
// F-L3 ruling depends on, so no non-_test.go file may assign ANY identifier
// with this prefix.
const testHookAnyPrefix = "testHook"

// flHookAssignsToName reports whether stmt assigns (via `=` or `:=`) to an
// identifier named name, anywhere among its LHS operands - the shape a
// plain package-level `testHookBeforeReferenceConflictRecheck = ...` or
// `testHookBeforeReferenceConflictRecheck = func() { ... }` assignment
// takes at the top level of a file, and the shape it would ALSO take
// inside any function body.
func flHookAssignsToName(stmt *ast.AssignStmt, name string) bool {
	for _, lhs := range stmt.Lhs {
		if ident, ok := lhs.(*ast.Ident); ok && (ident.Name == name || (name == testHookAnyPrefix && strings.HasPrefix(ident.Name, testHookAnyPrefix))) {
			return true
		}
	}
	return false
}

// flHookScanFile parses one non-test .go file and returns every line
// position where testHookBeforeReferenceConflictRecheck is assigned.
func flHookScanFile(path string, src []byte) ([]token.Position, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var found []token.Position
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if flHookAssignsToName(assign, testHookAnyPrefix) {
			found = append(found, fset.Position(assign.Pos()))
		}
		return true
	})
	return found, nil
}

// TestFL3_TestHookBeforeReferenceConflictRecheck_NeverAssignedOutsideTests
// walks every non-test .go file in this package (internal/payments) and
// fails if any of them assigns testHookBeforeReferenceConflictRecheck -
// the ONLY legitimate assignment site is
// TestRVLF_C1_PreconditionTwoReferenceConflict_OnlyReachableViaReadCommitted
// Race in rvlf_i1_regression_integration_test.go (an _test.go file,
// excluded from this walk by construction).
func TestFL3_TestHookBeforeReferenceConflictRecheck_NeverAssignedOutsideTests(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var violations []token.Position
	scannedFiles := 0
	sawDeclaration := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if strings.Contains(string(src), testHookBeforeReferenceConflictRecheckName) {
			sawDeclaration = true
		}
		v, scanErr := flHookScanFile(path, src)
		if scanErr != nil {
			t.Fatalf("parse %s: %v", path, scanErr)
		}
		violations = append(violations, v...)
		scannedFiles++
	}
	if scannedFiles == 0 {
		t.Fatal("this guard found NO non-test .go files in internal/payments at all - either the walk is broken or the tree moved, and a guard that inspects nothing proves nothing")
	}
	if !sawDeclaration {
		t.Fatal("this guard never even saw the identifier testHookBeforeReferenceConflictRecheck in any non-test file - the var's own declaration in receipt.go is expected to be found (a read, never an assignment, is fine and does not fail this test), so this guard is not actually exercising anything")
	}
	if len(violations) > 0 {
		var lines []string
		for _, v := range violations {
			lines = append(lines, v.String())
		}
		t.Fatalf("F-L3: testHookBeforeReferenceConflictRecheck must never be assigned outside a _test.go file:\n  %s", strings.Join(lines, "\n  "))
	}
}

// TestFL3_GuardCatchesPlantedViolation is the guard's own required
// negative control: a synthetic non-test-shaped file assigning
// testHookBeforeReferenceConflictRecheck must be flagged; a mere read (an
// `if testHookBeforeReferenceConflictRecheck != nil` check, exactly what
// the real production call site in receipt.go does) must not be.
func TestFL3_GuardCatchesPlantedViolation(t *testing.T) {
	planted := `package payments

func plant() {
	testHookBeforeReferenceConflictRecheck = func() {}
}
`
	v, err := flHookScanFile("planted.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse planted violation: %v", err)
	}
	if len(v) != 1 {
		t.Fatalf("expected exactly one violation, got %v", v)
	}

	safeRead := `package payments

func safe() {
	if testHookBeforeReferenceConflictRecheck != nil {
		testHookBeforeReferenceConflictRecheck()
	}
}
`
	v, err = flHookScanFile("safe.go", []byte(safeRead))
	if err != nil {
		t.Fatalf("parse safe-read case: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations for a mere nil-check read and call, got %v", v)
	}
}

// TestFL3_GuardCoversEveryTestHookSeam is the negative control for the
// generalised guard: a planted assignment to testHookAfterDeferredSelect (and
// to an arbitrary testHook* name) must be flagged, a nil-check read must not.
func TestFL3_GuardCoversEveryTestHookSeam(t *testing.T) {
	planted := `package payments

func plant() {
	testHookAfterDeferredSelect = func() {}
	testHookSomethingNew = nil
}
`
	v, err := flHookScanFile("planted.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(v) != 2 {
		t.Fatalf("expected two violations (every testHook* seam is guarded), got %v", v)
	}
	safe := `package payments

func safe() {
	if testHookAfterDeferredSelect != nil {
		testHookAfterDeferredSelect()
	}
}
`
	v, err = flHookScanFile("safe.go", []byte(safe))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("a nil-check read must not be flagged, got %v", v)
	}
}
