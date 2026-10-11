//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// Positive: the whole two-person flow for a brand, with the readiness acknowledged, one audit row
// per event and the transition history.
func TestFlow_BrandActivation_RequesterPlusOneApprover(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	jid := w.jurisdiction(tid)

	in := w.activation(sub, jid)
	if len(in.AcknowledgedCheckCodes) == 0 {
		t.Fatal("the fixture must have non-passing checks to acknowledge (R-LIC-VALID not_licence_backed, R-ALERTS, ...)")
	}
	res, err := w.svc.Request(ctx, w.req, in)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	r := res.Request
	if r.Status != launchgov.ReqPending || r.RequiredApprovals != 1 || r.FromStatus != "pending_launch" || r.ToStatus != "active" {
		t.Fatalf("request row: %+v", r)
	}
	if r.ReadinessSnapshotHash != res.Readiness.Hash() {
		t.Fatal("the request must bind the hash of the readiness the server computed")
	}
	if got := w.status(sub); got != "pending_launch" {
		t.Fatalf("nothing may be activated by a request: %s", got)
	}

	d, err := w.approve(w.ap1, r)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !d.Executed || d.Request.Status != launchgov.ReqExecuted {
		t.Fatalf("the single required approval must execute: %+v", d)
	}
	if got := w.status(sub); got != "active" {
		t.Fatalf("brand status after the governed activation: %s", got)
	}
	if k := w.transitions(sub); len(k) != 1 || k[0] != "governed:pending_launch>active" {
		t.Fatalf("transition history: %v", k)
	}

	// One audit row per event, platform scope, naming the subject tenant.
	for _, ev := range []string{"requested", "approved", "executed"} {
		rows := w.audit(tid, "launch_authorisation."+ev)
		if len(rows) != 1 || rows[0]["_platform_scope"] != true || rows[0]["_outcome"] != "success" {
			t.Fatalf("audit %s: want exactly one platform-scope success row, got %+v", ev, rows)
		}
	}
	ex := w.audit(tid, "launch_authorisation.executed")[0]
	if ex["before"] != "pending_launch" || ex["after"] != "active" || ex["action"] != "activate" || ex["reason_code"] != "initial_launch" {
		t.Fatalf("executed row: %+v", ex)
	}
	// No free text is copied into the audit trail.
	for _, rows := range [][]map[string]any{w.audit(tid, "launch_authorisation.requested"), w.audit(tid, "launch_authorisation.executed")} {
		for k, v := range rows[0] {
			if s, ok := v.(string); ok && strings.Contains(s, "first quarter") {
				t.Fatalf("conditions_note leaked into audit key %q", k)
			}
		}
	}
	// The requester cannot cancel an executed request; the history is append-only.
	if _, err := w.svc.Cancel(ctx, w.req, sub, r.ID, "requester_withdrew"); !errors.Is(err, launchgov.ErrNotPending) {
		t.Fatalf("cancel after execution: %v", err)
	}
}

// A tenant activation needs two distinct approvers; one approval leaves it pending.
func TestFlow_TenantActivation_NeedsTwoApprovers_ThenSuspendReactivateClose(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.pendingTenant()
	sub := launchgov.Subject{TenantID: tid}
	jid := w.jurisdiction(tid)

	r := w.mustRequest(w.req, w.activation(sub, jid))
	if r.RequiredApprovals != 2 {
		t.Fatalf("a tenant activation needs 2 approvals, got %d", r.RequiredApprovals)
	}
	d1, err := w.approve(w.ap1, r)
	if err != nil || d1.Executed || d1.Request.Status != launchgov.ReqPending {
		t.Fatalf("first approval must leave the request pending: %+v err=%v", d1, err)
	}
	if got := w.status(sub); got != "pending_launch" {
		t.Fatalf("status after one approval: %s", got)
	}
	d2, err := w.approve(w.ap2, r)
	if err != nil || !d2.Executed {
		t.Fatalf("second approval must execute: %+v err=%v", d2, err)
	}
	if got := w.status(sub); got != "active" {
		t.Fatalf("tenant status: %s", got)
	}
	if rows := w.audit(tid, "launch_authorisation.approved"); len(rows) != 2 {
		t.Fatalf("two approved rows expected, got %d", len(rows))
	}
	ex := w.audit(tid, "launch_authorisation.executed")
	if len(ex) != 1 || len(ex[0]["approver_ids"].([]any)) != 2 {
		t.Fatalf("executed row must name both approvers: %+v", ex)
	}

	// Immediate suspension by one authorised person; a reason code is mandatory.
	if _, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: ""}); !errors.Is(err, launchgov.ErrInvalidInput) {
		t.Fatalf("a suspension without a reason code must be refused: %v", err)
	}
	sres, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"})
	if err != nil || !sres.Executed || sres.Request.RequiredApprovals != 0 {
		t.Fatalf("suspend: %+v err=%v", sres, err)
	}
	if got := w.status(sub); got != "suspended" {
		t.Fatalf("tenant status after suspension: %s", got)
	}
	if rows := w.audit(tid, "launch_authorisation.suspended"); len(rows) != 1 || rows[0]["before"] != "active" || rows[0]["after"] != "suspended" {
		t.Fatalf("suspended audit: %+v", rows)
	}

	// Reactivation is always four-eyes: the suspender cannot reactivate alone.
	re := w.activation(sub, jid)
	re.Action, re.ReasonCode = launchgov.ActionReactivate, "issue_resolved"
	re.AcknowledgedCheckCodes = w.nonPassing(re)
	rr := w.mustRequest(w.req, re)
	if rr.RequiredApprovals != 2 {
		t.Fatalf("tenant reactivation needs 2 approvals: %d", rr.RequiredApprovals)
	}
	if _, err := w.approve(w.req, rr); !errors.Is(err, launchgov.ErrSelfApproval) {
		t.Fatalf("the requester cannot approve: %v", err)
	}
	if _, err := w.approve(w.ap1, rr); err != nil {
		t.Fatal(err)
	}
	if d, err := w.approve(w.ap2, rr); err != nil || !d.Executed {
		t.Fatalf("reactivation: %+v err=%v", d, err)
	}
	if got := w.status(sub); got != "active" {
		t.Fatalf("tenant status after reactivation: %s", got)
	}

	// Closure: two approvers, R-PLAYER-FUNDS acknowledged (here it passes: no funds), terminal.
	cin := launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
	cr := w.mustRequest(w.req, cin)
	if cr.RequiredApprovals != 2 {
		t.Fatalf("closure needs 2 approvals: %d", cr.RequiredApprovals)
	}
	if _, err := w.approve(w.ap1, cr); err != nil {
		t.Fatal(err)
	}
	if d, err := w.approve(w.ap2, cr); err != nil || !d.Executed {
		t.Fatalf("close: %+v err=%v", d, err)
	}
	if got := w.status(sub); got != "closed" {
		t.Fatalf("tenant status after closure: %s", got)
	}
	// Closed is terminal: any further request is refused as such.
	if _, err := w.svc.Request(ctx, w.req, cin); !errors.Is(err, launchgov.ErrSubjectClosed) {
		t.Fatalf("a request on a closed subject: %v", err)
	}
	if _, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"}); !errors.Is(err, launchgov.ErrSubjectClosed) {
		t.Fatalf("a suspension of a closed subject: %v", err)
	}
}
