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

// TestRoleHasPermission_Stage3BWithdrawalAndProviderConfigPermissions closes
// a gap this QA pass found: PermWithdrawalApprove/PermProviderConfigWrite
// (Stage 3B's own permissions, gating the withdrawal four-eyes queue and
// provider capability writes - see internal/httpserver/financial_routes.go)
// had zero positive/negative role-mapping coverage. This is the same
// pattern as every other TestRoleHasPermission_* test in this file, applied
// to the two permissions Stage 3B added. Test-only addition; no production
// code changed.
func TestRoleHasPermission_Stage3BWithdrawalAndProviderConfigPermissions(t *testing.T) {
	// finance exists specifically for withdrawal approval, and nothing else.
	if !RoleHasPermission(RoleFinance, PermWithdrawalApprove) {
		t.Error("expected finance to have withdrawal:approve")
	}
	if RoleHasPermission(RoleFinance, PermProviderConfigWrite) {
		t.Error("expected finance to NOT have provider_config:write")
	}

	// tenant_admin has both, per rolePermissions.
	if !RoleHasPermission(RoleTenantAdmin, PermWithdrawalApprove) {
		t.Error("expected tenant_admin to have withdrawal:approve")
	}
	if !RoleHasPermission(RoleTenantAdmin, PermProviderConfigWrite) {
		t.Error("expected tenant_admin to have provider_config:write")
	}

	// No other role may approve withdrawals or write provider config,
	// including a player - a client-side role claim must never grant a
	// financial administrative capability server-side.
	for _, role := range []Role{RolePlayer, RoleSupport, RoleCompliance} {
		if RoleHasPermission(role, PermWithdrawalApprove) {
			t.Errorf("expected %q to NOT have withdrawal:approve", role)
		}
		if RoleHasPermission(role, PermProviderConfigWrite) {
			t.Errorf("expected %q to NOT have provider_config:write", role)
		}
	}

	// platform_admin deliberately does NOT get withdrawal:approve/
	// provider_config:write via rolePermissions - RequireTenantScope
	// already excludes platform_admin's nil-tenant token from every
	// financial route (financial_routes.go), so these are the actual,
	// only intended grantees.
	if RoleHasPermission(RolePlatformAdmin, PermWithdrawalApprove) {
		t.Error("expected platform_admin to NOT have withdrawal:approve (RequireTenantScope is the actual gate)")
	}
	if RoleHasPermission(RolePlatformAdmin, PermProviderConfigWrite) {
		t.Error("expected platform_admin to NOT have provider_config:write (RequireTenantScope is the actual gate)")
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
