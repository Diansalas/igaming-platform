package auth

import (
	"sort"
	"testing"
)

// ADR 0119 S5 + S6 section 14.3: the staff lifecycle permissions, pinned against rolePermissions (the
// adr0114_role_matrix_test.go pattern). A grant added to (or removed from) any role fails the build.
func TestADR0119_StaffLifecycleRoleMatrix(t *testing.T) {
	all := []Role{RolePlatformAdmin, RoleTenantAdmin, RoleCompliance, RoleFinance, RoleRiskManager, RoleSupport,
		RolePromotionsManager, RoleBonusOperations, RolePlayer}
	rows := []struct {
		perm Permission
		want []Role
	}{
		{PermStaffRead, []Role{RolePlatformAdmin, RoleTenantAdmin, RoleCompliance}},
		{PermStaffManage, []Role{RolePlatformAdmin, RoleTenantAdmin}},
		{PermPlatformStaffGovern, []Role{RolePlatformAdmin}},
	}
	for _, row := range rows {
		var got, want []string
		for _, r := range all {
			if RoleHasPermission(r, row.perm) {
				got = append(got, string(r))
			}
		}
		for _, r := range row.want {
			want = append(want, string(r))
		}
		sort.Strings(got)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Errorf("%s held by %v, ADR 0119 s14.3 says %v", row.perm, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s held by %v, ADR 0119 s14.3 says %v", row.perm, got, want)
				break
			}
		}
	}
	if string(PermStaffRead) != "staff:read" || string(PermPlatformStaffGovern) != "platform_staff:govern" {
		t.Fatal("permission strings are part of the contract")
	}
}
