package auth

import "testing"

// Stage 4I introduced exactly two permissions and no new role
// (docs/governance/stage-4i-canonical-model.md §6.1: "No other permission
// and no new role is authorized"). Nothing in the repository asserted
// their scoping, so the platform-vs-tenant split they encode rested on
// reading the map by eye.
//
// These tests are `security`'s Stage 4I final independent
// security/compliance certification (item 3: re-audit the two new
// permissions in the FINAL role-permission mapping, verify rather than
// assume). They iterate `allRoles` (bonus_permission_test.go) rather than
// naming a subset, so a role added later cannot silently acquire either
// permission without failing here.

// TestJurisdictionRegistryManage_PlatformAdminOnly pins
// canonical-model §6.1's "one new PLATFORM-ONLY permission on
// RolePlatformAdmin, following PermCasinoCatalogueManage's exact
// precedent."
//
// `jurisdictions` and `licences` carry NO row-level security at all
// (canonical-model §6.1 keeps them platform-scoped deliberately, because
// every tenant-scoped transaction must be able to read them as an FK
// target). That makes this permission check at the HTTP layer the
// ENTIRE control on the write side - there is no database backstop
// behind it, unlike every tenant-owned table in this platform. So the
// grant set is load-bearing in a way most permissions' are not: a
// tenant-scoped role holding it could mint a jurisdiction or a licence
// row that every other tenant then reads, and the only trace would be
// the audit record.
func TestJurisdictionRegistryManage_PlatformAdminOnly(t *testing.T) {
	if !RoleHasPermission(RolePlatformAdmin, PermJurisdictionRegistryManage) {
		t.Error("platform_admin must hold jurisdiction_registry:manage - it is the sole intended grantee (canonical-model §6.1)")
	}
	for _, r := range allRoles {
		if r == RolePlatformAdmin {
			continue
		}
		if RoleHasPermission(r, PermJurisdictionRegistryManage) {
			t.Errorf("role %q must NOT hold jurisdiction_registry:manage - it is platform-only, and `jurisdictions`/`licences` have no RLS behind it", r)
		}
	}
}

// TestJurisdictionResolutionActiveWrite_TenantAdminOnly pins
// canonical-model §4.2's "write permission follows
// PermAssetAuthorizationWrite's precedent: tenant-scoped, granted to
// RoleTenantAdmin, never RolePlatformAdmin." The "never
// RolePlatformAdmin" half is explicit in the source ruling and is
// asserted separately below, because a platform principal carries a nil
// tenant and has no tenant scope in which migration 0071's RLS would
// accept the write - granting it would be a capability nothing can
// exercise, which CLAUDE.md's no-fake-completion rule treats as a defect
// in its own right.
func TestJurisdictionResolutionActiveWrite_TenantAdminOnly(t *testing.T) {
	if !RoleHasPermission(RoleTenantAdmin, PermJurisdictionResolutionActiveWrite) {
		t.Error("tenant_admin must hold jurisdiction_resolution_active:write - it is the sole intended grantee (canonical-model §4.2)")
	}
	if RoleHasPermission(RolePlatformAdmin, PermJurisdictionResolutionActiveWrite) {
		t.Error("platform_admin must NOT hold jurisdiction_resolution_active:write - canonical-model §4.2 says 'never RolePlatformAdmin'")
	}
	for _, r := range allRoles {
		if r == RoleTenantAdmin {
			continue
		}
		if RoleHasPermission(r, PermJurisdictionResolutionActiveWrite) {
			t.Errorf("role %q must NOT hold jurisdiction_resolution_active:write - it is tenant-scoped and granted only to tenant_admin", r)
		}
	}
}

// TestJurisdictionPermissions_NeverHeldTogether is the separation the
// platform-vs-tenant split exists to create, asserted directly rather
// than left as an emergent property of the two tests above. One role
// holding both would be able to author a jurisdiction row AND record its
// own tenant as resolution-active against it - the same
// "author-the-thing-that-gates-you" shape security-architecture.md
// §B1.1 hard constraint 4 forbids for bonus approval policy.
func TestJurisdictionPermissions_NeverHeldTogether(t *testing.T) {
	for _, r := range allRoles {
		if RoleHasPermission(r, PermJurisdictionRegistryManage) && RoleHasPermission(r, PermJurisdictionResolutionActiveWrite) {
			t.Errorf("role %q holds BOTH jurisdiction registry authority and resolution-active write authority - the platform/tenant split forbids it", r)
		}
	}
}

// TestJurisdictionPermissions_NoPlayerGrant restates, for the two Stage
// 4I permissions specifically, that RolePlayer holds nothing. RolePlayer
// is an empty permission set today, so this is cheap - and it is the
// assertion that would fail first if a future change ever attached
// jurisdiction authority to a player-facing role.
func TestJurisdictionPermissions_NoPlayerGrant(t *testing.T) {
	for _, p := range []Permission{PermJurisdictionRegistryManage, PermJurisdictionResolutionActiveWrite} {
		if RoleHasPermission(RolePlayer, p) {
			t.Errorf("RolePlayer must never hold %q", p)
		}
	}
}
