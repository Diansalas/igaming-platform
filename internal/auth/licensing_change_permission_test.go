package auth

import "testing"

// Migration 0177 (MKT-DUAL-1): the three static licensing change permissions. :approve is held by platform_admin ONLY (the
// interim answer to HDR-M-1); request / read by platform_admin, tenant_admin and compliance; nobody else (and no unknown role)
// holds any.
func TestLicensingChangePermissions_RoleMatrix(t *testing.T) {
	pa, ta, co := RolePlatformAdmin, RoleTenantAdmin, RoleCompliance
	rows := []struct {
		perm  Permission
		roles map[Role]bool
	}{
		{PermLicensingChangeRequest, map[Role]bool{pa: true, ta: true, co: true}},
		{PermLicensingChangeApprove, map[Role]bool{pa: true}},
		{PermLicensingChangeRead, map[Role]bool{pa: true, ta: true, co: true}},
	}
	for _, r := range rows {
		for _, role := range append(append([]Role(nil), allRoles...), "", "unknown", "PLATFORM_ADMIN", RolePlayer) {
			if got, want := RoleHasPermission(role, r.perm), r.roles[role]; got != want {
				t.Errorf("role %q %s = %v, want %v", role, r.perm, got, want)
			}
		}
	}
	if string(PermLicensingChangeRequest) != "licensing_change:request" || string(PermLicensingChangeApprove) != "licensing_change:approve" ||
		string(PermLicensingChangeRead) != "licensing_change:read" {
		t.Fatal("permission strings drifted from migration 0177")
	}
}

// lic4 slice 4a: the registry read permission is platform_admin ONLY.
func TestJurisdictionRegistryReadPermission_PlatformAdminOnly(t *testing.T) {
	if string(PermJurisdictionRegistryRead) != "jurisdiction_registry:read" {
		t.Fatal("permission string drifted")
	}
	for _, role := range append(append([]Role(nil), allRoles...), "", "unknown", "PLATFORM_ADMIN", RolePlayer) {
		if got, want := RoleHasPermission(role, PermJurisdictionRegistryRead), role == RolePlatformAdmin; got != want {
			t.Errorf("role %q jurisdiction_registry:read = %v, want %v", role, got, want)
		}
	}
}

// lic10 (L-20): the KYC enforcement policy authoring permissions are platform_admin ONLY, for every other role, the
// empty role and look-alikes (the table is platform-wide configuration; no tenant role may author it).
func TestKYCEnforcementPolicyPermissions_PlatformAdminOnly(t *testing.T) {
	if string(PermKYCEnforcementPolicyManage) != "kyc_enforcement_policy:manage" || string(PermKYCEnforcementPolicyRead) != "kyc_enforcement_policy:read" {
		t.Fatal("permission string drifted")
	}
	for _, perm := range []Permission{PermKYCEnforcementPolicyManage, PermKYCEnforcementPolicyRead} {
		for _, role := range append(append([]Role(nil), allRoles...), "", "unknown", "PLATFORM_ADMIN", RolePlayer) {
			if got, want := RoleHasPermission(role, perm), role == RolePlatformAdmin; got != want {
				t.Errorf("role %q %s = %v, want %v", role, perm, got, want)
			}
		}
	}
}
