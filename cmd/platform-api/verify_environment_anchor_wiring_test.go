// ADR 0115 S1 launch gate: platform-api must REFUSE to start in production when the
// database_environment anchor is unrecorded (and when it says development / test). The decision
// lives in db.VerifyEnvironmentAnchor (unit + integration tested); this structural test pins the
// call site in main.go's run(): the call exists, receives cfg.GuardEnvironment() (a missing APP_ENV
// is production), and a non-nil error RETURNS from run() - it must never be downgraded to a warning
// or discarded. Removing the refusal (call, argument, or return) fails this test.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestVerifyEnvironmentAnchor_CallSiteRefusesStart(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	isAnchorCall := func(e ast.Expr) (*ast.CallExpr, bool) {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return nil, false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "VerifyEnvironmentAnchor" {
			return nil, false
		}
		return call, true
	}
	calls, guarded := 0, 0
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if c, ok := isAnchorCall(call); ok {
				calls++
				if len(c.Args) != 3 {
					t.Fatalf("VerifyEnvironmentAnchor takes (ctx, guardEnvironment, reader), got %d args", len(c.Args))
				}
				arg, ok := c.Args[1].(*ast.CallExpr)
				if !ok {
					t.Fatalf("second argument must be cfg.GuardEnvironment(), got %T", c.Args[1])
				}
				if sel, ok := arg.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "GuardEnvironment" {
					t.Fatal("second argument must be cfg.GuardEnvironment(), not cfg.Environment")
				}
			}
		}
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		as, ok := ifs.Init.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		if _, ok := isAnchorCall(as.Rhs[0]); !ok {
			return true
		}
		bin, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.NEQ {
			t.Fatalf("anchor error must be tested with `err != nil`")
		}
		for _, st := range ifs.Body.List {
			if _, ok := st.(*ast.ReturnStmt); ok {
				guarded++
			}
		}
		if ifs.Else != nil {
			t.Fatal("the anchor check must not have an else branch that tolerates an unrecorded anchor")
		}
		return true
	})
	if calls != 1 {
		t.Fatalf("main.go must call db.VerifyEnvironmentAnchor exactly once, found %d", calls)
	}
	if guarded != 1 {
		t.Fatal("a VerifyEnvironmentAnchor error must return from run() (refuse to start), never be logged or ignored")
	}
}
