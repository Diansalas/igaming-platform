//go:build integration

package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	tenantctx "github.com/Diansalas/igaming-platform/internal/tenant"
)

// Security S-6: each authorization layer of ack/resolve is pinned on its own
// (neither may be silently redundant):
//   - layer 1, the alert:manage permission gate: a staff token with NO tenant
//     (so the platform-scope check passes) but a role that lacks alert:manage is
//     refused by the permission gate alone (without it the request would reach
//     the database guard and fail as a 500, not a 403);
//   - layer 2, the explicit platform-scope check: a platform_admin role (holds
//     alert:manage) presented with a tenant id is refused by the scope check.
//
// ADR 0119 S4a: neither claim combination can belong to a live staff session (no
// platform-scoped compliance row can exist, and a platform row has no tenant), so
// over HTTP both are now refused by the staff chain (401 session_revoked) before
// either layer runs. Each layer is still pinned on its own by invoking the route's
// handler behind the chain (behindChain).
func TestAlertAdmin_EachAuthorizationLayerIsPinnedOnItsOwn(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	disc := "switch:" + uuid.NewString()
	iwRaiseAlert(t, a, tenant, disc)
	id, _, _, _, _ := iwAlertState(t, a, tenant, disc)
	base := "/v1/admin/alerts/" + id.String()

	// Layer 1: no tenant, role without alert:manage.
	noPerm := a.token(uuid.New(), uuid.Nil, auth.RoleCompliance, auth.PrincipalStaff)
	// Layer 2: holds alert:manage but is tenant-scoped.
	admin := a.platformAdmin()
	withTenant := a.token(admin, tenant, auth.RolePlatformAdmin, auth.PrincipalStaff)
	deps := Deps{DB: a.pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, op := range []string{"/ack", "/resolve"} {
		for name, tok := range map[string]string{"layer 1 claims": noPerm, "layer 2 claims": withTenant} {
			if r := a.do("POST", base+op, tok, map[string]any{"reason_code": "x_y"}); r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), "session_revoked") {
				t.Fatalf("%s %s over HTTP: status %d %s, want 401 session_revoked (ADR 0119 S4a chain)", name, op, r.status, r.body)
			}
		}
		h := newAlertTransitionHandler(deps, strings.TrimPrefix(op, "/"))
		if rec := behindChain(t, h, "POST", base+op, map[string]string{"alertID": id.String()},
			tenantctx.Context{Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff), Subject: uuid.NewString()},
			map[string]any{"reason_code": "x_y"}); rec.Code != http.StatusForbidden {
			t.Fatalf("layer 1 (permission gate) %s: status %d, want 403", op, rec.Code)
		}
		if rec := behindChain(t, h, "POST", base+op, map[string]string{"alertID": id.String()},
			tenantctx.Context{TenantID: tenant, Role: string(auth.RolePlatformAdmin), PrincipalType: string(auth.PrincipalStaff), Subject: admin.String()},
			map[string]any{"reason_code": "x_y"}); rec.Code != http.StatusForbidden {
			t.Fatalf("layer 2 (platform-scope check) %s: status %d, want 403", op, rec.Code)
		}
	}
	if _, state, _, _, _ := iwAlertState(t, a, tenant, disc); state != "open" {
		t.Fatalf("a refused request changed the alert: %s", state)
	}
}
