package auth

import "testing"

// TestOperatingMarketPermissions_ExactRoleWiring asserts the four Stage
// 4I Phase E permissions (ADR 0045 §6.1) are held by EXACTLY the roles
// named in that ruling - no broader, no narrower.
func TestOperatingMarketPermissions_ExactRoleWiring(t *testing.T) {
	cases := []struct {
		perm    Permission
		granted []Role
	}{
		{PermOperatingMarketCeilingManage, []Role{RolePlatformAdmin}},
		{PermOperatingMarketTenantPolicyWrite, []Role{RoleCompliance}},
		{PermOperatingMarketBrandPolicyWrite, []Role{RoleTenantAdmin}},
		{PermOperatingMarketPolicyRead, []Role{RolePlatformAdmin, RoleCompliance, RoleTenantAdmin}},
	}
	for _, c := range cases {
		grantedSet := map[Role]bool{}
		for _, r := range c.granted {
			grantedSet[r] = true
			if !RoleHasPermission(r, c.perm) {
				t.Errorf("expected role %q to hold %q, it does not", r, c.perm)
			}
		}
		for _, r := range allRoles {
			if grantedSet[r] {
				continue
			}
			if RoleHasPermission(r, c.perm) {
				t.Errorf("role %q must NOT hold %q - only %v may", r, c.perm, c.granted)
			}
		}
	}
}

// TestOperatingMarketPermissions_NoPlayerGrant restates, for all four
// permissions specifically, that RolePlayer holds nothing.
func TestOperatingMarketPermissions_NoPlayerGrant(t *testing.T) {
	for _, perm := range []Permission{
		PermOperatingMarketCeilingManage, PermOperatingMarketTenantPolicyWrite,
		PermOperatingMarketBrandPolicyWrite, PermOperatingMarketPolicyRead,
	} {
		if RoleHasPermission(RolePlayer, perm) {
			t.Errorf("RolePlayer must never hold %q", perm)
		}
	}
}

// TestOperatingMarketPermissions_WriteAuthoritiesNeverOverlap confirms
// the three write permissions are held by three DISJOINT roles - no
// principal may hold more than one of the ceiling/tenant-policy/brand-
// policy write authorities, mirroring the jurisdiction permissions'
// own TestJurisdictionPermissions_NeverHeldTogether precedent.
func TestOperatingMarketPermissions_WriteAuthoritiesNeverOverlap(t *testing.T) {
	writePerms := []Permission{
		PermOperatingMarketCeilingManage, PermOperatingMarketTenantPolicyWrite, PermOperatingMarketBrandPolicyWrite,
	}
	for _, r := range allRoles {
		held := 0
		for _, p := range writePerms {
			if RoleHasPermission(r, p) {
				held++
			}
		}
		if held > 1 {
			t.Errorf("role %q holds %d of the three operating-market write authorities - they must be mutually exclusive per role", r, held)
		}
	}
}
