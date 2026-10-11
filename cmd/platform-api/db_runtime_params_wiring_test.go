package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/config"
)

// ADR 0112 slice-3 residual 14(f): the production pool is wired to db.ConnectWithRuntimeParams with
// the configured idle_in_transaction_session_timeout, and nothing in this binary opens a pool any
// other way. Source-level pins (DB-free); the live behaviour is in
// db_runtime_params_integration_test.go.
func TestRun_ProductionPoolUsesRuntimeParams(t *testing.T) {
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
	var connectCalls, plainConnect, launchNew int
	var lastArg string
	launchDeps := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name + "." + sel.Sel.Name {
			case "db.ConnectWithRuntimeParams":
				connectCalls++
				if len(x.Args) == 5 {
					if c, ok := x.Args[4].(*ast.CallExpr); ok {
						if s, ok := c.Fun.(*ast.SelectorExpr); ok {
							if cid, ok := s.X.(*ast.Ident); ok {
								lastArg = cid.Name + "." + s.Sel.Name
							}
						}
					}
				}
			case "db.Connect":
				plainConnect++
			case "launchgov.New":
				launchNew++
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "LaunchAuthorisation" {
				launchDeps = true
			}
		}
		return true
	})
	if connectCalls != 1 || plainConnect != 0 {
		t.Fatalf("run() must open the application pool exactly once, with db.ConnectWithRuntimeParams (got %d, plain db.Connect %d)", connectCalls, plainConnect)
	}
	if lastArg != "cfg.DatabaseRuntimeParams" {
		t.Fatalf("the pool must be opened with cfg.DatabaseRuntimeParams(), got %q", lastArg)
	}
	if launchNew != 1 || !launchDeps {
		t.Fatalf("run() must build the launch authorisation service and hand it to the HTTP server (New=%d, Deps field=%v)", launchNew, launchDeps)
	}
}

func TestDatabaseRuntimeParams_DefaultAndOverride(t *testing.T) {
	p := config.Config{}.DatabaseRuntimeParams()
	if len(p) != 1 || p["idle_in_transaction_session_timeout"] != "30000" {
		t.Fatalf("the zero Config must still yield the 30s default (the setting cannot be omitted): %v", p)
	}
	p = config.Config{DatabaseIdleInTxTimeout: 700 * time.Millisecond}.DatabaseRuntimeParams()
	if p["idle_in_transaction_session_timeout"] != "700" {
		t.Fatalf("milliseconds expected, got %v", p)
	}
	for k := range (config.Config{}).DatabaseRuntimeParams() {
		if strings.Contains(k, "role") {
			t.Fatalf("no role-level setting: %q", k)
		}
	}
}
