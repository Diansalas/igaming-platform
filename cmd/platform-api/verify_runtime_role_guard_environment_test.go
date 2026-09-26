// Stage 10.3 W1b follow-up (raised by `architect`'s W0 code check as the
// same class of gap as security condition C13):
// db.VerifyRuntimeRoleInProduction's call site in main.go's run() must
// pass cfg.GuardEnvironment(), not cfg.Environment directly - otherwise a
// production task deployed with APP_ENV missing entirely would be exempt
// from the role-ownership check (Environment defaults to "development" in
// that case, and the function itself only gates on the literal string
// "production"). This is defense-in-depth alongside the synthetic guard
// (which already refuses to start before db.Connect for a missing
// APP_ENV) - if a future change ever made every registered component
// ProductionEligible (so the synthetic guard stops refusing), this call
// site is the next, independent line of defense, and it needs its own
// direct check since the subprocess ordering test never reaches it (the
// synthetic guard already stops the process earlier in that scenario).
//
// A structural, source-level assertion (matching this package's existing
// kyc_prefix_e2_const_secret_test.go convention) rather than a live
// integration test - proving the exact argument expression at the call
// site, not merely that SOME value is passed.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestVerifyRuntimeRoleInProduction_CallSitePassesGuardEnvironment(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "VerifyRuntimeRoleInProduction" {
			return true
		}
		found = true
		if len(call.Args) < 2 {
			t.Fatalf("VerifyRuntimeRoleInProduction call has fewer than 2 arguments: %d", len(call.Args))
		}
		envArg := call.Args[1]
		envCall, ok := envArg.(*ast.CallExpr)
		if !ok {
			t.Fatalf("VerifyRuntimeRoleInProduction's second argument is not a method call, got %T - expected cfg.GuardEnvironment()", envArg)
			return false
		}
		envSel, ok := envCall.Fun.(*ast.SelectorExpr)
		if !ok || envSel.Sel.Name != "GuardEnvironment" {
			t.Fatalf("VerifyRuntimeRoleInProduction's second argument must be cfg.GuardEnvironment(), not cfg.Environment directly (a missing APP_ENV must be treated as production for this check too)")
		}
		return false
	})

	if !found {
		t.Fatal("no call to db.VerifyRuntimeRoleInProduction found in main.go")
	}
}
