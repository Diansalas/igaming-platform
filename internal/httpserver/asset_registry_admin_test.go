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
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

	requestID := func(operation string, extra ...map[string]any) string {
		t.Helper()
		body := map[string]any{"operation": operation, "asset_code": code, "reason_code": "new_listing"}
		if operation == "create" {
			body["asset_type"] = "fiat"
			body["decimal_exponent"] = 2
		}
		for _, e := range extra {
			for k, v := range e {
				body[k] = v
			}
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

	// Layer 7 platform default (platform tier). Since migration 0047 this
	// GRANT is dual-controlled too - it flips a gate for every tenant on
	// the platform - so an unapproved attempt is a 409, exactly like an
	// unapproved activation.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/operation-eligibility", http.MethodPut, tokenA,
		map[string]any{"operation": "wagering", "product": "casino", "eligible": true, "reason_code": "cleared"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 granting a platform-wide eligibility default with no approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	approve(requestID("platform_operation_eligibility", map[string]any{
		"eligibility_operation": "wagering", "eligibility_product": "casino"}), tokenB)

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/operation-eligibility", http.MethodPut, tokenA,
		map[string]any{"operation": "wagering", "product": "casino", "eligible": true, "reason_code": "cleared"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting the platform eligibility default, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The approval was consumed and was bound to (wagering, casino): a
	// different operation needs its own.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/operation-eligibility", http.MethodPut, tokenA,
		map[string]any{"operation": "deposit", "product": "casino", "eligible": true, "reason_code": "cleared"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 reusing a consumed/mismatched approval for a different operation, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Revoking a platform-wide default stays single-actor (the
	// fail-closed direction must not wait for a second approver), so this
	// needs no approval at all. Restored immediately afterwards, since
	// the tenant-tier assertions below depend on the grant.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/"+code+"/operation-eligibility", http.MethodPut, tokenA,
		map[string]any{"operation": "deposit", "product": "casino", "eligible": false, "reason_code": "not_offered"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 revoking a platform-wide default with no approval (single-actor by design), got %d", resp.StatusCode)
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

// ======================================================================
// Migration 0047 regression tests at the HTTP boundary.
// ======================================================================

// The exploit as an operator would actually run it: one human who has run
// cmd/seed-admin twice holds two platform_admin logins with person_id
// NULL - the only platform_admin state a real deployment can produce -
// and drives the whole registry flow through the public API. Every call
// is refused, with a 403 that names the actual reason rather than a
// generic error.
//
// This test FAILS against migration 0044 (the full create ->
// self-approve -> activate chain returns 201/201/200) and PASSES against
// 0047.
func TestAssetRegistryAPI_UnlinkedPlatformAdminsCannotCompleteFourEyes(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	// Two `seed-admin` runs by one person.
	unlinkedA := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-unlinked-a-1")
	unlinkedB := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-unlinked-b-1")
	tokenA := mustLoginStaff(t, srv, "", unlinkedA.Email, "pa-unlinked-a-1").AccessToken
	tokenB := mustLoginStaff(t, srv, "", unlinkedB.Email, "pa-unlinked-b-1").AccessToken

	code := newAssetCodeForHTTP()

	// Filing the request is refused - 403, because the refusal is a
	// property of the principal, not of the payload or the request state.
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests", http.MethodPost, tokenA, map[string]any{
		"operation": "create", "asset_code": code, "asset_type": "fiat",
		"decimal_exponent": 2, "reason_code": "exploit"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 filing a change request as an unlinked platform admin, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Creating without one is still a 409 - the mutation itself is gated
	// independently, so the exploit cannot be completed by skipping the
	// paperwork step.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets", http.MethodPost, tokenA, map[string]any{
		"code": code, "asset_type": "fiat", "decimal_exponent": 2,
		"display_name": "Exploit", "reason_code": "exploit"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 creating with no approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// And the approval side: a legitimately-filed request (by a properly
	// person-linked admin) cannot be approved by the unlinked account.
	linked := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-linked-1")
	linkedToken := mustLoginStaff(t, srv, "", linked.Email, "pa-linked-1").AccessToken
	r := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests", http.MethodPost, linkedToken, map[string]any{
		"operation": "create", "asset_code": code, "asset_type": "fiat",
		"decimal_exponent": 2, "reason_code": "new_listing"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 filing as a person-linked admin, got %d", r.StatusCode)
	}
	var filed map[string]any
	decodeBody(t, r, &filed)

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests/"+filed["id"].(string)+"/decision",
		http.MethodPost, tokenB, map[string]any{"decision": "approve", "reason_code": "rubber_stamp"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 approving as an unlinked platform admin, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Nothing was created.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets", http.MethodPost, linkedToken, map[string]any{
		"code": code, "asset_type": "fiat", "decimal_exponent": 2,
		"display_name": "Exploit", "reason_code": "new_listing"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 - the unlinked approval must not have authorized anything, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// The new request type's payload is validated at the boundary: the
// approver must be shown the exact layer-7 fact being granted, and
// eligibility fields must not be silently ignored when they cannot
// apply.
func TestAssetRegistryAPI_PlatformOperationEligibilityRequestPayloadIsValidated(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-elig-pw-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-elig-pw-1").AccessToken

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing eligibility_operation", map[string]any{
			"operation": "platform_operation_eligibility", "asset_code": "EUR", "reason_code": "x"}},
		{"unknown eligibility_operation", map[string]any{
			"operation": "platform_operation_eligibility", "asset_code": "EUR",
			"eligibility_operation": "teleport", "reason_code": "x"}},
		{"eligibility fields on an unrelated operation", map[string]any{
			"operation": "activate", "asset_code": "EUR",
			"eligibility_operation": "wagering", "reason_code": "x"}},
	}
	for _, c := range cases {
		resp := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests", http.MethodPost, token, c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", c.name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// eligibility_product may be omitted - that means EVERY product, and
	// is recorded explicitly in the approved payload rather than left
	// absent, so the approver approves the breadth too.
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests", http.MethodPost, token, map[string]any{
		"operation": "platform_operation_eligibility", "asset_code": "EUR",
		"eligibility_operation": "wagering", "reason_code": "every_product"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 filing an every-product eligibility request, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// The ordering dependency migration 0047's header names, proven rather
// than assumed: the hardened four-eyes control is only usable because a
// path exists to person-link a PLATFORM-scoped staff account. That path
// (`identity-compliance`'s POST /v1/admin/platform-staff/{id}/person-link
// and cmd/seed-admin's new -person-id/-create-person flags) was built in
// a parallel dispatch, so this test is the seam between the two: it takes
// an account in the exact state cmd/seed-admin's DEFAULT still produces
// (unlinked), remediates it through the public route, and then completes
// four-eyes with it.
//
// Without that path, migration 0047 is a permanent outage of the asset
// registry's administrative surface rather than a hardening. This test is
// what stops that from being a claim taken on trust.
func TestAssetRegistryAPI_PersonLinkingARemediatedPlatformAdminRestoresFourEyes(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	// The requester is properly linked; the approver is in the state a
	// real `seed-admin` run leaves behind.
	requester := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-relink-req-1")
	unlinked := mustCreateUnlinkedStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-relink-app-1")
	requesterToken := mustLoginStaff(t, srv, "", requester.Email, "pa-relink-req-1").AccessToken
	unlinkedToken := mustLoginStaff(t, srv, "", unlinked.Email, "pa-relink-app-1").AccessToken

	code := newAssetCodeForHTTP()
	r := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests", http.MethodPost, requesterToken, map[string]any{
		"operation": "create", "asset_code": code, "asset_type": "fiat",
		"decimal_exponent": 2, "reason_code": "new_listing"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 filing the request, got %d", r.StatusCode)
	}
	var filed map[string]any
	decodeBody(t, r, &filed)
	reqID := filed["id"].(string)

	// Before remediation: refused, and specifically as a 403 about the
	// principal rather than a 409 about the approval.
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests/"+reqID+"/decision",
		http.MethodPost, unlinkedToken, map[string]any{"decision": "approve", "reason_code": "reviewed"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 approving as an unlinked platform admin, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Remediate through the platform-scoped person-link route. A DISTINCT
	// person from the requester's - linking both to one person would
	// (correctly) still be refused as self-approval.
	personID := uuid.New()
	if err := pool.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("create person: %v", err)
	}
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/platform-staff/"+unlinked.ID.String()+"/person-link",
		http.MethodPost, requesterToken, map[string]any{"person_id": personID.String()})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected the platform-staff person-link route to succeed, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// After remediation the same account is an eligible approver, and the
	// dual-controlled create goes through.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets/change-requests/"+reqID+"/decision",
		http.MethodPost, unlinkedToken, map[string]any{"decision": "approve", "reason_code": "reviewed"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 approving as a now-person-linked platform admin, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = sendAssetJSON(t, srv.URL+"/v1/admin/assets", http.MethodPost, requesterToken, map[string]any{
		"code": code, "asset_type": "fiat", "decimal_exponent": 2,
		"display_name": "Relinked", "reason_code": "new_listing"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating the asset after a remediated approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
