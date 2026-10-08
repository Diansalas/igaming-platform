package auth

import "testing"

// B13 (ADR 0111 2.9): payout_instrument:read -> finance, compliance,
// platform_admin; payout_instrument:suspend -> compliance only. Every other
// role (tenant_admin, support, risk_manager, ...) holds neither.
func TestPayoutInstrumentPermissionMatrix(t *testing.T) {
	roles := []Role{RolePlatformAdmin, RoleTenantAdmin, RoleCompliance, RoleSupport, RoleFinance, RoleRiskManager, RolePlayer}
	wantRead := map[Role]bool{RolePlatformAdmin: true, RoleCompliance: true, RoleFinance: true}
	wantSuspend := map[Role]bool{RoleCompliance: true}
	for _, r := range roles {
		if got := RoleHasPermission(r, PermPayoutInstrumentRead); got != wantRead[r] {
			t.Errorf("role %s payout_instrument:read = %v, want %v", r, got, wantRead[r])
		}
		if got := RoleHasPermission(r, PermPayoutInstrumentSuspend); got != wantSuspend[r] {
			t.Errorf("role %s payout_instrument:suspend = %v, want %v", r, got, wantSuspend[r])
		}
	}
}
