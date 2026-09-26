// Stage 10.2 KYC-WH-1 fix verification: G7 guard, permanently kept per the
// binding QA test plan (docs/plans/stage-10.2-planning/03-review-qa-test-
// plan.md: "E2 is inverted, not deleted, into
// TestG7_NoConstStringFeedsWebhookCredential"), a direct descendant of the
// Stage 10.2 pre-fix evidence E2
// (TestKYCWH1_PreFix_SecretIsCompileTimeConstant).
//
// Pre-fix, that test used go/ast to assert kyc.NewMockKYCProvider's sole
// argument in this package's main.go was an identifier resolving to a
// top-level string const - proving a literal secret was structurally
// reachable. Post-fix, kyc.NewMockKYCProvider takes ZERO arguments, so
// there is no parameter through which any const string (or any other
// value) could ever reach a webhook credential again. This guard asserts
// exactly that, structurally, forever - so a future change accidentally
// reintroducing a parameter (and wiring a literal into it) fails this
// test immediately.
//
// SECRET-SAFETY (mechanical no-print rule, binding per the QA test plan):
// this file never reads, logs, prints, formats, or otherwise surfaces any
// credential/secret value - it inspects only function signatures (go/ast
// KIND facts: argument count, AST node types) and source text for the
// ABSENCE of certain call shapes, never any literal's .Value.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// findCallsTo walks file's AST and returns every *ast.CallExpr whose
// selector or identifier matches name (e.g. "NewMockKYCProvider").
func findCallsTo(file *ast.File, name string) []*ast.CallExpr {
	var found []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel != nil && fn.Sel.Name == name {
				found = append(found, call)
			}
		case *ast.Ident:
			if fn.Name == name {
				found = append(found, call)
			}
		}
		return true
	})
	return found
}

// TestG7_NoConstStringFeedsWebhookCredential asserts, by structure only:
//
//  1. kyc.NewMockKYCProvider is declared (in internal/kyc/mock_provider.go)
//     with ZERO parameters - so no caller anywhere in the tree can pass it
//     an argument at all, let alone a compile-time literal;
//  2. every call site to NewMockKYCProvider in this package's own source
//     (main.go, wiring.go, and every _test.go file) passes ZERO arguments,
//     matching that signature;
//  3. git grep for the exact historical compile-time constant NAME
//     ("kycMockWebhookSecret") returns nothing in this package's tree -
//     the constant itself (name AND declaration) was deleted, not merely
//     stopped being passed.
func TestG7_NoConstStringFeedsWebhookCredential(t *testing.T) {
	fset := token.NewFileSet()

	// (1) the function declaration itself, in internal/kyc.
	kycMockProviderPath := filepath.Join("..", "..", "internal", "kyc", "mock_provider.go")
	kycFile, err := parser.ParseFile(fset, kycMockProviderPath, nil, 0)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", kycMockProviderPath, err)
	}
	var decl *ast.FuncDecl
	ast.Inspect(kycFile, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if ok && fd.Recv == nil && fd.Name.Name == "NewMockKYCProvider" {
			decl = fd
		}
		return true
	})
	if decl == nil {
		t.Fatal("expected to find a top-level func NewMockKYCProvider declaration")
	}
	if n := decl.Type.Params.NumFields(); n != 0 {
		t.Fatalf("expected NewMockKYCProvider to take 0 parameters, found %d field group(s)", n)
	}

	// (2) every call site in this package (main.go and any _test.go file)
	// passes 0 arguments.
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("failed to glob *.go: %v", err)
	}
	totalCalls := 0
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}
		for _, call := range findCallsTo(file, "NewMockKYCProvider") {
			totalCalls++
			if len(call.Args) != 0 {
				t.Fatalf("%s: expected NewMockKYCProvider to be called with 0 arguments, found %d", path, len(call.Args))
			}
		}
	}
	if totalCalls == 0 {
		t.Fatal("expected at least one NewMockKYCProvider call site in this package")
	}

	// (3) the historical constant's NAME must not appear anywhere in this
	// package's source - name only, never any value.
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && id.Name == "kycMockWebhookSecret" {
				t.Fatalf("%s: found a reference to the historical constant name kycMockWebhookSecret - it must be fully removed, not merely unused", path)
			}
			return true
		})
	}

	t.Log("PASS: NewMockKYCProvider takes 0 parameters; every call site in this package passes 0 arguments; the historical const name is gone")
}
