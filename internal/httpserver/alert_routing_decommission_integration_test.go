//go:build integration

package httpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// SR9-A9-3 at the HTTP layer: a route that admin B DISABLED for another reason (a decommission) is not a
// four-eyes staging record, so admin A cannot re-enable it alone; only a version staged with
// stage_for_enable=true can be approved, and the flag is only valid on a disabled email/webhook version.
func TestAlertRoutingAPI_DecommissionedRouteCannotBeReEnabledByOneAdmin(t *testing.T) {
	a := newARAPI(t, true)
	tokA := a.adminToken()
	adminB := a.w.SeedAdmin()
	tokB := a.token(adminB, uuid.Nil, auth.RolePlatformAdmin)
	const recipient = "interim-owner"

	// Legitimate flow: A stages with intent, B enables.
	r := a.do("POST", "/v1/admin/alerting/routes", tokA, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": recipient, "enabled": false, "stage_for_enable": true,
	}))
	if r.status != http.StatusCreated {
		t.Fatalf("stage: %d %s", r.status, r.body)
	}
	staged := r.json()["id"].(string)
	r = a.do("POST", "/v1/admin/alerting/routes", tokB, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": recipient, "enabled": true,
		"supersedes_id": staged, "reason_code": "on_call_change",
	}))
	if r.status != http.StatusCreated {
		t.Fatalf("enable: %d %s", r.status, r.body)
	}
	live := r.json()["id"].(string)

	// B decommissions it: a disabled version, reason decommission, NO staging intent.
	r = a.do("POST", "/v1/admin/alerting/routes", tokB, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": recipient, "enabled": false,
		"supersedes_id": live, "reason_code": "decommission",
	}))
	if r.status != http.StatusCreated {
		t.Fatalf("decommission: %d %s", r.status, r.body)
	}
	decom := r.json()["id"].(string)

	// A alone re-enables the identical configuration against B's decommission row: refused (SR-7).
	r = a.do("POST", "/v1/admin/alerting/routes", tokA, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": recipient, "enabled": true,
		"supersedes_id": decom, "reason_code": "correction",
	}))
	if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "SR-7") {
		t.Fatalf("re-enabling a decommission row alone: %d %s", r.status, r.body)
	}

	// The intent flag is refused on an enabled route and on a non-human channel.
	for name, body := range map[string]map[string]any{
		"enabled":   {"severity": "p2", "channel_kind": "email", "recipient_ref": recipient, "enabled": true, "stage_for_enable": true},
		"non-human": {"severity": "p2", "channel_kind": "log", "recipient_ref": recipient, "enabled": false, "stage_for_enable": true},
	} {
		r = a.do("POST", "/v1/admin/alerting/routes", tokA, routeBody(body))
		if r.status != http.StatusBadRequest {
			t.Errorf("stage_for_enable on %s route: %d %s", name, r.status, r.body)
		}
	}
}
