//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// Security S-6: each authorization layer of ack/resolve is pinned on its own
// (neither may be silently redundant):
//   - layer 1, the alert:manage permission gate: a staff token with NO tenant
//     (so the platform-scope check passes) but a role that lacks alert:manage is
//     refused by the permission gate alone (without it the request would reach
//     the database guard and fail as a 500, not a 403);
//   - layer 2, the explicit platform-scope check: a platform_admin role (holds
//     alert:manage) presented with a tenant id is refused by the scope check.
func TestAlertAdmin_EachAuthorizationLayerIsPinnedOnItsOwn(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	disc := "switch:" + uuid.NewString()
	iwRaiseAlert(t, a, tenant, disc)
	id, _, _, _, _ := iwAlertState(t, a, tenant, disc)
	base := "/v1/admin/alerts/" + id.String()

	// Layer 1: no tenant, role without alert:manage.
	noPerm := a.token(uuid.New(), uuid.Nil, auth.RoleCompliance, auth.PrincipalStaff)
	for _, op := range []string{"/ack", "/resolve"} {
		if r := a.do("POST", base+op, noPerm, map[string]any{"reason_code": "x_y"}); r.status != http.StatusForbidden {
			t.Fatalf("layer 1 (permission gate) %s: status %d, want 403", op, r.status)
		}
	}
	// Layer 2: holds alert:manage but is tenant-scoped.
	withTenant := a.token(a.platformAdmin(), tenant, auth.RolePlatformAdmin, auth.PrincipalStaff)
	for _, op := range []string{"/ack", "/resolve"} {
		if r := a.do("POST", base+op, withTenant, map[string]any{"reason_code": "x_y"}); r.status != http.StatusForbidden {
			t.Fatalf("layer 2 (platform-scope check) %s: status %d, want 403", op, r.status)
		}
	}
	if _, state, _, _, _ := iwAlertState(t, a, tenant, disc); state != "open" {
		t.Fatalf("a refused request changed the alert: %s", state)
	}
}
