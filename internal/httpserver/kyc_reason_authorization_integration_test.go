//go:build integration

// Stage 10.3, KYC-REASON-BOUND-1 (QA binding test plan, W1d condition 2:
// "Authorization/tenant isolation (new)"). Runs against TEST_DATABASE_URL
// (the "igaming" role - NOBYPASSRLS, per deploy/init-app-role.sql; RLS on
// kyc_verifications is FORCE'd, so this exercises the real policy, not a
// privileged bypass), exactly like every other test in this package.
package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// rawResponseBody reads and closes resp.Body, returning the raw bytes -
// used where a test must inspect the ACTUAL wire JSON (field presence,
// substring leakage), not just what a typed struct happens to decode into.
func rawResponseBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return b
}

// TestKYCHandlers_PlayerResponse_NeverContainsRawReason is the QA binding
// plan's condition-2 case: for every adapter outcome fixture (approved,
// rejected, review_required, expired, pending), the player-facing JSON
// body from BOTH POST and GET /v1/me/kyc/verifications must not contain
// the raw reason string under any field name (checked structurally at
// the raw JSON level, not merely against the typed struct - a future
// re-add of the field under a different name must also be caught).
func TestKYCHandlers_PlayerResponse_NeverContainsRawReason(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mockProvider, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	outcomes := []kyc.ProviderOutcome{
		kyc.ProviderApproved, kyc.ProviderRejected, kyc.ProviderReviewRequired, kyc.ProviderExpired,
	}
	for _, outcome := range outcomes {
		outcome := outcome
		t.Run(string(outcome), func(t *testing.T) {
			player := mustRegisterPlayer(t, srv, brand.Slug)

			createResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
			createBody := rawResponseBody(t, createResp)
			assertNoReasonField(t, createBody, "distinctive-marker-"+string(outcome))

			var created playerVerificationResponse
			if err := json.Unmarshal(createBody, &created); err != nil {
				t.Fatalf("decode create response: %v", err)
			}
			providerReference := mustGetKYCProviderReference(t, pool, tenant.ID, created.ID)

			marker := "distinctive-marker-" + string(outcome)
			callback := mockProvider.CallbackPayload(tenant.ID, providerReference, outcome, marker)
			callbackResp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", callback)
			if callbackResp.StatusCode != http.StatusNoContent {
				t.Fatalf("%s: expected 204 from a valid callback, got %d", outcome, callbackResp.StatusCode)
			}
			_ = callbackResp.Body.Close()

			listResp := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
			listBody := rawResponseBody(t, listResp)
			assertNoReasonField(t, listBody, marker)
		})
	}
}

func assertNoReasonField(t *testing.T, body []byte, providerMarker string) {
	t.Helper()
	s := string(body)
	if strings.Contains(s, "\"reason\"") {
		t.Fatalf("HD-10.3-3 VIOLATION: player-facing response declares a \"reason\" field: %s", s)
	}
	if strings.Contains(s, "\"reason_code\"") {
		t.Fatalf("HD-10.3-3 VIOLATION: player-facing response declares a \"reason_code\" field: %s", s)
	}
	if strings.Contains(s, providerMarker) {
		t.Fatalf("HD-10.3-3 VIOLATION: the provider's reason marker %q leaked into the player-facing response body under some field: %s", providerMarker, s)
	}
}

// TestKYCVerifications_TenantIsolation_Reason is the QA binding plan's
// second condition-2 case: tenant B's staff cannot read tenant A's
// kyc_verifications.reason through any staff route, run as the same
// NOBYPASSRLS role every other test in this package uses. This is
// distinct from the existing general TestKYC_CrossTenantAccessDenied
// (K-series) coverage - it specifically asserts the `reason` value
// itself never crosses the tenant boundary via either staff route, not
// merely that the row count is zero.
func TestKYCVerifications_TenantIsolation_Reason(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mockProvider, _ := newKYCTestServer(t, pool, issuer)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", playerA.Tokens.AccessToken, map[string]any{})
	if verResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating tenant A's verification, got %d: %s", verResp.StatusCode, rawResponseBody(t, verResp))
	}
	var created playerVerificationResponse
	decodeBody(t, verResp, &created)
	providerReference := mustGetKYCProviderReference(t, pool, tenantA.ID, created.ID)

	const secretReason = "tenant-a-only-reason-marker"
	callback := mockProvider.CallbackPayload(tenantA.ID, providerReference, kyc.ProviderRejected, secretReason)
	callbackResp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenantA.Slug+"/mock", callback)
	if callbackResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", callbackResp.StatusCode)
	}
	_ = callbackResp.Body.Close()

	tenantB := mustCreateTenant(t, pool)
	complianceB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleCompliance, "compliance-kyc-reason-iso-1")
	complianceBTokens := mustLoginStaff(t, srv, tenantB.Slug, complianceB.Email, "compliance-kyc-reason-iso-1")

	// (1) Tenant B's per-account list route: filtering by tenant A's own
	// player_account_id must show NOTHING (RLS-scoped to tenant B), and
	// certainly never the reason value.
	//
	// Gate 10.3-W1 code review #12: the status is asserted (200 - an
	// authorized, RLS-scoped list, never a 4xx/5xx that would trivially
	// "not leak"), and a body that is not a JSON array FAILS the test
	// rather than silently skipping the zero-row check.
	listResp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+playerA.ID.String(), complianceBTokens.AccessToken)
	listBody := rawResponseBody(t, listResp)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from tenant B's per-account list route, got %d: %s", listResp.StatusCode, listBody)
	}
	if strings.Contains(string(listBody), secretReason) {
		t.Fatalf("CROSS-TENANT LEAK: tenant B's staff per-account list route exposed tenant A's reason: %s", listBody)
	}
	var listDecoded []map[string]any
	if err := json.Unmarshal(listBody, &listDecoded); err != nil {
		t.Fatalf("expected the per-account list body to be a JSON array, got %v: %s", err, listBody)
	}
	if len(listDecoded) != 0 {
		t.Fatalf("expected tenant B to see zero rows for tenant A's player_account_id, got %+v", listDecoded)
	}

	// (2) Tenant B's tenant-wide case queue: must never surface tenant
	// A's row or its reason at all.
	queueResp := getJSON(t, srv, "/v1/admin/kyc/cases", complianceBTokens.AccessToken)
	queueBody := rawResponseBody(t, queueResp)
	if queueResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from tenant B's case queue, got %d: %s", queueResp.StatusCode, queueBody)
	}
	if strings.Contains(string(queueBody), secretReason) {
		t.Fatalf("CROSS-TENANT LEAK: tenant B's staff case queue exposed tenant A's reason: %s", queueBody)
	}
	var queueDecoded struct {
		Items *[]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(queueBody, &queueDecoded); err != nil || queueDecoded.Items == nil {
		t.Fatalf("expected the case queue body to be a paged object with an items JSON array, got err=%v: %s", err, queueBody)
	}
	if len(*queueDecoded.Items) != 0 {
		t.Fatalf("expected tenant B's case queue to be empty (tenant B has no verifications), got %+v", *queueDecoded.Items)
	}

	// (3) A cross-tenant review attempt (tenant B staff guessing/
	// enumerating tenant A's verification id via the review route) must
	// 404 - a genuinely different route from the two list endpoints above
	// - and its response must never carry tenant A's reason either.
	reviewResp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+created.ID+"/review", complianceBTokens.AccessToken, map[string]any{"status": "approved"})
	reviewBody := rawResponseBody(t, reviewResp)
	if reviewResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant B reviewing tenant A's verification, got %d: %s", reviewResp.StatusCode, reviewBody)
	}
	if strings.Contains(string(reviewBody), secretReason) {
		t.Fatalf("CROSS-TENANT LEAK: tenant B's cross-tenant review attempt response exposed tenant A's reason: %s", reviewBody)
	}

	// Sanity: tenant A's OWN compliance staff can read it - proving the
	// isolation above is tenant-scoped, not simply "the field never
	// leaves the server".
	complianceA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleCompliance, "compliance-kyc-reason-iso-2")
	complianceATokens := mustLoginStaff(t, srv, tenantA.Slug, complianceA.Email, "compliance-kyc-reason-iso-2")
	ownResp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+playerA.ID.String(), complianceATokens.AccessToken)
	ownBody := rawResponseBody(t, ownResp)
	if ownResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from tenant A's own per-account list route, got %d: %s", ownResp.StatusCode, ownBody)
	}
	var ownDecoded []map[string]any
	if err := json.Unmarshal(ownBody, &ownDecoded); err != nil || len(ownDecoded) != 1 {
		t.Fatalf("expected tenant A's own list to be a JSON array with exactly its one verification, got err=%v: %s", err, ownBody)
	}
	if !strings.Contains(string(ownBody), secretReason) {
		t.Fatalf("test precondition failed: expected tenant A's OWN staff to see the bounded reason, got %s", ownBody)
	}
}
