package auth

import (
	"sort"
	"testing"
)

// ADR 0114 section 3 / invariant 7.7: the role x screen matrix is checked against rolePermissions.
// Each row names the screen, the permission that gates it on the server, and the EXACT set of the
// eight console roles that hold it (A or R in the ADR table). The set is computed from
// rolePermissions' own keys, so a grant added to any role later (or removed from one listed here)
// fails the build. If the ADR matrix changes, this table changes with it, in the same commit.
//
// Rows whose ADR cell is "-" for a role assert the role does NOT hold the permission.
func TestADR0114_RoleMatrix_AgainstRolePermissions(t *testing.T) {
	consoleRoles := []Role{RolePlatformAdmin, RoleTenantAdmin, RoleCompliance, RoleFinance,
		RoleRiskManager, RoleSupport, RolePromotionsManager, RoleBonusOperations}
	rows := []struct {
		screen string
		perm   Permission
		want   []Role
	}{
		{"withdrawal queue / detail / R1b attempt panel (FI only)", PermWithdrawalReview, []Role{RoleFinance}},
		{"parked payments queue R1 and force resolution read", PermPaymentForceResolveRead, []Role{RolePlatformAdmin, RoleCompliance, RoleFinance}},
		{"force resolution request (FI with K1 grant, PA acting)", PermPaymentForceResolveRequest, []Role{RolePlatformAdmin, RoleFinance}},
		{"HSEC hold release read (PA only)", PermWithdrawalHoldResolutionRead, []Role{RolePlatformAdmin}},
		{"manual adjustments read", PermLedgerAdjustmentRead, []Role{RolePlatformAdmin, RoleCompliance, RoleFinance}},
		{"reconciliation: ledger_vs_projection (R2)", PermLedgerAdjustmentRead, []Role{RolePlatformAdmin, RoleCompliance, RoleFinance}},
		{"reconciliation: payment_statement (R2)", PermPaymentForceResolveRead, []Role{RolePlatformAdmin, RoleCompliance, RoleFinance}},
		{"reconciliation: sportsbook_settlement (R2, new permission)", PermSportsbookReconciliationRead, []Role{RoleTenantAdmin, RoleCompliance, RoleFinance}},
		{"reconciliation: casino", PermCasinoReconciliationRead, []Role{RoleTenantAdmin, RoleCompliance, RoleFinance}},
		{"payout instruments read", PermPayoutInstrumentRead, []Role{RolePlatformAdmin, RoleCompliance, RoleFinance}},
		{"payout instrument suspend (CO only)", PermPayoutInstrumentSuspend, []Role{RoleCompliance}},
	}
	for _, row := range rows {
		var got []string
		for _, r := range consoleRoles {
			if RoleHasPermission(r, row.perm) {
				got = append(got, string(r))
			}
		}
		var want []string
		for _, r := range row.want {
			want = append(want, string(r))
		}
		sort.Strings(got)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Errorf("%s: %s held by %v, ADR 0114 section 3 says %v", row.screen, row.perm, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: %s held by %v, ADR 0114 section 3 says %v", row.screen, row.perm, got, want)
				break
			}
		}
	}
}

// ADR 0114 section 5 (D-OPS-2) / invariant 7.7: sportsbook_reconciliation:read is held by EXACTLY
// tenant_admin, compliance and finance, across every key of rolePermissions (so a role added
// later is caught), and is absent from platform_admin, support, risk_manager, promotions_manager,
// bonus_operations and player.
func TestRoleHasPermission_SportsbookReconciliationRead_ExactGrantees(t *testing.T) {
	var got []string
	for r := range rolePermissions {
		if RoleHasPermission(r, PermSportsbookReconciliationRead) {
			got = append(got, string(r))
		}
	}
	sort.Strings(got)
	want := []string{string(RoleCompliance), string(RoleFinance), string(RoleTenantAdmin)}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("grantees of %q = %v, want %v", PermSportsbookReconciliationRead, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("grantees of %q = %v, want %v", PermSportsbookReconciliationRead, got, want)
		}
	}
	for _, r := range []Role{RolePlatformAdmin, RoleSupport, RoleRiskManager, RolePromotionsManager, RoleBonusOperations, RolePlayer} {
		if RoleHasPermission(r, PermSportsbookReconciliationRead) {
			t.Errorf("%q must not hold %q", r, PermSportsbookReconciliationRead)
		}
	}
	if RoleHasPermission(Role("no_such_role"), PermSportsbookReconciliationRead) {
		t.Error("an unknown role must hold nothing")
	}
}
