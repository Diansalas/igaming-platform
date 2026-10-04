package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// ADR 0102 17.6: the durable alert dispatcher is started in main.go with the LogSink
// ONLY. The MOCK sink must never be referenced by a binary, the loop must be the
// panic-recovering, draining RunDispatcherLoop started in its own goroutine on the
// run context, and shutdown must wait for it (bounded) for LONGER than the loop's own
// drain timeout. checkAlertWiring is the whole check so that a negative-control
// fixture can prove each rule really bites (a rule that cannot fail proves nothing).

func parseSrc(t *testing.T, name, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func isSel(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == pkg && s.Sel.Name == name
}

func isAlertingCall(call *ast.CallExpr, name string) bool { return isSel(call.Fun, "alerting", name) }

// callsIn returns every call expression under n matching pred.
func callsIn(n ast.Node, pred func(*ast.CallExpr) bool) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok && pred(c) {
			out = append(out, c)
		}
		return true
	})
	return out
}

func isMethodOn(c *ast.CallExpr, recv, method string) bool {
	s, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != method {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == recv
}

func isBuiltinClose(c *ast.CallExpr, ident string) bool {
	id, ok := c.Fun.(*ast.Ident)
	if !ok || id.Name != "close" || len(c.Args) != 1 {
		return false
	}
	a, ok := c.Args[0].(*ast.Ident)
	return ok && a.Name == ident
}

// seconds evaluates `<int literal> * time.Second`.
func seconds(e ast.Expr) (time.Duration, bool) {
	b, ok := e.(*ast.BinaryExpr)
	if !ok || b.Op != token.MUL || !isSel(b.Y, "time", "Second") {
		return 0, false
	}
	lit, ok := b.X.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func goStmts(body ast.Node) []*ast.GoStmt {
	var out []*ast.GoStmt
	ast.Inspect(body, func(x ast.Node) bool {
		if g, ok := x.(*ast.GoStmt); ok {
			out = append(out, g)
		}
		return true
	})
	return out
}

// checkAlertWiring checks every non-test cmd/platform-api file (files: name -> source;
// the one named "main.go" holds run()) and returns the violations.
func checkAlertWiring(t *testing.T, files map[string]string, minWait time.Duration) []string {
	t.Helper()
	var v []string
	var newDisp, runLoop, logSink int
	var mainFile *ast.File
	for name, src := range files {
		f := parseSrc(t, name, src)
		if name == "main.go" {
			mainFile = f
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := s.X.(*ast.Ident); ok && id.Name == "alerting" {
					switch s.Sel.Name {
					case "MockSink":
						v = append(v, name+": must never reference alerting.MockSink")
					case "NewDispatcher":
						newDisp++
					case "RunDispatcherLoop", "RunDispatcherLoopWithConfig":
						runLoop++
					case "LogSink":
						logSink++
					}
				}
			}
			return true
		})
	}
	if newDisp != 1 || runLoop != 1 || logSink != 1 {
		v = append(v, fmt.Sprintf("expected exactly one NewDispatcher, RunDispatcherLoop and LogSink across cmd/platform-api, got %d/%d/%d", newDisp, runLoop, logSink))
	}
	if mainFile == nil {
		return append(v, "main.go missing")
	}
	var run *ast.FuncDecl
	for _, d := range mainFile.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "run" {
			run = fd
		}
	}
	if run == nil {
		return append(v, "func run not found in main.go")
	}

	// NewDispatcher(pool, cfg, alerting.LogSink{...}) - the sink argument.
	for _, c := range callsIn(run.Body, func(c *ast.CallExpr) bool { return isAlertingCall(c, "NewDispatcher") }) {
		cl, ok := (*ast.CompositeLit)(nil), false
		if len(c.Args) == 3 {
			cl, ok = c.Args[2].(*ast.CompositeLit)
		}
		if !ok || !isSel(cl.Type, "alerting", "LogSink") {
			v = append(v, "NewDispatcher's sink argument must be an alerting.LogSink literal")
		}
	}

	gos := goStmts(run.Body)

	// (1) RunDispatcherLoop is called inside a GoStmt, with the run context `ctx`.
	loopCalls := callsIn(run.Body, func(c *ast.CallExpr) bool { return isAlertingCall(c, "RunDispatcherLoop") })
	inGo := 0
	for _, g := range gos {
		for _, c := range callsIn(g, func(c *ast.CallExpr) bool { return isAlertingCall(c, "RunDispatcherLoop") }) {
			inGo++
			if len(c.Args) < 3 {
				v = append(v, "RunDispatcherLoop needs (ctx, dispatcher, interval)")
				continue
			}
			if id, ok := c.Args[0].(*ast.Ident); !ok || id.Name != "ctx" {
				v = append(v, "RunDispatcherLoop must be given the run context `ctx` (not a detached/background context)")
			}
			if !isSel(c.Args[2], "alerting", "DefaultLoopInterval") {
				v = append(v, "RunDispatcherLoop must use alerting.DefaultLoopInterval")
			}
			if len(callsIn(g, func(d *ast.CallExpr) bool { return isMethodOn(d, "alertDispatcherWG", "Done") })) == 0 {
				v = append(v, "the dispatcher goroutine must call alertDispatcherWG.Done")
			}
			// ctx must not be redeclared or reassigned inside the goroutine (a
			// context.WithoutCancel(ctx) shadow would keep the loop alive past shutdown).
			ast.Inspect(g, func(x ast.Node) bool {
				if vs, ok := x.(*ast.ValueSpec); ok { // var ctx = ... (inside a DeclStmt)
					for _, id := range vs.Names {
						if id.Name == "ctx" {
							v = append(v, "the dispatcher goroutine must not declare a variable named ctx")
						}
					}
				}
				if as, ok := x.(*ast.AssignStmt); ok {
					for _, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == "ctx" {
							v = append(v, "the dispatcher goroutine must not redeclare or reassign ctx")
						}
					}
				}
				return true
			})
			// Exactly one alertDispatcherWG.Add(1), before this go statement.
			adds := callsIn(run.Body, func(d *ast.CallExpr) bool { return isMethodOn(d, "alertDispatcherWG", "Add") })
			if len(adds) != 1 {
				v = append(v, fmt.Sprintf("expected exactly one alertDispatcherWG.Add, found %d", len(adds)))
			} else {
				lit, ok := (*ast.BasicLit)(nil), false
				if len(adds[0].Args) == 1 {
					lit, ok = adds[0].Args[0].(*ast.BasicLit)
				}
				if !ok || lit.Value != "1" {
					v = append(v, "alertDispatcherWG.Add must be Add(1)")
				}
				if adds[0].Pos() > g.Pos() {
					v = append(v, "alertDispatcherWG.Add(1) must come before the dispatcher go statement")
				}
			}
		}
	}
	if len(loopCalls) != 1 || inGo != 1 {
		v = append(v, fmt.Sprintf("RunDispatcherLoop must be called exactly once, inside a goroutine (found %d call(s), %d inside a go statement)", len(loopCalls), inGo))
	}

	// (2) alertDispatcherWG.Wait() runs in a goroutine that then closes alertDispatcherDone.
	waits := callsIn(run.Body, func(c *ast.CallExpr) bool { return isMethodOn(c, "alertDispatcherWG", "Wait") })
	waitInGo := 0
	for _, g := range gos {
		w := callsIn(g, func(c *ast.CallExpr) bool { return isMethodOn(c, "alertDispatcherWG", "Wait") })
		if len(w) == 0 {
			continue
		}
		waitInGo += len(w)
		if len(callsIn(g, func(c *ast.CallExpr) bool { return isBuiltinClose(c, "alertDispatcherDone") })) == 0 {
			v = append(v, "the goroutine that waits on alertDispatcherWG must close(alertDispatcherDone)")
		}
	}
	if len(waits) != 1 || waitInGo != 1 {
		v = append(v, fmt.Sprintf("alertDispatcherWG.Wait must be called exactly once, inside a goroutine (found %d, %d inside a go statement): a bare Wait blocks shutdown unboundedly", len(waits), waitInGo))
	}

	// (3) a select with <-alertDispatcherDone AND a time.After(N s) case, N s > drain timeout.
	selects := 0
	var shutdownPos token.Pos
	for _, c := range callsIn(run.Body, func(c *ast.CallExpr) bool {
		s, ok := c.Fun.(*ast.SelectorExpr)
		return ok && s.Sel.Name == "Shutdown"
	}) {
		if shutdownPos == token.NoPos || c.Pos() < shutdownPos {
			shutdownPos = c.Pos()
		}
	}
	if shutdownPos == token.NoPos {
		v = append(v, "run must call <server>.Shutdown; the dispatcher drain wait must come after it")
	}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectStmt)
		if !ok {
			return true
		}
		var gotDone bool
		var after time.Duration
		var gotAfter bool
		for _, cc := range sel.Body.List {
			comm := cc.(*ast.CommClause).Comm
			es, ok := comm.(*ast.ExprStmt)
			if !ok {
				continue
			}
			u, ok := es.X.(*ast.UnaryExpr)
			if !ok || u.Op != token.ARROW {
				continue
			}
			if id, ok := u.X.(*ast.Ident); ok && id.Name == "alertDispatcherDone" {
				gotDone = true
			}
			if c, ok := u.X.(*ast.CallExpr); ok && isSel(c.Fun, "time", "After") && len(c.Args) == 1 {
				if d, ok := seconds(c.Args[0]); ok {
					after, gotAfter = d, true
				}
			}
		}
		if !gotDone {
			return true
		}
		selects++
		if shutdownPos != token.NoPos && sel.Pos() < shutdownPos {
			v = append(v, "the dispatcher drain select must come after the server Shutdown call")
		}
		if !gotAfter {
			v = append(v, "the shutdown select on alertDispatcherDone must have a `time.After(N * time.Second)` case (bounded wait)")
		} else if after <= minWait {
			v = append(v, fmt.Sprintf("the bounded shutdown wait (%s) must be LONGER than the loop's DrainTimeout (%s)", after, minWait))
		}
		return true
	})
	if selects != 1 {
		v = append(v, fmt.Sprintf("expected exactly one shutdown select on alertDispatcherDone, found %d", selects))
	}
	return v
}

func readCmdFiles(t *testing.T) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".go" || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		files[n] = string(b)
	}
	if _, ok := files["main.go"]; !ok {
		t.Fatal("main.go not found")
	}
	return files
}

func TestMain_AlertDispatcherWiring_LogSinkOnlyNeverMock(t *testing.T) {
	if v := checkAlertWiring(t, readCmdFiles(t), alerting.DefaultLoopDrainTimeout); len(v) != 0 {
		t.Fatalf("alert dispatcher wiring violations:\n  %s", strings.Join(v, "\n  "))
	}
}

const goodRun = `package main
func run() error {
	d := alerting.NewDispatcher(pool, alerting.DefaultDispatcherConfig(), alerting.LogSink{Logger: logger})
	var alertDispatcherWG sync.WaitGroup
	alertDispatcherWG.Add(1)
	go func() {
		defer alertDispatcherWG.Done()
		alerting.RunDispatcherLoop(ctx, d, alerting.DefaultLoopInterval)
	}()
	server.Shutdown(shutdownCtx)
	alertDispatcherDone := make(chan struct{})
	go func() {
		alertDispatcherWG.Wait()
		close(alertDispatcherDone)
	}()
	select {
	case <-alertDispatcherDone:
	case <-time.After(15 * time.Second):
	}
	return nil
}
`

// Negative controls: the good fixture passes, and each mutation is flagged.
func TestMain_AlertDispatcherWiring_NegativeControls(t *testing.T) {
	drain := alerting.DefaultLoopDrainTimeout
	if v := checkAlertWiring(t, map[string]string{"main.go": goodRun}, drain); len(v) != 0 {
		t.Fatalf("the good fixture must pass, got %v", v)
	}
	cases := []struct {
		name, from, to string
	}{
		{"loop not in a goroutine", "go func() {\n\t\tdefer alertDispatcherWG.Done()\n\t\talerting.RunDispatcherLoop(ctx, d, alerting.DefaultLoopInterval)\n\t}()", "func() {\n\t\tdefer alertDispatcherWG.Done()\n\t\talerting.RunDispatcherLoop(ctx, d, alerting.DefaultLoopInterval)\n\t}()"},
		{"loop on a background context", "RunDispatcherLoop(ctx,", "RunDispatcherLoop(context.Background(),"},
		{"goroutine never Done", "defer alertDispatcherWG.Done()", ""},
		{"Wait outside a goroutine", "go func() {\n\t\talertDispatcherWG.Wait()\n\t\tclose(alertDispatcherDone)\n\t}()", "alertDispatcherWG.Wait()\n\tclose(alertDispatcherDone)"},
		{"Wait goroutine never closes Done", "\t\tclose(alertDispatcherDone)\n", ""},
		{"select without a time.After case", "\tcase <-time.After(15 * time.Second):\n", ""},
		{"bounded wait not longer than the drain timeout", "15 * time.Second", "10 * time.Second"},
		{"Add removed", "\talertDispatcherWG.Add(1)\n", ""},
		{"Add after the go statement", "\talertDispatcherWG.Add(1)\n\tgo func() {\n\t\tdefer alertDispatcherWG.Done()", "\tgo func() {\n\t\tdefer alertDispatcherWG.Done()"},
		{"ctx shadowed in the goroutine", "\t\tdefer alertDispatcherWG.Done()\n", "\t\tdefer alertDispatcherWG.Done()\n\t\tctx := context.WithoutCancel(ctx)\n"},
		{"ctx var-declared in the goroutine", "\t\tdefer alertDispatcherWG.Done()\n", "\t\tdefer alertDispatcherWG.Done()\n\t\tvar ctx = context.WithoutCancel(ctx)\n"},
		{"drain moved before Shutdown", "\tserver.Shutdown(shutdownCtx)\n", ""},
		{"interval changed", "alerting.DefaultLoopInterval)", "30 * time.Second)"},
		{"mock sink", "alerting.LogSink{Logger: logger}", "alerting.MockSink{}"},
		{"non-log sink", "alerting.LogSink{Logger: logger}", "newSink()"},
	}
	for _, c := range cases {
		if !strings.Contains(goodRun, c.from) {
			t.Fatalf("%s: fixture does not contain the text to mutate", c.name)
		}
		bad := strings.Replace(goodRun, c.from, c.to, 1)
		if v := checkAlertWiring(t, map[string]string{"main.go": bad}, drain); len(v) == 0 {
			t.Errorf("%s: the wiring check did not flag the mutation", c.name)
		}
	}
	// A second file anywhere in cmd/platform-api that references the MOCK sink or a
	// second dispatcher is flagged too (N-5: every non-test file is scanned).
	if v := checkAlertWiring(t, map[string]string{"main.go": goodRun, "other.go": "package main\nvar _ = alerting.MockSink{}\n"}, drain); len(v) == 0 {
		t.Error("MockSink in another cmd/platform-api file was not flagged")
	}
	if v := checkAlertWiring(t, map[string]string{"main.go": goodRun, "other.go": "package main\nvar _ = alerting.NewDispatcher\n"}, drain); len(v) == 0 {
		t.Error("a second NewDispatcher in another cmd/platform-api file was not flagged")
	}
}

// The production bounded wait must exceed the loop's drain timeout (also asserted on
// the real main.go above); pin the constants' relationship so a change to either is
// seen here first.
func TestMain_AlertDispatcherWiring_DrainTimeoutIsTenSecondsOrLess(t *testing.T) {
	if alerting.DefaultLoopDrainTimeout >= 15*time.Second {
		t.Fatalf("DefaultLoopDrainTimeout is %s; main.go's 15s bounded wait would no longer exceed it", alerting.DefaultLoopDrainTimeout)
	}
}
