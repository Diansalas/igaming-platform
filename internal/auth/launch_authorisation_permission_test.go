package auth

import (
	"os"
	"strings"
	"testing"
)

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

// ADR 0112 slice 6 (b): compliance gains own-tenant tenant and brand READ (brand discovery for the
// launch panel) and nothing else. This pins the exact holder sets of the two read permissions and
// proves the grant is not an escalation: compliance still cannot approve, cannot write a tenant or
// a brand, cannot bind a licence or manage staff, and the launch matrix is otherwise unchanged.
func TestCompliance_TenantBrandReadGrant_NoEscalation(t *testing.T) {
	for _, perm := range []Permission{PermTenantRead, PermBrandRead} {
		want := map[Role]bool{RolePlatformAdmin: true, RoleTenantAdmin: true, RoleCompliance: true}
		for _, role := range append(append([]Role(nil), allRoles...), "", "unknown", "COMPLIANCE") {
			if got := RoleHasPermission(role, perm); got != want[role] {
				t.Errorf("role %q %s = %v, want %v", role, perm, got, want[role])
			}
		}
	}
	for _, perm := range []Permission{
		PermLaunchAuthorisationApprove, PermTenantWrite, PermBrandWrite, PermStaffManage, PermTenantLicenceAssign,
		PermJurisdictionRegistryManage, PermProviderConfigWrite, PermWithdrawalApprove,
	} {
		if RoleHasPermission(RoleCompliance, perm) {
			t.Errorf("compliance must not hold %s", perm)
		}
	}
	for perm, want := range map[Permission]bool{
		PermLaunchAuthorisationRead: true, PermLaunchAuthorisationRequest: true, PermLaunchAuthorisationSuspend: true,
		PermLaunchAuthorisationApprove: false,
	} {
		if RoleHasPermission(RoleCompliance, perm) != want {
			t.Errorf("compliance %s want %v", perm, want)
		}
	}
}

// Slice 6 review: tenant:read / brand:read are safe to hold across a role only because the database
// makes every non-platform_admin staff row tenant-scoped (migration 0011 CHECK). Pin both halves, so
// a future platform-scoped compliance (or other) role cannot silently gain tenant:read across tenants:
// it would have to change the CHECK and the holder sets here in the same diff.
func TestTenantBrandRead_HoldersAreTenantScopedByTheDatabase(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0011_create_staff_users.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.Join(strings.Fields(string(raw)), " ")
	want := "CHECK ( (role = 'platform_admin' AND tenant_id IS NULL) OR (role != 'platform_admin' AND tenant_id IS NOT NULL) )"
	if !strings.Contains(sql, want) {
		t.Fatalf("migration 0011 no longer forces every non-platform_admin staff role to be tenant-scoped; revisit who may hold tenant:read / brand:read")
	}
	for _, perm := range []Permission{PermTenantRead, PermBrandRead} {
		var holders []Role
		for _, r := range allRoles {
			if RoleHasPermission(r, perm) {
				holders = append(holders, r)
			}
		}
		if len(holders) != 3 {
			t.Errorf("%s holders = %v, want exactly platform_admin, tenant_admin, compliance", perm, holders)
		}
	}
}
