//go:build integration

package httpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// ADR 0112 8.2 / 15 and ADR 0104: the tenant sees a platform-scope launch decision about its own
// brand in its platform-actions projection WITH the closed reason code and status before/after, and
// with nothing else (no readiness hash, no requester / approver ids, no ip / request id); a refusal
// row is columns only. Another tenant sees none of it.
func TestLaunchHTTP_TenantPlatformActionsProjection_ReasonAndBeforeAfterOnly(t *testing.T) {
	w := newLHWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	brand := w.activeBrand(a)
	closed := w.activeBrand(a)
	forceClosed(t, w, a, &closed)
	taA := w.tenantTok(a, w.staff(a, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)
	taB := w.tenantTok(b, w.staff(b, identity.StaffRoleTenantAdmin), auth.RoleTenantAdmin)

	if r := w.do("POST", "/v1/admin/tenants/"+a.String()+"/brands/"+brand.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "licence_issue"}); r.status != http.StatusOK {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	// A refusal (the brand is closed): a denied row, platform scope, about tenant A.
	if r := w.do("POST", "/v1/admin/tenants/"+a.String()+"/brands/"+closed.String()+"/suspend", w.tokReq(), map[string]any{"reason_code": "licence_issue"}); r.status != http.StatusConflict {
		t.Fatalf("suspend closed: %d %s", r.status, r.body)
	}

	type item struct {
		Action     string         `json:"action"`
		Outcome    string         `json:"outcome"`
		ReasonCode string         `json:"reason_code"`
		Before     any            `json:"before"`
		After      any            `json:"after"`
		Extra      map[string]any `json:"extra_metadata"`
	}
	var page struct {
		Items []item `json:"items"`
	}
	r := w.do("GET", "/v1/admin/audit-log/platform-actions", taA, nil)
	if r.status != http.StatusOK {
		t.Fatalf("projection: %d %s", r.status, r.body)
	}
	r.decode(t, &page)
	var sawSuspended, sawDenied bool
	for _, it := range page.Items {
		switch it.Action {
		case "launch_authorisation.suspended":
			sawSuspended = true
			if it.ReasonCode != "licence_issue" || it.Before != "active" || it.After != "suspended" || len(it.Extra) != 0 {
				t.Errorf("suspended row: %+v", it)
			}
		case "launch_authorisation.suspend_denied":
			sawDenied = true
			if it.Outcome != "denied" || it.ReasonCode != "" || it.Before != nil || it.After != nil || len(it.Extra) != 0 {
				t.Errorf("a refusal row must project the base columns only: %+v", it)
			}
		}
	}
	if !sawSuspended || !sawDenied {
		t.Fatalf("suspended=%v denied=%v in %s", sawSuspended, sawDenied, r.body)
	}
	for _, forbidden := range []string{"ip_address", "user_agent", "request_id", "readiness_snapshot_hash", "requested_by", "executed_by", "approver", "SQLSTATE", "payload_hash"} {
		if strings.Contains(string(r.body), forbidden) {
			t.Errorf("the tenant projection must not contain %q: %s", forbidden, r.body)
		}
	}
	// Tenant B sees none of tenant A's launch rows.
	rb := w.do("GET", "/v1/admin/audit-log/platform-actions", taB, nil)
	if rb.status != http.StatusOK || strings.Contains(string(rb.body), "launch_authorisation") {
		t.Fatalf("tenant B must not see tenant A's launch events: %d %s", rb.status, rb.body)
	}
}
