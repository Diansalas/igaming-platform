package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRun_GuardAndSchemeValidationPrecedeDBConnect pins the placement of
// the two pre-DB startup checks in run(): refuseSyntheticInProduction and
// validateWebhookSchemes (security S-1/I-1) are both called, and both
// before db.Connect. The synthetic guard's ordering is also proven end to
// end by the integration subprocess test.
func TestRun_GuardAndSchemeValidationPrecedeDBConnect(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("main.go has no run()")
	}
	first := map[string]token.Pos{}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			if id, ok := f.X.(*ast.Ident); ok {
				name = id.Name + "." + f.Sel.Name
			}
		}
		if _, seen := first[name]; name != "" && !seen {
			first[name] = call.Pos()
		}
		return true
	})
	connect, ok := first["db.Connect"]
	if !ok {
		t.Fatal("run() does not call db.Connect")
	}
	for _, check := range []string{"refuseSyntheticInProduction", "validateWebhookSchemes"} {
		pos, ok := first[check]
		if !ok {
			t.Fatalf("run() does not call %s", check)
		}
		if pos >= connect {
			t.Fatalf("run() calls %s after db.Connect; it must run before any database side effect", check)
		}
	}
}
