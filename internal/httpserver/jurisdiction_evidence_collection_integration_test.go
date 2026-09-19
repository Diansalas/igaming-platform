//go:build integration

// Stage 4I Phase B: HTTP-level tests for the tenant-scoped
// jurisdiction_evidence_collection_active admin surface. Mirrors
// jurisdiction_admin_integration_test.go's own resolution-active
// coverage conventions exactly, substituting
// PermJurisdictionEvidenceCollectionActivate (RoleCompliance-only) for
// PermJurisdictionResolutionActiveWrite (RoleTenantAdmin).
package httpserver

import (
	"net/http"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// A compliance-role token can turn an evidence-collection-active fact on
// and read it back via the list endpoint - the full happy path.
func TestJurisdictionEvidenceCollectionAPI_ComplianceRoundTrip(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	c := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-jec-1")
	token := mustLoginStaff(t, srv, tenant.Slug, c.Email, "compliance-jec-1").AccessToken

	// A fresh tenant starts with an empty list.
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection", http.MethodGet, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing a fresh tenant's evidence-collection-active facts, got %d", resp.StatusCode)
	}
	var empty []evidenceCollectionActiveResponse
	decodeBody(t, resp, &empty)
	if len(empty) != 0 {
		t.Fatalf("expected 0 rows for a fresh tenant, got %d", len(empty))
	}

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, token,
		map[string]any{"active": true, "reason_code": "stage-4i-phase-b-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting evidence-collection-active, got %d", resp.StatusCode)
	}
	var rec evidenceCollectionActiveResponse
	decodeBody(t, resp, &rec)
	if !rec.Active || rec.EvidenceType != "declared_residence" {
		t.Fatalf("unexpected response: %+v", rec)
	}

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection", http.MethodGet, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing evidence-collection-active, got %d", resp.StatusCode)
	}
	var list []evidenceCollectionActiveResponse
	decodeBody(t, resp, &list)
	found := false
	for _, r := range list {
		if r.EvidenceType == "declared_residence" && r.Active {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the declared_residence/active=true fact to be visible via the list endpoint")
	}
}

// A tenant_admin holds the DIFFERENT, near-miss permission
// PermJurisdictionResolutionActiveWrite, not
// PermJurisdictionEvidenceCollectionActivate - it must be denied both
// the GET and the PUT.
func TestJurisdictionEvidenceCollectionAPI_TenantAdminDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-jec-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-jec-1").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection", http.MethodGet, token, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin GET, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, token,
		map[string]any{"active": true, "reason_code": "attempt"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin PUT, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// A compliance-role token for tenant A must not be able to see or set
// tenant B's evidence-collection-active facts through this surface.
func TestJurisdictionEvidenceCollectionAPI_CrossTenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	cA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleCompliance, "compliance-jec-a-1")
	cB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleCompliance, "compliance-jec-b-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, cA.Email, "compliance-jec-a-1").AccessToken
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, cB.Email, "compliance-jec-b-1").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, tokenA,
		map[string]any{"active": true, "reason_code": "stage-4i-phase-b-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting tenant A's fact, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection", http.MethodGet, tokenB, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing tenant B's (empty) facts, got %d", resp.StatusCode)
	}
	var list []evidenceCollectionActiveResponse
	decodeBody(t, resp, &list)
	if len(list) != 0 {
		t.Fatalf("tenant B must see 0 evidence-collection-active rows belonging to tenant A, saw %d", len(list))
	}

	// Tenant B cannot flip tenant A's row via its own PUT either - the
	// write is scoped to tenant B's own connection-level tenant_id, so
	// this creates/updates tenant B's own row, never tenant A's.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, tokenB,
		map[string]any{"active": false, "reason_code": "stage-4i-phase-b-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting tenant B's own fact, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection", http.MethodGet, tokenA, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing tenant A's facts, got %d", resp.StatusCode)
	}
	decodeBody(t, resp, &list)
	found := false
	for _, r := range list {
		if r.EvidenceType == "declared_residence" && r.Active {
			found = true
		}
	}
	if !found {
		t.Fatal("tenant B's write must not have affected tenant A's declared_residence/active=true fact")
	}
}

// An unknown evidenceType path segment is a 400, not a silent pass-through
// to the database.
func TestJurisdictionEvidenceCollectionAPI_InvalidEvidenceType(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	c := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-jec-2")
	token := mustLoginStaff(t, srv, tenant.Slug, c.Email, "compliance-jec-2").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/not_a_real_type", http.MethodPut, token,
		map[string]any{"active": true, "reason_code": "attempt"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for an unknown evidence_type, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// A write with no reason_code is a 400 - CLAUDE.md's audit rule.
func TestJurisdictionEvidenceCollectionAPI_MissingReasonCode(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	c := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-jec-3")
	token := mustLoginStaff(t, srv, tenant.Slug, c.Email, "compliance-jec-3").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, token,
		map[string]any{"active": true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for a write with no reason_code, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// No token at all is a 401 on both routes.
func TestJurisdictionEvidenceCollectionAPI_NoToken(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection", http.MethodGet, "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for GET with no token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, "",
		map[string]any{"active": true, "reason_code": "attempt"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for PUT with no token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
