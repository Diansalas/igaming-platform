package reconciliation

// ADR 0113 slice 3 static wiring pins for check D (pure go/ast, like the alerting static wiring
// guards). They pin that D is reached by the hourly scheduler for EVERY tenant, raises through the
// in-tx helper, and reads in ONE statement (C7):
//   1. RunLedgerVsProjection calls withdrawalHoldFindings and returns its error (fail closed);
//   2. TryRunLedgerVsProjectionForTenant -> RunLedgerVsProjection, runLedgerStreamForTenant ->
//      TryRunLedgerVsProjectionForTenant inside an alerting.InTx closure, and raiseLedgerRunAlerts is
//      called there;
//   3. runLedgerStreamForTenant is called by BOTH sweepTenants (active tenants) and
//      observeNonActiveTenants (suspended / closed tenants), and RunSchedulerLoop runs RunSweep;
//   4. raiseLedgerRunAlerts raises discLedgerWithdrawalHold;
//   5. withdrawalHoldFindings issues exactly ONE database call (tx.Query of withdrawalHoldCheckSQL),
//      and rebuildAndProject exactly one (the ledger_vs_projection per-account compare).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func dFuncs(t *testing.T, files ...string) map[string]*ast.FuncDecl {
	t.Helper()
	out := map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, filepath.Clean(f), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				out[fd.Name.Name] = fd
			}
		}
	}
	return out
}

// dCalls returns the callee names inside n ("pkg.Fn", ".Method" for a selector on a non-ident, "Fn").
func dCalls(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(x ast.Node) bool {
		c, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := c.Fun.(type) {
		case *ast.Ident:
			out = append(out, f.Name)
		case *ast.SelectorExpr:
			if id, ok := f.X.(*ast.Ident); ok {
				out = append(out, id.Name+"."+f.Sel.Name)
			} else {
				out = append(out, "."+f.Sel.Name)
			}
		}
		return true
	})
	return out
}

func dHas(calls []string, name string) bool {
	for _, c := range calls {
		if c == name {
			return true
		}
	}
	return false
}

func TestADR0113D_Static_WiredIntoTheHourlyLedgerStream(t *testing.T) {
	fns := dFuncs(t, "reconciliation.go", "scheduler.go", "alerts.go", "withdrawal_hold.go")
	need := func(fn, callee string) {
		t.Helper()
		d, ok := fns[fn]
		if !ok {
			t.Fatalf("function %s not found", fn)
		}
		if !dHas(dCalls(d), callee) {
			t.Fatalf("%s must call %s (ADR 0113 D wiring)", fn, callee)
		}
	}
	need("RunLedgerVsProjection", "withdrawalHoldFindings")
	need("TryRunLedgerVsProjectionForTenant", "RunLedgerVsProjection")
	need("runLedgerStreamForTenant", "TryRunLedgerVsProjectionForTenant")
	need("runLedgerStreamForTenant", "alerting.InTx")
	need("runLedgerStreamForTenant", "raiseLedgerRunAlerts")
	need("sweepTenants", "runLedgerStreamForTenant")
	need("observeNonActiveTenants", "runLedgerStreamForTenant")
	// A11: RunSweep / RunSweepScoped share one body (runSweepAll); the wiring is pinned on that body and the two
	// entry points are pinned to reach it, so the ledger stream is still reachable from both loops.
	need("runSweepAll", "sweepTenants")
	need("runSweepAll", "observeNonActiveTenants")
	need("RunSweep", "runSweepAll")
	need("RunSweepScoped", "runSweepAll")
	need("RunSchedulerLoop", "RunSweep")
	need("RunSchedulerLoopScoped", "RunSweepScoped")
	need("RunSchedulerLoop", "runSchedulerLoop")
	need("RunSchedulerLoopScoped", "runSchedulerLoop")

	// The D raise uses its own discriminator.
	found := false
	ast.Inspect(fns["raiseLedgerRunAlerts"], func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && id.Name == "discLedgerWithdrawalHold" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("raiseLedgerRunAlerts must raise discLedgerWithdrawalHold")
	}

	// Fail closed: RunLedgerVsProjection returns withdrawalHoldFindings' error (an `if err != nil {
	// return ... err }` right after the call), it never swallows it.
	var assigned, returned bool
	ast.Inspect(fns["RunLedgerVsProjection"], func(x ast.Node) bool {
		switch s := x.(type) {
		case *ast.AssignStmt:
			for _, r := range s.Rhs {
				if c, ok := r.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "withdrawalHoldFindings" {
						assigned = true
					}
				}
			}
		case *ast.IfStmt:
			if assigned && !returned {
				if len(s.Body.List) == 1 {
					if r, ok := s.Body.List[0].(*ast.ReturnStmt); ok && len(r.Results) == 3 {
						if id, ok := r.Results[2].(*ast.Ident); ok && id.Name == "err" {
							returned = true
						}
					}
				}
			}
		}
		return true
	})
	if !assigned || !returned {
		t.Fatal("RunLedgerVsProjection must return withdrawalHoldFindings' error (fail closed)")
	}
}

func TestADR0113D_Static_OneStatementPerConsistentRead(t *testing.T) {
	fns := dFuncs(t, "reconciliation.go", "withdrawal_hold.go")
	for fn, want := range map[string]string{"withdrawalHoldFindings": "tx.Query", "rebuildAndProject": "tx.QueryRow"} {
		n := 0
		for _, c := range dCalls(fns[fn]) {
			switch c {
			case "tx.Query", "tx.QueryRow", "tx.Exec", "tx.SendBatch", "tx.CopyFrom":
				n++
				if c != want {
					t.Fatalf("%s must read through %s only, found %s", fn, want, c)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%s must issue exactly ONE database statement (C7: one snapshot), found %d", fn, n)
		}
	}
}
