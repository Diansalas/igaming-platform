//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

const raceRounds = 50

type outcome struct {
	res *launchgov.DecisionResult
	err error
}

func countAudit(w *world, tid uuid.UUID, action, requestID string) int {
	n := 0
	for _, r := range w.audit(tid, action) {
		if r["request_id"] == requestID {
			n++
		}
	}
	return n
}

// Two final approvals racing: exactly one executes, the other is refused as not pending; one
// governed transition, one executed audit row. x50 (the request row FOR UPDATE serialises them).
func TestConcurrency_TwoFinalApprovals_OneExecution_x50(t *testing.T) {
	w := newWorld(t)
	extra := w.extraPlatformStaff()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	plat := w.licence(tid, jid, "platform", "active")
	ta := w.tenantStaff(tid, auth.RoleTenantAdmin)

	for i := 0; i < raceRounds; i++ {
		brand := w.pendingBrand(tid)
		sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
		// A tenant-requested platform_licence launch needs 2 approvals (S14).
		in := w.activation(sub, jid)
		in.Licensing = &launchgov.LicensingRecord{Model: launchgov.LicModelPlatform, Status: launchgov.LicStatusInForce, LicenceID: &plat}
		in.AcknowledgedCheckCodes = w.nonPassing(in)
		res, err := w.svc.Request(context.Background(), ta, in)
		if err != nil {
			t.Fatalf("round %d request: %v", i, err)
		}
		r := res.Request
		if r.RequiredApprovals != 2 {
			t.Fatalf("round %d: required %d", i, r.RequiredApprovals)
		}
		if _, err := w.approve(w.ap1, r); err != nil {
			t.Fatalf("round %d first approval: %v", i, err)
		}
		// Two more approvers race for the final approval.
		var wg sync.WaitGroup
		outs := make([]outcome, 2)
		seen := w.seen(r)
		start := make(chan struct{})
		for k, who := range []launchgov.Actor{w.ap2, extra} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				d, err := w.svc.Decide(context.Background(), who, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: seen})
				outs[k] = outcome{d, err}
			}()
		}
		close(start)
		wg.Wait()
		executed, notPending := 0, 0
		for _, o := range outs {
			switch {
			case o.err == nil && o.res.Executed:
				executed++
			case errors.Is(o.err, launchgov.ErrNotPending):
				notPending++
			default:
				t.Fatalf("round %d: unexpected outcome %+v err=%v", i, o.res, o.err)
			}
		}
		if executed != 1 || notPending != 1 {
			t.Fatalf("round %d: want exactly one execution and one refusal, got executed=%d notPending=%d", i, executed, notPending)
		}
		if got := w.status(sub); got != "active" {
			t.Fatalf("round %d: status %s", i, got)
		}
		if k := w.transitions(sub); len(k) != 1 {
			t.Fatalf("round %d: transitions %v", i, k)
		}
		if n := countAudit(w, tid, "launch_authorisation.executed", r.ID.String()); n != 1 {
			t.Fatalf("round %d: %d executed audit rows", i, n)
		}
	}
}

// A suspension racing the final approval of a closure on the same subject: the suspension executes
// and supersedes the pending closure (its approval is then refused), or the closure executes first
// and the suspension is refused as illegal; never a deadlock, never a stale pending request.
func TestConcurrency_SuspensionRacesFinalApproval_x50(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	suspendedWins, closedWins := 0, 0
	for i := 0; i < raceRounds; i++ {
		brand := w.activeBrand(tid)
		sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
		in := launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
		r := w.mustRequest(w.req, in)
		if _, err := w.approve(w.ap1, r); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		seen := w.seen(r)
		var wg sync.WaitGroup
		var dres outcome
		var sres *launchgov.RequestResult
		var serr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 { // vary which side reaches the locks first: even rounds delay the approval, odd rounds the suspension
				time.Sleep(time.Duration(i%10) * 700 * time.Microsecond)
			}
			d, err := w.svc.Decide(context.Background(), w.ap2, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: seen})
			dres = outcome{d, err}
		}()
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 1 {
				time.Sleep(time.Duration(i%10) * 700 * time.Microsecond)
			}
			sres, serr = w.svc.Suspend(context.Background(), w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"})
		}()
		close(start)
		wg.Wait()

		final := w.status(sub)
		got, _, _ := w.svc.GetRequest(context.Background(), w.ap1, tid, r.ID)
		switch final {
		case "suspended":
			suspendedWins++
			if serr != nil || sres == nil || !sres.Executed {
				t.Fatalf("round %d: suspended but the suspension reported %+v %v", i, sres, serr)
			}
			if got.Status != launchgov.ReqSuperseded {
				t.Fatalf("round %d: the pending closure must be superseded, is %s", i, got.Status)
			}
			if !errors.Is(dres.err, launchgov.ErrNotPending) {
				t.Fatalf("round %d: the closure's final approval must be refused, got %+v %v", i, dres.res, dres.err)
			}
		case "closed":
			closedWins++
			if dres.err != nil || !dres.res.Executed || got.Status != launchgov.ReqExecuted {
				t.Fatalf("round %d: closed but the approval reported %+v %v (%s)", i, dres.res, dres.err, got.Status)
			}
			if !errors.Is(serr, launchgov.ErrIllegalTransition) && !errors.Is(serr, launchgov.ErrSubjectClosed) {
				t.Fatalf("round %d: a suspension after the closure must be refused as illegal, got %v", i, serr)
			}
		default:
			t.Fatalf("round %d: unexpected final status %s (decide=%v suspend=%v)", i, final, dres.err, serr)
		}
		pend, err := w.svc.ListRequests(context.Background(), w.ap1, tid, launchgov.ListFilter{BrandID: &brand, Statuses: []string{launchgov.ReqPending, launchgov.ReqExecuting}})
		if err != nil || len(pend) != 0 {
			t.Fatalf("round %d: stale pending/executing requests: %v err=%v", i, pend, err)
		}
	}
	t.Logf("races: suspension first %d, closure first %d", suspendedWins, closedWins)
	// Both orderings must have been exercised, or the test proves only one of the two paths.
	if suspendedWins == 0 || closedWins == 0 {
		t.Fatalf("the race must exercise both outcomes: suspended %d, closed %d", suspendedWins, closedWins)
	}
}

// Two simultaneous suspensions of the same brand: exactly one executes.
func TestConcurrency_TwoSuspensions_OneWins_x20(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	ta := w.tenantStaff(tid, auth.RoleTenantAdmin)
	for i := 0; i < 20; i++ {
		brand := w.activeBrand(tid)
		sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for k, who := range []launchgov.Actor{w.req, ta} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[k] = w.svc.Suspend(context.Background(), who, launchgov.SuspendInput{Subject: sub, ReasonCode: "operator_request"})
			}()
		}
		close(start)
		wg.Wait()
		ok, illegal := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case errors.Is(e, launchgov.ErrIllegalTransition) || errors.Is(e, launchgov.ErrSubjectClosed):
				illegal++
			default:
				t.Fatalf("round %d: %v", i, e)
			}
		}
		if ok != 1 || illegal != 1 || w.status(sub) != "suspended" {
			t.Fatalf("round %d: ok=%d illegal=%d status=%s", i, ok, illegal, w.status(sub))
		}
	}
}

// Many requests for one subject at once: the one-pending-per-subject UNIQUE admits exactly one.
func TestConcurrency_ManyRequests_OnePending_x20(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	for i := 0; i < 20; i++ {
		brand := w.activeBrand(tid)
		sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
		var wg sync.WaitGroup
		errs := make([]error, 4)
		start := make(chan struct{})
		for k := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[k] = w.svc.Request(context.Background(), w.req, launchgov.RequestInput{
					Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding", ConditionsNote: string(rune('a' + k)),
				})
			}()
		}
		close(start)
		wg.Wait()
		ok, pending := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case errors.Is(e, launchgov.ErrRequestPending):
				pending++
			default:
				t.Fatalf("round %d: %v", i, e)
			}
		}
		if ok != 1 || pending != 3 {
			t.Fatalf("round %d: ok=%d pending=%d", i, ok, pending)
		}
		var n int
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM launch_authorisation_requests WHERE brand_id = $1 AND status = 'pending'`, brand).Scan(&n)
		}); err != nil || n != 1 {
			t.Fatalf("round %d: %d pending rows (err=%v)", i, n, err)
		}
	}
}
