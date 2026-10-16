package reconciliation

import (
	"go/ast"
	"testing"
)

// identsIn returns every identifier name under n.
func identsIn(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok {
			out = append(out, id.Name)
		}
		return true
	})
	return out
}

// Static pins for the two reconciliation failure alerts (pure go/ast, like adr0113_check_d_static_test.go):
//
//   - RECON-RUN-FAILED-ALERT-1: every stream function that can fail for a tenant raises reconciliation.run_failed
//     through raiseRunFailed (the registry row said "not wired"; it has been wired since I-wire, and this pins it);
//   - R3-LISTING-FAILURE-LOG-ONLY-1: the scheduler loop turns a sweep failure into a durable alert
//     (handleSweepError) and recovers a panic into one (recoverSweepPanic), and RunSweep / RunSweepTenants wrap the
//     listing failures in the sentinels the phase mapping needs.
func TestStatic_EveryStreamRaisesRunFailed(t *testing.T) {
	fns := dFuncs(t, "scheduler.go", "alerts.go", "sweep_alert.go")
	for _, fn := range []string{
		"runLedgerStreamForTenant", "runSportsbookStreamForTenant", "runCasinoStreamForTenant",
		"runCasinoStatementStreamForTenant", "ReconcilePaymentStatementForTenant",
	} {
		d, ok := fns[fn]
		if !ok {
			t.Fatalf("function %s not found", fn)
		}
		if !dHas(dCalls(d), "raiseRunFailed") {
			t.Errorf("%s no longer calls raiseRunFailed: a failed tenant run would be log-only again", fn)
		}
	}
	// raiseRunFailed itself raises the registered Kind through RaiseDetached.
	d := fns["raiseRunFailed"]
	if d == nil || !dHas(dCalls(d), "alerting.RaiseDetached") {
		t.Fatal("raiseRunFailed must call alerting.RaiseDetached")
	}
}

func TestStatic_SchedulerLoopMakesSweepFailuresDurable(t *testing.T) {
	fns := dFuncs(t, "scheduler.go", "sweep_alert.go")
	// A11: both public loops (legacy RunSchedulerLoop and the production RunSchedulerLoopScoped) share one body,
	// runSchedulerLoop, which owns the durable-failure handling; each public loop must reach its sweep and that body.
	loop := fns["runSchedulerLoop"]
	if loop == nil {
		t.Fatal("runSchedulerLoop not found")
	}
	calls := dCalls(loop)
	for _, must := range []string{"handleSweepError", "recoverSweepPanic"} {
		if !dHas(calls, must) {
			t.Errorf("runSchedulerLoop must call %s", must)
		}
	}
	for fn, sweep := range map[string]string{"RunSchedulerLoop": "RunSweep", "RunSchedulerLoopScoped": "RunSweepScoped"} {
		d := fns[fn]
		if d == nil {
			t.Fatalf("%s not found", fn)
		}
		for _, must := range []string{sweep, "runSchedulerLoop"} {
			if !dHas(dCalls(d), must) {
				t.Errorf("%s must call %s", fn, must)
			}
		}
	}
	if h := fns["handleSweepError"]; h == nil || !dHas(dCalls(h), "alertSweepFailure") {
		t.Error("handleSweepError must raise through alertSweepFailure")
	}
	if h := fns["recoverSweepPanic"]; h == nil || !dHas(dCalls(h), "alertSweepFailure") {
		t.Error("recoverSweepPanic must raise through alertSweepFailure")
	}
	// The sentinel wrapping that selects the alert phase.
	for _, fn := range []string{"runSweepAll", "runSweepNamed"} {
		d := fns[fn]
		if d == nil {
			t.Fatalf("%s not found", fn)
		}
		found := map[string]bool{}
		for _, id := range identsIn(d) {
			found[id] = true
		}
		if !found["ErrListActiveTenants"] || !found["ErrListNonActiveTenants"] {
			t.Errorf("%s must wrap both listing sentinels (the alert phase depends on them)", fn)
		}
	}
	// ... and every public entry point reaches those bodies.
	for fn, body := range map[string]string{"RunSweep": "runSweepAll", "RunSweepScoped": "runSweepAll", "RunSweepTenants": "runSweepNamed", "RunSweepTenantsScoped": "runSweepNamed"} {
		d := fns[fn]
		if d == nil || !dHas(dCalls(d), body) {
			t.Errorf("%s must call %s (the listing-failure alert phases live there)", fn, body)
		}
	}
}
