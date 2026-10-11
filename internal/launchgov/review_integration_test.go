//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

func (w *world) reject(who launchgov.Actor, r launchgov.Request) error {
	w.t.Helper()
	_, err := w.svc.Decide(context.Background(), who, r.TenantID, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionReject, ReasonCode: "policy_concern"})
	return err
}

func (w *world) closeInput(sub launchgov.Subject, reason, note string) launchgov.RequestInput {
	w.t.Helper()
	in := launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: reason, ConditionsNote: note}
	in.AcknowledgedCheckCodes = w.nonPassing(in)
	return in
}

// age backdates a pending request's expires_at by one minute past now (owner, trigger disabled for
// this statement inside one transaction: a test-only fixture; the guard forbids it otherwise).
func (w *world) age(id uuid.UUID) {
	w.t.Helper()
	err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE launch_authorisation_requests DISABLE TRIGGER launch_requests_guard`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET expires_at = now() - interval '1 minute' WHERE id = $1 AND status = 'pending'`, id); err != nil {
			return err
		}
		// Fire the deferred constraint triggers queued by the UPDATE, or ALTER TABLE refuses.
		if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE launch_authorisation_requests ENABLE TRIGGER launch_requests_guard`)
		return err
	})
	if err != nil {
		w.t.Fatalf("age request: %v", err)
	}
}

// SECURITY C2: the carry-over rule is exact on the payload hash, so editing anything defeats it.
// Every reject of the same subject and action within the TTL is shown to approvers whatever its
// payload, and an approver whose own Person rejected one may not approve a request that carries
// no change note.
func TestPriorRejecter_VariantAfterReject_ShownAndRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}

	r1 := w.mustRequest(w.req, w.closeInput(sub, "offboarding", ""))
	if err := w.reject(w.ap1, r1); err != nil {
		t.Fatal(err)
	}
	// The identical request is refused (LA014); a VARIANT (another reason code, same intent) is filed.
	if _, err := w.svc.Request(ctx, w.req, w.closeInput(sub, "offboarding", "")); !errors.Is(err, launchgov.ErrPriorReject) {
		t.Fatalf("identical re-request: %v", err)
	}
	r2 := w.mustRequest(w.req, w.closeInput(sub, "operator_request", ""))
	got, _, err := w.svc.GetRequest(ctx, w.ap2, tid, r2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PriorRejects) != 1 || got.PriorRejects[0].RequestID != r1.ID || got.PriorRejects[0].SamePayload || got.PriorRejects[0].DecidedBy != w.ops.Approver1 {
		t.Fatalf("GetRequest must show the earlier reject of a variant: %+v", got.PriorRejects)
	}
	// The prior rejecter may not approve the unchanged variant; the denial is audited with a distinct code.
	if _, err := w.approve(w.ap1, r2); !errors.Is(err, launchgov.ErrPriorRejecter) {
		t.Fatalf("the prior rejecter approving a variant with no change note: %v", err)
	}
	if rows := w.audit(tid, "launch_authorisation.decide_denied"); len(rows) != 1 || rows[0]["code"] != "LAUNCH_PRIOR_REJECTER" {
		t.Fatalf("audit of the refusal: %+v", rows)
	}
	// A different approver (another Person) sees the prior reject in the decision result and may approve.
	d, err := w.approve(w.ap2, r2)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PriorRejects) != 1 || d.PriorRejects[0].RequestID != r1.ID {
		t.Fatalf("Decide must show the prior rejects: %+v", d.PriorRejects)
	}
	// The prior rejecter may REJECT again (nothing in the rule blocks a reject).
	if err := w.reject(w.ap1, r2); err != nil {
		t.Fatalf("a rejecter rejecting again: %v", err)
	}

	// With a change note the same approver may approve (the note is the recorded change).
	r3 := w.mustRequest(w.req, w.closeInput(sub, "compliance_decision", "scope narrowed after the 12 Oct review"))
	g3, _, err := w.svc.GetRequest(ctx, w.ap2, tid, r3.ID)
	if err != nil || len(g3.PriorRejects) != 2 || g3.PriorRejects[0].RequestID != r2.ID || g3.PriorRejects[1].RequestID != r1.ID {
		t.Fatalf("only reject decisions are listed (newest first), never approvals: %+v err=%v", g3.PriorRejects, err)
	}
	if _, err := w.approve(w.ap1, r3); err != nil {
		t.Fatalf("a prior rejecter approving a request that carries a change note: %v", err)
	}
	// A reject of ANOTHER subject or ANOTHER action never counts.
	other := w.activeBrand(tid)
	r4 := w.mustRequest(w.req, w.closeInput(launchgov.Subject{TenantID: tid, BrandID: &other}, "offboarding", ""))
	if _, err := w.approve(w.ap1, r4); err != nil {
		t.Fatalf("a reject of another subject must not block: %v", err)
	}
}

// A reject older than the TTL no longer counts (and is no longer shown).
func TestPriorRejecter_RejectOlderThanTTL_DoesNotCount(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	r1 := w.mustRequest(w.req, w.closeInput(sub, "offboarding", ""))
	if err := w.reject(w.ap1, r1); err != nil {
		t.Fatal(err)
	}
	// An injected clock 80 h ahead: the reject is outside the 72 h window.
	late := w.service(launchgov.Config{Clock: func() time.Time { return time.Now().Add(80 * time.Hour) }})
	r2 := w.mustRequest(w.req, w.closeInput(sub, "operator_request", ""))
	got, _, err := late.GetRequest(ctx, w.ap2, tid, r2.ID)
	if err != nil || len(got.PriorRejects) != 0 {
		t.Fatalf("a reject outside the TTL must not be shown: %+v err=%v", got, err)
	}
}

// The LA014 refusal names the request whose payload_hash MATCHED (the latest reject of the subject
// is a different payload), and its retry time is a real time (never year 0001).
func TestPriorReject_NamesTheMatchingRequest_AndRealRetryTime(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	rA := w.mustRequest(w.req, w.closeInput(sub, "offboarding", ""))
	if err := w.reject(w.ap1, rA); err != nil {
		t.Fatal(err)
	}
	rB := w.mustRequest(w.req, w.closeInput(sub, "operator_request", ""))
	if err := w.reject(w.ap1, rB); err != nil { // the LATEST reject is the other payload
		t.Fatal(err)
	}
	_, err := w.svc.Request(ctx, w.req, w.closeInput(sub, "offboarding", ""))
	var pr *launchgov.PriorRejectError
	if !errors.As(err, &pr) || pr.RequestID != rA.ID {
		t.Fatalf("the refusal must name the request with the matching payload (%s), got %+v", rA.ID, pr)
	}
	if pr.DecidedAt.Year() < 2026 || pr.RetryAfter.Sub(pr.DecidedAt) != launchgov.RequestTTL || pr.RetryAfter.Before(time.Now()) {
		t.Fatalf("a real retry time is required: %+v", pr)
	}
	if rows := w.audit(tid, "launch_authorisation.request_denied"); len(rows) != 1 || rows[0]["prior_request_id"] != rA.ID.String() {
		t.Fatalf("the audit row names the matching request: %+v", rows)
	}
}

// Expiry is MATERIALISED and audited once, not just relabelled on read: a sweep, a list, a
// decision attempt or a new request each expire the request at most once, with one audit row.
func TestExpiry_IsMaterialisedAndAuditedOnce(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	r := w.mustRequest(w.req, w.closeInput(sub, "offboarding", ""))
	w.age(r.ID)

	// Observed by a decision attempt: refused as not pending; the expiry is now durable and audited.
	// (Decide is called directly with a made-up readiness hash: the w.approve helper would read the
	// request first, and a read also materialises the expiry.)
	if _, err := w.svc.Decide(ctx, w.ap1, tid, r.ID, launchgov.DecideInput{
		Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: strings.Repeat("0", 64)}); !errors.Is(err, launchgov.ErrNotPending) {
		t.Fatalf("approving an expired request: %v", err)
	}
	// Durable and audited by the decision attempt itself, before anything else has read or listed.
	if rows := w.audit(tid, "launch_authorisation.expired"); len(rows) != 1 {
		t.Fatalf("a refused decision on an expired request must materialise and audit the expiry: %+v", rows)
	}
	for i := 0; i < 3; i++ { // list, get and sweep afterwards add nothing
		if _, err := w.svc.ListRequests(ctx, w.ap1, tid, launchgov.ListFilter{}); err != nil {
			t.Fatal(err)
		}
		if n, err := w.svc.SweepExpired(ctx, w.ap1, tid); err != nil || n != 0 {
			t.Fatalf("a sweep after the expiry was observed expires nothing: n=%d err=%v", n, err)
		}
	}
	got, _, err := w.svc.GetRequest(ctx, w.ap1, tid, r.ID)
	if err != nil || got.Status != launchgov.ReqExpired {
		t.Fatalf("stored status must be expired: %+v err=%v", got, err)
	}
	rows := w.audit(tid, "launch_authorisation.expired")
	if len(rows) != 1 || rows[0]["request_id"] != r.ID.String() {
		t.Fatalf("exactly one expired audit row: %+v", rows)
	}

	// A sweep is also the way a worker expires a request nobody looked at.
	r2 := w.mustRequest(w.req, w.closeInput(sub, "operator_request", ""))
	w.age(r2.ID)
	if n, err := w.svc.SweepExpired(ctx, w.ap1, tid); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	if rows := w.audit(tid, "launch_authorisation.expired"); len(rows) != 2 {
		t.Fatalf("one more row: %+v", rows)
	}

	// A new request after an unobserved expiry: the request itself expires the stale one (audited)
	// instead of the database doing it silently.
	r3 := w.mustRequest(w.req, w.closeInput(sub, "compliance_decision", "third"))
	w.age(r3.ID)
	r4 := w.mustRequest(w.req, w.closeInput(sub, "offboarding", "fourth"))
	rows = w.audit(tid, "launch_authorisation.expired")
	if len(rows) != 3 || rows[2]["request_id"] != r3.ID.String() {
		t.Fatalf("the stale request expired by the new request is audited once: %+v", rows)
	}
	if g, _, _ := w.svc.GetRequest(ctx, w.ap1, tid, r4.ID); g == nil || g.Status != launchgov.ReqPending {
		t.Fatalf("the new request is pending: %+v", g)
	}
}

// The Statuses filter includes time-expired rows and excludes them from pending, independent of
// whether the stored row has been materialised yet (a clock ahead of the database's: the stored
// row is still 'pending' there).
func TestListRequests_StatusesFilter_TreatsTimeExpiredAsExpired(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	r := w.mustRequest(w.req, w.closeInput(sub, "offboarding", ""))

	late := w.service(launchgov.Config{Clock: func() time.Time { return time.Now().Add(80 * time.Hour) }})
	exp, err := late.ListRequests(ctx, w.ap1, tid, launchgov.ListFilter{Statuses: []string{launchgov.ReqExpired}})
	if err != nil || len(exp) != 1 || exp[0].ID != r.ID || exp[0].Status != launchgov.ReqExpired {
		t.Fatalf("[expired] must list the time-expired request: %+v err=%v", exp, err)
	}
	pend, err := late.ListRequests(ctx, w.ap1, tid, launchgov.ListFilter{Statuses: []string{launchgov.ReqPending}})
	if err != nil || len(pend) != 0 {
		t.Fatalf("[pending] must not list a time-expired request: %+v err=%v", pend, err)
	}
	both, err := late.ListRequests(ctx, w.ap1, tid, launchgov.ListFilter{Statuses: []string{launchgov.ReqPending, launchgov.ReqExpired}})
	if err != nil || len(both) != 1 {
		t.Fatalf("[pending, expired]: %+v err=%v", both, err)
	}
	// With the real clock it is still pending.
	pend, err = w.svc.ListRequests(ctx, w.ap1, tid, launchgov.ListFilter{Statuses: []string{launchgov.ReqPending}})
	if err != nil || len(pend) != 1 {
		t.Fatalf("[pending] with the real clock: %+v err=%v", pend, err)
	}
}

// Security C4: a suspension whose every attempt ends in lock contention is a denial AND a P1
// alert (launch.suspension_exhausted), and the audit row carries the real attempt count.
func TestSuspend_ExhaustedAttempts_RaisesP1AndAuditsRealAttempts(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	svc := w.service(launchgov.Config{LockTimeout: "150ms", SuspendAttempts: 2})

	// A long-open transaction holding the brand row FOR SHARE (what C4 describes) starves the
	// suspension's FOR NO KEY UPDATE.
	holdDone := make(chan struct{})
	locked := make(chan struct{})
	go func() {
		defer close(holdDone)
		_ = w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM brands WHERE id = $1 FOR SHARE`, brand).Scan(&id); err != nil {
				return err
			}
			close(locked)
			time.Sleep(3 * time.Second)
			return nil
		})
	}()
	<-locked
	_, err := svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"})
	<-holdDone
	if !errors.Is(err, launchgov.ErrRetryableLockTimeout) {
		t.Fatalf("an exhausted suspension must report the retryable lock timeout: %v", err)
	}
	if got := w.status(sub); got != "active" {
		t.Fatalf("the brand must be untouched, is %s", got)
	}
	rows := w.audit(tid, "launch_authorisation.suspend_denied")
	if len(rows) != 1 || rows[0]["attempts"] != float64(2) || rows[0]["code"] != "LAUNCH_RETRYABLE_LOCK_TIMEOUT" {
		t.Fatalf("the denial records the real attempt count: %+v", rows)
	}
	var n int
	var sev string
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), COALESCE(max(severity), '') FROM alerts WHERE kind = 'launch.suspension_exhausted' AND subject_tenant_id = $1`, tid).Scan(&n, &sev)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 || sev != "p1" {
		t.Fatalf("exactly one P1 alert expected: n=%d severity=%q", n, sev)
	}

	// A suspension refused for another reason (illegal transition) is NOT an exhaustion alert.
	if _, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"}); !errors.Is(err, launchgov.ErrIllegalTransition) {
		t.Fatalf("suspending a suspended brand: %v", err)
	}
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'launch.suspension_exhausted' AND subject_tenant_id = $1`, tid).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("no further alert for a non-contention refusal: n=%d err=%v", n, err)
	}
	// The pre-check refusal never entered the retry loop: it records no attempt count at all.
	if rows := w.audit(tid, "launch_authorisation.suspend_denied"); len(rows) != 2 || rows[1]["attempts"] != nil || rows[1]["code"] != "LAUNCH_ILLEGAL_TRANSITION" {
		t.Fatalf("a pre-check refusal records no attempts: %+v", rows)
	}
}

// The same exhaustion raised by a TENANT-scope suspender (its own brand): the alert is raised in
// the tenant's own scope (the Kind is raisable by a tenant session, and the subject is its tenant).
func TestSuspend_ExhaustedAttempts_TenantScope_RaisesP1InOwnScope(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	ta := w.tenantStaff(tid, "tenant_admin")
	svc := w.service(launchgov.Config{LockTimeout: "150ms", SuspendAttempts: 2})

	holdDone, locked := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(holdDone)
		_ = w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM brands WHERE id = $1 FOR SHARE`, brand).Scan(&id); err != nil {
				return err
			}
			close(locked)
			time.Sleep(2500 * time.Millisecond)
			return nil
		})
	}()
	<-locked
	_, err := svc.Suspend(ctx, ta, launchgov.SuspendInput{Subject: sub, ReasonCode: "operator_request"})
	<-holdDone
	if !errors.Is(err, launchgov.ErrRetryableLockTimeout) {
		t.Fatalf("an exhausted tenant suspension: %v", err)
	}
	var n int
	if err := w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'launch.suspension_exhausted' AND subject_tenant_id = $1`, tid).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("one P1 alert visible in the tenant's own scope: n=%d err=%v", n, err)
	}
}

// Security L3: an approver whose staff row carries no Person cannot decide; the check fails closed.
func TestDecide_ApproverWithoutPerson_FailsClosed(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	r := w.mustRequest(w.req, w.closeInput(launchgov.Subject{TenantID: tid, BrandID: &brand}, "offboarding", ""))
	seen := w.seen(r)

	id := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!lgov-not-a-login', 'platform_admin', NULL)`, id, "lgov-noperson-"+id.String()+"@fixture.invalid")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	noPerson := w.req
	noPerson.StaffID = id
	_, err := w.svc.Decide(ctx, noPerson, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: seen})
	if !errors.Is(err, launchgov.ErrNoPerson) {
		t.Fatalf("an approver with no Person must fail closed: %v", err)
	}
	if _, apps, _ := w.svc.GetRequest(ctx, w.ap1, tid, r.ID); len(apps) != 0 {
		t.Fatalf("nothing may be recorded: %d approvals", len(apps))
	}
}

// Security L5: other_documented needs a note (suspend and request), every other reason needs none.
func TestOtherDocumented_RequiresNote(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	if _, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: launchgov.Subject{TenantID: tid, BrandID: &b1}, ReasonCode: "other_documented"}); !errors.Is(err, launchgov.ErrInvalidInput) ||
		!strings.Contains(err.Error(), "conditions_note") {
		t.Fatalf("other_documented without a note: %v", err)
	}
	if got := w.status(launchgov.Subject{TenantID: tid, BrandID: &b1}); got != "active" {
		t.Fatalf("status %s", got)
	}
	if _, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: launchgov.Subject{TenantID: tid, BrandID: &b2}, ReasonCode: "other_documented", ConditionsNote: "regulator letter ref on file"}); err != nil {
		t.Fatalf("other_documented with a note: %v", err)
	}
}
