//go:build integration

package httpserver

// ADR 0119 S4b section 17.2 "modal retry safety" (API half; the back-office half is S7b): on a four-eyes approve
// route (payments kill-switch release, class critical) a stale step-up is refused before the handler - no state
// change, no audit row of the approval (success or denied) - the single re-send after a step-up produces exactly one
// effect, and a second re-send gets the route's normal state-based 409.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
)

func TestStepUp_ModalRetrySafety_FourEyesApprove(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	adminA := a.tenantStaff(tenant, "tenant_admin")
	adminB := a.tenantStaff(tenant, "tenant_admin")
	tokA := a.token(adminA, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	staleB := authtest.LiveStaffTokenAged(t, a.pool, testStaffMFAKeys(), a.issuer, adminB, tenant, auth.RoleTenantAdmin, 11*time.Minute)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	engage := a.do("POST", base+"/kill-switches", tokA, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "incident_s4b"})
	if engage.status != http.StatusOK {
		t.Fatalf("engage: %d %s", engage.status, engage.body)
	}
	var ks killSwitchDTO
	engage.decode(t, &ks)
	req := a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tokA, map[string]any{"reason_code": "resolved"})
	if req.status != http.StatusCreated {
		t.Fatalf("release request: %d %s", req.status, req.body)
	}
	var rel killSwitchReleaseRequestDTO
	req.decode(t, &rel)
	path := base + "/kill-switch-release-requests/" + rel.ID + "/approve"

	refused := a.do("POST", path, staleB, nil)
	if refused.status != http.StatusForbidden || !strings.Contains(string(refused.body), `"code":"step_up_required"`) {
		t.Fatalf("stale step-up: %d %s", refused.status, refused.body)
	}
	if a.auditCount(tenant, "payments_kill_switch.approve_release") != 0 {
		t.Fatal("a step-up refusal must write no approval audit row (it happens before the handler)")
	}
	get := a.do("GET", base+"/kill-switches/"+ks.ID, tokA, nil)
	var still killSwitchDTO
	get.decode(t, &still)
	if !still.Engaged {
		t.Fatal("a step-up refusal must change nothing")
	}

	// The modal's single re-send with a fresh step-up: exactly one effect.
	freshB := authtest.LiveStaffToken(t, a.pool, testStaffMFAKeys(), a.issuer, adminB, tenant, auth.RoleTenantAdmin)
	ok := a.do("POST", path, freshB, nil)
	if ok.status != http.StatusOK {
		t.Fatalf("re-send after step-up: %d %s", ok.status, ok.body)
	}
	if a.auditCountByOutcome(tenant, "payments_kill_switch.approve_release", "success") != 1 {
		t.Fatal("exactly one successful approval")
	}
	// A second re-send: the route's own state-based answer.
	again := a.do("POST", path, freshB, nil)
	if again.status != http.StatusConflict {
		t.Fatalf("a second re-send: %d %s, want 409", again.status, again.body)
	}
	if a.auditCountByOutcome(tenant, "payments_kill_switch.approve_release", "success") != 1 {
		t.Fatal("the second re-send must not produce a second effect")
	}
}
