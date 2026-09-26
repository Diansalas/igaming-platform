// Stage 10.2 pre-fix evidence; retired or inverted by the fix commit.
//
// KYC-WH-1 (design doc §0, §H E2): TestKYCWH1_PreFix_SecretIsCompileTimeConstant
// uses go/ast (not go/types, not any reflection over the running binary)
// to assert that the single argument passed to kyc.NewMockKYCProvider in
// this package's main.go is an identifier bound to a package-level string
// constant. This is a KIND-only assertion (token.STRING), proving the
// argument is a compile-time literal wired through a named const, never
// anything derived at runtime (an env var, a secret-store lookup, a
// randomly generated value, etc).
//
// SECRET-SAFETY (mechanical no-print rule, binding per
// docs/plans/stage-10.2-planning/03-review-qa-test-plan.md): this file
// NEVER reads, logs, prints, formats, or otherwise surfaces the literal's
// .Value anywhere - not in a t.Log/t.Errorf/t.Fatalf message, not in a
// comment, not in an intermediate variable that could later be printed.
// Every assertion below inspects only: the number of call arguments, the
// AST node TYPE (*ast.Ident vs. something else), the resolved
// declaration's token.Token KIND (token.CONST), and the literal's
// token.Token KIND (token.STRING). If any of these ever needs debugging,
// the fix is to assert a different structural property - never to print
// the value.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// findNewMockKYCProviderCall walks file's AST and returns the single
// argument expression passed to a call whose selector is
// "NewMockKYCProvider" (e.g. kyc.NewMockKYCProvider(...)). It fails the
// test (via t.Fatalf, which never touches the argument's value - only
// structural facts) if it finds zero or more than one such call, or a
// call with argument count != 1.
func findNewMockKYCProviderCall(t *testing.T, file *ast.File) ast.Expr {
	t.Helper()
	var found []ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "NewMockKYCProvider" {
			return true
		}
		if len(call.Args) != 1 {
			t.Fatalf("expected exactly 1 argument to NewMockKYCProvider, found %d", len(call.Args))
		}
		found = append(found, call.Args[0])
		return true
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 call to NewMockKYCProvider in this file, found %d", len(found))
	}
	return found[0]
}

// resolveTopLevelConstStringLit finds a top-level `const <name> = "..."`
// (or `const ( <name> = "..." )`) declaration in file matching name, and
// returns the KIND of its value literal only - never the literal itself.
// Returns ok=false if name is not bound to any top-level const at all, or
// if that const's value is not a single *ast.BasicLit.
func resolveTopLevelConstStringLit(file *ast.File, name string) (kind token.Token, ok bool) {
	for _, decl := range file.Decls {
		gen, isGen := decl.(*ast.GenDecl)
		if !isGen || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, isVS := spec.(*ast.ValueSpec)
			if !isVS {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != name {
					continue
				}
				if i >= len(vs.Values) {
					// iota-style / no explicit value for this name.
					return 0, false
				}
				lit, isLit := vs.Values[i].(*ast.BasicLit)
				if !isLit {
					return 0, false
				}
				return lit.Kind, true
			}
		}
	}
	return 0, false
}

func TestKYCWH1_PreFix_SecretIsCompileTimeConstant(t *testing.T) {
	fset := token.NewFileSet()
	mainGoPath := filepath.Join(".", "main.go")
	file, err := parser.ParseFile(fset, mainGoPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", mainGoPath, err)
	}

	arg := findNewMockKYCProviderCall(t, file)

	// KIND only: the argument AST node type must be a plain identifier
	// (a name reference), never a literal spelled out inline, a function
	// call, an environment/config lookup, or any other expression shape.
	ident, isIdent := arg.(*ast.Ident)
	if !isIdent {
		t.Fatalf("expected NewMockKYCProvider's argument to be a plain identifier (*ast.Ident), got %T", arg)
	}

	// The identifier must resolve to a top-level `const` declaration
	// whose value is a string literal (token.STRING) - kind only, value
	// never read.
	kind, ok := resolveTopLevelConstStringLit(file, ident.Name)
	if !ok {
		t.Fatalf("expected identifier %q to resolve to a top-level const with an explicit literal value, found none", ident.Name)
	}
	if kind != token.STRING {
		t.Fatalf("expected identifier %q's const declaration to be a STRING literal kind, got %v", ident.Name, kind)
	}

	t.Log("PRE-FIX EVIDENCE: NewMockKYCProvider's sole argument is an identifier bound to a package-level string constant (kind asserted only; literal value never read, logged, or printed)")
}
