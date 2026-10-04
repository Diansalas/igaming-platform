package payments

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// PRH-2 H static guards. Lexical (go/ast, no go/types), in the style of the repository's other
// guards; each has a negative control proving it is not vacuous.

var sweeperSources = []string{"sweeper.go", "sweeper_loop.go", "sweeper_resolution_only.go", "payout_sweep.go"}

// itemWorkCalls are the sweeper functions that perform provider I/O or open their own transactions;
// none may be called lexically inside a database transaction closure (S-8).
var itemWorkCalls = map[string]bool{
	"processAttempt": true, "processPayoutAttempt": true, "processCreated": true, "processViaQueryStatus": true,
	"claimBatch": true, "RunPass": true, "RunOnce": true, "sweepTenant": true,
	"dispatchPayoutAttempt": true, "DispatchWithdraw": true, "callProvider": true,
	"reclaimPayoutCreated": true, "resubmitPayoutAmbiguous": true, "PollPayoutStatus": true,
	"driveCreatedAttempt": true,
}

var providerOutboundMethods = map[string]bool{"Deposit": true, "Withdraw": true, "QueryStatus": true}

func parseSrc(t *testing.T, name, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	var f *ast.File
	var err error
	if src == "" {
		f, err = parser.ParseFile(fset, name, nil, 0)
	} else {
		f, err = parser.ParseFile(fset, name, src, 0)
	}
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return fset, f
}

func calleeName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// txClosureViolations lists item-level work called inside a closure handed to a With* pool method.
func txClosureViolations(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !strings.HasPrefix(calleeName(call), "With") {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.FuncLit)
			if !ok {
				continue
			}
			ast.Inspect(lit.Body, func(m ast.Node) bool {
				if c, ok := m.(*ast.CallExpr); ok && itemWorkCalls[calleeName(c)] {
					out = append(out, fset.Position(c.Pos()).String()+": "+calleeName(c)+" inside a "+calleeName(call)+" closure")
				}
				return true
			})
		}
		return true
	})
	return out
}

func providerMethodCalls(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && providerOutboundMethods[sel.Sel.Name] {
				out = append(out, fset.Position(c.Pos()).String()+": "+sel.Sel.Name)
			}
		}
		return true
	})
	return out
}

func TestSweeperProcess_NoItemWorkInsideATransactionClosure_S8(t *testing.T) {
	for _, name := range sweeperSources {
		fset, f := parseSrc(t, name, "")
		for _, v := range txClosureViolations(fset, f) {
			t.Errorf("S-8: %s", v)
		}
	}
	// Negative control.
	fset, f := parseSrc(t, "bad.go", `package payments
func x(s *Sweeper) { _ = s.Pool.WithTenant(nil, nil, func(a, b any) error { return s.processAttempt(a, nil, nil) }) }`)
	if len(txClosureViolations(fset, f)) != 1 {
		t.Fatal("negative control: the guard did not flag item work inside a WithTenant closure")
	}
}

func TestSweeperLoopFiles_CallNoProviderMethodDirectly(t *testing.T) {
	// sweeper.go's own inline callProvider closure legitimately calls provider.QueryStatus (the
	// txscope guard owns that); the NEW files must call no outbound method at all.
	for _, name := range []string{"sweeper_loop.go", "sweeper_resolution_only.go"} {
		fset, f := parseSrc(t, name, "")
		for _, v := range providerMethodCalls(fset, f) {
			t.Errorf("%s must reach providers only through the gate: %s", name, v)
		}
	}
	fset, f := parseSrc(t, "bad.go", `package payments
func x(p PaymentProvider) { p.Withdraw(nil, WithdrawRequest{}) }`)
	if len(providerMethodCalls(fset, f)) != 1 {
		t.Fatal("negative control failed")
	}
}

// The advisory lock may appear only inside claimBatch (one short tx): never in a function that also
// calls a provider or runs item work, so it can never be held across a provider call.
func TestSweeperAdvisoryLock_OnlyInsideClaimBatch(t *testing.T) {
	entries, err := nonTestGoFiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range entries {
		fset, f := parseSrc(t, name, "")
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			uses := false
			ast.Inspect(fn, func(n ast.Node) bool {
				if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && strings.Contains(bl.Value, "advisory") {
					uses = true
				}
				return true
			})
			if !uses {
				continue
			}
			if name == "sweeper.go" && fn.Name.Name == "claimBatch" {
				found = true
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && itemWorkCalls[calleeName(c)] && calleeName(c) != "claimBatch" {
						t.Errorf("claimBatch must not do item work: %s", fset.Position(c.Pos()))
					}
					return true
				})
				continue
			}
			t.Errorf("%s: %s uses an advisory lock outside claimBatch", name, fn.Name.Name)
		}
	}
	if !found {
		t.Fatal("expected claimBatch to carry the advisory hint (vacuity guard)")
	}
}

// Tenants are swept whatever their status (in-flight money must resolve): the tenant listing must
// not filter on status, and every new-dispatch site must read the status in its own transaction.
func TestSweeperProcess_ListsAllTenants_AndDispatchSitesReadStatusInTx(t *testing.T) {
	_, loop := parseSrc(t, "sweeper_loop.go", "")
	ast.Inspect(loop, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && strings.Contains(strings.ToLower(bl.Value), "status") {
			t.Errorf("sweeper_loop.go must not filter tenants by status: %s", bl.Value)
		}
		return true
	})
	needs := map[string]map[string]string{ // file -> func -> required callee
		"sweeper.go":      {"processCreated": "deferIfResolutionOnly", "applyStatusEvidence": "tenantResolutionOnly"},
		"payout_sweep.go": {"reclaimPayoutCreated": "checkPayoutResolutionOnly", "resubmitPayoutAmbiguous": "checkPayoutResolutionOnly"},
	}
	for file, funcs := range needs {
		_, f := parseSrc(t, file, "")
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			want, ok := funcs[fn.Name.Name]
			if !ok {
				continue
			}
			delete(funcs, fn.Name.Name)
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && calleeName(c) == want {
					found = true
				}
				return true
			})
			if !found {
				t.Errorf("%s.%s must call %s before any new money-moving call (security addendum §2)", file, fn.Name.Name, want)
			}
		}
		for left := range funcs {
			t.Errorf("%s: function %s not found (vacuity guard)", file, left)
		}
	}
}

func TestSweeperValidateForLoop(t *testing.T) {
	var nilSweeper *Sweeper
	if nilSweeper.ValidateForLoop() == nil {
		t.Fatal("nil sweeper must be refused")
	}
	s := &Sweeper{Pool: new(db.Pool), Orchestrator: &Orchestrator{}, KYCGate: AllowAllDepositKYCGate{}}
	if err := s.ValidateForLoop(); err == nil || !strings.Contains(err.Error(), "payout KYC gate") {
		t.Fatalf("a sweeper without PayoutKYCGate must be refused, got %v", err)
	}
	s.PayoutKYCGate = KYCEnforcementPayoutGate{}
	if err := s.ValidateForLoop(); err != nil {
		t.Fatalf("complete sweeper refused: %v", err)
	}
}

func TestSweeperLoop_RefusedSweeperNeverRunsAPass(t *testing.T) {
	s := &Sweeper{Pool: new(db.Pool), Orchestrator: &Orchestrator{}, KYCGate: AllowAllDepositKYCGate{}} // no PayoutKYCGate
	ticks := make(chan time.Time)
	called := false
	done := make(chan struct{})
	go func() {
		runSweeperLoop(context.Background(), s, slog.New(slog.NewTextHandler(discardWriter{}, nil)), ticks, func(SweepPassStats) { called = true })
		close(done)
	}()
	<-done // returns immediately: refused at Error level
	if called {
		t.Fatal("a refused sweeper must never run a pass")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func nonTestGoFiles() ([]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	return out, nil
}
