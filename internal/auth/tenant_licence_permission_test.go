package auth

import "testing"

// Stage 4I Phase A introduced exactly one new permission
// (docs/plans/stage-4i-jurisdiction-implementation-plan.md Phase A):
// PermTenantLicenceAssign, PLATFORM-ONLY, granted only to
// RolePlatformAdmin, following PermJurisdictionRegistryManage's exact
// precedent (see jurisdiction_permission_test.go, which this file
// mirrors).

// TestTenantLicenceAssign_PlatformAdminOnly pins the "one new
// PLATFORM-ONLY permission on RolePlatformAdmin" grant. `tenants` and
// `licences` carry NO row-level security, so this permission check at the
// HTTP layer is the entire control on the write side - a tenant-scoped
// role holding it could bind ANY tenant (not just its own) to a licence,
// which is exactly the capability PermTenantLicenceAssign's own doc
// comment says a materially different authorizing act from
// PermTenantWrite/PermJurisdictionRegistryManage.
func TestTenantLicenceAssign_PlatformAdminOnly(t *testing.T) {
	if !RoleHasPermission(RolePlatformAdmin, PermTenantLicenceAssign) {
		t.Error("platform_admin must hold tenant_licence:assign - it is the sole intended grantee")
	}
	for _, r := range allRoles {
		if r == RolePlatformAdmin {
			continue
		}
		if RoleHasPermission(r, PermTenantLicenceAssign) {
			t.Errorf("role %q must NOT hold tenant_licence:assign - it is platform-only, and `tenants`/`licences` have no RLS behind it", r)
		}
	}
}

// TestTenantLicenceAssign_NoPlayerGrant restates, for this permission
// specifically, that RolePlayer holds nothing - the assertion that would
// fail first if a future change ever attached tenant-licence authority to
// a player-facing role.
func TestTenantLicenceAssign_NoPlayerGrant(t *testing.T) {
	if RoleHasPermission(RolePlayer, PermTenantLicenceAssign) {
		t.Error("RolePlayer must never hold tenant_licence:assign")
	}
}
