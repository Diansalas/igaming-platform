//go:build integration

package licensingchange

// d1 (migration 0191, D4): the governed LOOSENING of the screening TRIGGER policy (aml_screening_trigger_policy_loosen). Same shape as the 0190
// tests: every assertion about what the application can do runs as igaming_runtime; the owner pool provisions fixtures only. TEST FIXTURE
// VALUES - NOT LEGAL THRESHOLDS: the alert thresholds, intervals and cooling periods exist to exercise the mechanism; the platform ships none.
//
// Loosening of the trigger policy (from the schema): an event removed; gate_withdrawal on -> off; a LONGER gate_alert_after_seconds while the
// gate stays on. Enabling a gate, a shorter threshold and more events are tightening and stay on the route (internal/aml tests).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

type trigRow struct {
	ID         uuid.UUID
	Events     []string
	Gate       bool
	Alert      *int64
	Effective  time.Time
	Scope      string
	Request    *uuid.UUID
	LegalRef   string
	ReasonCode string
	CreatedBy  uuid.UUID
}

func (l *lw) triggers() []trigRow {
	l.t.Helper()
	var out []trigRow
	l.ownerTenant(func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `SELECT id, events, gate_withdrawal, gate_alert_after_seconds, effective_from, created_by_scope, change_request_id, legal_review_reference, reason_code, created_by
			FROM aml_screening_trigger_policies WHERE tenant_id = $1 AND licensing_jurisdiction_id = $2 ORDER BY created_at, id`, l.tenant, l.jur)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r trigRow
			if err := rows.Scan(&r.ID, &r.Events, &r.Gate, &r.Alert, &r.Effective, &r.Scope, &r.Request, &r.LegalRef, &r.ReasonCode, &r.CreatedBy); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
}

func (l *lw) trigPayload(events []string, gate bool, alert int64) ScreeningTriggerPolicyLoosenPayload {
	p := ScreeningTriggerPolicyLoosenPayload{TenantID: l.tenant, JurisdictionID: l.jur, Events: events, GateWithdrawal: gate,
		LegalReviewReference: "TEST FIXTURE LEGAL REF", EvidenceReference: "EVID-" + uuid.NewString()[:8]}
	if gate {
		p.GateAlertAfterSeconds = &alert
	}
	if p.Events == nil {
		p.Events = []string{}
	}
	return p
}

// gatedLW is a world whose tenant has the capability enabled (cooling 0) and a fixture trigger policy that GATES withdrawals with the given
// alert threshold (events registration + scheduled), in a database whose environment anchor allows a gate.
func gatedLW(t *testing.T, alert int64) *lw {
	t.Helper()
	rgfix.EnsureTestEnvironment(t, os.Getenv("TEST_DATABASE_URL"))
	l := newLW(t)
	l.enable(l.tenant, 0)
	l.seedTrigger(true, alert)
	return l
}

func TestD4_Unconfigured_AndDisabled_AreRefused_AndNothingIsWritten(t *testing.T) {
	rgfix.EnsureTestEnvironment(t, os.Getenv("TEST_DATABASE_URL"))
	l := newLW(t)
	l.seedTrigger(true, 1800)
	before := l.triggers()
	_, err := l.request(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration"}, false, 0))
	wantHint(t, err, "loosening_not_configured", "no capability row")
	if n := l.requestCount(KindAMLScreeningTriggerPolicyLoosen); n != 0 || len(l.triggers()) != len(before) {
		t.Fatalf("a refused request leaves nothing: %d requests, %d -> %d versions", n, len(before), len(l.triggers()))
	}
	if len(l.settings(l.tenant)) != 0 {
		t.Fatal("no capability row ships")
	}
	l.enable(l.tenant, 0)
	if _, err := l.svc.Request(context.Background(), l.req, RequestInput{Kind: KindAMLScreeningLooseningConfigure, Payload: payload(t, l.cfgPayload(l.tenant, false, 0)), ReasonCode: "loosening_disable_decision"}); err != nil {
		t.Fatal(err)
	}
	_, err = l.request(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration"}, false, 0))
	wantHint(t, err, "loosening_disabled", "capability disabled")
	// The capability is per tenant: another tenant is unconfigured.
	other := l.world.tenant("under_platform_licence")
	p := l.trigPayload([]string{"registration"}, false, 0)
	p.TenantID = other
	_, err = l.request(KindAMLScreeningTriggerPolicyLoosen, p)
	wantHint(t, err, "loosening_not_configured", "another tenant has no capability")
	if len(l.triggers()) != len(before) {
		t.Fatal("no trigger policy version was written")
	}
}

// Turning an existing gate OFF is a loosening: two Persons, same transaction, audit with before and after, requester and the requester's
// Person never decide, one approval is not enough, the same Person from a second account is not a second approver.
func TestD4_GateOff_EndToEnd_TwoPersons_SameTransaction_Audited(t *testing.T) {
	l := gatedLW(t, 1800)
	before := l.triggers()
	p := l.trigPayload([]string{"registration", "scheduled"}, false, 0) // the gate OFF, the events unchanged
	p.EffectiveOffsetSeconds = 600
	r := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, p, "screening_loosening_decision")
	if r.Executed || !r.Request.Widening || r.Request.RequiredApprovals != 2 || r.Request.SubjectTenantID != nil {
		t.Fatalf("a trigger loosening is filed as widening, 2 approvals, no tenant projection: %+v", r.Request)
	}
	id := r.Request.ID
	if _, err := l.request(KindAMLScreeningTriggerPolicyLoosen, p); !errors.Is(err, ErrDuplicatePending) {
		t.Fatalf("an identical open request: %v", err)
	}
	if _, err := l.approve(l.req, id); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("the requester approving: %v", err)
	}
	if _, err := l.approve(l.reqTwin, id); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("another account of the requester's Person approving: %v", err)
	}
	if first := l.mustApprove(l.ap1, id); first.Executed || len(l.triggers()) != len(before) {
		t.Fatal("ONE approval must not execute and must not write a trigger policy version")
	}
	if _, err := l.approve(l.ap1, id); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a duplicate approval by the same account: %v", err)
	}
	if _, err := l.approve(l.ap1Twin, id); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a second account of the same Person is not a second approver: %v", err)
	}
	if len(l.triggers()) != len(before) {
		t.Fatal("still nothing written")
	}
	done := l.mustApprove(l.ap2, id)
	if !done.Executed || done.Outcome["policy_id"] == nil {
		t.Fatalf("the second DISTINCT Person executes: %+v", done)
	}
	after := l.triggers()
	if len(after) != len(before)+1 {
		t.Fatalf("exactly one new version: %d -> %d", len(before), len(after))
	}
	for i := range before { // append-only
		if before[i].ID != after[i].ID || before[i].Gate != after[i].Gate {
			t.Fatalf("an earlier version changed: %+v -> %+v", before[i], after[i])
		}
	}
	nv := after[len(after)-1]
	if nv.Scope != "platform" || nv.Request == nil || *nv.Request != id || nv.Gate || nv.Alert != nil || strings.Join(nv.Events, ",") != "registration,scheduled" ||
		nv.LegalRef != "TEST FIXTURE LEGAL REF" || nv.ReasonCode != "governed_loosening" || nv.CreatedBy != l.ap2.StaffID {
		t.Fatalf("new version: %+v", nv)
	}
	if d := time.Until(nv.Effective); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("effective_from should be about 10 minutes ahead, got %s", d)
	}
	for _, ev := range []string{"licensing_change.requested", "licensing_change.approved", "licensing_change.executed", "aml.screening_trigger_policy_loosened"} {
		if l.auditCount(ev, id.String()) == 0 {
			t.Fatalf("no audit row %s", ev)
		}
	}
	var md []byte
	if err := l.owner.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE action = 'aml.screening_trigger_policy_loosened' AND target_id = $1`, id.String()).Scan(&md)
	}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(md, &m)
	out, _ := m["outcome"].(map[string]any)
	if out["previous_gate_withdrawal"] != true || out["gate_withdrawal"] != false || out["in_force_gate_withdrawal"] != true || out["previous_gate_alert_after_seconds"] != float64(1800) {
		t.Fatalf("audit before/after: %s", md)
	}
	if _, err := l.approve(l.ap3, id); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving an executed request: %v", err)
	}
}

// Every dimension, and the closed refusals of what is NOT a loosening.
func TestD4_Dimensions_AndTheClosedRefusals(t *testing.T) {
	l := gatedLW(t, 1800) // events registration + scheduled, gate on, threshold 1800 s; the fixture screening interval is 7200 s (= 2 x 3600)
	reqFor := func(p ScreeningTriggerPolicyLoosenPayload) error {
		_, err := l.request(KindAMLScreeningTriggerPolicyLoosen, p)
		return err
	}
	// Not a loosening: equal; a shorter threshold; the same gate with more events.
	wantHint(t, reqFor(l.trigPayload([]string{"registration", "scheduled"}, true, 1800)), "not_a_loosening", "equal")
	wantHint(t, reqFor(l.trigPayload([]string{"registration", "scheduled"}, true, 900)), "not_a_loosening", "a shorter threshold tightens")
	wantHint(t, reqFor(l.trigPayload([]string{"kyc_approval", "registration", "scheduled"}, true, 1800)), "not_a_loosening", "more events tighten")
	// A mix: an event removed AND a shorter threshold.
	wantHint(t, reqFor(l.trigPayload([]string{"scheduled"}, true, 900)), "loosening_mixes_tightening", "event removed and a shorter threshold")
	// A longer threshold must leave the screening policy interval at least twice the threshold (7200 >= 2 x 3600).
	wantHint(t, reqFor(l.trigPayload([]string{"registration", "scheduled"}, true, 3601)), "rescreen_interval_below_alert_threshold", "interval below twice the threshold")
	// No trigger policy to loosen in another jurisdiction.
	j2 := l.jurisdiction()
	p := l.trigPayload([]string{}, false, 0)
	p.JurisdictionID = j2
	wantHint(t, reqFor(p), "no_trigger_policy_to_loosen", "no version in that jurisdiction")
	// A loosening can never ENABLE a gate: with the latest version OFF, a payload that gates is refused with its own code.
	l.seedTrigger(false, 0) // events {registration}, no gate (newer version)
	wantHint(t, reqFor(l.trigPayload([]string{"registration", "scheduled"}, true, 1800)), "trigger_gate_enable_refused", "enabling a gate by loosening")
	if n := l.requestCount(KindAMLScreeningTriggerPolicyLoosen); n != 0 {
		t.Fatalf("refused requests are not stored: %d", n)
	}

	// Allowed dimensions, each executed end to end on a fresh world.
	t.Run("an event removed (the gate stays on)", func(t *testing.T) {
		l := gatedLW(t, 1800)
		l.seedTrigger(false, 0)
		l.seedTriggerRow([]string{"kyc_approval", "registration", "scheduled"}, true, 1800)
		done := l.widen(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, true, 1800), "trigger_loosening_decision")
		last := l.triggers()
		nv := last[len(last)-1]
		if !done.Executed || !nv.Gate || nv.Alert == nil || *nv.Alert != 1800 || strings.Join(nv.Events, ",") != "registration,scheduled" || nv.Scope != "platform" {
			t.Fatalf("event removal: %+v", nv)
		}
	})
	t.Run("a longer alert threshold (the gate stays on)", func(t *testing.T) {
		l := gatedLW(t, 1800)
		done := l.widen(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, true, 3600), "trigger_loosening_decision")
		last := l.triggers()
		nv := last[len(last)-1]
		if !done.Executed || !nv.Gate || nv.Alert == nil || *nv.Alert != 3600 {
			t.Fatalf("longer threshold: %+v", nv)
		}
	})
	t.Run("the last event removed with the gate off", func(t *testing.T) {
		l := gatedLW(t, 1800)
		l.seedTrigger(false, 0) // {registration}, off
		done := l.widen(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{}, false, 0), "trigger_loosening_decision")
		last := l.triggers()
		nv := last[len(last)-1]
		if !done.Executed || nv.Gate || len(nv.Events) != 0 {
			t.Fatalf("nothing triggers: %+v", nv)
		}
	})
}

// seedTriggerRow writes a trigger version with explicit events as the owner (fixture; the guard is disabled for it only).
func (l *lw) seedTriggerRow(events []string, gate bool, alert int64) {
	l.t.Helper()
	l.ownerTenant(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_trigger_policies DISABLE TRIGGER aml_screening_trigger_policies_10_guard`); err != nil {
			return err
		}
		var alertArg any
		if gate {
			alertArg = alert
		}
		if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds,
				effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
			VALUES ($1, $2, $3::text[], $4, $5, now() - interval '30 minutes', 'TEST FIXTURE REF', 'fixture', $6, $7, 'platform_acting')`,
			l.tenant, l.jur, events, gate, alertArg, l.ops.Requester, l.ops.Persons[0]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE aml_screening_trigger_policies ENABLE TRIGGER aml_screening_trigger_policies_10_guard`)
		return err
	})
}

// The officer controls of 0190 apply to the new kind: requester, every counted approver and the executor hold an in-force platform aml_officer
// grant and the owner-scope quorum holds. Plain platform admins cannot file or approve, and one officer is no quorum.
func TestD4_OfficerControls_PlainPlatformAdminsCannotLoosenTheTriggerPolicy(t *testing.T) {
	rgfix.EnsureTestEnvironment(t, os.Getenv("TEST_DATABASE_URL"))
	l := newLWNoOfficers(t)
	l.seedTrigger(true, 1800)
	for _, a := range []Actor{l.req, l.ap1, l.ap2} {
		amlfix.GrantOfficerAs(t, l.owner, l.tenant, a.StaffID, "platform")
	}
	l.enable(l.tenant, 0)
	// ap3 is a plain admin: it can neither file nor approve a trigger loosening.
	p := l.trigPayload([]string{"registration", "scheduled"}, false, 0)
	if _, err := l.svc.Request(context.Background(), l.ap3, RequestInput{Kind: KindAMLScreeningTriggerPolicyLoosen, Payload: payload(t, p), ReasonCode: "trigger_loosening_decision"}); hintOf(err) != "officer_grant_required" {
		t.Fatalf("a non-officer filing a trigger loosening: %v", err)
	}
	r := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, p, "trigger_loosening_decision").Request
	if _, err := l.approve(l.ap3, r.ID); GuardCode(err) != "LC050" || hintOf(err) != "officer_grant_required" {
		t.Fatalf("a non-officer approving a trigger loosening: %v", err)
	}
	if n := l.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r.ID); n != 0 {
		t.Fatalf("the refused approval is not recorded: %d", n)
	}
	// One approver is insufficient; two distinct authorised approvers execute.
	l.mustApprove(l.ap1, r.ID)
	if len(l.triggers()) != 1 {
		t.Fatal("one approver is insufficient")
	}
	if done := l.mustApprove(l.ap2, r.ID); !done.Executed || len(l.triggers()) != 2 {
		t.Fatalf("two distinct authorised approvers: %+v", done)
	}
	// The quorum: revoking officers below two refuses a new filing (the owner-scope quorum, not only the grant).
	amlfix.RevokeOfficer(t, l.owner, l.tenant, l.ap1.StaffID)
	amlfix.RevokeOfficer(t, l.owner, l.tenant, l.ap2.StaffID)
	l.seedTrigger(true, 1800)
	_, err := l.request(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, false, 0))
	wantHint(t, err, "officer_quorum_not_met", "the quorum no longer holds")
}

// An approver who loses standing before the final approval does not count.
func TestD4_OfficerControls_ApproverLosingStandingBeforeExecutionDoesNotCount(t *testing.T) {
	rgfix.EnsureTestEnvironment(t, os.Getenv("TEST_DATABASE_URL"))
	l := newLWNoOfficers(t)
	for _, a := range []Actor{l.req, l.ap2, l.ap3} {
		amlfix.GrantOfficerAs(t, l.owner, l.tenant, a.StaffID, "platform")
	}
	amlfix.GrantOfficerUntil(t, l.owner, l.tenant, l.ap1.StaffID, "platform", time.Now().Add(2500*time.Millisecond))
	l.enable(l.tenant, 0)
	l.seedTrigger(true, 1800)
	r := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, false, 0), "trigger_loosening_decision").Request
	l.mustApprove(l.ap1, r.ID)
	time.Sleep(2800 * time.Millisecond)
	_, err := l.approve(l.ap2, r.ID)
	wantHint(t, err, "officer_grant_required", "an approval by someone who is no longer an officer does not count")
	if len(l.triggers()) != 1 {
		t.Fatal("nothing is written")
	}
}

func TestD4_StaleApprovals_AndExpiredRequests_AreRefused(t *testing.T) {
	l := gatedLW(t, 1800)
	before := len(l.triggers())
	r := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, false, 0), "trigger_loosening_decision").Request
	l.mustApprove(l.ap1, r.ID)
	l.age("licensing_change_approvals", "licensing_change_approvals_guard", `UPDATE licensing_change_approvals SET decided_at = decided_at - interval '25 hours' WHERE request_id = $1`, r.ID)
	if _, err := l.approve(l.ap2, r.ID); !errors.Is(err, ErrApprovalsStale) {
		t.Fatalf("a >24 h old approval: %v", err)
	}
	lax := l.service(Config{ApprovalMaxAge: 1000 * time.Hour})
	if _, err := lax.Approve(context.Background(), l.ap2, r.ID, "reviewed_ok"); GuardCode(err) != "LC020" {
		t.Fatalf("the database refuses stale approvals even when Go does not: %v", err)
	}
	if len(l.triggers()) != before {
		t.Fatal("no trigger policy version from stale approvals")
	}
	e := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration"}, false, 0), "trigger_loosening_decision").Request
	l.age("licensing_change_requests", "licensing_change_requests_guard", `UPDATE licensing_change_requests SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, e.ID)
	if _, err := l.approve(l.ap1, e.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("an expired request (72 h): %v", err)
	}
	if len(l.triggers()) != before {
		t.Fatal("no trigger policy version from an expired request")
	}
}

func TestD4_CoolingPeriod_IsEnforcedFromFiling_AndDelaysTheEffectiveTime(t *testing.T) {
	rgfix.EnsureTestEnvironment(t, os.Getenv("TEST_DATABASE_URL"))
	l := newLW(t)
	l.enable(l.tenant, 3)
	l.seedTrigger(true, 1800)
	r := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, false, 0), "trigger_loosening_decision").Request
	l.mustApprove(l.ap1, r.ID)
	_, err := l.approve(l.ap2, r.ID)
	wantHint(t, err, "loosening_cooling_period", "approval inside the cooling period")
	if len(l.triggers()) != 1 || l.status(r.ID) != StatusRequested {
		t.Fatal("nothing is written inside the cooling period and the refused execution rolls back")
	}
	time.Sleep(3200 * time.Millisecond)
	if done := l.mustApprove(l.ap2, r.ID); !done.Executed {
		t.Fatalf("after the cooling period: %+v", done)
	}
	last := l.triggers()
	nv := last[len(last)-1]
	if d := time.Until(nv.Effective); d < 1500*time.Millisecond || d > 4*time.Second {
		t.Fatalf("effective_from = execution + the cooling period (3 s), got %s ahead", d)
	}
}

func TestD4_ConcurrentApprovers_ExecuteExactlyOnce(t *testing.T) {
	l := gatedLW(t, 1800)
	before := len(l.triggers())
	r := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, false, 0), "trigger_loosening_decision").Request
	var wg sync.WaitGroup
	type out struct {
		res *Result
		err error
	}
	results := make([]out, 3)
	for i, a := range []Actor{l.ap1, l.ap2, l.ap3} {
		wg.Add(1)
		go func(i int, a Actor) {
			defer wg.Done()
			res, err := l.approve(a, r.ID)
			results[i] = out{res, err}
		}(i, a)
	}
	wg.Wait()
	executed := 0
	for _, o := range results {
		switch {
		case o.err == nil && o.res.Executed:
			executed++
		case o.err == nil:
		case errors.Is(o.err, ErrNotPending), errors.Is(o.err, ErrRetryableLockTimeout):
		default:
			t.Fatalf("unexpected concurrent outcome: %v", o.err)
		}
	}
	if executed != 1 || len(l.triggers()) != before+1 {
		t.Fatalf("exactly one execution and one new version: executed=%d versions %d -> %d", executed, before, len(l.triggers()))
	}
}

func TestD4_TenantStaff_CannotFileSeeApproveOrWriteAnything(t *testing.T) {
	l := gatedLW(t, 1800)
	staff := l.tenantStaff(l.tenant, string(auth.RoleTenantAdmin))
	p := l.trigPayload([]string{"registration", "scheduled"}, false, 0)
	if _, err := l.svc.Request(context.Background(), staff, RequestInput{Kind: KindAMLScreeningTriggerPolicyLoosen, Payload: payload(t, p), ReasonCode: "tenant_attempt"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant staff filing: %v", err)
	}
	r := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, p, "trigger_loosening_decision").Request
	if _, err := l.svc.Get(context.Background(), staff, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant staff reading the request: %v", err)
	}
	if list, err := l.svc.List(context.Background(), staff, ListFilter{Kind: KindAMLScreeningTriggerPolicyLoosen}); err != nil || len(list) != 0 {
		t.Fatalf("tenant staff listing: %v %v", list, err)
	}
	if _, err := l.svc.Approve(context.Background(), staff, r.ID, "reviewed_ok"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant staff approving: %v", err)
	}
	tenantSQL := func(sql string, args ...any) error {
		return l.rt.WithPrincipalScope(context.Background(), l.tenant, staff.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	for _, sql := range []string{`SELECT count(*) FROM aml_screening_trigger_policies`, `UPDATE aml_screening_trigger_policies SET gate_withdrawal = false`,
		`INSERT INTO aml_screening_trigger_policies (tenant_id) VALUES ('` + l.tenant.String() + `')`, `DELETE FROM aml_screening_trigger_policies`} {
		if pgCode(tenantSQL(sql)) != "42501" {
			t.Fatalf("tenant principal %q must be a permission error, got %v", sql, tenantSQL(sql))
		}
	}
	if err := tenantSQL(`SELECT screening_trigger_loosen_assert($1::jsonb, NULL)`, `{}`); pgCode(err) != "LC002" {
		t.Fatalf("a tenant session calling the validator: %v", err)
	}
	if err := tenantSQL(`SELECT screening_trigger_loosen_apply($1)`, r.ID); pgCode(err) != "LC002" && pgCode(err) != "LC030" {
		t.Fatalf("a tenant session calling the executor: %v", err)
	}
	if err := tenantSQL(`INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code) VALUES ('aml_screening_trigger_policy_loosen', '{}'::jsonb, repeat('0', 64), 'x')`); pgCode(err) != "LC002" {
		t.Fatalf("a tenant session filing the kind at the database: %v", err)
	}
	// A foreign tenant's session sees none of this tenant's trigger policies.
	other := l.world.tenant("under_platform_licence")
	var visible int
	if err := l.owner.WithTenant(context.Background(), other, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_trigger_policies WHERE tenant_id = $1`, l.tenant).Scan(&visible)
	}); err != nil || visible != 0 {
		t.Fatalf("RLS: tenant B sees %d trigger policy versions of tenant A: %v", visible, err)
	}
}

// The executor and the table guard are not a back door, and a loosening can never enable a gate even if the validator were bypassed.
func TestD4_ExecutorAndGuard_NeedAnExecutedRequest_AndNeverEnableAGate(t *testing.T) {
	l := gatedLW(t, 1800)
	open := l.mustRequest(l.req, KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration"}, false, 0), "trigger_loosening_decision").Request
	before := len(l.triggers())
	for _, id := range []uuid.UUID{open.ID, uuid.New()} {
		err := l.rtPlatform(l.ap1.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT screening_trigger_loosen_apply($1)`, id)
			return err
		})
		if pgCode(err) != "LC040" {
			t.Fatalf("executor for %s: want LC040, got %v", id, err)
		}
	}
	done := l.widen(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, true, 3600), "trigger_loosening_decision")
	after := len(l.triggers())
	err := l.rtPlatform(l.ap2.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_trigger_loosen_apply($1)`, done.Request.ID)
		return err
	})
	if pgCode(err) != "LC040" || len(l.triggers()) != after || after != before+1 {
		t.Fatalf("a replayed executor call: %v (versions %d -> %d)", err, before, after)
	}
	// The wrong kind cannot drive the trigger executor.
	cfg := l.mustRequest(l.req, KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 10), "loosening_cooling_shorter").Request
	err = l.rtPlatform(l.ap1.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_trigger_loosen_apply($1)`, cfg.ID)
		return err
	})
	if pgCode(err) != "LC040" {
		t.Fatalf("wrong kind: %v", err)
	}
	// Table guard (owner path, direct INSERT with no executing request of the transaction): LC040.
	forgedInsert := func(gate bool, req uuid.UUID) error {
		var forged error
		_ = l.owner.WithTenant(context.Background(), l.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var alert any
			events := "{registration}"
			if gate {
				alert, events = 1800, "{registration,scheduled}"
			}
			_, forged = tx.Exec(ctx, `INSERT INTO aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds, effective_from,
					legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope, change_request_id)
				VALUES ($1, $2, $3::text[], $4, $5, now(), 'R', 'governed_loosening', $6, $7, 'platform', $8)`, l.tenant, l.jur, events, gate, alert, l.ap1.StaffID, l.ops.Persons[1], req)
			return errors.New("rollback")
		})
		return forged
	}
	if pgCode(forgedInsert(false, done.Request.ID)) != "LC040" {
		t.Fatalf("a platform-authored version without an executing request: %v", forgedInsert(false, done.Request.ID))
	}
	// Scope/request coupling (CHECK) and append-only (trigger).
	if err := l.owner.WithTenant(context.Background(), l.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE aml_screening_trigger_policies SET gate_withdrawal = false WHERE tenant_id = $1`, l.tenant)
		return err
	}); pgCode(err) != "AM005" {
		t.Fatalf("trigger policy versions stay append-only: %v", err)
	}
	// "Never enable a gate" at the TABLE: an executed (forged, owner path) request of this transaction whose payload gates, while the previous
	// latest version does NOT gate, is refused with its closed code. Previous latest = a non-gating fixture version.
	l.seedTriggerRow([]string{"registration"}, false, 0)
	payloadJSON := `{"tenant_id":"` + l.tenant.String() + `","jurisdiction_id":"` + l.jur.String() + `","events":["registration","scheduled"],"gate_withdrawal":true,"gate_alert_after_seconds":1800,"legal_review_reference":"R"}`
	forged := l.forgeExecutedRequestTx("aml_screening_trigger_policy_loosen", payloadJSON)
	if pgCode(forged) != "LC050" || !strings.Contains(forged.Error(), "never enables a withdrawal gate") {
		t.Fatalf("the table guard must refuse a platform-authored row that enables a gate: %v", forged)
	}
}

// forgeExecutedRequestTx, in ONE owner transaction (rolled back), forges an executed request of the kind of THIS transaction by ap2 and inserts a
// platform-scoped trigger row that points at it: the shape the table guard must judge. It returns the insert's error.
func (l *lw) forgeExecutedRequestTx(kind, payloadJSON string) error {
	l.t.Helper()
	var insertErr error
	_ = l.owner.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE licensing_change_requests DISABLE TRIGGER licensing_change_requests_guard`); err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code, widening, required_approvals, requested_by, requested_by_person_id,
				requested_scope, status, created_at, created_txid, expires_at, executed_by, executed_at, executed_txid)
			VALUES ($1, $2::jsonb, encode(sha256(gen_random_uuid()::text::bytea), 'hex'), 'x', true, 2, $3, $4, 'platform', 'executed', now(), txid_current(), now() + interval '1 day', $5, now(), txid_current())
			RETURNING id`, kind, payloadJSON, l.req.StaffID, l.ops.Persons[0], l.ap2.StaffID).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_trigger_policies NO FORCE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		_, insertErr = tx.Exec(ctx, `INSERT INTO aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds, effective_from,
				legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope, change_request_id)
			VALUES ($1, $2, '{registration,scheduled}', true, 1800, now(), 'R', 'governed_loosening', $3, $4, 'platform', $5)`, l.tenant, l.jur, l.ap2.StaffID, l.ops.Persons[2], id)
		return errors.New("rollback")
	})
	return insertErr
}

// The production anchor: a loosening never enables a gate, a gate-off loosening works everywhere (it only RELEASES), and a version that keeps a
// gate (a longer threshold) is refused where the anchor is production or unrecorded.
func TestD4_ProductionAnchor_GateOffAllowedEverywhere_KeepingAGateNeedsAnAllowedEnvironment(t *testing.T) {
	for _, tc := range []struct {
		anchor   string
		keepGate bool // may a longer threshold (gate stays on) be written
	}{{"production", false}, {"", false}, {"staging", true}} {
		name := tc.anchor
		if name == "" {
			name = "unrecorded"
		}
		t.Run(name, func(t *testing.T) {
			w := scratchWorldThrough(t, "d4env_", tc.anchor, 191)
			l := lwOn(w)
			l.enable(l.tenant, 0)
			l.seedTrigger(true, 1800)
			// A longer threshold keeps the gate: only where the environment allows a gate.
			_, err := l.request(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, true, 3600))
			if tc.keepGate {
				if err != nil {
					t.Fatalf("staging: %v", err)
				}
			} else {
				wantHint(t, err, "gate_environment_refused", "a version that keeps a gate in a production-anchored database")
			}
			// Switching the gate OFF only releases; it works in every environment and writes no gate.
			done := l.widen(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, false, 0), "trigger_loosening_decision")
			last := l.triggers()
			if !done.Executed || last[len(last)-1].Gate {
				t.Fatalf("gate off: %+v", last[len(last)-1])
			}
			// ... and from the OFF state nothing can turn it back on through loosening.
			_, err = l.request(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, true, 1800))
			wantHint(t, err, "trigger_gate_enable_refused", "enabling a gate through the loosening")
		})
	}
}

func TestD4_Reconciliation_IncludesTheTriggerKind(t *testing.T) {
	l := gatedLW(t, 1800)
	l.widen(KindAMLScreeningTriggerPolicyLoosen, l.trigPayload([]string{"registration", "scheduled"}, false, 0), "trigger_loosening_decision")
	if got := l.reconcileFor(l.tenant); len(got) != 0 {
		t.Fatalf("a clean tenant reconciles to nothing, got %+v", got)
	}
	payloadJSON := `{"tenant_id":"` + l.tenant.String() + `","jurisdiction_id":"` + l.jur.String() + `"}`
	a := l.forgeExecutedRequest("aml_screening_trigger_policy_loosen", payloadJSON)
	l.ownerTenant(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_trigger_policies DISABLE TRIGGER aml_screening_trigger_policies_10_guard`); err != nil {
			return err
		}
		// a platform row pointing at a request of ANOTHER kind (the forged configure request)
		b := l.forgeExecutedRequest("aml_screening_loosening_configure", payloadJSON)
		if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, effective_from, legal_review_reference, reason_code,
				created_by, created_by_person_id, created_by_scope, change_request_id)
			VALUES ($1, $2, '{}', false, now(), 'R', 'governed_loosening', $3, $4, 'platform', $5)`, l.tenant, l.jur, l.ap2.StaffID, l.ops.Persons[2], b); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE aml_screening_trigger_policies ENABLE TRIGGER aml_screening_trigger_policies_10_guard`)
		return err
	})
	got := map[string]bool{}
	for _, r := range l.reconcileFor(l.tenant) {
		got[r.Problem+"|"+r.Kind] = true
	}
	for _, want := range []string{"executed_request_without_exactly_one_row|aml_screening_trigger_policy_loosen", "trigger_policy_row_without_executed_request|aml_screening_trigger_policy_loosen"} {
		if !got[want] {
			t.Fatalf("missing %s in %v (request %s)", want, got, a)
		}
	}
}

// ---- migration 0191: up / down / up on a throwaway database --------------------------------------------------------------------

func TestMigration0191_UpDownUp_Snapshot_DownRefusedWithData(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0191_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 191)
	base, _ := migrationsThrough(t, 190)
	if _, err := pool.MigrateUp(ctx, base); err != nil {
		t.Fatalf("migrate to 0190: %v", err)
	}
	before := schemaSnapshot(t, pool)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != 191 {
		t.Fatalf("0191 up: %v %v", applied, err)
	}
	up := schemaSnapshot(t, pool)
	for _, want := range []string{"function:aml_screening_trigger_policy_author(uuid,uuid,text[],boolean,bigint,timestamp with time zone,text,text)", "function:screening_trigger_loosen_apply(uuid)",
		"function:screening_trigger_loosen_assert(jsonb,timestamp with time zone)", "function:screening_trigger_cmp(", "column:aml_screening_trigger_policies:change_request_id",
		"aml_screening_trigger_policies_scope_request", "aml_screening_trigger_policies_change_request"} {
		if !strings.Contains(strings.ReplaceAll(up, ", ", ","), want) {
			t.Fatalf("0191 up must create %q", want)
		}
	}
	var kinds int
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licensing_change_kinds WHERE kind LIKE 'aml\_screening\_%'`).Scan(&kinds)
	}); err != nil || kinds != 3 {
		t.Fatalf("0191 adds the third screening kind: %d (%v)", kinds, err)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0191 down: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != before {
		t.Fatalf("0191 down must restore the 0190 schema byte for byte; %s", firstDiff(before, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0191 up again: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatalf("0191 up again must equal the first up; %s", firstDiff(up, got))
	}
	// Down refusal while a request of the new kind exists.
	prooftest.InstallVia(t, pool.Raw(), u)
	launchfix.SeedOperatorsAt(t, u)
	ops := launchfix.Operators()
	var tid, jid uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0191", "m0191-"+uuid.NewString(), "under_platform_licence")
		tid = tn.ID
		jid = uuid.New()
		_, err2 := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'm0191')`, jid, "M91-"+jid.String()[:8])
		if err != nil {
			return err
		}
		return err2
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []uuid.UUID{ops.Requester, ops.Approver1, ops.Approver2} {
		amlfix.GrantOfficerAs(t, pool, tid, s, "platform")
	}
	// Forge (owner path) an open request of the new kind: the down must refuse while it exists.
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE licensing_change_requests DISABLE TRIGGER licensing_change_requests_guard`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code, widening, required_approvals, requested_by, requested_by_person_id,
				requested_scope, status, created_at, created_txid, expires_at)
			VALUES ('aml_screening_trigger_policy_loosen', $1::jsonb, encode(sha256(gen_random_uuid()::text::bytea), 'hex'), 'x', true, 2, $2, $3, 'platform', 'requested', now(), txid_current(), now() + interval '1 day')`,
			`{"tenant_id":"`+tid.String()+`"}`, ops.Requester, ops.Persons[0])
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `ALTER TABLE licensing_change_requests ENABLE TRIGGER licensing_change_requests_guard`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "LC095") {
		t.Fatalf("0191 down must refuse LC095 while a request of the kind exists, got %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("a refused down must change nothing")
	}
}

// ---- static pins --------------------------------------------------------------------------------------------------------------

func TestD4_Migration_DoesNotRedefineTheGateOrItsProductionGuard_AndTheAuthorKeepsIt(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0191_d1_screening_break_glass_and_trigger_loosening.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, forbidden := range []string{"FUNCTION aml_screening_trigger_policy_set(", "FUNCTION aml_screening_gate_environment_ok", "FUNCTION aml_screening_gate_denies",
		"FUNCTION screening_worker_available", "FUNCTION screening_worker_heartbeat", "database_environment"} {
		if strings.Contains(src, "CREATE FUNCTION "+strings.TrimPrefix(forbidden, "FUNCTION ")) || strings.Contains(src, "CREATE OR REPLACE FUNCTION "+strings.TrimPrefix(forbidden, "FUNCTION ")) {
			t.Errorf("migration 0191 must not define %q", forbidden)
		}
	}
	// The tighten-only author keeps the production guard and the heartbeat refusal of the setter it replaces on the route.
	i := strings.Index(src, "CREATE FUNCTION aml_screening_trigger_policy_author(")
	if i < 0 {
		t.Fatal("the author is not defined")
	}
	body := src[i : i+strings.Index(src[i:], "\n$$;")]
	for _, must := range []string{"IF NOT public.aml_screening_gate_environment_ok() THEN", "IF NOT public.screening_worker_available() THEN", "trigger_policy_loosening_refused", "screening_trigger_cmp("} {
		if !strings.Contains(body, must) {
			t.Errorf("the trigger policy author must contain %q", must)
		}
	}
	// The validator applies the environment guard to a version that keeps a gate, and the executor never writes gate_withdrawal = true unless the previous version gated.
	j := strings.Index(src, "CREATE FUNCTION screening_trigger_loosen_assert(")
	vbody := src[j : j+strings.Index(src[j:], "\n$$;")]
	if !strings.Contains(vbody, "public.aml_screening_gate_environment_ok()") || !strings.Contains(vbody, "trigger_gate_enable_refused") {
		t.Error("the validator must apply the environment guard and the never-enable-a-gate rule")
	}
	k, err := os.ReadFile("screening_loosening_kinds.go")
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, line := range strings.Split(string(k), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			code = append(code, line)
		}
	}
	if c := strings.Join(code, "\n"); strings.Contains(c, "INSERT INTO aml_screening_trigger_policies") || strings.Contains(c, "aml_screening_trigger_policy_set") {
		t.Error("the Go kinds must not write a trigger policy or call the setter")
	}
}
