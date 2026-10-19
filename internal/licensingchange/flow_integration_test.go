//go:build integration

package licensingchange

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// Widening (licence_create): two approvals by two distinct Persons, neither the requester's; the licence exists only after
// the final approval; the requester, an account of the requester's Person and a second account of an approver's Person
// cannot supply an approval.
func TestLicenceCreate_TwoPersonFlow_AndFourEyes(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	number := "LC-CREATE-" + uuid.NewString()[:8]
	res := w.mustRequest(w.req, KindLicenceCreate, LicenceCreatePayload{JurisdictionID: jid, Licensee: "platform", LicenceNumber: number}, "licence_granted")
	r := res.Request
	if r.Status != StatusRequested || !r.Widening || r.RequiredApprovals != 2 || res.Executed || r.RequestedScope != ScopePlatform {
		t.Fatalf("created request: %+v executed=%v", r, res.Executed)
	}
	if n := w.ownerInt(`SELECT count(*) FROM licences WHERE licence_number = $1`, number); n != 0 {
		t.Fatalf("a request writes no licence, found %d", n)
	}

	// The requester and another account of the requester's Person are refused.
	_, err := w.approve(w.req, r.ID)
	expectErr(t, err, ErrSelfApproval, "requester approves own request")
	_, err = w.approve(w.reqTwin, r.ID)
	expectErr(t, err, ErrSelfApproval, "same Person as the requester via a second staff account")

	// First approval: recorded, not executed.
	a1 := w.mustApprove(w.ap1, r.ID)
	if a1.Executed || w.status(r.ID) != StatusRequested {
		t.Fatalf("one approval of two must not execute: %+v", a1)
	}
	// The same Person cannot supply the second approval, neither with the same account nor with a second account.
	_, err = w.approve(w.ap1, r.ID)
	expectErr(t, err, ErrAlreadyDecided, "same approver twice")
	_, err = w.approve(w.ap1Twin, r.ID)
	expectErr(t, err, ErrAlreadyDecided, "same Person via a second staff account")
	if n := w.ownerInt(`SELECT count(*) FROM licences WHERE licence_number = $1`, number); n != 0 {
		t.Fatal("still no licence after one Person approved")
	}

	// The second distinct Person executes it.
	a2 := w.mustApprove(w.ap2, r.ID)
	if !a2.Executed || a2.Request.Status != StatusExecuted || a2.Outcome["licence_id"] == nil {
		t.Fatalf("the final approval must execute: %+v", a2)
	}
	if n := w.ownerInt(`SELECT count(*) FROM licences WHERE licence_number = $1 AND status = 'active'`, number); n != 1 {
		t.Fatalf("the executed change must create exactly one licence, found %d", n)
	}
	// Audit: requested, approved x2, executed (licensing_change.*), and the domain writer's licence_created row.
	for action, want := range map[string]int{"licensing_change.requested": 1, "licensing_change.approved": 2, "licensing_change.executed": 1} {
		if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = $1 AND target_id = $2`, action, r.ID.String()); n != want {
			t.Errorf("audit %s: %d rows, want %d", action, n, want)
		}
	}
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = 'jurisdiction_registry.licence_created' AND target_id = $1`, a2.Outcome["licence_id"]); n != 1 {
		t.Errorf("the licence creation must be audited once, got %d", n)
	}
	// Replays are stable refusals.
	_, err = w.approve(w.ap3, r.ID)
	expectErr(t, err, ErrNotPending, "approval after execution")
}

// Authorization: who may request / approve / read what.
func TestAuthorization_PlatformAdminVsTenantStaff(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	other := w.tenant("under_platform_licence")
	jid := w.jurisdiction()
	w.licence(jid, "platform", tid)
	ta := w.tenantStaff(tid, "tenant_admin")
	co := w.tenantStaff(tid, "compliance")
	support := w.tenantStaff(tid, "support")
	ctx := context.Background()

	// A role without licensing_change:request (support) is refused before any SQL.
	_, err := w.svc.Request(ctx, support, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "x",
		Payload: payload(t, OperatingPolicyPayload{TenantID: tid, Scope: "tenant", CountryCode: "MT", State: "disabled", Status: "active"})})
	expectErr(t, err, ErrForbidden, "support requests")

	// Tenant staff never request platform-only kinds.
	for _, k := range []Kind{KindLicenceCreate, KindTenantLicenceBind, KindTenantLicenceUnbind, KindLicenceCeilingVersion} {
		var p any = TenantLicenceUnbindPayload{TenantID: tid}
		switch k {
		case KindLicenceCreate:
			p = LicenceCreatePayload{JurisdictionID: jid, Licensee: "platform", LicenceNumber: "X"}
		case KindTenantLicenceBind:
			p = TenantLicenceBindPayload{TenantID: tid, LicenceID: uuid.New()}
		case KindLicenceCeilingVersion:
			p = LicenceCeilingPayload{LicenceID: uuid.New(), CountryCode: "MT", State: "disabled", Status: "active"}
		}
		_, err := w.svc.Request(ctx, ta, RequestInput{Kind: k, Payload: payload(t, p), ReasonCode: "x"})
		expectErr(t, err, ErrForbidden, "tenant admin requests "+string(k))
	}
	// The tenant-rung policy needs compliance (tenant_admin holds the brand / operation rung only).
	tenantRung := OperatingPolicyPayload{TenantID: tid, Scope: "tenant", CountryCode: "MT", State: "disabled", Status: "active"}
	_, err = w.svc.Request(ctx, ta, RequestInput{Kind: KindOperatingPolicyVersion, Payload: payload(t, tenantRung), ReasonCode: "x"})
	expectErr(t, err, ErrForbidden, "tenant_admin requests a tenant-rung policy")
	// Another tenant's subject is indistinguishable from an unknown one.
	foreign := tenantRung
	foreign.TenantID = other
	_, err = w.svc.Request(ctx, co, RequestInput{Kind: KindOperatingPolicyVersion, Payload: payload(t, foreign), ReasonCode: "x"})
	expectErr(t, err, ErrNotFound, "compliance requests for another tenant")

	// Compliance files a narrowing tenant-rung change for its own tenant: it needs ONE platform approval (tenant staff never
	// execute anything themselves).
	res := w.mustRequest(co, KindOperatingPolicyVersion, tenantRung, "market_exit")
	if res.Executed || res.Request.Widening || res.Request.RequiredApprovals != 1 || res.Request.RequestedScope != ScopeTenant {
		t.Fatalf("tenant narrowing request: %+v", res)
	}
	// Tenant staff never approve or reject (static permission, scope and database).
	_, err = w.svc.Approve(ctx, co, res.Request.ID, "x")
	expectErr(t, err, ErrForbidden, "compliance approves")
	_, err = w.svc.Approve(ctx, ta, res.Request.ID, "x")
	expectErr(t, err, ErrForbidden, "tenant admin approves")
	_, err = w.svc.Reject(ctx, ta, res.Request.ID, "x")
	expectErr(t, err, ErrForbidden, "tenant admin rejects")

	// Reads: the tenant sees the requests about itself only; the other tenant's staff see nothing of it.
	list, err := w.svc.List(ctx, co, ListFilter{})
	mustNotErr(t, err, "tenant list")
	if len(list) == 0 {
		t.Fatal("the tenant must see its own request")
	}
	for _, r := range list {
		if r.SubjectTenantID == nil || *r.SubjectTenantID != tid {
			t.Fatalf("tenant list leaked a request about another subject: %+v", r)
		}
	}
	otherCo := w.tenantStaff(other, "compliance")
	_, err = w.svc.Get(ctx, otherCo, res.Request.ID)
	expectErr(t, err, ErrNotFound, "another tenant reads the request")

	// A platform admin approves: executed (the tenant row is now disabled for MT).
	ap := w.mustApprove(w.ap1, res.Request.ID)
	if !ap.Executed {
		t.Fatalf("a platform approval executes the tenant's narrowing request: %+v", ap)
	}
	if n := w.ownerTenantInt(tid, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND scope_kind = 'tenant' AND state = 'disabled' AND effective_to IS NULL`, tid); n != 1 {
		t.Fatalf("the executed change must write the disabled tenant-rung policy, found %d", n)
	}
}

// Narrowing is single-actor (fail-closed asymmetry): a platform admin's disable / unbind / config close executes in the same
// call, with the requester as executor and a reason code; widening needs two approvals.
func TestNarrowing_SingleActor_WideningTwoPerson(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	jid := w.jurisdiction()
	lid := w.licence(jid, "platform", tid)

	// Widening: enable MT on the ceiling (2 approvals).
	enable := w.mustRequest(w.req, KindLicenceCeilingVersion, LicenceCeilingPayload{LicenceID: lid, CountryCode: "MT", State: "enabled", Status: "active",
		AuthorizationReference: sp("TICKET-1")}, "market_entry")
	if enable.Executed || !enable.Request.Widening || enable.Request.RequiredApprovals != 2 {
		t.Fatalf("an enable is widening and needs two approvals: %+v", enable.Request)
	}
	w.mustApprove(w.ap1, enable.Request.ID)
	if done := w.mustApprove(w.ap2, enable.Request.ID); !done.Executed {
		t.Fatal("the enable must execute on the second approval")
	}
	if n := w.ownerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1 AND state = 'enabled' AND effective_to IS NULL`, lid); n != 1 {
		t.Fatalf("enabled ceiling rows: %d", n)
	}

	// Widening at the tenant rung (2 approvals), then the brand rung enable.
	tp := w.mustRequest(w.req, KindOperatingPolicyVersion, OperatingPolicyPayload{TenantID: tid, Scope: "tenant", CountryCode: "MT", State: "enabled",
		Status: "active", AuthorizationReference: sp("TICKET-2")}, "market_entry")
	w.mustApprove(w.ap1, tp.Request.ID)
	if done := w.mustApprove(w.ap3, tp.Request.ID); !done.Executed {
		t.Fatal("the tenant-rung enable must execute")
	}

	// Narrowing: disable the ceiling, single actor, executed in the same call.
	dis := w.mustRequest(w.req, KindLicenceCeilingVersion, LicenceCeilingPayload{LicenceID: lid, CountryCode: "MT", State: "disabled", Status: "active"}, "emergency_stop")
	if !dis.Executed || dis.Request.Widening || dis.Request.RequiredApprovals != 0 || dis.Request.Status != StatusExecuted ||
		dis.Request.ExecutedBy == nil || *dis.Request.ExecutedBy != w.req.StaffID {
		t.Fatalf("a disable is single-actor and executes immediately: %+v", dis.Request)
	}
	if n := w.ownerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1 AND state = 'disabled' AND effective_to IS NULL`, lid); n != 1 {
		t.Fatalf("disabled ceiling rows: %d", n)
	}
	// A withdrawal at the brand rung WIDENS (absence inherits): two approvals.
	bid := w.brand(tid)
	wd := w.mustRequest(w.req, KindOperatingPolicyVersion, OperatingPolicyPayload{TenantID: tid, Scope: "brand", BrandID: &bid, CountryCode: "MT",
		State: "disabled", Status: "withdrawn", AuthorizationReference: sp("TICKET-3")}, "market_entry")
	if wd.Executed || !wd.Request.Widening {
		t.Fatalf("a brand-rung withdrawal is widening: %+v", wd.Request)
	}
	// Unbind: single actor.
	ub := w.mustRequest(w.req, KindTenantLicenceUnbind, TenantLicenceUnbindPayload{TenantID: tid}, "licence_withdrawn")
	if !ub.Executed {
		t.Fatalf("an unbind is single-actor: %+v", ub.Request)
	}
	if n := w.ownerInt(`SELECT count(*) FROM tenants WHERE id = $1 AND licence_id IS NULL`, tid); n != 1 {
		t.Fatal("the tenant must be unbound")
	}
	// Bind (rebind) is widening again.
	b := w.mustRequest(w.req, KindTenantLicenceBind, TenantLicenceBindPayload{TenantID: tid, LicenceID: lid}, "licence_granted")
	if b.Executed || b.Request.RequiredApprovals != 2 {
		t.Fatalf("a bind needs two approvals: %+v", b.Request)
	}
}

// tenant_jurisdiction_configs: a first set widens (two approvals); a subset narrows (single actor); adding a currency widens;
// a close narrows; a tenant-requested narrowing that would now widen is refused at execution (LC024).
func TestTenantJurisdictionConfig_Versions(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	jid := w.jurisdiction()
	set := func(cur []string) TenantJurisdictionConfigPayload {
		return TenantJurisdictionConfigPayload{TenantID: tid, JurisdictionID: jid, Action: ConfigActionSet, KYCRulesetID: sp("kyc-1"),
			AMLRulesetID: sp("aml-1"), RGRulesetID: sp("rg-1"), AllowedCurrencies: cur, AllowedPaymentMethods: []string{"card"}, GeoBlockList: []string{}}
	}
	first := w.mustRequest(w.req, KindTenantJurisdictionConfigVersion, set([]string{"EUR", "USD"}), "configuration_initial")
	if !first.Request.Widening || first.Request.RequiredApprovals != 2 {
		t.Fatalf("a first configuration is widening: %+v", first.Request)
	}
	w.mustApprove(w.ap1, first.Request.ID)
	if !w.mustApprove(w.ap2, first.Request.ID).Executed {
		t.Fatal("first configuration must execute")
	}
	open := func() int {
		return w.ownerTenantInt(tid, `SELECT count(*) FROM tenant_jurisdiction_configs WHERE tenant_id = $1 AND jurisdiction_id = $2 AND effective_to IS NULL`, tid, jid)
	}
	if open() != 1 {
		t.Fatal("one open version")
	}
	// Narrowing subset: single actor.
	narrow := w.mustRequest(w.req, KindTenantJurisdictionConfigVersion, set([]string{"EUR"}), "configuration_narrowing")
	if !narrow.Executed || narrow.Request.Widening {
		t.Fatalf("removing a currency is narrowing: %+v", narrow.Request)
	}
	if open() != 1 || w.ownerTenantInt(tid, `SELECT count(*) FROM tenant_jurisdiction_configs WHERE tenant_id = $1 AND jurisdiction_id = $2`, tid, jid) != 2 {
		t.Fatal("append-only: two versions, one open")
	}
	// A tenant-requested narrowing (compliance): [EUR] -> [] needs one platform approval; meanwhile a platform widening adds
	// GBP; the tenant's request, now compared with [GBP, EUR]... is still a subset, so it stays narrowing. Use a ruleset change
	// to make it widen at execution instead.
	co := w.tenantStaff(tid, "compliance")
	tnarrow := set([]string{})
	tres := w.mustRequest(co, KindTenantJurisdictionConfigVersion, tnarrow, "configuration_narrowing")
	if tres.Request.Widening || tres.Request.RequiredApprovals != 1 {
		t.Fatalf("tenant narrowing: %+v", tres.Request)
	}
	wid := set([]string{"EUR"})
	wid.KYCRulesetID = sp("kyc-2")
	wres := w.mustRequest(w.req, KindTenantJurisdictionConfigVersion, wid, "configuration_update")
	if !wres.Request.Widening {
		t.Fatal("a ruleset change is classified widening")
	}
	w.mustApprove(w.ap1, wres.Request.ID)
	w.mustApprove(w.ap2, wres.Request.ID)
	// Now the tenant's request (kyc-1) would change the ruleset back: widening at execution, refused.
	_, err := w.approve(w.ap1, tres.Request.ID)
	if sqlstateOf(err) != "LC024" {
		t.Fatalf("a change that now widens must be refused at execution (LC024), got %v", err)
	}
	if w.status(tres.Request.ID) != StatusRequested {
		t.Fatal("the refused execution rolls back")
	}
	// Close: narrowing, single actor.
	cl := w.mustRequest(w.req, KindTenantJurisdictionConfigVersion, TenantJurisdictionConfigPayload{TenantID: tid, JurisdictionID: jid, Action: ConfigActionClose}, "configuration_withdrawn")
	if !cl.Executed || open() != 0 {
		t.Fatalf("a close is single-actor and leaves nothing open: %+v", cl.Request)
	}
	// The audit carries before / after, projected to the tenant.
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = 'jurisdiction_registry.tenant_jurisdiction_config_version_set'
		AND subject_tenant_id = $1 AND metadata ? 'before' AND metadata ? 'after'`, tid); n < 3 {
		t.Fatalf("configuration versions must be audited with before / after (projected), got %d", n)
	}
}

// An approval older than the approval window cannot complete the four-eyes (the window is configurable down for the test; the
// database caps it at 24 h).
func TestApprovalOlderThanWindow_Refused(t *testing.T) {
	w := newWorld(t)
	w.svc = w.service(Config{ApprovalMaxAge: 150 * time.Millisecond})
	jid := w.jurisdiction()
	r := w.mustRequest(w.req, KindLicenceCreate, LicenceCreatePayload{JurisdictionID: jid, Licensee: "platform", LicenceNumber: "LC-STALE-" + uuid.NewString()[:6]}, "licence_granted").Request
	w.mustApprove(w.ap1, r.ID)
	time.Sleep(400 * time.Millisecond)
	_, err := w.approve(w.ap2, r.ID)
	expectErr(t, err, ErrApprovalsStale, "a stale first approval")
	if w.status(r.ID) != StatusRequested || w.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r.ID) != 1 {
		t.Fatal("the refused final approval must roll back")
	}
}

// Two final approvals racing: exactly one executes.
func TestConcurrentFinalApprovals_ExecuteOnce(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	number := "LC-RACE-" + uuid.NewString()[:8]
	r := w.mustRequest(w.req, KindLicenceCreate, LicenceCreatePayload{JurisdictionID: jid, Licensee: "platform", LicenceNumber: number}, "licence_granted").Request
	w.mustApprove(w.ap1, r.ID)
	var wg sync.WaitGroup
	results := make([]*Result, 2)
	errs := make([]error, 2)
	for i, a := range []Actor{w.ap2, w.ap3} {
		wg.Add(1)
		go func(i int, a Actor) {
			defer wg.Done()
			results[i], errs[i] = w.approve(a, r.ID)
		}(i, a)
	}
	wg.Wait()
	executed := 0
	for i := range errs {
		if errs[i] == nil && results[i].Executed {
			executed++
		} else if errs[i] != nil && !errors.Is(errs[i], ErrNotPending) {
			t.Fatalf("the losing approval must be NOT_PENDING, got %v", errs[i])
		}
	}
	if executed != 1 {
		t.Fatalf("exactly one approval executes, got %d (%v)", executed, errs)
	}
	if n := w.ownerInt(`SELECT count(*) FROM licences WHERE licence_number = $1`, number); n != 1 {
		t.Fatalf("exactly one licence, got %d", n)
	}
}

// Fail closed: when the approvals cannot be read, nothing is recorded and nothing executes.
func TestApprovalsUnreadable_FailsClosed(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	r := w.mustRequest(w.req, KindLicenceCreate, LicenceCreatePayload{JurisdictionID: jid, Licensee: "platform", LicenceNumber: "LC-FC-" + uuid.NewString()[:6]}, "licence_granted").Request
	broken := w.service(Config{})
	broken.loadApprovals = func(context.Context, pgx.Tx, uuid.UUID) ([]Approval, error) {
		return nil, errors.New("approvals unreadable")
	}
	if _, err := broken.Approve(context.Background(), w.ap1, r.ID, "reviewed_ok"); err == nil {
		t.Fatal("an unreadable approvals state must refuse")
	}
	if n := w.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r.ID); n != 0 || w.status(r.ID) != StatusRequested {
		t.Fatal("nothing may be recorded when approvals cannot be read")
	}
	// The same, for the post-insert recount (the approval just inserted rolls back too).
	calls := 0
	broken.loadApprovals = func(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]Approval, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("approvals unreadable on recount")
		}
		return loadApprovals(ctx, tx, id)
	}
	if _, err := broken.Approve(context.Background(), w.ap1, r.ID, "reviewed_ok"); err == nil {
		t.Fatal("an unreadable recount must refuse")
	}
	if n := w.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r.ID); n != 0 {
		t.Fatal("the approval must roll back")
	}
}

// Cancel / reject / duplicates.
func TestCancelRejectDuplicate(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()
	p := LicenceCreatePayload{JurisdictionID: jid, Licensee: "platform", LicenceNumber: "LC-DUP-" + uuid.NewString()[:6]}
	r := w.mustRequest(w.req, KindLicenceCreate, p, "licence_granted").Request
	_, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindLicenceCreate, Payload: payload(t, p), ReasonCode: "licence_granted"})
	expectErr(t, err, ErrDuplicatePending, "an identical open request")
	_, err = w.svc.Cancel(context.Background(), w.ap1, r.ID)
	expectErr(t, err, ErrForbidden, "a non-requester cancels")
	_, err = w.svc.Reject(context.Background(), w.reqTwin, r.ID, "not_needed")
	expectErr(t, err, ErrSelfApproval, "the requester's Person rejects")
	rej, err := w.svc.Reject(context.Background(), w.ap1, r.ID, "insufficient_evidence")
	mustNotErr(t, err, "reject")
	if rej.Status != StatusRejected || rej.DecisionReasonCode == nil || *rej.DecisionReasonCode != "insufficient_evidence" {
		t.Fatalf("rejected: %+v", rej)
	}
	_, err = w.approve(w.ap2, r.ID)
	expectErr(t, err, ErrNotPending, "approve a rejected request")
	r2 := w.mustRequest(w.req, KindLicenceCreate, p, "licence_granted").Request
	c, err := w.svc.Cancel(context.Background(), w.req, r2.ID)
	mustNotErr(t, err, "cancel")
	if c.Status != StatusCancelled {
		t.Fatalf("cancelled: %+v", c)
	}
}

// The ceiling stays the domain's: an enable at the tenant rung beyond the licence ceiling is refused when filed.
func TestPolicyEnableBeyondCeiling_RefusedWhenFiled(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	w.licence(w.jurisdiction(), "platform", tid)
	_, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "market_entry",
		Payload: payload(t, OperatingPolicyPayload{TenantID: tid, Scope: "tenant", CountryCode: "MT", State: "enabled", Status: "active", AuthorizationReference: sp("T")})})
	if !errors.Is(err, operatingmarket.ErrCeilingExceeded) {
		t.Fatalf("an enable beyond the ceiling must be refused with ErrCeilingExceeded, got %v", err)
	}
}
