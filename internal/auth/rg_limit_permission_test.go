package auth

import (
	"sort"
	"testing"
)

// ADR 0115 S2: the exact holder set of every RG limit / configuration permission, computed from the
// keys of rolePermissions (so a grant added to any role later, or removed from one listed here,
// fails the build), plus unknown / empty / wrong-case roles holding nothing. Authorization is
// enforced server-side per route; the controls that matter beyond the role (tighten-only, floor
// four-eyes by distinct Person, signed actor proofs) are the database's.
func TestRGLimitPermissions_RoleMatrix(t *testing.T) {
	pa, ta, co := RolePlatformAdmin, RoleTenantAdmin, RoleCompliance
	rows := []struct {
		perm   Permission
		str    string
		holder []Role
	}{
		{PermRGLimitRead, "rg_limit:read", []Role{ta, co}},
		{PermRGLimitWriteOnPlayerRequest, "rg_limit:write_on_player_request", []Role{co}},
		{PermRGConfigRead, "rg_config:read", []Role{ta, co}},
		{PermRGConfigWrite, "rg_config:write", []Role{co}},
		{PermRGFloorRead, "rg_floor:read", []Role{pa}},
		{PermRGFloorRequest, "rg_floor:request", []Role{pa}},
		{PermRGFloorDecide, "rg_floor:decide", []Role{pa}},
	}
	for _, r := range rows {
		if string(r.perm) != r.str {
			t.Errorf("permission string drifted: %q want %q", r.perm, r.str)
		}
		var got []string
		for role := range rolePermissions {
			if RoleHasPermission(role, r.perm) {
				got = append(got, string(role))
			}
		}
		var want []string
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
		for _, bad := range []Role{"", "unknown", "PLATFORM_ADMIN", "Compliance", RolePlayer} {
			if RoleHasPermission(bad, r.perm) {
				t.Errorf("role %q must not hold %s", bad, r.perm)
			}
		}
	}
	// Separation of duties: nobody who can WRITE operator config also decides a floor, and the
	// platform role can never write a tenant's player limits or operator bounds.
	for role := range rolePermissions {
		if RoleHasPermission(role, PermRGConfigWrite) && RoleHasPermission(role, PermRGFloorDecide) {
			t.Errorf("%s holds both operator-config write and floor decide", role)
		}
	}
	if RoleHasPermission(RoleTenantAdmin, PermRGConfigWrite) || RoleHasPermission(RoleTenantAdmin, PermRGLimitWriteOnPlayerRequest) {
		t.Error("tenant_admin (which holds staff management) must not write RG bounds or player limits by role alone")
	}
}
