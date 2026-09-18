package auth

import "testing"

// allRoles is used by the cross-role invariant tests below to iterate
// every role this codebase defines, rather than hardcoding a subset that
// could silently miss a newly-added role.
var allRoles = []Role{
	RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleCompliance,
	RoleFinance, RolePlayer, RoleRiskManager, RolePromotionsManager, RoleBonusOperations,
}

// TestBonusSuggestionCreate_GrantedToNoRole is security-architecture.md
// §W15.4.3 binding wiring constraint 1: "bonus_suggestion:create is
// granted to no role. The permission is absent from every entry of
// permission.go's role map, and the service-identity path is its only
// grant."
func TestBonusSuggestionCreate_GrantedToNoRole(t *testing.T) {
	for _, r := range allRoles {
		if RoleHasPermission(r, PermBonusSuggestionCreate) {
			t.Errorf("role %q must NOT hold bonus_suggestion:create (service-identity only, §W15.4.3)", r)
		}
	}
}

// TestBonusPermissions_HardConstraint3_OfferManageNeverWithAdjustOrResolve
// is security-architecture.md §B1.1 hard constraint 3, extended verbatim
// by §W15.1.12 to bonus_held_disposition:resolve: "no role may hold both
// bonus_offer:manage and bonus_adjustment:write" / "...and
// bonus_held_disposition:resolve" - the "configure it instead of
// adjusting it" bypass is only closed if authoring and adjusting/
// resolving are separate authorities.
func TestBonusPermissions_HardConstraint3_OfferManageNeverWithAdjustOrResolve(t *testing.T) {
	for _, r := range allRoles {
		hasOfferManage := RoleHasPermission(r, PermBonusOfferManage)
		if hasOfferManage && RoleHasPermission(r, PermBonusAdjustmentWrite) {
			t.Errorf("role %q must NOT hold both bonus_offer:manage and bonus_adjustment:write (§B1.1 hard constraint 3)", r)
		}
		if hasOfferManage && RoleHasPermission(r, PermBonusHeldDispositionResolve) {
			t.Errorf("role %q must NOT hold both bonus_offer:manage and bonus_held_disposition:resolve (§W15.1.12)", r)
		}
	}
}

// TestBonusPermissions_HardConstraint4_ApprovalPolicyWriteNeverWithFourEyesGated
// is security-architecture.md §B1.1 hard constraint 4: "no role may hold
// both bonus_approval_policy:write and any four-eyes-gated bonus
// permission" - the role that approves must not also be the role that
// can loosen the policy gating its own approvals.
func TestBonusPermissions_HardConstraint4_ApprovalPolicyWriteNeverWithFourEyesGated(t *testing.T) {
	fourEyesGated := []Permission{
		PermBonusGrantIssue, PermBonusAdjustmentWrite, PermBonusBulkExecute,
		PermBonusHeldDispositionResolve, PermBonusCampaignActivate, PermBonusOfferManage, PermBonusGrantCancel,
	}
	for _, r := range allRoles {
		if !RoleHasPermission(r, PermBonusApprovalPolicyWrite) {
			continue
		}
		for _, p := range fourEyesGated {
			if RoleHasPermission(r, p) {
				t.Errorf("role %q holds bonus_approval_policy:write and four-eyes-gated permission %q (§B1.1 hard constraint 4)", r, p)
			}
		}
	}
}

// TestBonusPermissions_SuggestionReviewNeverWithBulkOrGrantIssue is
// security-architecture.md §W15.4.3 binding wiring constraint 2:
// bonus_suggestion:review must never be bundled with bonus_bulk:execute
// or bonus_grant:issue in the same role - otherwise "one principal can
// approve a suggestion and then activate it," which the section calls
// "worse than no control" (a self-authored justification attached to a
// grant the same person issued).
func TestBonusPermissions_SuggestionReviewNeverWithBulkOrGrantIssue(t *testing.T) {
	for _, r := range allRoles {
		if !RoleHasPermission(r, PermBonusSuggestionReview) {
			continue
		}
		if RoleHasPermission(r, PermBonusBulkExecute) {
			t.Errorf("role %q holds both bonus_suggestion:review and bonus_bulk:execute (§W15.4.3 constraint 2)", r)
		}
		if RoleHasPermission(r, PermBonusGrantIssue) {
			t.Errorf("role %q holds both bonus_suggestion:review and bonus_grant:issue (§W15.4.3 constraint 2)", r)
		}
	}
}

// TestBonusPermissions_HeldDispositionResolve_OnlyBonusOperations is
// security-architecture.md §W15.1.12's binding wiring: granted to
// RoleBonusOperations only - never RolePromotionsManager, RoleTenantAdmin,
// RoleFinance, or RolePlatformAdmin.
func TestBonusPermissions_HeldDispositionResolve_OnlyBonusOperations(t *testing.T) {
	for _, r := range allRoles {
		want := r == RoleBonusOperations
		got := RoleHasPermission(r, PermBonusHeldDispositionResolve)
		if got != want {
			t.Errorf("role %q: RoleHasPermission(bonus_held_disposition:resolve) = %v, want %v", r, got, want)
		}
	}
}

// TestBonusPermissions_FinanceAndPlatformAdminHoldNoBonusPermission is
// security-architecture.md §B1.1's explicit exclusions: RoleFinance gets
// nothing (the two-step-drain scenario: adjust a colluding player's
// bonus balance below threshold, then approve the withdrawal below
// threshold - neither step needs a second human if one role held both);
// RolePlatformAdmin gets nothing (cannot resolve a tenant's
// player_account at all, so any bonus permission would be a capability
// nothing can use).
func TestBonusPermissions_FinanceAndPlatformAdminHoldNoBonusPermission(t *testing.T) {
	bonusPerms := []Permission{
		PermBonusRead, PermBonusConfigRead, PermBonusCampaignCreate, PermBonusCampaignUpdate,
		PermBonusCampaignActivate, PermBonusCampaignSuspend, PermBonusOfferManage, PermBonusSegmentManage,
		PermBonusGrantIssue, PermBonusGrantReview, PermBonusGrantCancel, PermBonusAdjustmentWrite,
		PermBonusBulkExecute, PermBonusReportRead, PermBonusApprovalPolicyWrite,
		PermBonusSuggestionCreate, PermBonusSuggestionReview, PermBonusHeldDispositionResolve,
	}
	for _, r := range []Role{RoleFinance, RolePlatformAdmin} {
		for _, p := range bonusPerms {
			if RoleHasPermission(r, p) {
				t.Errorf("role %q must hold NO bonus permission, found %q (§B1.1)", r, p)
			}
		}
	}
}

// TestBonusPermissions_TenantAdminHasNoBonusWriteAuthority is
// security-architecture.md §B1.1: RoleTenantAdmin gets read-only bonus
// visibility plus bonus_approval_policy:write - "and nothing else."
func TestBonusPermissions_TenantAdminHasNoBonusWriteAuthority(t *testing.T) {
	writePerms := []Permission{
		PermBonusCampaignCreate, PermBonusCampaignUpdate, PermBonusCampaignActivate,
		PermBonusOfferManage, PermBonusSegmentManage, PermBonusGrantIssue, PermBonusGrantReview,
		PermBonusGrantCancel, PermBonusAdjustmentWrite, PermBonusBulkExecute,
		PermBonusSuggestionCreate, PermBonusSuggestionReview, PermBonusHeldDispositionResolve,
	}
	for _, p := range writePerms {
		if RoleHasPermission(RoleTenantAdmin, p) {
			t.Errorf("tenant_admin must NOT hold bonus write authority %q (§B1.1)", p)
		}
	}
	if !RoleHasPermission(RoleTenantAdmin, PermBonusRead) || !RoleHasPermission(RoleTenantAdmin, PermBonusConfigRead) {
		t.Error("tenant_admin should retain read-only bonus visibility")
	}
}

// TestBonusPermissions_PromotionsManagerAndBonusOperationsAreDisjointInPurpose
// documents hard constraint 1's downstream consequence at the permission
// level: RolePromotionsManager never holds a grant-issuing/adjustment/
// bulk-execution/held-disposition-resolution authority, and
// RoleBonusOperations never holds an authoring/activation authority -
// each role's own value-moving surface is exclusive to it.
func TestBonusPermissions_PromotionsManagerAndBonusOperationsAreDisjointInPurpose(t *testing.T) {
	moneyMovingPerms := []Permission{
		PermBonusGrantIssue, PermBonusGrantCancel, PermBonusAdjustmentWrite,
		PermBonusBulkExecute, PermBonusHeldDispositionResolve,
	}
	for _, p := range moneyMovingPerms {
		if RoleHasPermission(RolePromotionsManager, p) {
			t.Errorf("promotions_manager must NOT hold value-moving permission %q (§B1.1 hard constraint 1)", p)
		}
	}
	authoringPerms := []Permission{PermBonusOfferManage, PermBonusSegmentManage, PermBonusCampaignCreate}
	for _, p := range authoringPerms {
		if RoleHasPermission(RoleBonusOperations, p) {
			t.Errorf("bonus_operations must NOT hold authoring permission %q (§B1.1 hard constraint 1)", p)
		}
	}
}
