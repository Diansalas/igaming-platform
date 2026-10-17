package auth

import "testing"

// F6 / SA-3: platform_status:read has exactly one grantee, RolePlatformAdmin, asserted over rolePermissions' own keys so
// a grant added to any role later fails the build, and it is distinct from the alert permissions.
func TestRoleHasPermission_PlatformStatusRead_ExactlyPlatformAdmin(t *testing.T) {
	var grantees []Role
	for r := range rolePermissions {
		if RoleHasPermission(r, PermPlatformStatusRead) {
			grantees = append(grantees, r)
		}
	}
	if len(grantees) != 1 || grantees[0] != RolePlatformAdmin {
		t.Fatalf("grantees of %q = %v, want exactly [%s]", PermPlatformStatusRead, grantees, RolePlatformAdmin)
	}
	if string(PermPlatformStatusRead) != "platform_status:read" {
		t.Fatalf("permission string changed: %q", PermPlatformStatusRead)
	}
	if PermPlatformStatusRead == PermAlertManage || PermPlatformStatusRead == PermAlertRouteManage {
		t.Fatal("platform_status:read must be distinct from the alert permissions")
	}
}
