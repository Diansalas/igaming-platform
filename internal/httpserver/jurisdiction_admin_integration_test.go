//go:build integration

// Stage 4I item B-1/B-6: HTTP-level tests for the jurisdiction admin
// surface. Covers what internal/jurisdiction's own package-level tests
// cannot: that the new platform-only/tenant-scoped RBAC split is
// actually wired into the routes, mirroring
// asset_registry_admin_test.go's own conventions exactly.
package httpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

func newJurisdictionCodeForHTTP() string {
	return "HT-" + strings.ToUpper(uuid.NewString()[:8])
}

// A tenant_admin must be denied every platform-only jurisdiction-registry
// operation, even though it holds PermJurisdictionResolutionActiveWrite
// (the tenant-scoped sibling permission) - holding the narrower
// permission must never leak into the platform tier, exactly like
// PermAssetAuthorizationWrite versus PermAssetRegistryManage.
func TestJurisdictionRegistryAPI_TenantAdminDeniedPlatformOperations(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-jur-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-jur-pw-1").AccessToken

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/v1/admin/jurisdictions", nil},
		{http.MethodPost, "/v1/admin/jurisdictions", map[string]any{
			"code": newJurisdictionCodeForHTTP(), "name": "Attempt", "reason_code": "attempt"}},
		{http.MethodGet, "/v1/admin/licences", nil},
		{http.MethodPost, "/v1/admin/licences", map[string]any{
			"jurisdiction_id": uuid.NewString(), "licensee": "tenant", "licence_number": "L-1", "reason_code": "attempt"}},
	}
	for _, c := range cases {
		resp := sendAssetJSON(t, srv.URL+c.path, c.method, token, c.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: expected 403 for tenant_admin, got %d", c.method, c.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// The mirror: a platform_admin's nil-tenant token cannot write the
// tenant-scoped jurisdiction_resolution_active fact - RequireTenantScope
// denies it before any handler runs.
func TestJurisdictionRegistryAPI_PlatformAdminDeniedResolutionActiveWrite(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-jur-pw-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-jur-pw-1").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-resolution-active/play", http.MethodPut, token, map[string]any{"active": true})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a platform_admin writing a tenant-scoped resolution-active fact, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// The full happy path: a platform_admin creates a jurisdiction and a
// licence for it over HTTP, and both are visible via the list endpoints.
func TestJurisdictionRegistryAPI_CreateAndListOverHTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-jur-pw-2")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-jur-pw-2").AccessToken

	code := newJurisdictionCodeForHTTP()
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdictions", http.MethodPost, token, map[string]any{
		"code": code, "name": "HTTP Test Jurisdiction", "reason_code": "onboarding"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating a jurisdiction, got %d", resp.StatusCode)
	}
	var created jurisdictionResponse
	decodeBody(t, resp, &created)

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/licences", http.MethodPost, token, map[string]any{
		"jurisdiction_id": created.ID, "licensee": "platform", "licence_number": "HTTP-LIC-1", "reason_code": "onboarding"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating a licence, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdictions", http.MethodGet, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing jurisdictions, got %d", resp.StatusCode)
	}
	var list []jurisdictionResponse
	decodeBody(t, resp, &list)
	found := false
	for _, j := range list {
		if j.Code == code {
			found = true
		}
	}
	if !found {
		t.Fatalf("created jurisdiction %q not found via GET /v1/admin/jurisdictions", code)
	}

	// A forged jurisdiction code with unknown fields is a 400, not a
	// silent ignore (DisallowUnknownFields, canonical-model §4.4 Layer 1).
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdictions", http.MethodPost, token, map[string]any{
		"code": newJurisdictionCodeForHTTP(), "name": "Forged", "reason_code": "attempt", "id": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for an unknown field (id) in the request body, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// A tenant_admin CAN write its own tenant's resolution-active fact, and
// reading it back shows the change - the tenant-scoped half of B-6's
// surface, RequireTenantScope + PermJurisdictionResolutionActiveWrite.
func TestJurisdictionResolutionActiveAPI_TenantAdminRoundTrip(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-jur-active-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-jur-active-1").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-resolution-active/play", http.MethodPut, token, map[string]any{"active": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting resolution-active, got %d", resp.StatusCode)
	}
	var rec resolutionActiveResponse
	decodeBody(t, resp, &rec)
	if !rec.Active || rec.OperationClass != "play" {
		t.Fatalf("unexpected response: %+v", rec)
	}

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-resolution-active", http.MethodGet, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing resolution-active, got %d", resp.StatusCode)
	}
	var list []resolutionActiveResponse
	decodeBody(t, resp, &list)
	found := false
	for _, r := range list {
		if r.OperationClass == "play" && r.Active {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the play/active=true fact to be visible via the list endpoint")
	}
}

// A second tenant must not be able to observe or influence the first
// tenant's resolution-active facts through this surface.
func TestJurisdictionResolutionActiveAPI_CrossTenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	taA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleTenantAdmin, "ta-jur-a-1")
	taB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "ta-jur-b-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, taA.Email, "ta-jur-a-1").AccessToken
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, taB.Email, "ta-jur-b-1").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-resolution-active/play", http.MethodPut, tokenA, map[string]any{"active": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting tenant A's fact, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-resolution-active", http.MethodGet, tokenB, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing tenant B's (empty) facts, got %d", resp.StatusCode)
	}
	var list []resolutionActiveResponse
	decodeBody(t, resp, &list)
	if len(list) != 0 {
		t.Fatalf("tenant B must see 0 resolution-active rows belonging to tenant A, saw %d", len(list))
	}
}
