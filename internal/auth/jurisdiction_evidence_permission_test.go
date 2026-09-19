package auth

import "testing"

// Stage 4I Phase B introduced exactly two new permissions
// (docs/plans/stage-4i-jurisdiction-implementation-plan.md Phase B):
// PermPlayerResidenceRead and PermJurisdictionEvidenceCollectionActivate,
// both granted ONLY to RoleCompliance, following
// PermTenantLicenceAssign's exact single-grantee precedent (see
// tenant_licence_permission_test.go, which this file mirrors).

// TestJurisdictionEvidenceCollectionActivate_ComplianceOnly pins the "two
// new permissions, RoleCompliance only" grant.
func TestJurisdictionEvidenceCollectionActivate_ComplianceOnly(t *testing.T) {
	if !RoleHasPermission(RoleCompliance, PermPlayerResidenceRead) {
		t.Error("compliance must hold player_residence:read - it is the sole intended grantee")
	}
	if !RoleHasPermission(RoleCompliance, PermJurisdictionEvidenceCollectionActivate) {
		t.Error("compliance must hold jurisdiction_evidence_collection:activate - it is the sole intended grantee")
	}
	for _, r := range allRoles {
		if r == RoleCompliance {
			continue
		}
		if RoleHasPermission(r, PermPlayerResidenceRead) {
			t.Errorf("role %q must NOT hold player_residence:read - it is granted only to compliance", r)
		}
		if RoleHasPermission(r, PermJurisdictionEvidenceCollectionActivate) {
			t.Errorf("role %q must NOT hold jurisdiction_evidence_collection:activate - it is granted only to compliance", r)
		}
	}
}

// TestJurisdictionEvidenceCollectionActivate_NoPlayerGrant restates, for
// these two permissions specifically, that RolePlayer holds nothing.
func TestJurisdictionEvidenceCollectionActivate_NoPlayerGrant(t *testing.T) {
	if RoleHasPermission(RolePlayer, PermPlayerResidenceRead) {
		t.Error("RolePlayer must never hold player_residence:read")
	}
	if RoleHasPermission(RolePlayer, PermJurisdictionEvidenceCollectionActivate) {
		t.Error("RolePlayer must never hold jurisdiction_evidence_collection:activate")
	}
}

// TestJurisdictionEvidenceCollectionActivate_NotTenantAdmin is the most
// plausible accidental-grant target: RoleTenantAdmin holds the analogous
// engineering-precondition permission PermJurisdictionResolutionActiveWrite,
// but switching on collection of privacy-sensitive personal data is a
// lawful-basis act, not a commercial-configuration act, so it must NOT
// also hold either new Phase B permission.
func TestJurisdictionEvidenceCollectionActivate_NotTenantAdmin(t *testing.T) {
	if RoleHasPermission(RoleTenantAdmin, PermPlayerResidenceRead) {
		t.Error("RoleTenantAdmin must NOT hold player_residence:read")
	}
	if RoleHasPermission(RoleTenantAdmin, PermJurisdictionEvidenceCollectionActivate) {
		t.Error("RoleTenantAdmin must NOT hold jurisdiction_evidence_collection:activate, despite holding the analogous PermJurisdictionResolutionActiveWrite")
	}
}
