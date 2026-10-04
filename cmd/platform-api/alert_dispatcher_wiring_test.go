package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// ADR 0102 17.6: the durable alert dispatcher is started in main.go with the LogSink
// ONLY. The MOCK sink must never be referenced by a binary, and the loop must be the
// panic-recovering, draining RunDispatcherLoop.
func TestMain_AlertDispatcherWiring_LogSinkOnlyNeverMock(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var newDispatcher, runLoop, logSink int
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "alerting" {
				switch x.Sel.Name {
				case "MockSink":
					t.Errorf("main.go must never reference alerting.MockSink")
				case "NewDispatcher":
					newDispatcher++
				case "RunDispatcherLoop":
					runLoop++
				case "LogSink":
					logSink++
				}
			}
		}
		return true
	})
	if newDispatcher != 1 || runLoop != 1 || logSink != 1 {
		t.Fatalf("expected exactly one NewDispatcher, RunDispatcherLoop and LogSink in main.go, got %d/%d/%d", newDispatcher, runLoop, logSink)
	}
}
