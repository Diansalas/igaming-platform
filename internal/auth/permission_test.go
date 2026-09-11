package auth

import "testing"

func TestRoleHasPermission_PlatformAdminHasEverything(t *testing.T) {
	all := []Permission{
		PermTenantRead, PermTenantWrite, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
	}
	for _, p := range all {
		if !RoleHasPermission(RolePlatformAdmin, p) {
			t.Errorf("expected platform_admin to have permission %q", p)
		}
	}
}

func TestRoleHasPermission_PlayerHasNoAdminPermissions(t *testing.T) {
	all := []Permission{
		PermTenantRead, PermTenantWrite, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
	}
	for _, p := range all {
		if RoleHasPermission(RolePlayer, p) {
			t.Errorf("expected player role to NOT have admin permission %q", p)
		}
	}
}

func TestRoleHasPermission_SupportCanReadButNotSuspendPlayers(t *testing.T) {
	if !RoleHasPermission(RoleSupport, PermPlayerRead) {
		t.Error("expected support to have player:read")
	}
	if RoleHasPermission(RoleSupport, PermPlayerSuspend) {
		t.Error("expected support to NOT have player:suspend")
	}
}

func TestRoleHasPermission_ComplianceCanSuspendPlayers(t *testing.T) {
	if !RoleHasPermission(RoleCompliance, PermPlayerSuspend) {
		t.Error("expected compliance to have player:suspend")
	}
	if !RoleHasPermission(RoleCompliance, PermAuditRead) {
		t.Error("expected compliance to have audit:read")
	}
}

func TestRoleHasPermission_TenantAdminCannotWriteTenant(t *testing.T) {
	// tenant_admin manages their own tenant's brands/staff/players, but
	// creating/modifying the tenant record itself (licence, licensing
	// model) is platform_admin-only.
	if RoleHasPermission(RoleTenantAdmin, PermTenantWrite) {
		t.Error("expected tenant_admin to NOT have tenant:write")
	}
	if !RoleHasPermission(RoleTenantAdmin, PermTenantRead) {
		t.Error("expected tenant_admin to have tenant:read")
	}
	if !RoleHasPermission(RoleTenantAdmin, PermStaffManage) {
		t.Error("expected tenant_admin to have staff:manage (for their own tenant)")
	}
}

func TestRoleHasPermission_UnknownRoleHasNoPermissions(t *testing.T) {
	if RoleHasPermission(Role("not_a_real_role"), PermPlayerRead) {
		t.Error("expected an unrecognized role to have no permissions (fail closed)")
	}
}

func TestRoleHasPermission_FinanceIsDefinedButEmptyPlaceholder(t *testing.T) {
	// Stage 2 defines the finance role (per the instructions' explicit
	// list) but grants it nothing yet - Stage 3's wallet/ledger work is
	// what it exists for. This test documents that as intentional so a
	// future accidental permission grant is a visible diff, not silent
	// scope creep.
	all := []Permission{
		PermTenantRead, PermTenantWrite, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
	}
	for _, p := range all {
		if RoleHasPermission(RoleFinance, p) {
			t.Errorf("expected finance to currently have no permissions, but has %q", p)
		}
	}
}
