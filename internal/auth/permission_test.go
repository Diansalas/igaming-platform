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

	// tenant_admin has provider_config:write, but - as of Stage 3D's
	// business decision #4/#5 (a broad administrative role must never
	// hold withdrawal authority implicitly, and staff-management authority
	// must be separable from withdrawal-approval authority) - it no longer
	// gets ANY withdrawal permission. RoleFinance is the sole grantee.
	if RoleHasPermission(RoleTenantAdmin, PermWithdrawalApprove) {
		t.Error("expected tenant_admin to NOT have withdrawal:approve (Stage 3D removed this privilege-escalation path)")
	}
	if !RoleHasPermission(RoleTenantAdmin, PermProviderConfigWrite) {
		t.Error("expected tenant_admin to have provider_config:write")
	}

	// No other role may approve withdrawals or write provider config,
	// including a player - a client-side role claim must never grant a
	// financial administrative capability server-side. tenant_admin is
	// included here now too (see above).
	for _, role := range []Role{RolePlayer, RoleSupport, RoleCompliance, RoleTenantAdmin} {
		if RoleHasPermission(role, PermWithdrawalApprove) {
			t.Errorf("expected %q to NOT have withdrawal:approve", role)
		}
	}
	for _, role := range []Role{RolePlayer, RoleSupport, RoleCompliance} {
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

// TestRoleHasPermission_Stage3DWithdrawalGovernanceSeparation proves
// business decision #4/#5 in full: the four withdrawal-decision
// permissions (review/approve/reject/submit) belong to RoleFinance alone,
// while the withdrawal-POLICY-write permission belongs to RoleTenantAdmin
// alone - the two are deliberately disjoint, so the role that approves
// withdrawals can never also loosen the policy gating its own approvals,
// and the role that administers tenant/staff config can never silently
// gain withdrawal-decision authority.
func TestRoleHasPermission_Stage3DWithdrawalGovernanceSeparation(t *testing.T) {
	decisionPerms := []Permission{PermWithdrawalReview, PermWithdrawalApprove, PermWithdrawalReject, PermWithdrawalSubmit}
	for _, perm := range decisionPerms {
		if !RoleHasPermission(RoleFinance, perm) {
			t.Errorf("expected finance to have %q", perm)
		}
		if RoleHasPermission(RoleFinance, PermWithdrawalPolicyWrite) {
			t.Error("expected finance to NOT have withdrawal_policy:write (it must not be able to loosen its own approval gate)")
		}
	}

	for _, role := range []Role{RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleCompliance, RolePlayer} {
		for _, perm := range decisionPerms {
			if RoleHasPermission(role, perm) {
				t.Errorf("expected %q to NOT have %q - only finance may decide withdrawals", role, perm)
			}
		}
	}

	if !RoleHasPermission(RoleTenantAdmin, PermWithdrawalPolicyWrite) {
		t.Error("expected tenant_admin to have withdrawal_policy:write")
	}
	for _, role := range []Role{RolePlatformAdmin, RoleFinance, RoleSupport, RoleCompliance, RolePlayer} {
		if RoleHasPermission(role, PermWithdrawalPolicyWrite) {
			t.Errorf("expected %q to NOT have withdrawal_policy:write", role)
		}
	}

	// staff:manage (tenant_admin) must never imply any withdrawal-decision
	// permission - business decision #5's literal wording.
	if RoleHasPermission(RoleTenantAdmin, PermStaffManage) {
		for _, perm := range decisionPerms {
			if RoleHasPermission(RoleTenantAdmin, perm) {
				t.Errorf("expected staff:manage (tenant_admin) to NOT imply %q", perm)
			}
		}
	} else {
		t.Fatal("test assumption violated: expected tenant_admin to have staff:manage")
	}
}

// TestRoleHasPermission_Stage4DRGRestrictionPermissions proves Stage
// 4D-RG's separation-of-duties requirement (ADR 0026 §12, mirroring Stage
// 3D's identical withdrawal-governance precedent): PermRGRestrictionWrite
// belongs to RoleCompliance alone - never RoleTenantAdmin (a broad
// administrator must not get RG-restriction WRITE authority automatically
// just for holding PermStaffManage/PermPlayerSuspend) and never
// RolePlatformAdmin (which has no path to resolve a tenant-scoped
// player_account at all, so the permission would be unusable dead
// weight - see internal/rg.CreateStaffRestriction's own doc comment).
func TestRoleHasPermission_Stage4DRGRestrictionPermissions(t *testing.T) {
	if !RoleHasPermission(RoleCompliance, PermRGRestrictionWrite) {
		t.Error("expected compliance to have rg_restriction:write")
	}
	if !RoleHasPermission(RoleCompliance, PermRGRestrictionRead) {
		t.Error("expected compliance to have rg_restriction:read")
	}

	for _, role := range []Role{RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleFinance, RolePlayer} {
		if RoleHasPermission(role, PermRGRestrictionWrite) {
			t.Errorf("expected %q to NOT have rg_restriction:write - only compliance may create a restriction", role)
		}
	}

	// RoleTenantAdmin gets READ-only visibility (an operational admin
	// reasonably needs to see why a player is restricted) but never write.
	if !RoleHasPermission(RoleTenantAdmin, PermRGRestrictionRead) {
		t.Error("expected tenant_admin to have rg_restriction:read")
	}
	for _, role := range []Role{RolePlatformAdmin, RoleSupport, RoleFinance, RolePlayer} {
		if RoleHasPermission(role, PermRGRestrictionRead) {
			t.Errorf("expected %q to NOT have rg_restriction:read", role)
		}
	}
}

// TestRoleHasPermission_Stage4EIdentityReviewManagePermission proves Stage
// 4E's identical separation-of-duties requirement (ADR 0027):
// PermIdentityReviewManage belongs to RoleCompliance alone - a broad
// administrator (RoleTenantAdmin) must never be able to clear a
// self-exclusion-relevant identity review just for holding
// PermPlayerSuspend/PermStaffManage, and RolePlatformAdmin has no path to
// resolve a tenant-scoped player_account at all (mirrors
// TestRoleHasPermission_Stage4DRGRestrictionPermissions exactly).
func TestRoleHasPermission_Stage4EIdentityReviewManagePermission(t *testing.T) {
	if !RoleHasPermission(RoleCompliance, PermIdentityReviewManage) {
		t.Error("expected compliance to have identity_review:manage")
	}
	for _, role := range []Role{RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleFinance, RolePlayer} {
		if RoleHasPermission(role, PermIdentityReviewManage) {
			t.Errorf("expected %q to NOT have identity_review:manage - only compliance may clear an identity review", role)
		}
	}
}

// TestRoleHasPermission_Stage4FVerificationPermissions proves Stage 4F's
// separation-of-duties requirement (ADR 0028): PermVerificationReview
// belongs to RoleCompliance alone, PermVerificationRead additionally to
// RoleTenantAdmin (read-only), and neither ever reaches RolePlatformAdmin
// or RoleFinance - mirroring TestRoleHasPermission_Stage4DRGRestriction
// Permissions/TestRoleHasPermission_Stage4EIdentityReviewManagePermission
// exactly.
func TestRoleHasPermission_Stage4FVerificationPermissions(t *testing.T) {
	if !RoleHasPermission(RoleCompliance, PermVerificationReview) {
		t.Error("expected compliance to have verification:review")
	}
	if !RoleHasPermission(RoleCompliance, PermVerificationRead) {
		t.Error("expected compliance to have verification:read")
	}
	if !RoleHasPermission(RoleTenantAdmin, PermVerificationRead) {
		t.Error("expected tenant_admin to have verification:read")
	}

	for _, role := range []Role{RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleFinance, RolePlayer} {
		if RoleHasPermission(role, PermVerificationReview) {
			t.Errorf("expected %q to NOT have verification:review - only compliance may approve/reject", role)
		}
	}
	for _, role := range []Role{RolePlatformAdmin, RoleSupport, RoleFinance, RolePlayer} {
		if RoleHasPermission(role, PermVerificationRead) {
			t.Errorf("expected %q to NOT have verification:read", role)
		}
	}
}

func TestRoleHasPermission_Stage4GRiskConfigPermissions(t *testing.T) {
	if !RoleHasPermission(RoleRiskManager, PermRiskConfigManage) {
		t.Error("expected risk_manager to have risk_config:manage")
	}
	if !RoleHasPermission(RoleRiskManager, PermRiskConfigRead) {
		t.Error("expected risk_manager to have risk_config:read")
	}
	if !RoleHasPermission(RoleTenantAdmin, PermRiskConfigRead) {
		t.Error("expected tenant_admin to have risk_config:read")
	}

	for _, role := range []Role{RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleCompliance, RoleFinance, RolePlayer} {
		if RoleHasPermission(role, PermRiskConfigManage) {
			t.Errorf("expected %q to NOT have risk_config:manage - only risk_manager may create/disable rules", role)
		}
	}
	for _, role := range []Role{RolePlatformAdmin, RoleSupport, RoleCompliance, RoleFinance, RolePlayer} {
		if RoleHasPermission(role, PermRiskConfigRead) {
			t.Errorf("expected %q to NOT have risk_config:read", role)
		}
	}

	// risk_manager holds ONLY the two risk_config permissions - never
	// PermStaffManage/PermPlayerRead/PermAuditRead, mirroring RoleFinance's
	// own "one role, one narrow authority" precedent.
	for _, perm := range []Permission{PermStaffManage, PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermTenantWrite} {
		if RoleHasPermission(RoleRiskManager, perm) {
			t.Errorf("expected risk_manager to NOT have %q", perm)
		}
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
