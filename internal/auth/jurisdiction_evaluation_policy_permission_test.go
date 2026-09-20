package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stage 4I Phase D introduced exactly one new permission
// (docs/decisions/0043-jurisdiction-evaluation-policy-configuration.md
// Decision 5): PermJurisdictionEvaluationPolicyWrite, PLATFORM-ONLY,
// granted only to RolePlatformAdmin, following
// PermJurisdictionRegistryManage's exact precedent (see
// jurisdiction_permission_test.go/tenant_licence_permission_test.go,
// which this file mirrors).

// TestJurisdictionEvaluationPolicyWrite_PlatformAdminOnly pins the "one
// new PLATFORM-ONLY permission on RolePlatformAdmin" grant.
// jurisdiction_precedence_configs' write-side RLS (migration 0075)
// independently requires app.platform_admin_principal_id to be set AND
// app.tenant_id/app.player_account_id to be unset. No caller checks this
// permission yet (no HTTP route exists, and CreateEvaluationPolicyVersion
// itself checks only assertPlatformScope + RLS - see
// PermJurisdictionEvaluationPolicyWrite's own doc comment) - this test
// pins the role grant in advance so it is not re-litigated by whoever
// builds the future authoring surface. The grant must still be exactly
// this narrow, since no tenant-scoped role has any legitimate call to
// author platform-wide evaluation policy shared by every tenant licensed
// in that jurisdiction.
func TestJurisdictionEvaluationPolicyWrite_PlatformAdminOnly(t *testing.T) {
	if !RoleHasPermission(RolePlatformAdmin, PermJurisdictionEvaluationPolicyWrite) {
		t.Error("platform_admin must hold jurisdiction_evaluation_policy:write - it is the sole intended grantee")
	}
	for _, r := range allRoles {
		if r == RolePlatformAdmin {
			continue
		}
		if RoleHasPermission(r, PermJurisdictionEvaluationPolicyWrite) {
			t.Errorf("role %q must NOT hold jurisdiction_evaluation_policy:write - it is platform-only", r)
		}
	}
}

// TestJurisdictionEvaluationPolicyWrite_NoPlayerGrant restates, for this
// permission specifically, that RolePlayer holds nothing.
func TestJurisdictionEvaluationPolicyWrite_NoPlayerGrant(t *testing.T) {
	if RoleHasPermission(RolePlayer, PermJurisdictionEvaluationPolicyWrite) {
		t.Error("RolePlayer must never hold jurisdiction_evaluation_policy:write")
	}
}

// TestJurisdictionEvaluationPolicyWrite_NotTenantAdmin is the most
// plausible accidental-grant target: RoleTenantAdmin holds the analogous
// PermJurisdictionResolutionActiveWrite and PermAssetAuthorizationWrite
// (both tenant-scoped precedents), but authoring platform-wide evaluation
// policy shared by every tenant licensed in a jurisdiction is a
// materially different, platform-only authorizing act.
func TestJurisdictionEvaluationPolicyWrite_NotTenantAdmin(t *testing.T) {
	if RoleHasPermission(RoleTenantAdmin, PermJurisdictionEvaluationPolicyWrite) {
		t.Error("RoleTenantAdmin must NOT hold jurisdiction_evaluation_policy:write")
	}
}

// TestJurisdictionEvaluationPolicyWrite_NoHTTPRouteConsumesIt pins, with a
// real test rather than manual grep, ADR 0043 Decision 5's "no HTTP route
// or OpenAPI change is added in this phase" claim: no file under
// internal/httpserver/ references this permission constant, which would
// indicate an HTTP route consumes it. Stage 4I Phase D deliberately builds
// the Go write API only, exercised by integration tests under
// db.Pool.WithPlatformAdmin.
func TestJurisdictionEvaluationPolicyWrite_NoHTTPRouteConsumesIt(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "httpserver"))
	if err != nil {
		t.Fatalf("resolve internal/httpserver path: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("internal/httpserver directory not found at %s: %v", root, err)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "PermJurisdictionEvaluationPolicyWrite") {
			t.Errorf("%s references PermJurisdictionEvaluationPolicyWrite - no HTTP route may consume this permission in this phase (ADR 0043 Decision 5)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/httpserver: %v", err)
	}
}
