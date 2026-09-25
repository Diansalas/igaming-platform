package auth

import "testing"

// ADR 0088 §9.1/§14 (security review S1, QA finding Q4; P3-4): the
// permission table must assert EXACTLY ONE grantee of
// PermSportsbookSettlementSimulate - RoleRiskManager - and the assertion
// is by PERMISSION CONSTANT, not by role name, so a future accidental
// grant on ANY role is caught. Never RoleFinance in particular (ADR 0024
// separation of duties: it already holds withdrawal-approval authority).
//
// This iterates rolePermissions' OWN keys, not the hand-maintained
// allRoles slice (bonus_permission_test.go): a role added to
// rolePermissions but never added to allRoles would otherwise escape this
// assertion entirely (security review P3-4).
func TestRoleHasPermission_SportsbookSettlementSimulate_ExactlyOneGrantee(t *testing.T) {
	var grantees []Role
	for r := range rolePermissions {
		if RoleHasPermission(r, PermSportsbookSettlementSimulate) {
			grantees = append(grantees, r)
		}
	}
	if len(grantees) != 1 {
		t.Fatalf("expected exactly one grantee of %q, got %v", PermSportsbookSettlementSimulate, grantees)
	}
	if grantees[0] != RoleRiskManager {
		t.Fatalf("expected the sole grantee of %q to be %q, got %q", PermSportsbookSettlementSimulate, RoleRiskManager, grantees[0])
	}
}

// TestRolePermissions_AllRolesTestFixtureIsComplete pins the precondition
// the OTHER test in this file (which does iterate allRoles) relies on: if
// rolePermissions ever gains a role that allRoles does not have, this
// fails loudly instead of letting that role's grants go unchecked by any
// allRoles-based test in the auth package.
func TestRolePermissions_AllRolesTestFixtureIsComplete(t *testing.T) {
	seen := make(map[Role]bool, len(allRoles))
	for _, r := range allRoles {
		seen[r] = true
	}
	for r := range rolePermissions {
		if !seen[r] {
			t.Errorf("role %q has permissions in rolePermissions but is missing from allRoles", r)
		}
	}
}

// Named-role negative assertions in addition to the exhaustive loop above -
// pins the specific separation-of-duties reasoning (ADR 0024) so a reviewer
// reading test output sees exactly which roles were deliberately excluded
// and why, not just "not risk_manager".
func TestRoleHasPermission_SportsbookSettlementSimulate_NeverGrantedTo(t *testing.T) {
	excluded := []Role{
		RolePlatformAdmin, RoleTenantAdmin, RoleFinance, RoleCompliance, RoleSupport,
		RolePlayer, RolePromotionsManager, RoleBonusOperations,
	}
	for _, r := range excluded {
		if RoleHasPermission(r, PermSportsbookSettlementSimulate) {
			t.Errorf("expected %q to NOT have %q", r, PermSportsbookSettlementSimulate)
		}
	}
}
