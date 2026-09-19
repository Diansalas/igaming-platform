//go:build integration

// Stage 4I item JV-2 / canonical-model §6.2 scenario 4 ("forged
// jurisdiction payload"), qa test-floor pass.
//
// canonical-model §8.2 identifies FOUR structs across FIVE routed
// handler surfaces from which `jurisdiction_code` was deleted (JV-2):
// issueManualGrantRequest (routed at BOTH POST /v1/admin/bonus/grants
// and POST /v1/admin/bonus/manual-grants - "two endpoints, one struct"),
// resolveHeldDispositionRequest, activateManualGrantRequest, and
// executeBulkGrantJobRequest. The commit that removed the field
// (1b07747) argued the decisive property is free: httpserver's decodeJSON
// already calls dec.DisallowUnknownFields() (json.go), so deleting the
// field turns a submitted "jurisdiction_code" into a 400, not a silent
// ignore. That claim had NO regression test on any of the five routed
// surfaces prior to this file - only internal/jurisdiction's own admin
// registry endpoints (jurisdiction_admin_integration_test.go) exercised
// this pattern. canonical-model §8.2's own instruction is explicit:
// "qa still writes FIVE A-1 cases, one per routed surface, because
// routing is what a regression breaks" - not four, and not one shared
// case, since re-adding the field to the struct but forgetting to wire a
// second route (or vice versa) is exactly the class of regression a
// struct-level-only test cannot catch.
//
// Every case below relies on decodeJSON failing BEFORE any entity lookup,
// permission-scoped business logic, or database write runs (confirmed by
// reading each handler: tenant/staff context extraction, then
// decodeJSON, then everything else) - so a random, never-persisted UUID
// in a path parameter is sufficient; the request must never reach far
// enough to notice the referenced grant/disposition/job does not exist.
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestJV2ForgedJurisdictionPayload_AllFiveRoutedSurfacesReject400 is the
// five-case A-1 suite canonical-model §8.2 requires: a request body still
// carrying "jurisdiction_code" is rejected with 400 on EVERY routed
// surface JV-2 touched, never silently ignored and never reaching the
// deleted field's old call site.
func TestJV2ForgedJurisdictionPayload_AllFiveRoutedSurfacesReject400(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "bonus-ops-jv2-pw2")
	token := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "bonus-ops-jv2-pw2").AccessToken

	forgedJurisdiction := map[string]any{
		"parent_operation_id": uuid.NewString(),
		"amount":              "1000",
		"jurisdiction_code":   "MT", // FORGED - deleted from every struct below by JV-2
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{
			// issueManualGrantRequest, surface 1 of 2 ("two endpoints, one
			// struct" - canonical-model §8.2 table row 1).
			name:   "POST /v1/admin/bonus/grants (issueManualGrantRequest)",
			method: http.MethodPost,
			path:   "/v1/admin/bonus/grants",
			body: map[string]any{
				"player_account_id": uuid.NewString(), "campaign_id": uuid.NewString(),
				"offer_version_id": uuid.NewString(), "asset_code": "USD", "amount": "1000",
				"funding_source": "manual", "parent_operation_id": uuid.NewString(),
				"reason_code": "test", "jurisdiction_code": "MT",
			},
		},
		{
			// issueManualGrantRequest, surface 2 of 2 - the SAME struct,
			// a DIFFERENT route (bonus_domain_ops_handlers.go's four-eyes
			// phase-1 issuance).
			name:   "POST /v1/admin/bonus/manual-grants (issueManualGrantRequest, four-eyes phase 1)",
			method: http.MethodPost,
			path:   "/v1/admin/bonus/manual-grants",
			body: map[string]any{
				"player_account_id": uuid.NewString(), "campaign_id": uuid.NewString(),
				"offer_version_id": uuid.NewString(), "asset_code": "USD", "amount": "1000",
				"funding_source": "manual", "parent_operation_id": uuid.NewString(),
				"jurisdiction_code": "MT",
			},
		},
		{
			name:   "POST /v1/admin/bonus/manual-grants/{grantID}/activate (activateManualGrantRequest)",
			method: http.MethodPost,
			path:   "/v1/admin/bonus/manual-grants/" + uuid.NewString() + "/activate",
			body:   forgedJurisdiction,
		},
		{
			name:   "POST /v1/admin/bonus/bulk-jobs/{jobID}/execute (executeBulkGrantJobRequest)",
			method: http.MethodPost,
			path:   "/v1/admin/bonus/bulk-jobs/" + uuid.NewString() + "/execute",
			body: map[string]any{
				"amount": "1000", "jurisdiction_code": "MT",
			},
		},
		{
			name:   "POST /v1/admin/bonus/held-dispositions/{dispositionID}/resolve (resolveHeldDispositionRequest)",
			method: http.MethodPost,
			path:   "/v1/admin/bonus/held-dispositions/" + uuid.NewString() + "/resolve",
			body: map[string]any{
				"action": "route_to_cash", "reason_code": "test", "request_id": uuid.NewString(),
				"jurisdiction_code": "MT",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := sendAssetJSON(t, srv.URL+tc.path, tc.method, token, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("expected 400 for a forged jurisdiction_code on %s, got %d", tc.name, resp.StatusCode)
			}
		})
	}
}
