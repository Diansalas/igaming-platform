package httpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// I-wire final review F1: flushResponse must NEVER run from a defer. On a panic
// before any write, a deferred Flush commits an implicit 200 and the recover
// middleware's 500 is dropped, so a payment/casino provider would acknowledge a
// callback whose transaction rolled back. This guard covers every handler file
// (including the simulation route, which has no panic test of its own).
func deferredFlushSites(t *testing.T, name, src string) (sites, seenFlush int) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	isFlush := func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "flushResponse"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if isFlush(n) {
			seenFlush++
		}
		if d, ok := n.(*ast.DeferStmt); ok {
			ast.Inspect(d, func(m ast.Node) bool {
				if isFlush(m) {
					sites++
				}
				return true
			})
		}
		return true
	})
	return sites, seenFlush
}

func TestFlushResponse_IsNeverCalledFromADefer(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".go" || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		sites, flushes := deferredFlushSites(t, n, string(b))
		seen += flushes
		if sites != 0 {
			t.Errorf("%s: flushResponse is called inside a defer (%d site(s)): a panic would reach the provider as HTTP 200", n, sites)
		}
	}
	if seen < 2 { // the two inline call sites (kill switch, simulation payload mismatch)
		t.Fatalf("expected to see the 2 inline flushResponse call sites (kill switch, simulation), saw %d", seen)
	}
}

func TestFlushResponse_NotDeferred_NegativeControl(t *testing.T) {
	bad := "package p\nfunc h(w W) {\n\tdefer func() {\n\t\tflushResponse(w)\n\t}()\n}\n"
	if s, _ := deferredFlushSites(t, "bad.go", bad); s != 1 {
		t.Fatalf("the guard must flag a deferred flush, got %d", s)
	}
	bad2 := "package p\nfunc h(w W) {\n\tdefer flushResponse(w)\n}\n"
	if s, _ := deferredFlushSites(t, "bad2.go", bad2); s != 1 {
		t.Fatalf("the guard must flag `defer flushResponse(w)`, got %d", s)
	}
	good := "package p\nfunc h(w W) {\n\twrite(w)\n\tflushResponse(w)\n}\n"
	if s, f := deferredFlushSites(t, "good.go", good); s != 0 || f != 1 {
		t.Fatalf("an inline flush must pass, got sites=%d flushes=%d", s, f)
	}
}
