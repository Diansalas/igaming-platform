//go:build integration

// ADR 0112 slice 4: the launch authorisation HTTP routes end to end, on the real stack as the
// igaming_runtime role. A recorded launch decision is evidence that named people took a decision on
// stated inputs, nothing more; these tests assert behaviour of the routes, never a legal conclusion.
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

func lhRequireDisclaimer(t *testing.T, res cgResp) {
	t.Helper()
	if !strings.Contains(string(res.body), `"disclaimer":`+jsonString(launchgov.Disclaimer)) {
		t.Fatalf("a 2xx launch response must carry the fixed disclaimer: %s", res.body)
	}
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// The complete two-person flow of a BRAND activation through HTTP: tenant staff request, tenant
// staff cannot approve, a platform admin approves, the brand becomes active, one audit row per
// event and a governed history entry. The readiness preview names the checks to acknowledge.
func TestLaunchHTTP_BrandActivation_TwoPersonFlow(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	ta := w.staff(tenantID, identity.StaffRoleTenantAdmin)
	tt := w.tenantTok(tenantID, ta, auth.RoleTenantAdmin)

	// Readiness preview as tenant staff (read permission, own tenant), with the disclaimer.
	prev := w.do("GET", lhReadinessPath(tenantID, &brand, "activate", &jid), tt, nil)
	if prev.status != http.StatusOK {
		t.Fatalf("readiness preview: %d %s", prev.status, prev.body)
	}
	lhRequireDisclaimer(t, prev)
	var rd lhReadiness
	prev.decode(t, &rd)
	if len(rd.Readiness.ReadinessHash) != 64 || len(rd.Readiness.Checks) == 0 {
		t.Fatalf("readiness preview content: %s", prev.body)
	}
	if got := w.requestCount(tenantID); got != 0 {
		t.Fatalf("a readiness read writes nothing, got %d requests", got)
	}

	// Without the acknowledgements the request is refused with the check codes.
	body := lhActivationBody(&brand, jid, nil)
	res := w.do("POST", lhBase(tenantID), tt, body)
	if len(rd.Readiness.NonPassingCheckCodes) > 0 {
		if res.status != http.StatusConflict || res.code(t) != "LAUNCH_UNACKNOWLEDGED_CHECKS" || len(res.checkCodes(t)) == 0 {
			t.Fatalf("unacknowledged checks must be a 409 with check codes: %d %s", res.status, res.body)
		}
	}
	created := w.createRequest(tt, tenantID, lhActivationBody(&brand, jid, rd.Readiness.NonPassingCheckCodes))
	if created.Request.Status != "pending" || created.Request.RequiredApprovals != 1 || created.Request.RequestedByScope != "tenant" ||
		created.Request.RequestedBy != ta.String() || created.Request.TenantID != tenantID.String() || created.Executed {
		t.Fatalf("created request: %+v", created.Request)
	}
	rid := created.Request.ID
	if created.Disclaimer != launchgov.Disclaimer {
		t.Fatal("create response must carry the disclaimer")
	}
	if got := w.status(tenantID, &brand); got != "pending_launch" {
		t.Fatalf("a request changes no status, got %s", got)
	}

	// Tenant staff may never approve (static permission, 403 at the middleware).
	if r := w.decide(tt, tenantID, rid, "approve", "reviewed_ok", rd.Readiness.ReadinessHash); r.status != http.StatusForbidden {
		t.Fatalf("tenant staff approve: %d %s", r.status, r.body)
	}
	if got := w.status(tenantID, &brand); got != "pending_launch" {
		t.Fatalf("status after a refused approval: %s", got)
	}

	// The platform admin approves with the readiness hash it was shown: executed.
	ap := w.approve(w.tokAp1(), tenantID, rid)
	if ap.status != http.StatusOK {
		t.Fatalf("platform approve: %d %s", ap.status, ap.body)
	}
	lhRequireDisclaimer(t, ap)
	var dec lhRequestResp
	ap.decode(t, &dec)
	if !dec.Executed || dec.Request.Status != "executed" || dec.Decision != "approve" {
		t.Fatalf("decision response: %+v", dec)
	}
	if got := w.status(tenantID, &brand); got != "active" {
		t.Fatalf("the brand must be active after the final approval, got %s", got)
	}
	for _, ev := range []string{"requested", "approved", "executed"} {
		if n := w.auditCount(tenantID, "launch_authorisation."+ev); n != 1 {
			t.Fatalf("audit launch_authorisation.%s: want exactly 1, got %d", ev, n)
		}
	}

	// History (tenant staff, own tenant): the governed transition to active names the request.
	h := w.do("GET", "/v1/admin/tenants/"+tenantID.String()+"/launch-status-history?brand_id="+brand.String(), tt, nil)
	if h.status != http.StatusOK {
		t.Fatalf("history: %d %s", h.status, h.body)
	}
	var hist struct {
		Items []struct {
			Kind      string  `json:"kind"`
			ToStatus  string  `json:"to_status"`
			RequestID *string `json:"request_id"`
		} `json:"items"`
	}
	h.decode(t, &hist)
	if len(hist.Items) != 1 || hist.Items[0].Kind != "governed" || hist.Items[0].ToStatus != "active" || hist.Items[0].RequestID == nil || *hist.Items[0].RequestID != rid {
		t.Fatalf("history: %s", h.body)
	}

	// The executed request shows both the requester and the approval (read by tenant staff).
	g := w.do("GET", lhBase(tenantID)+"/"+rid, tt, nil)
	var got lhRequestResp
	g.decode(t, &got)
	if g.status != http.StatusOK || got.Request.Status != "executed" || len(got.Request.Approvals) != 1 || got.Request.Approvals[0].DecidedBy != "" {
		t.Fatalf("get executed request: %d %s", g.status, g.body)
	}
	// A decided request is not pending: a further approval is a stable 409.
	if r := w.decide(w.tokAp2(), tenantID, rid, "approve", "reviewed_ok", strings.Repeat("a", 64)); r.status != http.StatusConflict || r.code(t) != "LAUNCH_REQUEST_NOT_PENDING" {
		t.Fatalf("approve a decided request: %d %s", r.status, r.body)
	}
}

// A TENANT activation needs two distinct platform approvers; the requester cannot approve.
func TestLaunchHTTP_TenantActivation_TwoApprovers_SelfApprovalRefused(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.pendingTenant()
	jid := w.jurisdiction(tenantID)
	acks := w.nonPassing(tenantID, nil, "activate", &jid)
	created := w.createRequest(w.tokReq(), tenantID, lhActivationBody(nil, jid, acks))
	if created.Request.RequiredApprovals != 2 || created.Request.RequestedByScope != "platform" {
		t.Fatalf("tenant activation needs 2 approvals: %+v", created.Request)
	}
	rid := created.Request.ID
	// The requester cannot approve (409, nothing recorded).
	if r := w.approve(w.tokReq(), tenantID, rid); r.status != http.StatusConflict || r.code(t) != "LAUNCH_SELF_APPROVAL" {
		t.Fatalf("self approval: %d %s", r.status, r.body)
	}
	first := w.approve(w.tokAp1(), tenantID, rid)
	var f1 lhRequestResp
	first.decode(t, &f1)
	if first.status != http.StatusOK || f1.Executed || f1.Request.Status != "pending" {
		t.Fatalf("first approval must leave the request pending: %d %s", first.status, first.body)
	}
	if got := w.status(tenantID, nil); got != "pending_launch" {
		t.Fatalf("status after one of two approvals: %s", got)
	}
	second := w.approve(w.tokAp2(), tenantID, rid)
	var f2 lhRequestResp
	second.decode(t, &f2)
	if second.status != http.StatusOK || !f2.Executed || f2.Request.Status != "executed" {
		t.Fatalf("second approval must execute: %d %s", second.status, second.body)
	}
	if got := w.status(tenantID, nil); got != "active" {
		t.Fatalf("the tenant must be active, got %s", got)
	}
}

// Reject: a platform admin rejects, the request is rejected, nothing changes, and an identical new
// request within the window is refused (LAUNCH_PRIOR_REJECT).
func TestLaunchHTTP_Reject_Then_IdenticalRequestRefused(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	acks := w.nonPassing(tenantID, &brand, "activate", &jid)
	body := lhActivationBody(&brand, jid, acks)
	c := w.createRequest(w.tokReq(), tenantID, body)
	r := w.decide(w.tokAp1(), tenantID, c.Request.ID, "reject", "policy_concern", "")
	if r.status != http.StatusOK {
		t.Fatalf("reject: %d %s", r.status, r.body)
	}
	var d lhRequestResp
	r.decode(t, &d)
	if d.Request.Status != "rejected" || d.Executed || d.Decision != "reject" {
		t.Fatalf("reject response: %s", r.body)
	}
	if got := w.status(tenantID, &brand); got != "pending_launch" {
		t.Fatalf("status after a reject: %s", got)
	}
	again := w.do("POST", lhBase(tenantID), w.tokReq(), body)
	if again.status != http.StatusConflict || again.code(t) != "LAUNCH_PRIOR_REJECT" {
		t.Fatalf("identical request after a reject: %d %s", again.status, again.body)
	}
	// Approving a rejected request is a stable 409.
	if x := w.decide(w.tokAp2(), tenantID, c.Request.ID, "approve", "reviewed_ok", strings.Repeat("b", 64)); x.status != http.StatusConflict || x.code(t) != "LAUNCH_REQUEST_NOT_PENDING" {
		t.Fatalf("approve a rejected request: %d %s", x.status, x.body)
	}
}

// Cancel: only the requester may; the proof binds the request; a replay is NOT_PENDING.
func TestLaunchHTTP_Cancel_RequesterOnly_AndReplay(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	ta, ta2 := w.staff(tenantID, identity.StaffRoleTenantAdmin), w.staff(tenantID, identity.StaffRoleTenantAdmin)
	tt, tt2 := w.tenantTok(tenantID, ta, auth.RoleTenantAdmin), w.tenantTok(tenantID, ta2, auth.RoleTenantAdmin)
	acks := w.nonPassing(tenantID, &brand, "activate", &jid)
	c := w.createRequest(tt, tenantID, lhActivationBody(&brand, jid, acks))
	path := lhBase(tenantID) + "/" + c.Request.ID + "/cancel"

	// Another staff member of the tenant is not the requester.
	if r := w.do("POST", path, tt2, map[string]any{"reason_code": "requester_withdrew"}); r.status != http.StatusForbidden {
		t.Fatalf("cancel by a non-requester: %d %s", r.status, r.body)
	}
	// An unknown reason code is a 400.
	if r := w.do("POST", path, tt, map[string]any{"reason_code": "nope"}); r.status != http.StatusBadRequest {
		t.Fatalf("cancel reason outside the closed set: %d %s", r.status, r.body)
	}
	r := w.do("POST", path, tt, map[string]any{"reason_code": "requester_withdrew"})
	var d lhRequestResp
	r.decode(t, &d)
	if r.status != http.StatusOK || d.Request.Status != "cancelled" {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
	lhRequireDisclaimer(t, r)
	if r2 := w.do("POST", path, tt, map[string]any{"reason_code": "requester_withdrew"}); r2.status != http.StatusConflict || r2.code(t) != "LAUNCH_REQUEST_NOT_PENDING" {
		t.Fatalf("cancel replay: %d %s", r2.status, r2.body)
	}
	if n := w.auditCount(tenantID, "launch_authorisation.cancelled"); n != 1 {
		t.Fatalf("exactly one cancelled audit row, got %d", n)
	}
}

// Replays change no state (a denied audit row may be written) and answer a stable 409.
func TestLaunchHTTP_ReplaysAreStable409_NothingRecorded(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	acks := w.nonPassing(tenantID, &brand, "activate", &jid)
	body := lhActivationBody(&brand, jid, acks)
	w.createRequest(w.tokReq(), tenantID, body)
	// Replayed create: one pending request per subject.
	if r := w.do("POST", lhBase(tenantID), w.tokReq(), body); r.status != http.StatusConflict || r.code(t) != "LAUNCH_REQUEST_PENDING" {
		t.Fatalf("create replay: %d %s", r.status, r.body)
	}
	if n := w.requestCount(tenantID); n != 1 {
		t.Fatalf("a replayed create must not add a request, got %d", n)
	}

	// Replayed approve by the same Person on a 2-approval request.
	tenant2 := w.pendingTenant()
	jid2 := w.jurisdiction(tenant2)
	acks2 := w.nonPassing(tenant2, nil, "activate", &jid2)
	c := w.createRequest(w.tokReq(), tenant2, lhActivationBody(nil, jid2, acks2))
	if r := w.approve(w.tokAp1(), tenant2, c.Request.ID); r.status != http.StatusOK {
		t.Fatalf("first approve: %d %s", r.status, r.body)
	}
	if r := w.approve(w.tokAp1(), tenant2, c.Request.ID); r.status != http.StatusConflict || r.code(t) != "LAUNCH_ALREADY_DECIDED" {
		t.Fatalf("approve replay: %d %s", r.status, r.body)
	}
	if r := w.decide(w.tokAp1(), tenant2, c.Request.ID, "reject", "policy_concern", ""); r.status != http.StatusConflict || r.code(t) != "LAUNCH_ALREADY_DECIDED" {
		t.Fatalf("a reject after an approve by the same Person: %d %s", r.status, r.body)
	}
	g := w.do("GET", lhBase(tenant2)+"/"+c.Request.ID, w.tokAp2(), nil)
	var got lhRequestResp
	g.decode(t, &got)
	if len(got.Request.Approvals) != 1 || got.Request.Status != "pending" {
		t.Fatalf("exactly one approval and still pending: %s", g.body)
	}

	// Replayed suspension: already suspended.
	b := w.activeBrand(tenantID)
	sus := "/v1/admin/tenants/" + tenantID.String() + "/brands/" + b.String() + "/suspend"
	if r := w.do("POST", sus, w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusOK {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	if r := w.do("POST", sus, w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusConflict || r.code(t) != "LAUNCH_ILLEGAL_TRANSITION" {
		t.Fatalf("suspend replay: %d %s", r.status, r.body)
	}
	if n := w.auditCount(tenantID, "launch_authorisation.suspended"); n != 1 {
		t.Fatalf("exactly one suspended audit row, got %d", n)
	}
}

// The authorization matrix: static permissions, scopes and roles. 401 without a token, 403 for a
// player principal and for staff roles without the permission, 404 for a foreign tenant.
func TestLaunchHTTP_AuthzMatrix(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	suspBrandTA, suspBrandCo := w.activeBrand(tenantID), w.activeBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	ta, co, sup, fin := w.staff(tenantID, identity.StaffRoleTenantAdmin), w.staff(tenantID, identity.StaffRoleCompliance),
		w.staff(tenantID, identity.StaffRoleSupport), w.staff(tenantID, identity.StaffRoleFinance)
	tokTA, tokCo := w.tenantTok(tenantID, ta, auth.RoleTenantAdmin), w.tenantTok(tenantID, co, auth.RoleCompliance)
	tokSup, tokFin := w.tenantTok(tenantID, sup, auth.RoleSupport), w.tenantTok(tenantID, fin, auth.RoleFinance)
	player, err := w.issuer.Issue(uuid.NewString(), tenantID, auth.RolePlayer, auth.PrincipalPlayer, 3600e9)
	if err != nil {
		t.Fatal(err)
	}

	base := "/v1/admin/tenants/" + tenantID.String()
	someReq := uuid.NewString()
	reads := []string{
		base + "/launch-readiness?action=activate&brand_id=" + brand.String(),
		base + "/launch-authorisations",
		base + "/launch-authorisations/" + someReq,
		base + "/launch-status-history",
	}
	// Reads: tenant_admin and compliance (own tenant) yes; support / finance / player / anonymous no.
	for _, p := range reads {
		for name, tok := range map[string]string{"tenant_admin": tokTA, "compliance": tokCo} {
			if r := w.do("GET", p, tok, nil); r.status == http.StatusForbidden || r.status == http.StatusUnauthorized {
				t.Errorf("%s GET %s must pass the permission gate, got %d", name, p, r.status)
			}
		}
		for name, tok := range map[string]string{"support": tokSup, "finance": tokFin, "player": player} {
			if r := w.do("GET", p, tok, nil); r.status != http.StatusForbidden {
				t.Errorf("%s GET %s: want 403, got %d %s", name, p, r.status, r.body)
			}
		}
		if r := w.do("GET", p, "", nil); r.status != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: want 401, got %d", p, r.status)
		}
		if r := w.do("GET", p, "garbage", nil); r.status != http.StatusUnauthorized {
			t.Errorf("bad token GET %s: want 401, got %d", p, r.status)
		}
	}
	// The status codes of the readable ones for a platform admin.
	if r := w.do("GET", reads[1], w.tokReq(), nil); r.status != http.StatusOK {
		t.Errorf("platform list: %d %s", r.status, r.body)
	}
	if r := w.do("GET", reads[2], w.tokReq(), nil); r.status != http.StatusNotFound {
		t.Errorf("platform get unknown request: %d %s", r.status, r.body)
	}

	// Writes that no tenant role may do: approve / reject (static permission, 403).
	for _, op := range []string{"approve", "reject"} {
		for name, tok := range map[string]string{"tenant_admin": tokTA, "compliance": tokCo, "support": tokSup, "finance": tokFin, "player": player} {
			if r := w.do("POST", lhBase(tenantID)+"/"+someReq+"/"+op, tok, map[string]any{"reason_code": "reviewed_ok", "seen_readiness_hash": strings.Repeat("a", 64)}); r.status != http.StatusForbidden {
				t.Errorf("%s %s: want 403, got %d %s", name, op, r.status, r.body)
			}
		}
		if r := w.do("POST", lhBase(tenantID)+"/"+someReq+"/"+op, "", nil); r.status != http.StatusUnauthorized {
			t.Errorf("anonymous %s: want 401, got %d", op, r.status)
		}
	}
	// Create / cancel: request permission. support / finance / player: 403.
	for name, tok := range map[string]string{"support": tokSup, "finance": tokFin, "player": player} {
		if r := w.do("POST", lhBase(tenantID), tok, lhActivationBody(&brand, jid, nil)); r.status != http.StatusForbidden {
			t.Errorf("%s create: want 403, got %d", name, r.status)
		}
		if r := w.do("POST", lhBase(tenantID)+"/"+someReq+"/cancel", tok, map[string]any{"reason_code": "requester_withdrew"}); r.status != http.StatusForbidden {
			t.Errorf("%s cancel: want 403, got %d", name, r.status)
		}
		if r := w.do("POST", base+"/brands/"+suspBrandTA.String()+"/suspend", tok, map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusForbidden {
			t.Errorf("%s suspend: want 403, got %d", name, r.status)
		}
	}
	// Tenant staff request AND suspend their OWN brand ...
	acks := w.nonPassing(tenantID, &brand, "activate", &jid)
	if r := w.do("POST", lhBase(tenantID), tokCo, lhActivationBody(&brand, jid, acks)); r.status != http.StatusCreated {
		t.Errorf("compliance request own brand: %d %s", r.status, r.body)
	}
	if r := w.do("POST", base+"/brands/"+suspBrandTA.String()+"/suspend", tokTA, map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusOK {
		t.Errorf("tenant_admin suspend own brand: %d %s", r.status, r.body)
	}
	if r := w.do("POST", base+"/brands/"+suspBrandCo.String()+"/suspend", tokCo, map[string]any{"reason_code": "compliance_hold"}); r.status != http.StatusOK {
		t.Errorf("compliance suspend own brand: %d %s", r.status, r.body)
	}
	// ... but never the tenant itself (platform only), nor a tenant subject request.
	if r := w.do("POST", base+"/suspend", tokTA, map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusForbidden {
		t.Errorf("tenant_admin suspend the tenant: want 403, got %d %s", r.status, r.body)
	}
	if got := w.status(tenantID, nil); got != "active" {
		t.Errorf("the tenant must be untouched, got %s", got)
	}
	tenantBody := lhActivationBody(nil, jid, nil)
	tenantBody["action"] = "close"
	delete(tenantBody, "licensing")
	if r := w.do("POST", lhBase(tenantID), tokTA, tenantBody); r.status != http.StatusForbidden {
		t.Errorf("tenant_admin request on the tenant subject: want 403, got %d %s", r.status, r.body)
	}
	// The platform-wide queue: platform only.
	for name, tok := range map[string]string{"tenant_admin": tokTA, "compliance": tokCo} {
		if r := w.do("GET", "/v1/admin/launch-authorisations", tok, nil); r.status != http.StatusForbidden {
			t.Errorf("%s platform queue: want 403, got %d %s", name, r.status, r.body)
		}
	}
	if r := w.do("GET", "/v1/admin/launch-authorisations", w.tokAp1(), nil); r.status != http.StatusOK {
		t.Errorf("platform queue: %d %s", r.status, r.body)
	}
	// A platform admin may suspend the tenant itself.
	tenant2 := w.activeTenant()
	if r := w.do("POST", "/v1/admin/tenants/"+tenant2.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusOK {
		t.Errorf("platform suspend tenant: %d %s", r.status, r.body)
	}
	if got := w.status(tenant2, nil); got != "suspended" {
		t.Errorf("tenant status: %s", got)
	}
}

// A request for tenant A's data with tenant B's token must fail: 404, never data and never a write.
func TestLaunchHTTP_CrossTenant_404(t *testing.T) {
	w := newLHWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	brandA, brandB := w.pendingBrand(a), w.pendingBrand(b)
	jidA := w.jurisdiction(a)
	taA, taB := w.staff(a, identity.StaffRoleTenantAdmin), w.staff(b, identity.StaffRoleTenantAdmin)
	tokA, tokB := w.tenantTok(a, taA, auth.RoleTenantAdmin), w.tenantTok(b, taB, auth.RoleTenantAdmin)
	acks := w.nonPassing(a, &brandA, "activate", &jidA)
	reqA := w.createRequest(tokA, a, lhActivationBody(&brandA, jidA, acks))

	pathA := "/v1/admin/tenants/" + a.String()
	pathB := "/v1/admin/tenants/" + b.String()
	// B's token on A's path: every route is 404 (indistinguishable from an unknown tenant).
	type call struct {
		method, path string
		body         any
	}
	for _, c := range []call{
		{"GET", pathA + "/launch-readiness?action=activate", nil},
		{"GET", pathA + "/launch-authorisations", nil},
		{"GET", pathA + "/launch-authorisations/" + reqA.Request.ID, nil},
		{"GET", pathA + "/launch-status-history", nil},
		{"POST", pathA + "/launch-authorisations", lhActivationBody(&brandA, jidA, acks)},
		{"POST", pathA + "/launch-authorisations/" + reqA.Request.ID + "/cancel", map[string]any{"reason_code": "requester_withdrew"}},
		{"POST", pathA + "/brands/" + brandA.String() + "/suspend", map[string]any{"reason_code": "operational_incident"}},
	} {
		r := w.do(c.method, c.path, tokB, c.body)
		if r.status != http.StatusNotFound || r.code(t) != "not_found" {
			t.Errorf("tenant B token on %s %s: want 404 not_found, got %d %s", c.method, c.path, r.status, r.body)
		}
	}
	// A's request id under B's own path: 404 for B's staff and for a platform admin alike.
	for name, tok := range map[string]string{"tenant B": tokB, "platform": w.tokAp1()} {
		if r := w.do("GET", pathB+"/launch-authorisations/"+reqA.Request.ID, tok, nil); r.status != http.StatusNotFound {
			t.Errorf("%s: A's request under B's path: want 404, got %d %s", name, r.status, r.body)
		}
		if r := w.decide(tok, b, reqA.Request.ID, "approve", "reviewed_ok", strings.Repeat("a", 64)); name == "platform" && r.status != http.StatusNotFound {
			t.Errorf("platform approve A's request under B's path: want 404, got %d %s", r.status, r.body)
		}
	}
	if r := w.do("POST", pathB+"/launch-authorisations/"+reqA.Request.ID+"/cancel", tokB, map[string]any{"reason_code": "requester_withdrew"}); r.status != http.StatusNotFound {
		t.Errorf("B cancel A's request: %d %s", r.status, r.body)
	}
	// A's brand id as a reference inside B's route tenant: 404 in the body, the query and the path.
	jidB := w.jurisdiction(b)
	if r := w.do("POST", pathB+"/launch-authorisations", tokB, lhActivationBody(&brandA, jidB, nil)); r.status != http.StatusNotFound {
		t.Errorf("A's brand in B's create body: want 404, got %d %s", r.status, r.body)
	}
	if r := w.do("GET", pathB+"/launch-readiness?action=activate&brand_id="+brandA.String(), tokB, nil); r.status != http.StatusNotFound {
		t.Errorf("A's brand in B's readiness query: want 404, got %d %s", r.status, r.body)
	}
	if r := w.do("POST", pathB+"/brands/"+brandA.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusNotFound {
		t.Errorf("platform suspend A's brand under B's path: want 404, got %d %s", r.status, r.body)
	}
	// B's list shows nothing of A's; A's request is untouched and its brand unchanged.
	lst := w.do("GET", pathB+"/launch-authorisations", tokB, nil)
	var items struct {
		Items []map[string]any `json:"items"`
	}
	lst.decode(t, &items)
	if lst.status != http.StatusOK || len(items.Items) != 0 {
		t.Errorf("B's list must be empty: %d %s", lst.status, lst.body)
	}
	if got := w.status(a, &brandA); got != "pending_launch" {
		t.Errorf("A's brand: %s", got)
	}
	if w.requestCount(b) != 0 || w.requestCount(a) != 1 {
		t.Errorf("request counts a=%d b=%d", w.requestCount(a), w.requestCount(b))
	}
	_ = brandB
	// An unknown tenant id for a platform admin: 404 (the subject does not exist).
	if r := w.do("POST", "/v1/admin/tenants/"+uuid.NewString()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusNotFound {
		t.Errorf("unknown tenant: want 404, got %d %s", r.status, r.body)
	}
}

// A body tenant_id (or brand_id on the suspend routes) is accepted and IGNORED: the subject is the
// validated route.
func TestLaunchHTTP_BodyTenantIDIgnored(t *testing.T) {
	w := newLHWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	brandA := w.pendingBrand(a)
	jidA := w.jurisdiction(a)
	taA := w.staff(a, identity.StaffRoleTenantAdmin)
	tokA := w.tenantTok(a, taA, auth.RoleTenantAdmin)
	acks := w.nonPassing(a, &brandA, "activate", &jidA)

	body := lhActivationBody(&brandA, jidA, acks)
	body["tenant_id"] = b.String()
	created := w.createRequest(tokA, a, body)
	if created.Request.TenantID != a.String() {
		t.Fatalf("a body tenant_id must be ignored: %+v", created.Request)
	}
	if w.requestCount(b) != 0 || w.requestCount(a) != 1 {
		t.Fatalf("request counts a=%d b=%d", w.requestCount(a), w.requestCount(b))
	}
	// A platform caller too.
	other := w.activeBrand(a)
	otherPending := w.activeBrand(b)
	r := w.do("POST", "/v1/admin/tenants/"+a.String()+"/brands/"+other.String()+"/suspend", w.tokReq(),
		map[string]any{"reason_code": "operational_incident", "tenant_id": b.String(), "brand_id": otherPending.String()})
	if r.status != http.StatusOK {
		t.Fatalf("suspend with ignored body ids: %d %s", r.status, r.body)
	}
	if w.status(a, &other) != "suspended" || w.status(b, &otherPending) != "active" {
		t.Fatalf("only the path brand may be suspended: a=%s b=%s", w.status(a, &other), w.status(b, &otherPending))
	}
	// An unknown body field is still rejected (strict decoding).
	if r := w.do("POST", "/v1/admin/tenants/"+a.String()+"/launch-authorisations", tokA, map[string]any{"action": "activate", "status": "active"}); r.status != http.StatusBadRequest {
		t.Fatalf("unknown field: want 400, got %d %s", r.status, r.body)
	}
}

// A process with no actor-proof issuer fails closed: 500, nothing written, no status change.
func TestLaunchHTTP_NoProofIssuer_FailsClosed500(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	active := w.activeBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	acks := w.nonPassing(tenantID, &brand, "activate", &jid)

	prev := actorproof.Default()
	actorproof.SetDefault(nil)
	defer actorproof.SetDefault(prev)

	r := w.do("POST", lhBase(tenantID), w.tokReq(), lhActivationBody(&brand, jid, acks))
	if r.status != http.StatusInternalServerError || r.code(t) != "internal_error" {
		t.Fatalf("create without an issuer: %d %s", r.status, r.body)
	}
	if strings.Contains(string(r.body), "proof") || strings.Contains(string(r.body), "issuer") {
		t.Fatalf("the 500 body must not describe the internal cause: %s", r.body)
	}
	s := w.do("POST", "/v1/admin/tenants/"+tenantID.String()+"/brands/"+active.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"})
	if s.status != http.StatusInternalServerError {
		t.Fatalf("suspend without an issuer: %d %s", s.status, s.body)
	}
	if w.requestCount(tenantID) != 0 || w.status(tenantID, &active) != "active" || w.status(tenantID, &brand) != "pending_launch" {
		t.Fatal("nothing may be written or changed without a proof")
	}
}

// Wiring: a process whose launch service is nil answers 503 and writes nothing.
func TestLaunchHTTP_ServiceNotWired_503(t *testing.T) {
	w := newLHWorld(t)
	w.serve(nil)
	tenantID := w.activeTenant()
	for _, c := range [][2]string{
		{"GET", "/v1/admin/tenants/" + tenantID.String() + "/launch-authorisations"},
		{"POST", "/v1/admin/tenants/" + tenantID.String() + "/suspend"},
		{"GET", "/v1/admin/launch-authorisations"},
	} {
		r := w.do(c[0], c[1], w.tokReq(), map[string]any{"reason_code": "operational_incident"})
		if r.status != http.StatusServiceUnavailable || r.code(t) != "LAUNCH_AUTHORISATION_UNAVAILABLE" {
			t.Errorf("%s %s: %d %s", c[0], c[1], r.status, r.body)
		}
	}
}

// Real-flow error mapping: each launchgov refusal reaches the wire as its closed LAUNCH_* code.
func TestLaunchHTTP_ErrorCodeMapping_RealFlows(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	jid := w.jurisdiction(tenantID)
	post := func(tok string, body map[string]any) cgResp { return w.do("POST", lhBase(tenantID), tok, body) }

	// H-3: a jurisdiction with no in-effect configuration -> LAUNCH_PRECONDITION_FAILED with the check code.
	brand := w.pendingBrand(tenantID)
	noJur := lhActivationBody(&brand, uuid.New(), nil)
	if r := post(w.tokReq(), noJur); r.status != http.StatusConflict || r.code(t) != "LAUNCH_PRECONDITION_FAILED" ||
		!contains(r.checkCodes(t), "H-3:config_missing_or_not_in_effect") {
		t.Fatalf("no jurisdiction: %d %s", r.status, r.body)
	}
	// H-2: a licence-backed model without a licence id.
	badLic := lhActivationBody(&brand, jid, nil)
	badLic["licensing"] = map[string]any{"model": launchgov.LicModelPlatform, "status": launchgov.LicStatusInForce}
	if r := post(w.tokReq(), badLic); r.status != http.StatusConflict || r.code(t) != "LAUNCH_PRECONDITION_FAILED" ||
		!contains(r.checkCodes(t), "H-2:licence_id_required") {
		t.Fatalf("licence id required: %d %s", r.status, r.body)
	}
	// 400s: malformed ids, unknown action, reason outside the closed set, over-long note.
	bad := lhActivationBody(&brand, jid, nil)
	bad["jurisdiction_ids"] = []string{"not-a-uuid"}
	if r := post(w.tokReq(), bad); r.status != http.StatusBadRequest || r.code(t) != "validation_error" {
		t.Fatalf("malformed jurisdiction id: %d %s", r.status, r.body)
	}
	bad = lhActivationBody(&brand, jid, nil)
	bad["brand_id"] = "x"
	if r := post(w.tokReq(), bad); r.status != http.StatusBadRequest {
		t.Fatalf("malformed brand id: %d %s", r.status, r.body)
	}
	bad = lhActivationBody(&brand, jid, nil)
	bad["action"] = "launch_everything"
	if r := post(w.tokReq(), bad); r.status != http.StatusBadRequest || r.code(t) != "validation_error" {
		t.Fatalf("unknown action: %d %s", r.status, r.body)
	}
	bad = lhActivationBody(&brand, jid, nil)
	bad["reason_code"] = "because"
	if r := post(w.tokReq(), bad); r.status != http.StatusBadRequest || r.code(t) != "validation_error" {
		t.Fatalf("reason outside the closed catalogue: %d %s", r.status, r.body)
	}
	bad = lhActivationBody(&brand, jid, nil)
	bad["conditions_note"] = "call me on +49 170 1234567 or mail a.b@example.com"
	if r := post(w.tokReq(), bad); r.status != http.StatusBadRequest {
		t.Fatalf("PII in the note: %d %s", r.status, r.body)
	}
	if strings.Contains(string(post(w.tokReq(), bad).body), "example.com") {
		t.Fatal("a validation error must not echo the offending content")
	}
	if n := w.requestCount(tenantID); n != 0 {
		t.Fatalf("refused requests write nothing, got %d", n)
	}

	// Illegal transition / closed subject: activating an ACTIVE brand; anything on a closed brand.
	active := w.activeBrand(tenantID)
	if r := post(w.tokReq(), lhActivationBody(&active, jid, nil)); r.status != http.StatusConflict ||
		(r.code(t) != "LAUNCH_ILLEGAL_TRANSITION" && r.code(t) != "LAUNCH_PRECONDITION_FAILED") {
		t.Fatalf("activate an active brand: %d %s", r.status, r.body)
	}
	closed := w.activeBrand(tenantID)
	forceClosed(t, w, tenantID, &closed)
	if r := post(w.tokReq(), lhActivationBody(&closed, jid, nil)); r.status != http.StatusConflict ||
		(r.code(t) != "LAUNCH_SUBJECT_CLOSED" && r.code(t) != "LAUNCH_PRECONDITION_FAILED") {
		t.Fatalf("a closed brand: %d %s", r.status, r.body)
	}
	if r := w.do("POST", "/v1/admin/tenants/"+tenantID.String()+"/brands/"+closed.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusConflict || r.code(t) != "LAUNCH_SUBJECT_CLOSED" {
		t.Fatalf("suspend a closed brand: %d %s", r.status, r.body)
	}
	// Suspend an already pending_launch brand: illegal transition.
	if r := w.do("POST", "/v1/admin/tenants/"+tenantID.String()+"/brands/"+brand.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "operational_incident"}); r.status != http.StatusConflict || r.code(t) != "LAUNCH_ILLEGAL_TRANSITION" {
		t.Fatalf("suspend a pending_launch brand: %d %s", r.status, r.body)
	}
	// Suspend reason rules: outside the closed set, other_documented without a note.
	if r := w.do("POST", "/v1/admin/tenants/"+tenantID.String()+"/brands/"+active.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "bored"}); r.status != http.StatusBadRequest {
		t.Fatalf("suspend reason: %d %s", r.status, r.body)
	}
	if r := w.do("POST", "/v1/admin/tenants/"+tenantID.String()+"/brands/"+active.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "other_documented"}); r.status != http.StatusBadRequest {
		t.Fatalf("other_documented without a note: %d %s", r.status, r.body)
	}

	// LAUNCH_NOT_LEGACY_BASELINE: ratify a subject that is not a legacy baseline (an owner-provisioned fixture).
	rat := lhActivationBody(&active, jid, w.nonPassing(tenantID, &active, "ratify", &jid))
	rat["action"], rat["reason_code"] = "ratify", "legacy_ratification"
	if r := post(w.tokReq(), rat); r.status != http.StatusConflict || r.code(t) != "LAUNCH_NOT_LEGACY_BASELINE" {
		t.Fatalf("ratify a non-baseline subject: %d %s", r.status, r.body)
	}

	// LAUNCH_STALE_READINESS: approving with a hash that is not the current one records nothing.
	pb := w.pendingBrand(tenantID)
	c := w.createRequest(w.tokReq(), tenantID, lhActivationBody(&pb, jid, w.nonPassing(tenantID, &pb, "activate", &jid)))
	if r := w.decide(w.tokAp1(), tenantID, c.Request.ID, "approve", "reviewed_ok", strings.Repeat("0", 64)); r.status != http.StatusConflict || r.code(t) != "LAUNCH_STALE_READINESS" {
		t.Fatalf("stale readiness: %d %s", r.status, r.body)
	}
	// Approve without the hash, with an unknown reason: 400.
	if r := w.decide(w.tokAp1(), tenantID, c.Request.ID, "approve", "reviewed_ok", ""); r.status != http.StatusBadRequest {
		t.Fatalf("approve without the readiness hash: %d %s", r.status, r.body)
	}
	if r := w.decide(w.tokAp1(), tenantID, c.Request.ID, "approve", "nope", strings.Repeat("0", 64)); r.status != http.StatusBadRequest {
		t.Fatalf("approve reason outside the closed set: %d %s", r.status, r.body)
	}
	g := w.do("GET", lhBase(tenantID)+"/"+c.Request.ID, w.tokAp1(), nil)
	var got lhRequestResp
	g.decode(t, &got)
	if got.Request.Status != "pending" || len(got.Request.Approvals) != 0 {
		t.Fatalf("refused decisions record nothing: %s", g.body)
	}
	// Malformed path ids and query values.
	if r := w.do("GET", lhBase(tenantID)+"/not-a-uuid", w.tokAp1(), nil); r.status != http.StatusBadRequest {
		t.Fatalf("malformed request id: %d", r.status)
	}
	if r := w.do("GET", "/v1/admin/tenants/not-a-uuid/launch-authorisations", w.tokAp1(), nil); r.status != http.StatusBadRequest {
		t.Fatalf("malformed tenant id: %d", r.status)
	}
	for _, q := range []string{"?status=bogus", "?limit=0", "?limit=9999", "?limit=x", "?brand_id=zzz"} {
		if r := w.do("GET", lhBase(tenantID)+q, w.tokAp1(), nil); r.status != http.StatusBadRequest {
			t.Fatalf("query %s: %d %s", q, r.status, r.body)
		}
	}
	if r := w.do("GET", "/v1/admin/tenants/"+tenantID.String()+"/launch-readiness?action=nope", w.tokAp1(), nil); r.status != http.StatusBadRequest {
		t.Fatalf("readiness unknown action: %d %s", r.status, r.body)
	}
	if r := w.do("GET", "/v1/admin/tenants/"+tenantID.String()+"/launch-readiness", w.tokAp1(), nil); r.status != http.StatusBadRequest {
		t.Fatalf("readiness without an action: %d %s", r.status, r.body)
	}
	if r := w.do("GET", "/v1/admin/tenants/"+tenantID.String()+"/launch-readiness?action=close&jurisdiction_ids=zz", w.tokAp1(), nil); r.status != http.StatusBadRequest {
		t.Fatalf("readiness bad jurisdiction ids: %d %s", r.status, r.body)
	}
}

// H-6 over HTTP: closing a brand with a hold-bearing withdrawal is refused with the closed code and
// the check code; resolving the request (rejecting the control withdrawal) lets the closure through
// the full two-person flow, and the closed brand then stays closed (terminal).
func TestLaunchHTTP_Close_WithdrawalsHolding_ThenClosed(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.activeBrand(tenantID)
	player, wallet := w.playerWithWallet(tenantID, brand)
	w.withdrawal(tenantID, brand, player, wallet, "requested")

	closeBody := map[string]any{"action": "close", "brand_id": brand.String(), "reason_code": "offboarding",
		"acknowledged_check_codes": w.nonPassing(tenantID, &brand, "close", nil)}
	r := w.do("POST", lhBase(tenantID), w.tokReq(), closeBody)
	if r.status != http.StatusConflict || r.code(t) != "LAUNCH_WITHDRAWALS_HOLDING_FUNDS" || !contains(r.checkCodes(t), "H-6:withdrawals_holding_funds") {
		t.Fatalf("close over a hold: %d %s", r.status, r.body)
	}
	if w.requestCount(tenantID) != 0 || w.status(tenantID, &brand) != "active" {
		t.Fatal("a refused closure writes nothing")
	}
	// Resolve the control withdrawal (a terminal state), then the closure runs.
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected' WHERE tenant_id = $1`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("resolve the control withdrawal: %v", err)
	}
	closeBody["acknowledged_check_codes"] = w.nonPassing(tenantID, &brand, "close", nil)
	c := w.createRequest(w.tokReq(), tenantID, closeBody)
	if c.Request.RequiredApprovals != 2 {
		t.Fatalf("closure needs 2 approvals: %+v", c.Request)
	}
	if x := w.approve(w.tokAp1(), tenantID, c.Request.ID); x.status != http.StatusOK {
		t.Fatalf("approve 1: %d %s", x.status, x.body)
	}
	if x := w.approve(w.tokAp2(), tenantID, c.Request.ID); x.status != http.StatusOK {
		t.Fatalf("approve 2: %d %s", x.status, x.body)
	}
	if got := w.status(tenantID, &brand); got != "closed" {
		t.Fatalf("closed expected, got %s", got)
	}
	// Terminal: nothing reopens it.
	if r := w.do("POST", lhBase(tenantID), w.tokReq(), lhActivationBody(&brand, w.jurisdiction(tenantID), nil)); r.status != http.StatusConflict {
		t.Fatalf("reopen a closed brand: %d %s", r.status, r.body)
	}
}

// Suspension: one authorised person, immediate, supersedes a pending request (audit rows), and the
// reactivation is always four-eyes: tenant staff can request it but never approve it.
func TestLaunchHTTP_Suspend_Supersedes_ReactivationNeedsApproval(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.activeBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	ta := w.staff(tenantID, identity.StaffRoleTenantAdmin)
	tt := w.tenantTok(tenantID, ta, auth.RoleTenantAdmin)

	closeBody := map[string]any{"action": "close", "brand_id": brand.String(), "reason_code": "operator_request",
		"acknowledged_check_codes": w.nonPassing(tenantID, &brand, "close", nil)}
	pend := w.createRequest(w.tokReq(), tenantID, closeBody)

	s := w.do("POST", "/v1/admin/tenants/"+tenantID.String()+"/brands/"+brand.String()+"/suspend", tt, map[string]any{"reason_code": "operational_incident"})
	if s.status != http.StatusOK {
		t.Fatalf("suspend: %d %s", s.status, s.body)
	}
	lhRequireDisclaimer(t, s)
	var sr lhRequestResp
	s.decode(t, &sr)
	if !sr.Executed || sr.Request.Action != "suspend" || sr.Request.RequiredApprovals != 0 || len(sr.Superseded) != 1 || sr.Superseded[0] != pend.Request.ID {
		t.Fatalf("suspend response: %s", s.body)
	}
	if got := w.status(tenantID, &brand); got != "suspended" {
		t.Fatalf("brand status: %s", got)
	}
	g := w.do("GET", lhBase(tenantID)+"/"+pend.Request.ID, w.tokReq(), nil)
	var pr lhRequestResp
	g.decode(t, &pr)
	if pr.Request.Status != "superseded" {
		t.Fatalf("the pending closure must be superseded: %s", g.body)
	}
	for _, ev := range []string{"suspended", "superseded"} {
		if n := w.auditCount(tenantID, "launch_authorisation."+ev); n != 1 {
			t.Fatalf("audit %s: %d", ev, n)
		}
	}
	// The suspension readiness carries the distinct funds notice when funds are present.
	// Reactivation: tenant staff request, platform approves; tenant staff cannot approve.
	acks := w.nonPassing(tenantID, &brand, "reactivate", &jid)
	body := lhActivationBody(&brand, jid, acks)
	body["action"], body["reason_code"] = "reactivate", "issue_resolved"
	c := w.createRequest(tt, tenantID, body)
	if x := w.decide(tt, tenantID, c.Request.ID, "approve", "reviewed_ok", strings.Repeat("a", 64)); x.status != http.StatusForbidden {
		t.Fatalf("tenant staff reactivate alone: %d %s", x.status, x.body)
	}
	if x := w.approve(w.tokAp1(), tenantID, c.Request.ID); x.status != http.StatusOK {
		t.Fatalf("approve reactivation: %d %s", x.status, x.body)
	}
	if got := w.status(tenantID, &brand); got != "active" {
		t.Fatalf("brand status after reactivation: %s", got)
	}
	// Review condition (e): create with action=suspend is refused (400, pointing at the suspend routes);
	// it must not suspend anything.
	again := w.do("POST", lhBase(tenantID), w.tokReq(), map[string]any{"action": "suspend", "brand_id": brand.String(), "reason_code": "compliance_hold"})
	if again.status != http.StatusBadRequest || again.code(t) != "validation_error" || !strings.Contains(string(again.body), "suspend routes") {
		t.Fatalf("create with action=suspend must be a 400: %d %s", again.status, again.body)
	}
	if got := w.status(tenantID, &brand); got != "active" {
		t.Fatalf("brand status after the refused create: %s", got)
	}
}

// Reads: list filters, platform-wide queue across tenants, history of the tenant.
func TestLaunchHTTP_ReadsListQueueHistory(t *testing.T) {
	w := newLHWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	brandA, brandB := w.pendingBrand(a), w.pendingBrand(b)
	jidA, jidB := w.jurisdiction(a), w.jurisdiction(b)
	w.createRequest(w.tokReq(), a, lhActivationBody(&brandA, jidA, w.nonPassing(a, &brandA, "activate", &jidA)))
	rb := w.createRequest(w.tokReq(), b, lhActivationBody(&brandB, jidB, w.nonPassing(b, &brandB, "activate", &jidB)))
	if r := w.do("POST", lhBase(b)+"/"+rb.Request.ID+"/cancel", w.tokReq(), map[string]any{"reason_code": "requester_withdrew"}); r.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}

	type list struct {
		Items []struct {
			ID       string  `json:"id"`
			TenantID string  `json:"tenant_id"`
			Status   string  `json:"status"`
			BrandID  *string `json:"brand_id"`
		} `json:"items"`
		Disclaimer string `json:"disclaimer"`
	}
	get := func(path, tok string) list {
		r := w.do("GET", path, tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, r.status, r.body)
		}
		lhRequireDisclaimer(t, r)
		var l list
		r.decode(t, &l)
		return l
	}
	if l := get(lhBase(a), w.tokAp1()); len(l.Items) != 1 || l.Items[0].TenantID != a.String() || l.Items[0].Status != "pending" {
		t.Fatalf("tenant A list: %+v", l)
	}
	if l := get(lhBase(a)+"?status=executed", w.tokAp1()); len(l.Items) != 0 {
		t.Fatalf("status filter: %+v", l)
	}
	if l := get(lhBase(a)+"?brand_id="+brandA.String()+"&status=pending,executed&limit=5", w.tokAp1()); len(l.Items) != 1 {
		t.Fatalf("brand + status filter: %+v", l)
	}
	if l := get(lhBase(b)+"?status=cancelled", w.tokAp1()); len(l.Items) != 1 {
		t.Fatalf("cancelled filter: %+v", l)
	}
	// The queue sees pending requests across tenants for platform staff.
	q := get("/v1/admin/launch-authorisations?status=pending&limit=500", w.tokAp1())
	seenA := false
	for _, it := range q.Items {
		if it.Status != "pending" {
			t.Fatalf("queue status filter: %+v", it)
		}
		if it.TenantID == a.String() {
			seenA = true
		}
		if it.TenantID == b.String() {
			t.Fatalf("B's only request is cancelled: %+v", it)
		}
	}
	if !seenA {
		t.Fatalf("the platform queue must include tenant A's pending request: %+v", q)
	}
	// History of a tenant with no governed change is empty, never another tenant's.
	h := w.do("GET", "/v1/admin/tenants/"+a.String()+"/launch-status-history", w.tokAp1(), nil)
	var hist struct {
		Items []map[string]any `json:"items"`
	}
	h.decode(t, &hist)
	for _, it := range hist.Items {
		if it["tenant_id"] != nil && it["tenant_id"] != a.String() {
			t.Fatalf("history leaks: %s", h.body)
		}
	}
}

// Race through HTTP: a suspension against the FINAL approval of a pending closure of the same
// brand. Whatever the order, the outcome is consistent: either the brand is closed and the
// suspension was refused (illegal/closed), or it is suspended and the approval was refused (not
// pending); never a 5xx, never a stale pending request.
func TestLaunchHTTP_Race_SuspendVsFinalApproval(t *testing.T) {
	w := newLHWorld(t)
	suspendedWins, closeWins := 0, 0
	for i := 0; i < 12; i++ {
		tenantID := w.activeTenant()
		brand := w.activeBrand(tenantID)
		body := map[string]any{"action": "close", "brand_id": brand.String(), "reason_code": "offboarding",
			"acknowledged_check_codes": w.nonPassing(tenantID, &brand, "close", nil)}
		c := w.createRequest(w.tokReq(), tenantID, body)
		if x := w.approve(w.tokAp1(), tenantID, c.Request.ID); x.status != http.StatusOK {
			t.Fatalf("first approval: %d %s", x.status, x.body)
		}
		seen := w.seenHash(w.tokAp2(), tenantID, c.Request.ID)

		var wg sync.WaitGroup
		var approve, suspend cgResp
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			approve = w.decide(w.tokAp2(), tenantID, c.Request.ID, "approve", "reviewed_ok", seen)
		}()
		go func() {
			defer wg.Done()
			<-start
			suspend = w.do("POST", "/v1/admin/tenants/"+tenantID.String()+"/brands/"+brand.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "security_incident"})
		}()
		close(start)
		wg.Wait()

		final := w.status(tenantID, &brand)
		for name, r := range map[string]cgResp{"approve": approve, "suspend": suspend} {
			if r.status >= 500 {
				t.Fatalf("iteration %d: %s answered %d %s", i, name, r.status, r.body)
			}
		}
		switch final {
		case "closed":
			closeWins++
			if approve.status != http.StatusOK || suspend.status != http.StatusConflict {
				t.Fatalf("iteration %d closed: approve=%d %s suspend=%d %s", i, approve.status, approve.body, suspend.status, suspend.body)
			}
			if c := suspend.code(t); c != "LAUNCH_SUBJECT_CLOSED" && c != "LAUNCH_ILLEGAL_TRANSITION" {
				t.Fatalf("iteration %d: suspend code %s", i, c)
			}
		case "suspended":
			suspendedWins++
			if suspend.status != http.StatusOK || approve.status != http.StatusConflict {
				t.Fatalf("iteration %d suspended: approve=%d %s suspend=%d %s", i, approve.status, approve.body, suspend.status, suspend.body)
			}
			if c := approve.code(t); c != "LAUNCH_REQUEST_NOT_PENDING" {
				t.Fatalf("iteration %d: approve code %s", i, c)
			}
			g := w.do("GET", lhBase(tenantID)+"/"+c.Request.ID, w.tokReq(), nil)
			var pr lhRequestResp
			g.decode(t, &pr)
			if pr.Request.Status != "superseded" {
				t.Fatalf("iteration %d: the closure must be superseded, got %s", i, pr.Request.Status)
			}
		default:
			t.Fatalf("iteration %d: unexpected final status %s", i, final)
		}
	}
	t.Logf("race outcomes over 12 rounds: suspension won %d, closure won %d", suspendedWins, closeWins)
}
