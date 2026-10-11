package auth

import "testing"

// ADR 0112 slice 3 (6.1): the four static launch authorisation permissions. :approve is held by
// platform_admin ONLY; request / suspend / read by platform_admin, tenant_admin and compliance;
// nobody else (and no unknown role) holds any.
func TestLaunchAuthorisationPermissions_RoleMatrix(t *testing.T) {
	type row struct {
		perm  Permission
		roles map[Role]bool
	}
	pa, ta, co := RolePlatformAdmin, RoleTenantAdmin, RoleCompliance
	rows := []row{
		{PermLaunchAuthorisationRequest, map[Role]bool{pa: true, ta: true, co: true}},
		{PermLaunchAuthorisationApprove, map[Role]bool{pa: true}},
		{PermLaunchAuthorisationSuspend, map[Role]bool{pa: true, ta: true, co: true}},
		{PermLaunchAuthorisationRead, map[Role]bool{pa: true, ta: true, co: true}},
	}
	for _, r := range rows {
		for _, role := range append(append([]Role(nil), allRoles...), "", "unknown", "PLATFORM_ADMIN") {
			if got, want := RoleHasPermission(role, r.perm), r.roles[role]; got != want {
				t.Errorf("role %q %s = %v, want %v", role, r.perm, got, want)
			}
		}
	}
	if string(PermLaunchAuthorisationRequest) != "launch_authorisation:request" || string(PermLaunchAuthorisationApprove) != "launch_authorisation:approve" ||
		string(PermLaunchAuthorisationSuspend) != "launch_authorisation:suspend" || string(PermLaunchAuthorisationRead) != "launch_authorisation:read" {
		t.Fatal("permission strings drifted from ADR 0112 6.1")
	}
}
