//go:build integration

package httpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// Review condition (g): the licensing vocabularies, the jurisdiction bounds and the format-character
// refusal are enforced at the wire.
func TestLaunchHTTP_CreateShape_LicensingEnumsAndJurisdictionBounds(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	cases := map[string]func(b map[string]any){
		"unknown licensing model":  func(b map[string]any) { b["licensing"].(map[string]any)["model"] = "made_up" },
		"unknown licensing status": func(b map[string]any) { b["licensing"].(map[string]any)["status"] = "mostly_fine" },
		"no jurisdiction ids":      func(b map[string]any) { delete(b, "jurisdiction_ids") },
		"empty jurisdiction ids":   func(b map[string]any) { b["jurisdiction_ids"] = []string{} },
		"33 jurisdiction ids": func(b map[string]any) {
			ids := make([]string, 33)
			for i := range ids {
				ids[i] = uuid.NewString()
			}
			b["jurisdiction_ids"] = ids
		},
		"bidi override in the note": func(b map[string]any) { b["conditions_note"] = "approved‮ by counsel" },
	}
	for name, mutate := range cases {
		body := lhActivationBody(&brand, jid, nil)
		mutate(body)
		if r := w.do("POST", lhBase(tenantID), w.tokReq(), body); r.status != http.StatusBadRequest || r.code(t) != "validation_error" {
			t.Errorf("%s: want 400 validation_error, got %d %s", name, r.status, r.body)
		}
	}
	if n := w.requestCount(tenantID); n != 0 {
		t.Fatalf("refused requests write nothing, got %d", n)
	}
}

// Review conditions (c) and (h): a tenant-scoped caller never sees platform staff ids (requester,
// approvers, executor) nor the environment name and process-internal readiness details; the platform
// caller still does; the tenant's own staff id stays visible to it.
func TestLaunchHTTP_TenantView_HidesPlatformStaffIdentities_AndCoarsensReadiness(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	brand := w.pendingBrand(tenantID)
	jid := w.jurisdiction(tenantID)
	ta := w.staff(tenantID, identity.StaffRoleTenantAdmin)
	tt := w.tenantTok(tenantID, ta, auth.RoleTenantAdmin)
	acks := w.nonPassing(tenantID, &brand, "activate", &jid)

	// A platform-filed request, approved by a platform approver.
	c := w.createRequest(w.tokReq(), tenantID, lhActivationBody(&brand, jid, acks))
	if x := w.approve(w.tokAp1(), tenantID, c.Request.ID); x.status != http.StatusOK {
		t.Fatalf("approve: %d %s", x.status, x.body)
	}
	for _, id := range []string{w.ops.Requester.String(), w.ops.Approver1.String(), w.ops.Approver2.String()} {
		for _, path := range []string{
			lhBase(tenantID) + "/" + c.Request.ID, lhBase(tenantID),
			"/v1/admin/tenants/" + tenantID.String() + "/launch-status-history",
		} {
			r := w.do("GET", path, tt, nil)
			if r.status != http.StatusOK || strings.Contains(string(r.body), id) {
				t.Errorf("a tenant caller must not see platform staff id %s in GET %s: %d %s", id, path, r.status, r.body)
			}
		}
	}
	// The platform caller does see them.
	pr := w.do("GET", lhBase(tenantID)+"/"+c.Request.ID, w.tokAp2(), nil)
	if !strings.Contains(string(pr.body), w.ops.Requester.String()) || !strings.Contains(string(pr.body), w.ops.Approver1.String()) {
		t.Errorf("the platform caller sees the requester and the approver: %s", pr.body)
	}
	ph := w.do("GET", "/v1/admin/tenants/"+tenantID.String()+"/launch-status-history", w.tokAp2(), nil)
	if !strings.Contains(string(ph.body), w.ops.Approver1.String()) {
		t.Errorf("the platform caller sees the approver ids in the history: %s", ph.body)
	}
	// The tenant's OWN staff id is shown to it on its own request.
	pb := w.pendingBrand(tenantID)
	own := w.createRequest(tt, tenantID, lhActivationBody(&pb, jid, w.nonPassing(tenantID, &pb, "activate", &jid)))
	g := w.do("GET", lhBase(tenantID)+"/"+own.Request.ID, tt, nil)
	if !strings.Contains(string(g.body), ta.String()) {
		t.Errorf("a tenant sees its own staff id on its own request: %s", g.body)
	}
	// Readiness: coarse for the tenant, full for the platform; the hash is identical.
	tr := w.do("GET", lhReadinessPath(tenantID, &pb, "activate", &jid), tt, nil)
	pr2 := w.do("GET", lhReadinessPath(tenantID, &pb, "activate", &jid), w.tokReq(), nil)
	type rd struct {
		Readiness struct {
			Environment   string `json:"environment"`
			ReadinessHash string `json:"readiness_hash"`
			Checks        []struct {
				Code       string `json:"code"`
				DetailCode string `json:"detail_code"`
			} `json:"checks"`
		} `json:"readiness"`
	}
	var trd, prd rd
	tr.decode(t, &trd)
	pr2.decode(t, &prd)
	if trd.Readiness.Environment != "redacted" || prd.Readiness.Environment != "test" {
		t.Errorf("environment: tenant=%q platform=%q", trd.Readiness.Environment, prd.Readiness.Environment)
	}
	if trd.Readiness.ReadinessHash != prd.Readiness.ReadinessHash {
		t.Errorf("the readiness hash must not depend on the viewer")
	}
	for _, ck := range trd.Readiness.Checks {
		if (ck.Code == "R-ALERTS" || ck.Code == "R-PROVIDER-TIER") && ck.DetailCode != "see_platform" {
			t.Errorf("%s detail for a tenant: %q", ck.Code, ck.DetailCode)
		}
	}
	sawInternal := false
	for _, ck := range prd.Readiness.Checks {
		if (ck.Code == "R-ALERTS" || ck.Code == "R-PROVIDER-TIER") && ck.DetailCode != "see_platform" {
			sawInternal = true
		}
	}
	if !sawInternal {
		t.Errorf("the platform caller keeps the detailed codes")
	}
}

// Review condition (a): the tenant's platform-actions view of an APPROVAL or a REJECTION shows the
// DECIDER's reason code, never the requester's reason; nothing else leaks.
func TestLaunchHTTP_TenantProjection_ApprovalAndRejection_ShowTheDecisionReason(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	jid := w.jurisdiction(tenantID)
	taTok := w.tenantTok(tenantID, w.staff(tenantID, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)
	b1, b2 := w.pendingBrand(tenantID), w.pendingBrand(tenantID)

	rej := w.createRequest(w.tokReq(), tenantID, lhActivationBody(&b1, jid, w.nonPassing(tenantID, &b1, "activate", &jid)))
	if r := w.decide(w.tokAp1(), tenantID, rej.Request.ID, "reject", "readiness_concerns", ""); r.status != http.StatusOK {
		t.Fatalf("reject: %d %s", r.status, r.body)
	}
	appr := w.createRequest(w.tokReq(), tenantID, lhActivationBody(&b2, jid, w.nonPassing(tenantID, &b2, "activate", &jid)))
	if r := w.approve(w.tokAp1(), tenantID, appr.Request.ID); r.status != http.StatusOK {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}

	var page struct {
		Items []struct {
			Action     string `json:"action"`
			ReasonCode string `json:"reason_code"`
		} `json:"items"`
	}
	r := w.do("GET", "/v1/admin/audit-log/platform-actions", taTok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("projection: %d %s", r.status, r.body)
	}
	r.decode(t, &page)
	got := map[string]string{}
	for _, it := range page.Items {
		got[it.Action] = it.ReasonCode
	}
	if got["launch_authorisation.rejected"] != "readiness_concerns" {
		t.Errorf("the rejection must show the decider's reason, got %q (items %s)", got["launch_authorisation.rejected"], r.body)
	}
	if got["launch_authorisation.approved"] != "reviewed_ok" {
		t.Errorf("the approval must show the decider's reason, got %q", got["launch_authorisation.approved"])
	}
	if got["launch_authorisation.requested"] != "initial_launch" {
		t.Errorf("the request keeps the requester's reason, got %q", got["launch_authorisation.requested"])
	}
}
