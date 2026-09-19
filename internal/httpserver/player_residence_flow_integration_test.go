//go:build integration

// Stage 4I Phase B: HTTP-level tests for GET/PUT /v1/me/residence.
// Follows identity_flow_integration_test.go/casino_flow_integration_test.go's
// own conventions (fixtures via internal packages, bearer tokens minted
// through the real HTTP register/login flow).
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

func TestPlayerResidence_GetPutHappyPath(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-residence-pw-1")
	complianceToken := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-residence-pw-1").AccessToken

	// Before enabling collection: PUT is refused (403).
	resp := putJSON(t, srv, "/v1/me/residence", player.Tokens.AccessToken, map[string]any{"country_code": "US"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 before evidence collection is enabled, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Not-set GET before anything is written.
	resp = getJSON(t, srv, "/v1/me/residence", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 GET before any residence is set, got %d", resp.StatusCode)
	}
	var notSet map[string]any
	decodeBody(t, resp, &notSet)
	if notSet["is_set"] != false {
		t.Fatalf("expected is_set=false before any write, got %+v", notSet)
	}

	// Enable collection.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, complianceToken,
		map[string]any{"active": true, "reason_code": "residence-http-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling declared_residence collection, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Now the PUT succeeds.
	resp = putJSON(t, srv, "/v1/me/residence", player.Tokens.AccessToken, map[string]any{"country_code": "US"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 setting declared residence, got %d", resp.StatusCode)
	}
	var setResp map[string]any
	decodeBody(t, resp, &setResp)
	if setResp["is_set"] != true || setResp["country_code"] != "US" {
		t.Fatalf("unexpected set response: %+v", setResp)
	}
	if setResp["captured_at"] == nil || setResp["captured_at"] == "" {
		t.Fatalf("expected a non-empty captured_at, got %+v", setResp)
	}

	// GET reflects it.
	resp = getJSON(t, srv, "/v1/me/residence", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 GET after set, got %d", resp.StatusCode)
	}
	var getResp map[string]any
	decodeBody(t, resp, &getResp)
	if getResp["is_set"] != true || getResp["country_code"] != "US" {
		t.Fatalf("unexpected get response: %+v", getResp)
	}
}

func TestPlayerResidence_InvalidCountryCodeIs400(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-residence-pw-2")
	complianceToken := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-residence-pw-2").AccessToken
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/declared_residence", http.MethodPut, complianceToken,
		map[string]any{"active": true, "reason_code": "residence-http-test"})
	resp.Body.Close()

	resp = putJSON(t, srv, "/v1/me/residence", player.Tokens.AccessToken, map[string]any{"country_code": "ZZ"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid country code, got %d", resp.StatusCode)
	}
}

func TestPlayerResidence_NoTokenIs401(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	resp := getJSON(t, srv, "/v1/me/residence", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for GET with no token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = putJSON(t, srv, "/v1/me/residence", "", map[string]any{"country_code": "US"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for PUT with no token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Player-self-service-only: a staff token (even compliance) must be
// denied both routes.
func TestPlayerResidence_StaffTokenIs403(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-residence-pw-3")
	complianceToken := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-residence-pw-3").AccessToken

	resp := getJSON(t, srv, "/v1/me/residence", complianceToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a staff token on GET /v1/me/residence, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = putJSON(t, srv, "/v1/me/residence", complianceToken, map[string]any{"country_code": "US"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a staff token on PUT /v1/me/residence, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Sanity: IsEvidenceCollectionActive really is checked inside the same
// transaction as the write - a direct internal-package check must agree
// with the HTTP-level 403 above.
func TestPlayerResidence_ActivationGateMatchesInternalCheck(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	var active bool
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		active, err = jurisdiction.IsEvidenceCollectionActive(ctx, tx, tenant.ID, jurisdiction.EvidenceDeclaredResidence)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active {
		t.Fatal("expected declared_residence collection to be OFF by default")
	}

	resp := putJSON(t, srv, "/v1/me/residence", player.Tokens.AccessToken, map[string]any{"country_code": "US"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 while the internal check also reports inactive, got %d", resp.StatusCode)
	}
}
