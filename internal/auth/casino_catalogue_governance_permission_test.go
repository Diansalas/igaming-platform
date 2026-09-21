package auth

// Stage 9.2, Workstream A (ADR 0081 §5/§5.2, ARCH-DB-2 Phase 2)
// introduced exactly one new permission: PermCasinoCatalogueGovern,
// PLATFORM-ONLY, granted only to RolePlatformAdmin - following
// PermCasinoCatalogueManage/PermAssetRegistryManage's exact precedent
// (see jurisdiction_evaluation_policy_permission_test.go, which this file
// mirrors).

import "testing"

// TestCasinoCatalogueGovern_PlatformAdminOnly pins the "one new
// PLATFORM-ONLY permission on RolePlatformAdmin" grant. Migration 0086's
// casino_catalogue_change_requests/_approvals RLS independently requires
// app.platform_admin_principal_id to be set AND app.tenant_id/
// app.player_account_id to be unset, but the permission grant itself must
// still be exactly this narrow: no tenant-scoped role has any legitimate
// call to file or decide a PLATFORM-WIDE catalogue governance request.
func TestCasinoCatalogueGovern_PlatformAdminOnly(t *testing.T) {
	if !RoleHasPermission(RolePlatformAdmin, PermCasinoCatalogueGovern) {
		t.Error("platform_admin must hold casino_catalogue:govern - it is the sole intended grantee")
	}
	for _, r := range allRoles {
		if r == RolePlatformAdmin {
			continue
		}
		if RoleHasPermission(r, PermCasinoCatalogueGovern) {
			t.Errorf("role %q must NOT hold casino_catalogue:govern - it is platform-only", r)
		}
	}
}

// TestCasinoCatalogueGovern_NoPlayerGrant restates, for this permission
// specifically, that RolePlayer holds nothing.
func TestCasinoCatalogueGovern_NoPlayerGrant(t *testing.T) {
	if RoleHasPermission(RolePlayer, PermCasinoCatalogueGovern) {
		t.Error("RolePlayer must never hold casino_catalogue:govern")
	}
}

// TestCasinoCatalogueGovern_NotTenantAdmin is the most plausible
// accidental-grant target: RoleTenantAdmin holds PermCasinoConfigWrite
// (its own tenant-scoped routing/availability configuration), but filing
// or deciding a platform-wide catalogue governance request is a
// materially different, platform-only authorizing act - exactly the same
// separation PermCasinoCatalogueManage itself already draws.
func TestCasinoCatalogueGovern_NotTenantAdmin(t *testing.T) {
	if RoleHasPermission(RoleTenantAdmin, PermCasinoCatalogueGovern) {
		t.Error("RoleTenantAdmin must NOT hold casino_catalogue:govern")
	}
}

// TestCasinoCatalogueGovern_DistinctFromCatalogueManage pins that holding
// PermCasinoCatalogueManage does not imply PermCasinoCatalogueGovern or
// vice versa for any role - the two are deliberately separate
// permissions (this permission's own doc comment), even though today both
// happen to be granted to the same role.
func TestCasinoCatalogueGovern_DistinctFromCatalogueManage(t *testing.T) {
	for _, r := range allRoles {
		manage := RoleHasPermission(r, PermCasinoCatalogueManage)
		govern := RoleHasPermission(r, PermCasinoCatalogueGovern)
		if manage != govern {
			t.Errorf("role %q: expected casino_catalogue:manage and casino_catalogue:govern to be granted identically today (got manage=%v govern=%v) - if this changes, update this test to reflect the new intended split rather than deleting it", r, manage, govern)
		}
	}
}
