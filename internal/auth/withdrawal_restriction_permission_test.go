package auth

import "testing"

// lic11 (migration 0182): :change is platform_admin ONLY; :read is platform_admin, tenant_admin and compliance; nobody else
// (and no unknown role) holds either.
func TestWithdrawalRestrictionPolicyPermissions_RoleMatrix(t *testing.T) {
	pa, ta, co := RolePlatformAdmin, RoleTenantAdmin, RoleCompliance
	rows := []struct {
		perm  Permission
		roles map[Role]bool
	}{
		{PermWithdrawalRestrictionPolicyChange, map[Role]bool{pa: true}},
		{PermWithdrawalRestrictionPolicyRead, map[Role]bool{pa: true, ta: true, co: true}},
	}
	for _, r := range rows {
		for _, role := range append(append([]Role(nil), allRoles...), "", "unknown", "PLATFORM_ADMIN", RolePlayer) {
			if got, want := RoleHasPermission(role, r.perm), r.roles[role]; got != want {
				t.Errorf("role %q %s = %v, want %v", role, r.perm, got, want)
			}
		}
	}
	if string(PermWithdrawalRestrictionPolicyChange) != "withdrawal_restriction_policy:change" ||
		string(PermWithdrawalRestrictionPolicyRead) != "withdrawal_restriction_policy:read" {
		t.Fatal("permission strings drifted")
	}
}
