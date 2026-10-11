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

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// Self-approval, same-Person, tenant approval, cross-tenant ids, acting sessions.
func TestRules_SelfApproval_SamePerson_TenantAndActingRefused_CrossTenant404(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	jid := w.jurisdiction(tid)
	r := w.mustRequest(w.req, w.activation(sub, jid))

	// Self-approval by staff id.
	if _, err := w.approve(w.req, r); !errors.Is(err, launchgov.ErrSelfApproval) {
		t.Fatalf("self-approval: %v", err)
	}
	// A SECOND account of the same Person as the requester.
	second := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!x', 'platform_admin', $3)`, second, "lgov-second-"+second.String()+"@fixture.invalid", w.ops.Persons[0])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sameUser := launchgov.Actor{StaffID: second, Scope: launchgov.ScopePlatform, Role: auth.RolePlatformAdmin}
	if _, err := w.approve(sameUser, r); !errors.Is(err, launchgov.ErrSelfApproval) {
		t.Fatalf("same-Person approval: %v", err)
	}
	// A tenant principal never approves (service) and cannot at the database either.
	ta := w.tenantStaff(tid, auth.RoleTenantAdmin)
	if _, err := w.svc.Decide(ctx, ta, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: w.seen(r)}); !errors.Is(err, launchgov.ErrForbidden) {
		t.Fatalf("tenant approval: %v", err)
	}
	if _, err := w.svc.Decide(ctx, launchgov.Actor{StaffID: w.ops.Approver1, Scope: "platform_acting", Role: auth.RolePlatformAdmin}, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: w.seen(r)}); !errors.Is(err, launchgov.ErrForbidden) && !errors.Is(err, launchgov.ErrActingSession) {
		t.Fatalf("acting-scope approval: %v", err)
	}
	// A role without the permission (compliance / finance / support) never approves or requests as platform.
	for _, role := range []auth.Role{auth.RoleCompliance, auth.RoleFinance, auth.RoleSupport, ""} {
		a := launchgov.Actor{StaffID: w.ops.Approver1, Scope: launchgov.ScopePlatform, Role: role}
		if _, err := w.svc.Decide(ctx, a, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: w.seen(r)}); !errors.Is(err, launchgov.ErrForbidden) {
			t.Fatalf("role %q approving: %v", role, err)
		}
	}
	// Cross-tenant: the request id under ANOTHER tenant's path is a 404, for every operation.
	other := w.activeTenant()
	if _, err := w.svc.Decide(ctx, w.ap1, other, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: strings.Repeat("0", 64)}); !errors.Is(err, launchgov.ErrNotFound) {
		t.Fatalf("cross-tenant decide: %v", err)
	}
	if _, _, err := w.svc.GetRequest(ctx, w.ap1, other, r.ID); !errors.Is(err, launchgov.ErrNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
	if _, err := w.svc.Cancel(ctx, w.req, launchgov.Subject{TenantID: other, BrandID: &brand}, r.ID, "requester_withdrew"); !errors.Is(err, launchgov.ErrNotFound) {
		t.Fatalf("cross-tenant cancel: %v", err)
	}
	// A tenant-B staff member names tenant A's brand: indistinguishable from an unknown one.
	tb := w.tenantStaff(other, auth.RoleTenantAdmin)
	in := w.activation(sub, jid)
	if _, err := w.svc.Request(ctx, tb, in); !errors.Is(err, launchgov.ErrNotFound) {
		t.Fatalf("tenant B requesting for tenant A's brand: %v", err)
	}
	if _, err := w.svc.Suspend(ctx, tb, launchgov.SuspendInput{Subject: sub, ReasonCode: "operator_request"}); !errors.Is(err, launchgov.ErrNotFound) {
		t.Fatalf("tenant B suspending tenant A's brand: %v", err)
	}
	if _, err := w.svc.ListRequests(ctx, tb, tid, launchgov.ListFilter{}); !errors.Is(err, launchgov.ErrNotFound) {
		t.Fatalf("tenant B listing tenant A's requests: %v", err)
	}
	// A tenant principal requesting a TENANT subject, and cancelling a platform request.
	tin := w.activation(launchgov.Subject{TenantID: tid}, jid)
	if _, err := w.svc.Request(ctx, ta, tin); !errors.Is(err, launchgov.ErrForbidden) {
		t.Fatalf("tenant session requesting a tenant subject: %v", err)
	}
	if _, err := w.svc.Cancel(ctx, ta, sub, r.ID, "requester_withdrew"); !errors.Is(err, launchgov.ErrForbidden) {
		t.Fatalf("a tenant cancelling the platform requester's request: %v", err)
	}
	if _, err := w.svc.Cancel(ctx, w.ap1, sub, r.ID, "requester_withdrew"); !errors.Is(err, launchgov.ErrForbidden) {
		t.Fatalf("a non-requester cancelling: %v", err)
	}
	// Nothing changed: still pending, no approval recorded.
	got, apps, err := w.svc.GetRequest(ctx, w.ap1, tid, r.ID)
	if err != nil || got.Status != launchgov.ReqPending || len(apps) != 0 {
		t.Fatalf("after the refusals: %+v apps=%d err=%v", got, len(apps), err)
	}
	// The refusals were audited (denied rows), codes only.
	if rows := w.audit(tid, "launch_authorisation.decide_denied"); len(rows) < 2 {
		t.Fatalf("self-approval refusals must be audited: %d", len(rows))
	}
}

// H-5: the explicit acknowledgement rule, the stale-readiness rule, and a check flipping between
// request and approval.
func TestRules_Acknowledgement_StaleReadiness_FlippingCheck(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)

	// No acknowledgement of non-passing checks => refused with the codes; an extra / unknown one too.
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	in := w.activation(sub, jid)
	need := in.AcknowledgedCheckCodes
	if len(need) < 2 {
		t.Fatalf("fixture must leave at least two checks non-passing, got %v", need)
	}
	bad := in
	bad.AcknowledgedCheckCodes = need[1:]
	var pe *launchgov.PreconditionError
	if _, err := w.svc.Request(ctx, w.req, bad); !errors.Is(err, launchgov.ErrUnacknowledged) || !errors.As(err, &pe) || pe.Codes[0] != need[0] {
		t.Fatalf("a missing acknowledgement must be refused naming %s: %v", need[0], err)
	}
	bad.AcknowledgedCheckCodes = append(append([]string(nil), need...), launchgov.CheckPlayerFunds)
	if _, err := w.svc.Request(ctx, w.req, bad); !errors.Is(err, launchgov.ErrUnacknowledged) {
		t.Fatalf("an acknowledgement of a check that is not in the set must be refused: %v", err)
	}
	if n := w.countRequests(sub); n != 0 {
		t.Fatalf("a refused request must not be stored: %d", n)
	}

	// Stale snapshot: the approver's hash does not match the server's fresh evaluation => refused, nothing recorded.
	r := w.mustRequest(w.req, in)
	if _, err := w.svc.Decide(ctx, w.ap1, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: strings.Repeat("a", 64)}); !errors.Is(err, launchgov.ErrStaleReadiness) {
		t.Fatalf("stale readiness: %v", err)
	}
	if _, err := w.svc.Decide(ctx, w.ap1, tid, r.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok"}); !errors.Is(err, launchgov.ErrInvalidInput) {
		t.Fatalf("an approval without the seen hash: %v", err)
	}
	if _, apps, _ := w.svc.GetRequest(ctx, w.ap1, tid, r.ID); len(apps) != 0 {
		t.Fatalf("a stale approval must record nothing: %d", len(apps))
	}

	// A check flips pass -> fail between the request and the final approval (R-RULESETS: an AML
	// ruleset is unset), and the flip is NOT acknowledged => refused_at_execution, no status change,
	// one audit row, the approval stays on record.
	w.setRuleset(tid, jid, "aml_ruleset_id", nil)
	d, err := w.approve(w.ap1, r)
	var ref *launchgov.RefusedAtExecutionError
	if !errors.As(err, &ref) || ref.Code != launchgov.RefusedReadinessUnacknowledged {
		t.Fatalf("a regressed, unacknowledged check at execution must refuse: res=%+v err=%v", d, err)
	}
	if got := w.status(sub); got != "pending_launch" {
		t.Fatalf("a refused execution changed the status: %s", got)
	}
	got, apps, _ := w.svc.GetRequest(ctx, w.ap1, tid, r.ID)
	if got.Status != launchgov.ReqRefusedAtExec || got.RefusalCode == nil || *got.RefusalCode != launchgov.RefusedReadinessUnacknowledged || len(apps) != 1 {
		t.Fatalf("refused request: %+v apps=%d", got, len(apps))
	}
	if rows := w.audit(tid, "launch_authorisation.refused_at_execution"); len(rows) != 1 || rows[0]["refusal_code"] != launchgov.RefusedReadinessUnacknowledged {
		t.Fatalf("one refused_at_execution audit row expected: %+v", rows)
	}
	if rows := w.audit(tid, "launch_authorisation.executed"); len(rows) != 0 {
		t.Fatalf("no executed row for a refused execution: %+v", rows)
	}
	// ... and a request filed AFTER the flip (acknowledging it) is fine: the acknowledgement is the rule.
	in2 := w.activation(sub, jid)
	if !contains(in2.AcknowledgedCheckCodes, launchgov.CheckRulesets) {
		t.Fatalf("the regressed check must now be in the acknowledgement set: %v", in2.AcknowledgedCheckCodes)
	}
	r2 := w.mustRequest(w.req, in2)
	if d, err := w.approve(w.ap1, r2); err != nil || !d.Executed {
		t.Fatalf("an acknowledged non-passing check must not block: %+v %v", d, err)
	}

	// Two approvals: an earlier approver's recorded readiness hash no longer matches at the final one.
	tenant2 := w.pendingTenant()
	j2 := w.jurisdiction(tenant2)
	sub2 := launchgov.Subject{TenantID: tenant2}
	r3 := w.mustRequest(w.req, w.activation(sub2, j2))
	if _, err := w.approve(w.ap1, r3); err != nil {
		t.Fatal(err)
	}
	w.setRuleset(tenant2, j2, "kyc_ruleset_id", nil)
	// The second approver sees the CHANGED readiness (fresh hash) and approves it; the first approver
	// decided on the old one, so the executor refuses.
	d, err = w.approve(w.ap2, r3)
	if !errors.As(err, &ref) || (ref.Code != launchgov.RefusedReadinessChangedSince && ref.Code != launchgov.RefusedReadinessUnacknowledged) {
		t.Fatalf("readiness changed since the first approval: %+v %v", d, err)
	}
	if got := w.status(sub2); got != "pending_launch" {
		t.Fatalf("status %s", got)
	}

	// The strict case: a check flips fail -> pass between the two approvals. Nothing is
	// unacknowledged (a passing check needs no acknowledgement), but the first approver decided on a
	// readiness that is no longer current, so the executor refuses on the hash alone.
	tenant3 := w.pendingTenant()
	j3 := w.jurisdiction(tenant3)
	sub3 := launchgov.Subject{TenantID: tenant3}
	aml := "aml1"
	w.setRuleset(tenant3, j3, "aml_ruleset_id", nil)
	r4 := w.mustRequest(w.req, w.activation(sub3, j3))
	if _, err := w.approve(w.ap1, r4); err != nil {
		t.Fatal(err)
	}
	w.setRuleset(tenant3, j3, "aml_ruleset_id", &aml)
	d, err = w.approve(w.ap2, r4)
	if !errors.As(err, &ref) || ref.Code != launchgov.RefusedReadinessChangedSince {
		t.Fatalf("a readiness that changed (for the better) since the first approval must still refuse on the hash: %+v %v", d, err)
	}
	if got := w.status(sub3); got != "pending_launch" {
		t.Fatalf("status %s", got)
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// setRuleset edits the tenant's jurisdiction configuration (tenant scope, owner pool as a fixture).
func (w *world) setRuleset(tenantID, jid uuid.UUID, col string, val *string) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenant_jurisdiction_configs SET `+col+` = $1 WHERE tenant_id = $2 AND jurisdiction_id = $3`, val, tenantID, jid)
		return err
	}); err != nil {
		w.t.Fatalf("edit jurisdiction config: %v", err)
	}
}

// Approvals older than the policy window are refused at execution.
func TestRules_ApprovalAge_StaleApprovalRefusedAtExecution(t *testing.T) {
	w := newWorld(t)
	svc := w.service(launchgov.Config{ApprovalMaxAge: 150 * time.Millisecond})
	w.svc = svc
	tid := w.pendingTenant()
	jid := w.jurisdiction(tid)
	sub := launchgov.Subject{TenantID: tid}
	r := w.mustRequest(w.req, w.activation(sub, jid))
	if _, err := w.approve(w.ap1, r); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	_, err := w.approve(w.ap2, r)
	var ref *launchgov.RefusedAtExecutionError
	if !errors.As(err, &ref) || ref.Code != launchgov.RefusedApprovalsStale {
		t.Fatalf("a stale first approval must refuse the execution: %v", err)
	}
	if got := w.status(sub); got != "pending_launch" {
		t.Fatalf("status %s", got)
	}
}

// H-1, H-2 (each model), H-3, S14 and the four-eyes counts.
func TestRules_HardPreconditions_H1_H2_H3_And_FourEyesCounts(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	base := w.activation(sub, jid)

	// H-1: illegal moves.
	act := w.activeBrand(tid)
	for name, in := range map[string]launchgov.RequestInput{
		"activate an active brand": func() launchgov.RequestInput {
			x := base
			x.Subject = launchgov.Subject{TenantID: tid, BrandID: &act}
			return x
		}(),
		"reactivate a pending brand": func() launchgov.RequestInput {
			x := base
			x.Action, x.ReasonCode = launchgov.ActionReactivate, "issue_resolved"
			return x
		}(),
		"ratify a pending brand": func() launchgov.RequestInput {
			x := base
			x.Action, x.ReasonCode = launchgov.ActionRatify, "legacy_ratification"
			return x
		}(),
	} {
		if _, err := w.svc.Request(ctx, w.req, in); !errors.Is(err, launchgov.ErrIllegalTransition) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// H-2: every model is accepted when consistent, and each inconsistency is refused with its code.
	try := func(name string, mut func(*launchgov.RequestInput), wantCodes ...string) {
		t.Helper()
		in := base
		lic := *base.Licensing
		in.Licensing = &lic
		mut(&in)
		in.AcknowledgedCheckCodes = w.nonPassing(in)
		res, err := w.svc.Request(ctx, w.req, in)
		if len(wantCodes) == 0 {
			if err != nil {
				t.Fatalf("%s must be accepted: %v", name, err)
			}
			if _, err := w.svc.Cancel(ctx, w.req, sub, res.Request.ID, "other_documented"); err != nil {
				t.Fatalf("%s cleanup: %v", name, err)
			}
			return
		}
		var pe *launchgov.PreconditionError
		if !errors.Is(err, launchgov.ErrPreconditionFailed) || !errors.As(err, &pe) {
			t.Fatalf("%s must be refused with a precondition error: %v", name, err)
		}
		for _, c := range wantCodes {
			if !contains(pe.Codes, c) {
				t.Fatalf("%s: want code %s in %v", name, c, pe.Codes)
			}
		}
	}
	// Accepted: the recorded-determination bases (never "no licence => blocked").
	try("other_manually_approved with a determination and no licence", func(in *launchgov.RequestInput) {})
	try("not_applicable_recorded_determination", func(in *launchgov.RequestInput) {
		in.Licensing.Model, in.Licensing.Status = launchgov.LicModelNotApplic, launchgov.LicStatusNotRequired
	})
	// Refused.
	try("other_manually_approved without a determination reference", func(in *launchgov.RequestInput) { in.Licensing.DeterminationReference = "" }, launchgov.H2Determination)
	try("other_manually_approved without a determining role", func(in *launchgov.RequestInput) { in.Licensing.DeterminationByRole = "" }, launchgov.H2Determination)
	try("not_applicable with a licence id", func(in *launchgov.RequestInput) {
		in.Licensing.Model, in.Licensing.Status = launchgov.LicModelNotApplic, launchgov.LicStatusNotRequired
		l := uuid.New()
		in.Licensing.LicenceID = &l
	}, launchgov.H2LicenceMustBeAbsent)
	try("not_applicable with the wrong status", func(in *launchgov.RequestInput) {
		in.Licensing.Model = launchgov.LicModelNotApplic
	}, launchgov.H2StatusMismatch)
	try("other with not_required_per_determination", func(in *launchgov.RequestInput) { in.Licensing.Status = launchgov.LicStatusNotRequired }, launchgov.H2StatusMismatch)
	try("platform_licence without a licence id", func(in *launchgov.RequestInput) { in.Licensing.Model = launchgov.LicModelPlatform }, launchgov.H2LicenceRequired)
	try("tenant_licence without a licence id", func(in *launchgov.RequestInput) { in.Licensing.Model = launchgov.LicModelTenant }, launchgov.H2LicenceRequired)
	try("platform_licence naming an unknown licence", func(in *launchgov.RequestInput) {
		in.Licensing.Model = launchgov.LicModelPlatform
		l := uuid.New()
		in.Licensing.LicenceID = &l
	}, launchgov.H2LicenceNotFound)
	try("an unknown licensing model", func(in *launchgov.RequestInput) { in.Licensing.Model = "wishful" }, launchgov.H2UnknownModel)

	// Licence-backed models against the registry: the tenant is under_platform_licence.
	plat := w.licence(tid, jid, "platform", "active")
	try("platform_licence consistent with the registry", func(in *launchgov.RequestInput) {
		in.Licensing.Model, in.Licensing.Status, in.Licensing.LicenceID = launchgov.LicModelPlatform, launchgov.LicStatusInForce, &plat
	})
	try("tenant_licence contradicting the tenant's platform licensing", func(in *launchgov.RequestInput) {
		in.Licensing.Model, in.Licensing.Status, in.Licensing.LicenceID = launchgov.LicModelTenant, launchgov.LicStatusInForce, &plat
	}, launchgov.H2LicenseeMismatch, launchgov.H2TenantModel)
	try("other_manually_approved naming the consistent licence", func(in *launchgov.RequestInput) { in.Licensing.LicenceID = &plat })
	// A licence of the right licensee that the tenant is NOT bound to.
	unbound := w.unboundLicence(jid, "platform")
	try("platform_licence naming a licence the tenant is not bound to", func(in *launchgov.RequestInput) {
		in.Licensing.Model, in.Licensing.Status, in.Licensing.LicenceID = launchgov.LicModelPlatform, launchgov.LicStatusInForce, &unbound
	}, launchgov.H2LicenceNotFound) // a tenant-scope read sees only its bound licence (licences_read), so a foreign licence is "not found"
	// An own_licence tenant: tenant_licence is the consistent basis.
	own := w.ownLicenceTenant(jid)
	ownBrand := w.pendingBrand(own.tenant)
	ownSub := launchgov.Subject{TenantID: own.tenant, BrandID: &ownBrand}
	oin := w.activation(ownSub, own.juris)
	oin.Licensing = &launchgov.LicensingRecord{Model: launchgov.LicModelTenant, Status: launchgov.LicStatusInForce, LicenceID: &own.licence}
	oin.AcknowledgedCheckCodes = w.nonPassing(oin)
	if res, err := w.svc.Request(ctx, w.req, oin); err != nil {
		t.Fatalf("tenant_licence on an own_licence tenant: %v", err)
	} else if res.Readiness.Checks == nil {
		t.Fatal("the request must return the readiness it bound")
	} else if lv, ok := resultOf(res.Readiness, launchgov.CheckLicenceValid); !ok || lv != launchgov.ResultPass {
		t.Fatalf("R-LIC-VALID must pass for an active licence: %v %v", lv, res.Readiness.Checks)
	}

	// H-3: at least one jurisdiction (content rule), each with a configuration in effect.
	noJur := base
	noJur.JurisdictionIDs = nil
	if _, err := w.svc.Request(ctx, w.req, noJur); !errors.Is(err, launchgov.ErrInvalidInput) {
		t.Fatalf("no jurisdiction: %v", err)
	}
	dupJur := base
	dupJur.JurisdictionIDs = []uuid.UUID{jid, jid}
	if _, err := w.svc.Request(ctx, w.req, dupJur); !errors.Is(err, launchgov.ErrInvalidInput) {
		t.Fatalf("duplicate jurisdictions: %v", err)
	}
	try("a jurisdiction without any tenant configuration", func(in *launchgov.RequestInput) {
		in.JurisdictionIDs = []uuid.UUID{w.bareJurisdiction()}
	}, launchgov.H3ConfigMissing)
	try("one configured and one unconfigured jurisdiction", func(in *launchgov.RequestInput) {
		in.JurisdictionIDs = []uuid.UUID{jid, w.bareJurisdiction()}
	}, launchgov.H3ConfigMissing)
	ended := w.jurisdiction(tid)
	w.endConfig(tid, ended)
	try("a configuration that is no longer in effect", func(in *launchgov.RequestInput) { in.JurisdictionIDs = []uuid.UUID{ended} }, launchgov.H3ConfigMissing)

	// Four-eyes counts (S14): a platform-requested brand launch needs 1; a tenant-requested
	// platform_licence brand launch needs 2; a tenant activation and any closure need 2.
	one := w.mustRequest(w.req, base)
	if one.RequiredApprovals != 1 {
		t.Fatalf("platform-requested brand activation: %d", one.RequiredApprovals)
	}
	if _, err := w.svc.Cancel(ctx, w.req, sub, one.ID, "other_documented"); err != nil {
		t.Fatal(err)
	}
	ta := w.tenantStaff(tid, auth.RoleTenantAdmin)
	tin := base
	tin.Licensing = &launchgov.LicensingRecord{Model: launchgov.LicModelPlatform, Status: launchgov.LicStatusInForce, LicenceID: &plat}
	tin.AcknowledgedCheckCodes = w.nonPassing(tin)
	two, err := w.svc.Request(ctx, ta, tin)
	if err != nil || two.Request.RequiredApprovals != 2 || two.Request.RequestedByScope != launchgov.ScopeTenant {
		t.Fatalf("tenant-requested platform_licence launch: %+v err=%v", two, err)
	}
	if _, err := w.svc.Cancel(ctx, ta, sub, two.Request.ID, "requester_withdrew"); err != nil {
		t.Fatal(err)
	}
	tother := base
	tother.AcknowledgedCheckCodes = w.nonPassing(tother)
	oneT, err := w.svc.Request(ctx, ta, tother)
	if err != nil || oneT.Request.RequiredApprovals != 1 {
		t.Fatalf("tenant-requested other_manually_approved launch needs 1: %+v err=%v", oneT, err)
	}
}

func resultOf(r launchgov.Readiness, code string) (string, bool) {
	for _, c := range r.Checks {
		if c.Code == code {
			return c.Result, true
		}
	}
	return "", false
}
