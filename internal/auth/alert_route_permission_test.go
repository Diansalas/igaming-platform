package auth

import "testing"

// ALERT-DELIVERY-1 (ADR 0102 section 18): alert:route_manage has exactly one
// grantee, RolePlatformAdmin, asserted by permission constant over
// rolePermissions' own keys. Separation of duties: ack/resolve
// (alert:manage) and route authoring are distinct permissions, and no
// non-platform role holds either.
func TestRoleHasPermission_AlertRouteManage_ExactlyPlatformAdmin(t *testing.T) {
	var grantees []Role
	for r := range rolePermissions {
		if RoleHasPermission(r, PermAlertRouteManage) {
			grantees = append(grantees, r)
		}
	}
	if len(grantees) != 1 || grantees[0] != RolePlatformAdmin {
		t.Fatalf("grantees of %q = %v, want exactly [%s]", PermAlertRouteManage, grantees, RolePlatformAdmin)
	}
}

func TestAlertRouteManage_IsDistinctFromAlertManage(t *testing.T) {
	if PermAlertRouteManage == PermAlertManage {
		t.Fatal("alert:route_manage must be a different permission from alert:manage")
	}
	if string(PermAlertRouteManage) != "alert:route_manage" {
		t.Fatalf("permission string changed: %q", PermAlertRouteManage)
	}
}
