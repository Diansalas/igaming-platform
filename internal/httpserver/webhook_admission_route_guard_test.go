// Security review C3 of PRH-I4: a structural guard that every one of the
// three public webhook routes actually goes through ADR 0097 admission
// (admitPreAuth) AND RL-F4's path redaction (markWebhookRouteForLogging),
// so a future edit that quietly drops either call from one handler is
// caught by CI rather than discovered in production. This is an AST-based
// scan of each handler's own function body (not a live HTTP smoke test -
// see webhook_admission_integration_test.go for that), matching this
// codebase's established pattern for structural guarantees (e.g.
// internal/webhookauth's constant-time-compare scan,
// internal/providerkind's mock-naming completeness scan).
package httpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// webhookHandlerGuardSpec names one webhook route's handler constructor
// function and the file it lives in.
type webhookHandlerGuardSpec struct {
	file string
	fn   string
}

var webhookHandlerGuardSpecs = []webhookHandlerGuardSpec{
	{file: "deposit_handlers.go", fn: "newPaymentWebhookHandler"},
	{file: "casino_handlers.go", fn: "newCasinoWebhookHandler"},
	{file: "kyc_admin_handlers.go", fn: "newKYCWebhookHandler"},
}

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

func TestWebhookRouteGuard_EveryHandlerCallsAdmissionAndRedaction(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range webhookHandlerGuardSpecs {
		t.Run(spec.fn, func(t *testing.T) {
			path := filepath.Join(dir, spec.file)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", spec.file, err)
			}
			var fn *ast.FuncDecl
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == spec.fn {
					fn = fd
					break
				}
			}
			if fn == nil {
				t.Fatalf("%s: function %s not found", spec.file, spec.fn)
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
					t.Errorf("%s (%s): missing a call to %s - every webhook route must go through ADR 0097 admission and RL-F4 redaction (security review C3)", spec.fn, spec.file, want)
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
