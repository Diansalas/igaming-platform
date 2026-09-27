package auth

import "testing"

// PRH-I1 (ADR 0095 §10.4/§10.5): the payment kill-switch permission set is
// granted identically to RolePlatformAdmin and RoleTenantAdmin only - never
// any other role, and never RolePlayer (a kill switch is never reachable on
// a player route regardless; this is defence in depth at the RBAC layer
// too). See payments_kill_switch_handlers.go's own doc comment for why
// release authority is granted at BOTH scopes (the database, not this
// grant, is what stops a tenant session from self-approving or touching a
// platform-engaged switch).
var paymentsKillSwitchPermissions = []Permission{
	PermPaymentsKillSwitchEngage, PermPaymentsKillSwitchRelease, PermPaymentsKillSwitchRead,
}

func TestPaymentsKillSwitchPermissions_PlatformAdminAndTenantAdminOnly(t *testing.T) {
	for _, perm := range paymentsKillSwitchPermissions {
		if !RoleHasPermission(RolePlatformAdmin, perm) {
			t.Errorf("platform_admin must hold %s", perm)
		}
		if !RoleHasPermission(RoleTenantAdmin, perm) {
			t.Errorf("tenant_admin must hold %s", perm)
		}
		for _, r := range allRoles {
			if r == RolePlatformAdmin || r == RoleTenantAdmin {
				continue
			}
			if RoleHasPermission(r, perm) {
				t.Errorf("role %q must NOT hold %s - only platform_admin and tenant_admin may act on a payment kill switch", r, perm)
			}
		}
	}
}

func TestPaymentsKillSwitchPermissions_NoPlayerGrant(t *testing.T) {
	for _, perm := range paymentsKillSwitchPermissions {
		if RoleHasPermission(RolePlayer, perm) {
			t.Errorf("player must never hold %s - a kill switch is a staff-only control (ADR 0095 §10.4: never exposed on a player route)", perm)
		}
	}
}
