//go:build integration

package httpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// A9 / migration 0156: SR-7 four-eyes for human-notification routes (email, webhook) at the HTTP layer.
func TestAlertRoutingAPI_HumanRoute_NeedsStagingByOneAdminAndEnablingByAnother(t *testing.T) {
	a := newARAPI(t, true)
	tokA := a.adminToken()
	adminB := a.w.SeedAdmin()
	tokB := a.token(adminB, uuid.Nil, auth.RolePlatformAdmin)
	const recipient = "interim-owner"

	// Admin A stages the route DISABLED.
	r := a.do("POST", "/v1/admin/alerting/routes", tokA, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": recipient, "enabled": false,
	}))
	if r.status != http.StatusCreated {
		t.Fatalf("stage: %d %s", r.status, r.body)
	}
	staged := r.json()["id"].(string)

	// A cannot approve their own staging.
	r = a.do("POST", "/v1/admin/alerting/routes", tokA, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": recipient, "enabled": true,
		"supersedes_id": staged, "reason_code": "on_call_change",
	}))
	if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "SR-7") {
		t.Fatalf("self-approval: %d %s", r.status, r.body)
	}
	// B cannot enable a human route at a step that has no staged version (nothing to approve).
	r = a.do("POST", "/v1/admin/alerting/routes", tokB, routeBody(map[string]any{
		"severity": "p1", "escalation_step": 1, "channel_kind": "webhook", "recipient_ref": recipient, "enabled": true,
	}))
	if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "SR-7") {
		t.Fatalf("enable without staging: %d %s", r.status, r.body)
	}
	// B cannot approve a staged version with a DIFFERENT recipient.
	r = a.do("POST", "/v1/admin/alerting/routes", tokB, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": "someone-else", "enabled": true,
		"supersedes_id": staged, "reason_code": "on_call_change",
	}))
	if r.status != http.StatusBadRequest {
		t.Fatalf("approving a different recipient: %d %s", r.status, r.body)
	}
	if n := a.routeCount(); n != 1 {
		t.Fatalf("refused attempts must not write routes: %d", n)
	}

	// B approves the identical staged version.
	r = a.do("POST", "/v1/admin/alerting/routes", tokB, routeBody(map[string]any{
		"severity": "p1", "channel_kind": "email", "recipient_ref": recipient, "enabled": true,
		"supersedes_id": staged, "reason_code": "on_call_change",
	}))
	if r.status != http.StatusCreated {
		t.Fatalf("approval by a different admin: %d %s", r.status, r.body)
	}
	got := r.json()
	if got["enabled"] != true || got["channel_kind"] != "email" || got["human_notification"] != true {
		t.Fatalf("approved route = %v", got)
	}

	// The audit trail names who staged what the approver approved, and never the recipient.
	var found bool
	for _, m := range a.platformAudits("alert_route.create") {
		if m["_outcome"] == "success" && m["four_eyes_staged_route_id"] == staged {
			found = true
			if m["four_eyes_staged_by"] != a.admin.String() {
				t.Errorf("four_eyes_staged_by = %v, want %s", m["four_eyes_staged_by"], a.admin)
			}
			if m["_actor"] != adminB.String() {
				t.Errorf("approving actor = %v, want %s", m["_actor"], adminB)
			}
		}
		for k, v := range m {
			if s, ok := v.(string); ok && strings.Contains(s, recipient) {
				t.Errorf("the recipient reference leaked into audit field %s", k)
			}
		}
	}
	if !found {
		t.Fatal("no success audit row carries the four-eyes staging record")
	}
	denied := 0
	for _, m := range a.platformAudits("alert_route.create") {
		if m["_outcome"] == "denied" && m["denied"] == "sr7_four_eyes_required" {
			denied++
		}
	}
	if denied != 3 {
		t.Errorf("denied SR-7 audit rows = %d, want 3 (self-approval, enable-without-staging, mismatched recipient)", denied)
	}

	// The email route is not wired in this process (LogSink only), so readiness says so, honestly.
	st := a.do("GET", "/v1/admin/alerting/status", tokA, nil)
	if st.status != http.StatusOK {
		t.Fatalf("status: %d %s", st.status, st.body)
	}
}
