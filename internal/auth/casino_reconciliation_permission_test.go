package auth

import (
	"sort"
	"testing"
)

// Stage 10.3 W2b (CAS-RECON-1): PermCasinoReconciliationRead is granted to
// EXACTLY tenant_admin, finance and compliance. Iterates rolePermissions'
// own keys so an accidental grant to any role (including one added later)
// is caught.
func TestRoleHasPermission_CasinoReconciliationRead_ExactGrantees(t *testing.T) {
	var got []string
	for r := range rolePermissions {
		if RoleHasPermission(r, PermCasinoReconciliationRead) {
			got = append(got, string(r))
		}
	}
	sort.Strings(got)
	want := []string{string(RoleCompliance), string(RoleFinance), string(RoleTenantAdmin)}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("grantees of %q = %v, want %v", PermCasinoReconciliationRead, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("grantees of %q = %v, want %v", PermCasinoReconciliationRead, got, want)
		}
	}
	for _, r := range []Role{RoleSupport, RolePlatformAdmin, RolePlayer, RoleRiskManager} {
		if RoleHasPermission(r, PermCasinoReconciliationRead) {
			t.Errorf("%q must not hold %q", r, PermCasinoReconciliationRead)
		}
	}
}
