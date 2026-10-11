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

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// rawSuspend runs a complete governed suspension in the CURRENT platform transaction as the
// session actor, with real proofs (prooftest signs for whoever the session names), and optionally
// WITHOUT superseding the other pending requests of the subject.
func rawSuspend(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brand *uuid.UUID) error {
	s := newSpec(tenantID, brand, "suspend")
	if err := prooftest.AttachForSession(ctx, tx, actorproof.OpLaunchSuspend, actorproof.TargetNew, s.digest()); err != nil {
		return err
	}
	id, _, err := s.insert(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
		VALUES ($1, $2, $3, 'governed', 'active', 'suspended', $4)`, tenantID, s.kind(), brand, id); err != nil {
		return err
	}
	if brand == nil {
		if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, tenantID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE brands SET status = 'suspended' WHERE id = $1`, *brand); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, id)
	return err
}

// Tenant staff may request but never approve; they may suspend their OWN brand, which supersedes the
// platform's pending requests for that subject and records the superseded ids in the audit trail.
func TestTenantStaff_RequestSuspendSupersede_NeverApprove_NoLoneReactivation(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	ta := w.tenantStaff(tid, auth.RoleTenantAdmin)
	co := w.tenantStaff(tid, auth.RoleCompliance)

	// A platform-filed pending closure of the brand.
	closeReq := w.mustRequest(w.req, launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding", AcknowledgedCheckCodes: nil})
	if closeReq.Status != launchgov.ReqPending {
		t.Fatalf("pending close: %+v", closeReq)
	}
	// The tenant sees the platform request (RLS), cannot approve it, and cannot execute anything.
	if _, _, err := w.svc.GetRequest(ctx, ta, tid, closeReq.ID); err != nil {
		t.Fatalf("a tenant reads its own tenant's requests: %v", err)
	}
	for _, who := range []launchgov.Actor{ta, co} {
		if _, err := w.svc.Decide(ctx, who, tid, closeReq.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: w.seen(closeReq)}); !errors.Is(err, launchgov.ErrForbidden) {
			t.Fatalf("tenant approval: %v", err)
		}
	}
	// Raw SQL: a tenant principal driving the execution of the platform's request is refused (S8).
	err := w.rt.WithPrincipalScope(ctx, tid, ta.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, closeReq.ID)
		return err
	})
	w.wantCode("a tenant session executing a platform request", err, "LA011")

	// Own-brand suspension by tenant_admin: allowed, immediate, audited at tenant scope.
	res, err := w.svc.Suspend(ctx, ta, launchgov.SuspendInput{Subject: sub, ReasonCode: "operator_request"})
	if err != nil || !res.Executed {
		t.Fatalf("tenant own-brand suspension: %+v err=%v", res, err)
	}
	if got := w.status(sub); got != "suspended" {
		t.Fatalf("brand status %s", got)
	}
	if len(res.Superseded) != 1 || res.Superseded[0] != closeReq.ID {
		t.Fatalf("the suspension must supersede the platform's pending closure: %v", res.Superseded)
	}
	got, _, err := w.svc.GetRequest(ctx, w.ap1, tid, closeReq.ID)
	if err != nil || got.Status != launchgov.ReqSuperseded {
		t.Fatalf("the pending closure must be superseded: %+v err=%v", got, err)
	}
	// Not a silent disappearance: the superseded request id, the superseding request and the actor
	// are in the audit trail (tenant scope, since a tenant session wrote them).
	sup := w.audit(tid, "launch_authorisation.superseded")
	if len(sup) != 1 || sup[0]["request_id"] != closeReq.ID.String() || sup[0]["superseded_by_request_id"] != res.Request.ID.String() ||
		sup[0]["superseded_by"] != ta.StaffID.String() || sup[0]["_platform_scope"] != false {
		t.Fatalf("superseded audit row: %+v", sup)
	}
	susp := w.audit(tid, "launch_authorisation.suspended")
	ids, _ := susp[0]["superseded_request_ids"].([]any)
	if len(susp) != 1 || len(ids) != 1 || ids[0] != closeReq.ID.String() {
		t.Fatalf("suspended audit row must list the superseded ids: %+v", susp)
	}
	// Nothing stale remains pending (C-3(b) at the service level).
	pend, err := w.svc.ListRequests(ctx, w.ap1, tid, launchgov.ListFilter{BrandID: &brand, Statuses: []string{launchgov.ReqPending}})
	if err != nil || len(pend) != 0 {
		t.Fatalf("stale pending requests after a suspension: %v err=%v", pend, err)
	}
	// The superseded request takes no more approval.
	if _, err := w.approve(w.ap1, closeReq); !errors.Is(err, launchgov.ErrNotPending) {
		t.Fatalf("approving a superseded request: %v", err)
	}

	// Compliance may request and suspend too (ADR 0112 6.1); a tenant cannot reactivate alone: it may
	// REQUEST a reactivation, but only platform approvers can execute it.
	re := launchgov.RequestInput{
		Subject: sub, Action: launchgov.ActionReactivate, ReasonCode: "issue_resolved",
		Licensing:               &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional, DeterminationReference: "ext-1", DeterminationByRole: "external counsel"},
		ResponsibleOperatorName: "Operator Ltd", JurisdictionIDs: []uuid.UUID{jid},
	}
	re.AcknowledgedCheckCodes = w.nonPassing(re)
	rr, err := w.svc.Request(ctx, co, re)
	if err != nil {
		t.Fatalf("compliance requesting a reactivation: %v", err)
	}
	if rr.Request.RequestedByScope != launchgov.ScopeTenant || rr.Request.RequiredApprovals != 1 {
		t.Fatalf("reactivation request: %+v", rr.Request)
	}
	if got := w.status(sub); got != "suspended" {
		t.Fatalf("a request must not reactivate: %s", got)
	}
	// Only the requester cancels: the tenant admin cannot cancel the compliance officer's request.
	if _, err := w.svc.Cancel(ctx, ta, sub, rr.Request.ID, "requester_withdrew"); !errors.Is(err, launchgov.ErrForbidden) {
		t.Fatalf("a non-requester cancelling: %v", err)
	}
	if d, err := w.approve(w.ap1, rr.Request); err != nil || !d.Executed {
		t.Fatalf("a platform approver executes the tenant's reactivation request: %+v err=%v", d, err)
	}
	if got := w.status(sub); got != "active" {
		t.Fatalf("brand status after the platform-approved reactivation: %s", got)
	}
	// Tenant staff suspend again via the compliance role; and the tenant cannot touch the tenant row.
	if _, err := w.svc.Suspend(ctx, co, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"}); err != nil {
		t.Fatalf("compliance suspending its own brand: %v", err)
	}
	if _, err := w.svc.Suspend(ctx, ta, launchgov.SuspendInput{Subject: launchgov.Subject{TenantID: tid}, ReasonCode: "operator_request"}); !errors.Is(err, launchgov.ErrForbidden) {
		t.Fatalf("a tenant session suspending its tenant: %v", err)
	}
	// A role without the permission is refused.
	fin := launchgov.Actor{StaffID: ta.StaffID, Scope: launchgov.ScopeTenant, TenantID: tid, Role: auth.RoleFinance}
	if _, err := w.svc.Suspend(ctx, fin, launchgov.SuspendInput{Subject: sub, ReasonCode: "operator_request"}); !errors.Is(err, launchgov.ErrForbidden) {
		t.Fatalf("finance suspending: %v", err)
	}
}

// C-3(b): the DATABASE fails the COMMIT of a suspension that left a stale pending request behind.
func TestSuspension_StalePendingRequest_FailsTheCommit_LA030(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	closeReq := w.mustRequest(w.req, launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding"})

	err := w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error { return rawSuspend(ctx, tx, tid, &brand) })
	w.wantCode("a suspension that leaves a pending closure behind", err, "LA030")
	if got := w.status(sub); got != "active" {
		t.Fatalf("a refused commit changed the brand: %s", got)
	}
	// Through the service the same situation commits, with the closure superseded.
	res, err := w.svc.Suspend(context.Background(), w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"})
	if err != nil || len(res.Superseded) != 1 || res.Superseded[0] != closeReq.ID {
		t.Fatalf("the executor supersedes the stale request: %+v err=%v", res, err)
	}
	// A pending request created AFTER the suspension is not "stale" (it is legal against the new status).
	other := w.activeBrand(tid)
	osub := launchgov.Subject{TenantID: tid, BrandID: &other}
	pend := w.mustRequest(w.req, launchgov.RequestInput{Subject: osub, Action: launchgov.ActionClose, ReasonCode: "offboarding"})
	if err := w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return rawSuspendOther(ctx, tx, tid, &other, pend.ID)
	}); err != nil {
		t.Fatalf("a suspension that supersedes the pending request commits: %v", err)
	}
}

// rawSuspendOther is rawSuspend plus the supersede of one named pending request (what the executor does).
func rawSuspendOther(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brand *uuid.UUID, pending uuid.UUID) error {
	if err := rawSuspend(ctx, tx, tenantID, brand); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'superseded' WHERE id = $1`, pending)
	return err
}

// Security C2: one subject per executing transaction. A tenant status change after a brand status
// change of that tenant in the SAME transaction is refused (brand key then tenant key inverts the
// placement lock order); the reverse order and different tenants are fine.
func TestExecutor_RefusesBrandThenTenantStatusChangeInOneTransaction(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	err := w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := rawSuspend(ctx, tx, tid, &brand); err != nil {
			return err
		}
		return rawSuspend(ctx, tx, tid, nil)
	})
	w.wantCode("brand then tenant in one transaction", err, "LA020")
	if got := w.status(launchgov.Subject{TenantID: tid}); got != "active" {
		t.Fatalf("tenant status %s", got)
	}
	if got := w.status(launchgov.Subject{TenantID: tid, BrandID: &brand}); got != "active" {
		t.Fatalf("brand status %s (the whole transaction must roll back)", got)
	}
	// The permitted order (tenant X, then its brand: the placement lock order) works.
	t2 := w.activeTenant()
	b2 := w.activeBrand(t2)
	if err := w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := rawSuspend(ctx, tx, t2, nil); err != nil {
			return err
		}
		return rawSuspend(ctx, tx, t2, &b2)
	}); err != nil {
		t.Fatalf("tenant then brand in one transaction: %v", err)
	}
	// Two different tenants' subjects in one transaction are not the C2 hazard.
	t3, t4 := w.activeTenant(), w.activeTenant()
	b3 := w.activeBrand(t3)
	if err := w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := rawSuspend(ctx, tx, t3, &b3); err != nil {
			return err
		}
		return rawSuspend(ctx, tx, t4, nil)
	}); err != nil {
		t.Fatalf("brand of tenant 3 then tenant 4: %v", err)
	}
}

// Reject carry-over (ADR 0112 12.12 (i), the slice-3 policy decision): a recorded reject carries over
// to a later IDENTICAL request (same subject, action and payload hash) within the request TTL, so an
// approver cannot be shopped by re-requesting; any change of content is a different request.
func TestRejectCarryOver_IdenticalRerequestRefused_ChangedContentAccepted(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	in := w.activation(sub, jid)
	r1 := w.mustRequest(w.req, in)

	// The requester cannot reject its own request; a platform approver can, with a closed reason.
	if _, err := w.svc.Decide(ctx, w.req, tid, r1.ID, launchgov.DecideInput{Decision: launchgov.DecisionReject, ReasonCode: "policy_concern"}); !errors.Is(err, launchgov.ErrSelfApproval) {
		t.Fatalf("self-reject: %v", err)
	}
	if _, err := w.svc.Decide(ctx, w.ap1, tid, r1.ID, launchgov.DecideInput{Decision: launchgov.DecisionReject, ReasonCode: "reviewed_ok"}); !errors.Is(err, launchgov.ErrInvalidInput) {
		t.Fatalf("a reject needs a reject reason code: %v", err)
	}
	d, err := w.svc.Decide(ctx, w.ap1, tid, r1.ID, launchgov.DecideInput{Decision: launchgov.DecisionReject, ReasonCode: "readiness_concerns"})
	if err != nil || d.Request.Status != launchgov.ReqRejected {
		t.Fatalf("reject: %+v err=%v", d, err)
	}
	if rows := w.audit(tid, "launch_authorisation.rejected"); len(rows) != 1 || rows[0]["decision_reason_code"] != "readiness_concerns" {
		t.Fatalf("rejected audit: %+v", rows)
	}
	// A rejected request cannot be approved by someone else afterwards.
	if _, err := w.approve(w.ap2, r1); !errors.Is(err, launchgov.ErrNotPending) {
		t.Fatalf("approving a rejected request: %v", err)
	}

	// Identical re-request: by the same requester AND by another platform admin (approver shopping).
	var pr *launchgov.PriorRejectError
	_, err = w.svc.Request(ctx, w.req, in)
	if !errors.Is(err, launchgov.ErrPriorReject) || !errors.As(err, &pr) || pr.RequestID != r1.ID || !pr.RetryAfter.After(pr.DecidedAt) {
		t.Fatalf("identical re-request must carry the reject over: %v", err)
	}
	if got := pr.RetryAfter.Sub(pr.DecidedAt); got != launchgov.RequestTTL {
		t.Fatalf("the carry-over window is the request TTL, got %s", got)
	}
	if _, err = w.svc.Request(ctx, w.ap2, in); !errors.Is(err, launchgov.ErrPriorReject) {
		t.Fatalf("identical re-request by another platform admin: %v", err)
	}
	if rows := w.audit(tid, "launch_authorisation.request_denied"); len(rows) < 2 || rows[0]["code"] != "LAUNCH_PRIOR_REJECT" || rows[0]["prior_request_id"] != r1.ID.String() {
		t.Fatalf("the carried-over refusal must be audited with the prior request id: %+v", rows)
	}
	// A CHANGED request (different note) is a different payload and is accepted; so is another action.
	changed := in
	changed.ConditionsNote = "reviewed again with the external counsel's memo"
	if _, err := w.svc.Request(ctx, w.req, changed); err != nil {
		t.Fatalf("changed content must be accepted: %v", err)
	}
	// A reject of a request of ANOTHER brand does not carry over to this one.
	b2 := w.pendingBrand(tid)
	in2 := w.activation(launchgov.Subject{TenantID: tid, BrandID: &b2}, jid)
	if _, err := w.svc.Request(ctx, w.req, in2); err != nil {
		t.Fatalf("a reject on brand 1 must not block brand 2: %v", err)
	}
}

// A suspension waits for in-flight gameplay lock holders (the brand status gate key, held SHARED by a
// new-stake placement): the executor sets lock_timeout and retries a bounded number of times.
func TestSuspend_LockTimeout_RetriesBounded_AndGameplayGateRace(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	fast := w.service(launchgov.Config{LockTimeout: "100ms", SuspendAttempts: 6, Backoff: func(n int) time.Duration { return 40 * time.Millisecond }})
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}

	hold := func(d time.Duration) (started <-chan struct{}, done <-chan error) {
		st, dn := make(chan struct{}), make(chan error, 1)
		go func() {
			dn <- w.rt.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
				// A new-stake placement: the shared brand key, then the gate read, held to commit.
				if err := tenant.RequireBrandActiveForGameplay(ctx, tx, tid, brand); err != nil {
					return err
				}
				close(st)
				time.Sleep(d)
				return nil
			})
		}()
		return st, dn
	}

	// (1) The placement finishes inside the retry budget: the suspension waits, retries, succeeds.
	started, done := hold(450 * time.Millisecond)
	<-started
	t0 := time.Now()
	res, err := fast.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "operator_request"})
	if err != nil || !res.Executed {
		t.Fatalf("a suspension must wait out an in-flight placement and retry: %+v err=%v", res, err)
	}
	if el := time.Since(t0); el < 300*time.Millisecond {
		t.Fatalf("the suspension must have WAITED for the placement (took %s)", el)
	}
	if err := <-done; err != nil {
		t.Fatalf("the placement: %v", err)
	}
	if rows := w.audit(tid, "launch_authorisation.suspend_denied"); len(rows) != 0 {
		t.Fatalf("a suspension that succeeded after retries writes no denied row: %+v", rows)
	}
	// After the suspension a NEW placement is refused by the gate (race freedom).
	err = w.rt.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		return tenant.RequireBrandActiveForGameplay(ctx, tx, tid, brand)
	})
	if !errors.Is(err, tenant.ErrBrandNotActiveForGameplay) {
		t.Fatalf("a placement after the suspension must be refused: %v", err)
	}

	// (2) The holder outlasts the bounded retries: a retryable error, ONE denied audit row with the
	// attempt count, the status unchanged and no request row left behind.
	b2 := w.activeBrand(tid)
	sub2 := launchgov.Subject{TenantID: tid, BrandID: &b2}
	few := w.service(launchgov.Config{LockTimeout: "80ms", SuspendAttempts: 2, Backoff: func(n int) time.Duration { return 20 * time.Millisecond }})
	started2 := make(chan struct{})
	holdDone := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		holdDone <- w.rt.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			if err := tenant.RequireBrandActiveForGameplay(ctx, tx, tid, b2); err != nil {
				return err
			}
			close(started2)
			time.Sleep(900 * time.Millisecond)
			return nil
		})
	}()
	<-started2
	_, err = few.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub2, ReasonCode: "operator_request"})
	if !errors.Is(err, launchgov.ErrRetryableLockTimeout) {
		t.Fatalf("an exhausted retry budget must be a retryable lock timeout: %v", err)
	}
	wg.Wait()
	if err := <-holdDone; err != nil {
		t.Fatal(err)
	}
	if got := w.status(sub2); got != "active" {
		t.Fatalf("a timed-out suspension changed the status: %s", got)
	}
	if n := w.countRequests(sub2); n != 0 {
		t.Fatalf("a rolled-back suspension must leave no request row: %d", n)
	}
	rows := w.audit(tid, "launch_authorisation.suspend_denied")
	if len(rows) != 1 || rows[0]["code"] != "LAUNCH_RETRYABLE_LOCK_TIMEOUT" || rows[0]["attempts"] != float64(2) {
		t.Fatalf("exactly one denied audit row with the attempts: %+v", rows)
	}
	// Retrying after the holder finished succeeds.
	if res, err := few.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub2, ReasonCode: "operator_request"}); err != nil || !res.Executed {
		t.Fatalf("retry after release: %+v err=%v", res, err)
	}
}

// An approval-based execution that times out on the brand gate key rolls back as a unit: the final
// approval is NOT recorded, the request stays pending, and a retry executes.
func TestDecide_LockTimeout_RollsBackTheFinalApproval_RequestStaysPending(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.svc = w.service(launchgov.Config{LockTimeout: "100ms"})
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	r := w.mustRequest(w.req, w.activation(sub, jid))

	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- w.rt.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			// hold the brand key SHARED (a placement-like holder)
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(public.brand_status_gate_key($1))`, brand); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	_, err := w.approve(w.ap1, r)
	if !errors.Is(err, launchgov.ErrRetryableLockTimeout) {
		t.Fatalf("lock timeout on the brand key: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, apps, _ := w.svc.GetRequest(ctx, w.ap1, tid, r.ID)
	if got.Status != launchgov.ReqPending || len(apps) != 0 {
		t.Fatalf("a rolled-back execution must leave the request pending with no approval: %+v apps=%d", got, len(apps))
	}
	if st := w.status(sub); st != "pending_launch" {
		t.Fatalf("status %s", st)
	}
	if rows := w.audit(tid, "launch_authorisation.decide_denied"); len(rows) != 0 {
		t.Fatalf("a retryable lock timeout is not an audited refusal: %+v", rows)
	}
	if d, err := w.approve(w.ap1, r); err != nil || !d.Executed {
		t.Fatalf("retry: %+v err=%v", d, err)
	}
}
