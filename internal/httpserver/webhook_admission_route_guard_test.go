// Security review C3 of PRH-I4 (part 2, security re-verification round 3,
// N2): a structural guard that DISCOVERS every /v1/webhooks/ route
// registration in the package - by walking the actual route-registration
// call sites (mux.HandleFunc/mux.Handle), not by scanning a fixed list of
// three named constructors - and requires each one's handler to call
// admitPreAuth and markWebhookRouteForLogging directly. The earlier,
// round-3 version of this guard checked only
// {newPaymentWebhookHandler, newCasinoWebhookHandler, newKYCWebhookHandler}
// by name; security's re-verification (rv-prh-i4-security.md §6, N2)
// proved that a FOURTH webhook route, registered with a handler that never
// calls admitPreAuth, survives that version untouched - the whole point of
// C3 (a future route must be forced through admission, not just today's
// three). This version instead enumerates every HandleFunc/Handle call
// whose pattern literal contains "/v1/webhooks/", resolves the handler
// argument to a named, in-package constructor function, and checks THAT
// function's own body - so a fourth route is caught at its very first
// registration, with no list to remember to update.
//
// Security re-verification #2 (rv-prh-i4-security.md §7.3/§7.5, C3
// narrowed to Low L8): the round-4 guard above only considered a first
// argument that is an *ast.BasicLit and silently SKIPPED anything else -
// which N2b (a route pattern behind a package `const`) and N2c (a route
// registered through a helper that itself calls `m.HandleFunc(p, h)` with
// a non-literal `p`) both evade, since neither one's pattern is a literal
// AT THE POINT this guard used to look. Per security's explicit
// requirement ("fail on any pattern it cannot resolve" - not just skip
// it), this version additionally scans EVERY HandleFunc/Handle call in
// the package's own non-test files - not only the ones whose (already
// resolved) pattern happens to contain "/v1/webhooks/" - and fails the
// whole test outright if any such call's first argument is not a literal
// string. Security's own audit found all 152 current registrations use a
// literal pattern, so this costs nothing today and closes both evasions:
// N2b's direct call and N2c's helper's own inner call are both
// HandleFunc/Handle calls with a non-literal first argument, so both are
// caught by this one additional, package-wide rule.
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

// requiredWebhookCalls are the two calls C3 requires inside every webhook
// handler's returned closure: admitPreAuth (the ADR 0097 admission
// entrypoint) and markWebhookRouteForLogging (RL-F4's redaction hook -
// admitPreAuth itself also calls this internally, but the security
// review's "leftover" fix additionally requires a direct call BEFORE any
// early orchestrator-nil return, so this guard checks for it explicitly
// rather than relying on admitPreAuth's own internal call, which a future
// refactor could remove without this guard noticing if it only checked
// for admitPreAuth).
var requiredWebhookCalls = []string{"admitPreAuth", "markWebhookRouteForLogging"}

// webhookRoutePattern is what marks a route registration as an ADR 0097
// webhook route: any HandleFunc/Handle pattern literal containing this
// substring, regardless of which file registers it or which HTTP verb it
// uses.
const webhookRoutePattern = "/v1/webhooks/"

// discoveredWebhookRoute is one route registration this guard found, with
// enough information to report a useful failure.
type discoveredWebhookRoute struct {
	file        string
	pattern     string
	handlerName string // "" if the guard could not resolve a named constructor
}

// TestWebhookRouteGuard_EveryHandlerCallsAdmissionAndRedaction parses
// every non-test .go file in this package's own directory, finds every
// call to (mux).HandleFunc/(mux).Handle whose pattern literal contains
// "/v1/webhooks/", and for each one:
//  1. requires the handler argument to be a call to a named,
//     zero-or-more-argument, in-package function (e.g.
//     `newCasinoWebhookHandler(deps)`) - anything else (an inline func
//     literal, a method value, a variable) is unresolvable and FAILS the
//     test outright, per security's "fail on any pattern it cannot
//     resolve" requirement, rather than silently skipping it;
//  2. locates that named function's own *ast.FuncDecl somewhere in the
//     package and requires its body to directly call every entry in
//     requiredWebhookCalls.
//
// This enumerates routes structurally instead of trusting a maintained
// list, so a brand-new webhook route (security review N2's exact
// scenario: a fourth `/v1/webhooks/...` registration whose handler never
// calls admitPreAuth) is caught the moment it is registered.
func TestWebhookRouteGuard_EveryHandlerCallsAdmissionAndRedaction(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("found no non-test .go files to scan - the guard's own discovery is broken")
	}

	var routes []discoveredWebhookRoute
	var nonLiteralFailures int
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				// Security re-verification #2 (§7.3, L8): a HandleFunc/
				// Handle call whose pattern is not a string literal - a
				// package const (N2b) or a variable/parameter forwarded by
				// a registration helper (N2c) - cannot be resolved to a
				// concrete route pattern by this guard at all, so it
				// fails CLOSED here instead of silently skipping the call
				// (which is exactly how N2b/N2c evaded the round-4 guard).
				t.Errorf("%s: a %s call's first argument is not a string literal - this guard cannot "+
					"determine whether it registers a /v1/webhooks/ route, and per security review C3/L8 "+
					"an unresolvable route-registration pattern fails this guard closed rather than being "+
					"silently skipped (use a literal string pattern, or extend this guard to resolve the "+
					"const/variable in question)", name, sel.Sel.Name)
				nonLiteralFailures++
				return true
			}
			pattern := strings.Trim(lit.Value, "\"`")
			if !strings.Contains(pattern, webhookRoutePattern) {
				return true
			}
			route := discoveredWebhookRoute{file: name, pattern: pattern}
			if handlerCall, ok := call.Args[1].(*ast.CallExpr); ok {
				if ident, ok := handlerCall.Fun.(*ast.Ident); ok {
					route.handlerName = ident.Name
				}
			}
			routes = append(routes, route)
			return true
		})
	}
	if nonLiteralFailures > 0 {
		t.Fatalf("%d HandleFunc/Handle call(s) with an unresolvable (non-literal) pattern - see the errors "+
			"above; this guard cannot verify those routes go through admission (security review C3/L8)", nonLiteralFailures)
	}

	if len(routes) == 0 {
		t.Fatal("discovered zero /v1/webhooks/ route registrations - the guard's own pattern match is broken")
	}

	for _, route := range routes {
		route := route
		t.Run(route.pattern, func(t *testing.T) {
			if route.handlerName == "" {
				t.Fatalf("%s: route %q is registered with a handler expression this guard cannot resolve to a "+
					"named, in-package constructor function (e.g. `newCasinoWebhookHandler(deps)`) - "+
					"per security review C3/N2, an unresolvable webhook route handler fails this guard "+
					"closed rather than being silently skipped", route.file, route.pattern)
			}

			var fn *ast.FuncDecl
			var declFile string
			for fname, f := range files {
				for _, d := range f.Decls {
					if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == route.handlerName {
						fn = fd
						declFile = fname
						break
					}
				}
				if fn != nil {
					break
				}
			}
			if fn == nil {
				t.Fatalf("%s: route %q names handler constructor %s, but no such function is declared "+
					"anywhere in this package", route.file, route.pattern, route.handlerName)
			}

			found := map[string]bool{}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calleeName(call)
				for _, want := range requiredWebhookCalls {
					if name == want {
						found[want] = true
					}
				}
				return true
			})
			for _, want := range requiredWebhookCalls {
				if !found[want] {
					t.Errorf("%s (%s, registered for %q in %s): missing a call to %s - every webhook route "+
						"must go through ADR 0097 admission and RL-F4 redaction (security review C3)",
						route.handlerName, declFile, route.pattern, route.file, want)
				}
			}
		})
	}
}

// calleeName returns the identifier or method name a call expression
// invokes - "admitPreAuth" for both a bare call and a method call like
// deps.webhookAdmission.admitPreAuth(...).
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}
