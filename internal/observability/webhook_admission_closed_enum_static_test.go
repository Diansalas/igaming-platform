// Security review J-L3 (optional, `docs/plans/prh2-hardening-round/
// reviews/j-security.md`): a static test keeping WebhookAdmissionReason,
// WebhookProviderKind, WebhookAdmissionDecision and WebhookAdmissionStage
// closed by construction. The compile-time closed-enum discipline these
// types rely on (only this file's/webhook_admission_metrics.go's own
// declared constants are ever assigned) is only real if nothing OUTSIDE
// internal/observability can convert an arbitrary string to one of these
// types - a raw `observability.WebhookAdmissionReason("whatever-a-caller-
// wants")` at any external call site would silently defeat the whole
// "bounded label cardinality" security property this metric exists to
// guarantee (ADR 0097 §8, security review's "labels are bounded and not
// attacker-controlled" finding).
//
// Deliberately lexical only (go/ast + go/parser, no go/types), matching
// this codebase's established static-guard convention (internal/txscope's
// no_provider_call_in_tx_closure_static_test.go, internal/ledger's
// lockorder_static_test.go): a `pkgIdent.TypeName(...)` call expression
// where pkgIdent is literally named "observability" and TypeName is one
// of the four closed-enum type names is flagged, wherever it appears
// outside this package. This does not require full type-checking/module
// resolution and stays independent of build tags.
package observability

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// closedEnumTypeNames is the exact set of ADR 0097 §8 label types this
// guard protects. Every other exported symbol in this package (the
// Record*/Inc/Dec functions, the declared constants themselves) is
// unaffected - only a CONVERSION expression naming one of these types is
// in scope.
var closedEnumTypeNames = map[string]bool{
	"WebhookAdmissionDecision": true,
	"WebhookAdmissionReason":   true,
	"WebhookProviderKind":      true,
	"WebhookAdmissionStage":    true,
}

// closedEnumViolation is one `observability.<Type>(...)` conversion found
// outside internal/observability.
type closedEnumViolation struct {
	pos      token.Position
	typeName string
}

func (v closedEnumViolation) String() string {
	return v.pos.String() + ": observability." + v.typeName + "(...) conversion outside internal/observability"
}

// closedEnumScanFile parses one non-test .go file and returns every
// `observability.<ClosedEnumType>(...)` call/conversion expression found
// anywhere in it. A conversion is a *ast.CallExpr whose Fun is exactly a
// *ast.SelectorExpr `observability.<Name>` - the identical shape a type
// conversion and a qualified function call share syntactically at the
// go/ast level (indistinguishable without go/types), which is fine here:
// this package exports no function named after any of these four types,
// so every such shape IS a conversion.
func closedEnumScanFile(path string, src []byte) ([]closedEnumViolation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var violations []closedEnumViolation
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || pkgIdent.Name != "observability" {
			return true
		}
		if closedEnumTypeNames[sel.Sel.Name] {
			violations = append(violations, closedEnumViolation{pos: fset.Position(call.Pos()), typeName: sel.Sel.Name})
		}
		return true
	})
	return violations, nil
}

// closedEnumSkipDir reports whether a directory name should never be
// descended into: hidden (dot) directories, underscore-prefixed
// directories (Go's own build-exclusion convention), "testdata"
// (fixtures, never real source), "vendor", and internal/observability
// itself (where these conversions are legitimate - the constants'
// declarations are conversions of their own literal values).
func closedEnumSkipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules"
}

func closedEnumWalkNonTestGoFiles(root string, skipRoot string, visit func(path string, src []byte) error) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && closedEnumSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			if skipRoot != "" && path == skipRoot {
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
		return visit(path, src)
	})
}

func closedEnumRepoRoot(t *testing.T) string {
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

// TestClosedEnum_NoConversionOutsideObservability is security review
// J-L3 made permanent: no package other than internal/observability may
// convert an arbitrary value to WebhookAdmissionDecision/
// WebhookAdmissionReason/WebhookProviderKind/WebhookAdmissionStage - every
// caller (internal/httpserver today) must use only this package's own
// exported constants.
func TestClosedEnum_NoConversionOutsideObservability(t *testing.T) {
	root := closedEnumRepoRoot(t)
	skipRoot := filepath.Join(root, "internal", "observability")

	var violations []closedEnumViolation
	filesScanned := 0
	if err := closedEnumWalkNonTestGoFiles(root, skipRoot, func(path string, src []byte) error {
		filesScanned++
		v, err := closedEnumScanFile(path, src)
		if err != nil {
			return err
		}
		violations = append(violations, v...)
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if filesScanned == 0 {
		t.Fatal("this guard scanned NO .go files at all - the walk/skip logic is broken, and a guard that " +
			"inspects nothing proves nothing")
	}
	if len(violations) > 0 {
		var lines []string
		for _, v := range violations {
			lines = append(lines, v.String())
		}
		t.Fatalf("closed-enum conversion found outside internal/observability (defeats ADR 0097 §8's bounded "+
			"label cardinality guarantee):\n  %s", strings.Join(lines, "\n  "))
	}
}

// TestClosedEnum_GuardCatchesPlantedViolation is the guard's own required
// negative control: a synthetic file that converts a caller-controlled
// string to observability.WebhookAdmissionReason must be flagged; a call
// to an unrelated observability function (not one of the four closed
// types) must not be.
func TestClosedEnum_GuardCatchesPlantedViolation(t *testing.T) {
	planted := `package x

import "github.com/Diansalas/igaming-platform/internal/observability"

func evil(userInput string) observability.WebhookAdmissionReason {
	return observability.WebhookAdmissionReason(userInput)
}
`
	v, err := closedEnumScanFile("planted.go", []byte(planted))
	if err != nil {
		t.Fatalf("parse planted violation: %v", err)
	}
	if len(v) != 1 || v[0].typeName != "WebhookAdmissionReason" {
		t.Fatalf("expected exactly one WebhookAdmissionReason violation, got %v", v)
	}

	safe := `package x

import (
	"context"

	"github.com/Diansalas/igaming-platform/internal/observability"
)

func fine(ctx context.Context) {
	observability.RecordWebhookAdmissionDecision(ctx, observability.WebhookAdmissionAdmitted, observability.WebhookAdmissionReasonAdmitted, observability.WebhookProviderKindPayments, observability.WebhookAdmissionStagePreAuth)
}
`
	v, err = closedEnumScanFile("safe.go", []byte(safe))
	if err != nil {
		t.Fatalf("parse safe case: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations for using only the declared constants, got %v", v)
	}
}
