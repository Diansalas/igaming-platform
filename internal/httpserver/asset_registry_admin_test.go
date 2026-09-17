//go:build integration

// Stage 4H-B0-R6, Workstream A: HTTP-level tests for the Asset Registry /
// Asset Authorization admin API. These cover the part the package-level
// tests in internal/assetregistry cannot: that ADR 0037 §C.1's two-tier
// RBAC split is actually wired into the routes, in the exact shape
// `security`'s Stage 4H-B0-R5 test list demands ("a tenant-scoped staff
// token attempting every layer-1-3 operation is denied, INCLUDING when
// the tenant-scoped permission for layers 4-7 is held").
package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

func sendAssetJSON(t *testing.T, url, method, bearerToken string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	return resp
}

func newAssetCodeForHTTP() string {
	return "HT" + strings.ToUpper(uuid.NewString()[:8])
}

// A tenant_admin holds PermAssetAuthorizationWrite (layers 4-7) and must
// still be denied every layer-1-3 operation - holding the narrower
// permission must never leak into the platform tier.
func TestAssetRegistryAPI_TenantAdminDeniedEveryLayer1To3Operation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-asset-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-asset-pw-1").AccessToken

	code := newAssetCodeForHTTP()
	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/v1/admin/assets", nil},
		{http.MethodPost, "/v1/admin/assets/change-requests", map[string]any{
			"operation": "create", "asset_code": code, "asset_type": "fiat",
			"decimal_exponent": 2, "reason_code": "attempt"}},
		{http.MethodPost, "/v1/admin/assets/change-requests/" + uuid.NewString() + "/decision", map[string]any{
			"decision": "approve", "reason_code": "attempt"}},
		{http.MethodPost, "/v1/admin/assets", map[string]any{
			"code": code, "asset_type": "fiat", "decimal_exponent": 2,
			"display_name": "Attempt", "reason_code": "attempt"}},
		{http.MethodPatch, "/v1/admin/assets/EUR", map[string]any{
			"display_name": "Hijacked", "reason_code": "attempt"}},
		{http.MethodPut, "/v1/admin/assets/EUR/activation", map[string]any{
			"value": true, "reason_code": "attempt"}},
		{http.MethodPut, "/v1/admin/assets/EUR/platform-authorization", map[string]any{
			"value": true, "reason_code": "attempt"}},
		{http.MethodPut, "/v1/admin/assets/EUR/operation-eligibility", map[string]any{
			"operation": "wagering", "product": "casino", "eligible": true, "reason_code": "attempt"}},
	}
	for _, c := range cases {
		resp := sendAssetJSON(t, srv.URL+c.path, c.method, token, c.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: expected 403 for tenant_admin, got %d", c.method, c.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// And the mirror: a platform_admin's nil-tenant token cannot write
// layer-4-7 rows. RequireTenantScope denies it before any handler runs,
// and migration 0045's RLS would refuse the write anyway.
func TestAssetRegistryAPI_PlatformAdminDeniedTenantScopedLayers(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-asset-pw-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-asset-pw-1").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/asset-authorizations/tenant", http.MethodPut, token, map[string]any{
		"asset_code": "EUR", "product": "casino", "eligible": true, "reason_code": "attempt"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a platform_admin writing a tenant-scoped authorization, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/asset-authorizations/operation-eligibility/EUR", http.MethodPut, token, map[string]any{
		"operation": "wagering", "product": "casino", "eligible": true, "reason_code": "attempt"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a platform_admin writing a tenant eligibility override, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// The full administrative path over HTTP, in the order ADR 0037 §C.5.1
// requires - including that each dual-controlled step needs its own
// independently-approved request, and that an unapproved attempt is a 409
// rather than a 500.
func TestAssetRegistryAPI_FullDualControlledPathOverHTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	adminA := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-asset-a-1")
	adminB := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-asset-b-1")
	tokenA := mustLoginStaff(t, srv, "", adminA.Email, "pa-asset-a-1").AccessToken
	tokenB := mustLoginStaff(t, srv, "", adminB.Email, "pa-asset-b-1").AccessToken

	code := newAssetCodeForHTTP()

	// Creating with no approved request at all: 409, not 500.
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/assets", http.MethodPost, tokenA, map[string]any{
		"code": code, "asset_type": "fiat", "decimal_exponent": 2,
		"display_name": "HTTP Token", "reason_code": "new_listing"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 creating an asset with no approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	requestID := func(operation string) string {
		t.Helper()
		body := map[string]any{"operation": operation, "asset_code": code, "reason_code": "new_listing"}
		if operation == "create" {
			body["asset_type"] = "fiat"
			body["decimal_exponent"] = 2
		}
		r := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests", http.MethodPost, tokenA, body)
		defer r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 filing a %s request, got %d", operation, r.StatusCode)
		}
		var out map[string]any
		decodeBody(t, r, &out)
		return out["id"].(string)
	}

	createReq := requestID("create")

	// Self-approval over HTTP: 409.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests/"+createReq+"/decision", http.MethodPost, tokenA,
		map[string]any{"decision": "approve", "reason_code": "self"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for self-approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	approve := func(id, token string) {
		t.Helper()
		r := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests/"+id+"/decision", http.MethodPost, token,
			map[string]any{"decision": "approve", "reason_code": "reviewed"})
		defer r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 approving request %s, got %d", id, r.StatusCode)
		}
	}
	approve(createReq, tokenB)

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets", http.MethodPost, tokenA, map[string]any{
		"code": code, "asset_type": "fiat", "decimal_exponent": 2,
		"display_name": "HTTP Token", "reason_code": "new_listing"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating the asset, got %d", resp.StatusCode)
	}
	var created assetResponse
	decodeBody(t, resp, &created)
	if created.Active || created.PlatformAuthorized {
		t.Fatalf("a created asset must be inactive and unauthorized: %+v", created)
	}

	// Activation and platform authorization each need their own approval.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/activation", http.MethodPut, tokenA,
		map[string]any{"value": true, "reason_code": "ready"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 activating with no approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	approve(requestID("activate"), tokenB)
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/activation", http.MethodPut, tokenA,
		map[string]any{"value": true, "reason_code": "ready"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 activating with an approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	approve(requestID("platform_authorize"), tokenB)
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/platform-authorization", http.MethodPut, tokenA,
		map[string]any{"value": true, "reason_code": "cleared"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 granting platform authorization, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Layer 7 platform default (platform tier).
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/operation-eligibility", http.MethodPut, tokenA,
		map[string]any{"operation": "wagering", "product": "casino", "eligible": true, "reason_code": "cleared"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting the platform eligibility default, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Layers 4/6 (tenant tier).
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-asset-pw-2")
	tenantToken := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-asset-pw-2").AccessToken
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/asset-authorizations/tenant", http.MethodPut, tenantToken,
		map[string]any{"asset_code": code, "product": "casino", "eligible": true, "reason_code": "offering"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 authorizing the tenant, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A widening tenant override is refused at write time: 403, not 500.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/asset-authorizations/operation-eligibility/"+code, http.MethodPut, tenantToken,
		map[string]any{"operation": "withdrawal", "product": "casino", "eligible": true, "reason_code": "widen"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a widening tenant override, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A narrowing one is accepted.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/asset-authorizations/operation-eligibility/"+code, http.MethodPut, tenantToken,
		map[string]any{"operation": "wagering", "product": "casino", "eligible": false, "reason_code": "narrow"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a narrowing tenant override, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The identity fields are not even ADDRESSABLE through the metadata
	// endpoint: its request body has no such fields, and decodeJSON
	// rejects unknown ones outright (DisallowUnknownFields), so an attempt
	// to smuggle decimal_exponent/asset_type/network/code through op 2 is
	// a 400 before any SQL runs. Migration 0044's immutability trigger is
	// the backstop behind that (proven in internal/assetregistry's own
	// TestIdentityFields_AreImmutableAtTheDatabase).
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code, http.MethodPatch, tokenA,
		map[string]any{"display_name": "Renamed", "decimal_exponent": 8, "reason_code": "rename"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when smuggling an identity field into the metadata endpoint, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code, http.MethodPatch, tokenA,
		map[string]any{"display_name": "Renamed", "reason_code": "rename"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 updating display metadata, got %d", resp.StatusCode)
	}
	var patched assetResponse
	decodeBody(t, resp, &patched)
	if patched.DisplayName != "Renamed" {
		t.Fatalf("display_name was not updated: %+v", patched)
	}
	if patched.DecimalExponent != 2 || patched.AssetType != "fiat" || patched.Network != "" || patched.Code != code {
		t.Fatalf("identity fields must be unchanged by the metadata endpoint, got %+v", patched)
	}

	// Creation likewise has no `active`/`platform_authorized` field to
	// set (ADR 0037 §C.5.5), so an attempt is a 400, not a silent
	// auto-authorization.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets", http.MethodPost, tokenA, map[string]any{
		"code": newAssetCodeForHTTP(), "asset_type": "fiat", "decimal_exponent": 2,
		"display_name": "Auto", "active": true, "platform_authorized": true, "reason_code": "attempt"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when passing active/platform_authorized to create, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Every mutating endpoint requires a reason_code - CLAUDE.md's audit rule
// includes it, so a silently-empty one is not acceptable.
func TestAssetRegistryAPI_ReasonCodeIsMandatory(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-asset-pw-2")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-asset-pw-2").AccessToken

	resp := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests", http.MethodPost, token, map[string]any{
		"operation": "create", "asset_code": newAssetCodeForHTTP(), "asset_type": "fiat", "decimal_exponent": 2})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 with no reason_code, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/EUR/activation", http.MethodPut, token, map[string]any{
		"value": true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 with no reason_code, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A missing boolean is also a 400, never defaulted - defaulting would
	// make "grant" reachable by omission, the same fail-open shape as
	// finding S-4.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/EUR/activation", http.MethodPut, token, map[string]any{
		"reason_code": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 with no explicit value, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
