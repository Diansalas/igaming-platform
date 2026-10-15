package auth

import (
	"sort"
	"testing"
)

// ADR 0116 s6 (A7 / S6 minimal): the exact holder set of every AML static permission, computed from the keys of
// rolePermissions so a grant added to (or removed from) any role later fails the build.
//
// These are the STATIC half of the two-layer design (the ledger_adjustment pattern): the route gate. The authority is the
// in-force aml_officer capability GRANT plus a signed actor proof, re-checked in SQL by aml_authorize (migration 0140). The
// only roles that can ever be an aml_officer grantee are `compliance` (tenant) and `platform_admin` (platform), so those are
// the only roles that hold the static permissions. aml_officer itself is NOT a role and is never added to a role.
func TestAMLPermissions_RoleMatrix(t *testing.T) {
	pa, co := RolePlatformAdmin, RoleCompliance
	rows := []struct {
		perm   Permission
		str    string
		holder []Role
	}{
		{PermAMLCaseRead, "aml_case:read", []Role{pa, co}},
		{PermAMLCaseWork, "aml_case:work", []Role{pa, co}},
		{PermAMLCaseCloseApprove, "aml_case:close_approve", []Role{pa, co}},
		{PermAMLHoldManage, "aml_hold:manage", []Role{pa, co}},
	}
	for _, r := range rows {
		if string(r.perm) != r.str {
			t.Errorf("permission string drifted: %q want %q", r.perm, r.str)
		}
		var got, want []string
		for role := range rolePermissions {
			if RoleHasPermission(role, r.perm) {
				got = append(got, string(role))
			}
		}
		for _, role := range r.holder {
			want = append(want, string(role))
		}
		sort.Strings(got)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Errorf("%s held by %v, want exactly %v", r.perm, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s held by %v, want exactly %v", r.perm, got, want)
				break
			}
		}
		for _, bad := range []Role{"", "unknown", "PLATFORM_ADMIN", "Compliance", RolePlayer, RoleTenantAdmin, RoleSupport, RoleFinance, RoleRiskManager} {
			if RoleHasPermission(bad, r.perm) {
				t.Errorf("role %q must not hold %s", bad, r.perm)
			}
		}
	}
	// `staff:manage` (tenant_admin) must never carry an AML permission: a tenant admin cannot create or assign an officer
	// (ADR 0116 s6 B4) and holds no AML route.
	for _, r := range rows {
		if RoleHasPermission(RoleTenantAdmin, r.perm) {
			t.Errorf("tenant_admin must not hold %s", r.perm)
		}
	}
}

// aml_officer is a capability, not a role: it must not appear as a Role constant or a rolePermissions key.
func TestAMLOfficerIsNotARole(t *testing.T) {
	if _, ok := rolePermissions[Role("aml_officer")]; ok {
		t.Fatal("aml_officer must not be a role (ADR 0116 s6 B4)")
	}
	if RoleHasPermission(Role("aml_officer"), PermAMLCaseRead) {
		t.Fatal("a token claiming role aml_officer must hold nothing")
	}
}
