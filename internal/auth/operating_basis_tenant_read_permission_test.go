package auth

import "testing"

// c2: operating_basis:tenant_read is held by tenant_admin and compliance ONLY. platform_admin (platform routes, no tenant),
// support, finance, player, risk_manager, promotions_manager, bonus_operations and unknown roles never hold it.
func TestOperatingBasisTenantReadPermission_RoleMatrix(t *testing.T) {
	want := map[Role]bool{RoleTenantAdmin: true, RoleCompliance: true}
	for _, role := range append(append([]Role(nil), allRoles...), "", "unknown", "PLATFORM_ADMIN") {
		if got := RoleHasPermission(role, PermOperatingBasisTenantRead); got != want[role] {
			t.Errorf("role %q %s = %v, want %v", role, PermOperatingBasisTenantRead, got, want[role])
		}
	}
	if string(PermOperatingBasisTenantRead) != "operating_basis:tenant_read" {
		t.Fatal("permission string drifted")
	}
	// The platform-wide registry read must stay platform_admin only (the tenant route must not have widened it).
	for _, role := range allRoles {
		if got, w := RoleHasPermission(role, PermJurisdictionRegistryRead), role == RolePlatformAdmin; got != w {
			t.Errorf("role %q jurisdiction_registry:read = %v, want %v", role, got, w)
		}
	}
}
