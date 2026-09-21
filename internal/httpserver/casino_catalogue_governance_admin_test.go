//go:build integration

// Stage 9.2, Workstream A: closes ARCH-DB-2 Phase 2 (docs/decisions/0081
// §5.2) - HTTP-level coverage for the two new endpoints
// (POST /v1/admin/casino/games/{gameID}/change-requests,
// POST /v1/admin/casino/change-requests/{requestID}/approvals) and for
// PUT /v1/admin/casino/games's new dual-control refusal path. Mirrors
// asset_registry_admin_test.go's own structure/rationale exactly (the
// closest existing precedent), and reuses this file's mustCreateTenant/
// mustCreateBrand/mustCreateStaff/mustLoginStaff/mustRegisterPlayer/
// postJSON/putJSON/getJSON/decodeBody helpers (identity_flow_integration_
// test.go, financial_flow_integration_test.go, casino_flow_integration_
// test.go).
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// mustCreateGameForGovernance registers a fresh platform-catalogue title
// with the given jurisdiction_blocklist/status directly through
// db.Pool.WithPlatformAdmin (never through the HTTP PUT endpoint, so these
// tests do not depend on that endpoint already working).
func mustCreateGameForGovernance(t *testing.T, pool *db.Pool, principalID uuid.UUID, blocklist []string, status string) uuid.UUID {
	t.Helper()
	if blocklist == nil {
		blocklist = []string{}
	}
	var id uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type, jurisdiction_blocklist, status)
			 VALUES ($1, $1, 'Governance HTTP Test Game', 'slot', $2, $3) RETURNING id`,
			"gov-http-"+uuid.New().String()[:8], blocklist, status).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed governance game: %v", err)
	}
	return id
}

// --- 1. Unauthorized scope attempts -----------------------------------

// TestCasinoCatalogueGovernanceAPI_UnauthenticatedDenied proves both new
// endpoints reject a request with no bearer token at all.
func TestCasinoCatalogueGovernanceAPI_UnauthenticatedDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	resp := postJSON(t, srv, "/v1/admin/casino/games/"+uuid.NewString()+"/change-requests", "", map[string]any{
		"operation": "status_activate", "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with no bearer token filing a request, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+uuid.NewString()+"/approvals", "", map[string]any{
		"decision": "approve", "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with no bearer token deciding a request, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCasinoCatalogueGovernanceAPI_PlayerTokenDenied proves a genuine,
// real-HTTP-registered player token (never a staff principal) is denied
// both endpoints.
func TestCasinoCatalogueGovernanceAPI_PlayerTokenDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/admin/casino/games/"+uuid.NewString()+"/change-requests", player.Tokens.AccessToken, map[string]any{
		"operation": "status_activate", "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token filing a request, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+uuid.NewString()+"/approvals", player.Tokens.AccessToken, map[string]any{
		"decision": "approve", "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token deciding a request, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCasinoCatalogueGovernanceAPI_StaffWithoutPermissionDenied proves a
// real staff principal that does NOT hold casino_catalogue:govern -
// including one holding the narrower casino_catalogue:manage (a tenant
// operator role has neither, but this also checks a role that plausibly
// could be mistaken for eligible) - is denied both endpoints.
func TestCasinoCatalogueGovernanceAPI_StaffWithoutPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-gov-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-gov-pw-1").AccessToken

	resp := postJSON(t, srv, "/v1/admin/casino/games/"+uuid.NewString()+"/change-requests", token, map[string]any{
		"operation": "status_activate", "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin filing a request, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+uuid.NewString()+"/approvals", token, map[string]any{
		"decision": "approve", "reason_code": "attempt",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin deciding a request, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 2. Full end-to-end flow, including the dual-control refusal --------

// TestCasinoCatalogueGovernanceAPI_FullFlowOverHTTP exercises: filing with
// no game -> 404; an unapproved PUT removal -> 409; self-approval -> 409;
// a genuinely distinct approver's approval -> 201; the approved PUT -> 200
// (and the request is consumed, a second identical PUT is a no-op 200 that
// does not require anything further since there is nothing left to
// remove).
func TestCasinoCatalogueGovernanceAPI_FullFlowOverHTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	adminA := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-gov-a-1")
	adminB := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-gov-b-1")
	tokenA := mustLoginStaff(t, srv, "", adminA.Email, "pa-gov-a-1").AccessToken
	tokenB := mustLoginStaff(t, srv, "", adminB.Email, "pa-gov-b-1").AccessToken

	gameID := mustCreateGameForGovernance(t, pool, adminA.ID, []string{"DE"}, "active")

	// Unknown game: 404.
	resp := postJSON(t, srv, "/v1/admin/casino/games/"+uuid.NewString()+"/change-requests", tokenA, map[string]any{
		"operation": "jurisdiction_unblock", "removed_codes": []string{"DE"}, "reason_code": "cleared",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 filing a request against an unknown game, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Removing DE with no approved request at all: 409, not 500.
	resp = putJSON(t, srv, "/v1/admin/casino/games", tokenA, map[string]any{
		"provider_id": mustGetGameProviderID(t, pool, gameID), "provider_game_id": mustGetGameProviderGameID(t, pool, gameID),
		"name": "Governance HTTP Test Game", "game_type": "slot",
		"jurisdiction_blocklist": []string{}, "status": "active",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 removing a blocklist code with no approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// File the request.
	resp = postJSON(t, srv, "/v1/admin/casino/games/"+gameID.String()+"/change-requests", tokenA, map[string]any{
		"operation": "jurisdiction_unblock", "removed_codes": []string{"DE"}, "reason_code": "licence_cleared",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 filing a jurisdiction_unblock request, got %d", resp.StatusCode)
	}
	var filed map[string]any
	decodeBody(t, resp, &filed)
	requestID := filed["id"].(string)

	// Self-approval: 409.
	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+requestID+"/approvals", tokenA, map[string]any{
		"decision": "approve", "reason_code": "self",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for self-approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A genuinely different platform principal approves.
	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+requestID+"/approvals", tokenB, map[string]any{
		"decision": "approve", "reason_code": "reviewed",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for a distinct approver's approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Duplicate decision by the SAME approver: 409.
	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+requestID+"/approvals", tokenB, map[string]any{
		"decision": "approve", "reason_code": "again",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a duplicate decision by the same approver, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Now the removal succeeds.
	resp = putJSON(t, srv, "/v1/admin/casino/games", tokenA, map[string]any{
		"provider_id": mustGetGameProviderID(t, pool, gameID), "provider_game_id": mustGetGameProviderGameID(t, pool, gameID),
		"name": "Governance HTTP Test Game", "game_type": "slot",
		"jurisdiction_blocklist": []string{}, "status": "active",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 removing an approved blocklist code, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A decision on the now-applied request: 409.
	third := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-gov-c-1")
	thirdToken := mustLoginStaff(t, srv, "", third.Email, "pa-gov-c-1").AccessToken
	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+requestID+"/approvals", thirdToken, map[string]any{
		"decision": "approve", "reason_code": "late",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 deciding an already-applied request, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 3. status_activate over HTTP ------------------------------------

// TestCasinoCatalogueGovernanceAPI_StatusActivateOverHTTP proves
// reactivating a disabled game follows the identical dual-control shape.
func TestCasinoCatalogueGovernanceAPI_StatusActivateOverHTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	adminA := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-gov-sa-a-1")
	adminB := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-gov-sa-b-1")
	tokenA := mustLoginStaff(t, srv, "", adminA.Email, "pa-gov-sa-a-1").AccessToken
	tokenB := mustLoginStaff(t, srv, "", adminB.Email, "pa-gov-sa-b-1").AccessToken

	gameID := mustCreateGameForGovernance(t, pool, adminA.ID, nil, "disabled")

	resp := putJSON(t, srv, "/v1/admin/casino/games", tokenA, map[string]any{
		"provider_id": mustGetGameProviderID(t, pool, gameID), "provider_game_id": mustGetGameProviderGameID(t, pool, gameID),
		"name": "Governance HTTP Test Game", "game_type": "slot", "status": "active",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 reactivating a disabled game with no approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/admin/casino/games/"+gameID.String()+"/change-requests", tokenA, map[string]any{
		"operation": "status_activate", "reason_code": "licence_restored",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 filing a status_activate request, got %d", resp.StatusCode)
	}
	var filed map[string]any
	decodeBody(t, resp, &filed)
	requestID := filed["id"].(string)

	resp = postJSON(t, srv, "/v1/admin/casino/change-requests/"+requestID+"/approvals", tokenB, map[string]any{
		"decision": "approve", "reason_code": "reviewed",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 approving the status_activate request, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = putJSON(t, srv, "/v1/admin/casino/games", tokenA, map[string]any{
		"provider_id": mustGetGameProviderID(t, pool, gameID), "provider_game_id": mustGetGameProviderGameID(t, pool, gameID),
		"name": "Governance HTTP Test Game", "game_type": "slot", "status": "active",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reactivating with an approval, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// mustGetGameProviderID/mustGetGameProviderGameID read back the (provider_id,
// provider_game_id) pair mustCreateGameForGovernance minted, since the PUT
// endpoint is keyed on that pair, not the platform id. casino_games' own
// read policy is USING (true) (migration 0084) - open to every scope - so
// a plain WithoutTenant read suffices here.
func mustGetGameProviderID(t *testing.T, pool *db.Pool, gameID uuid.UUID) string {
	t.Helper()
	return mustGetGameField(t, pool, gameID, "provider_id")
}

func mustGetGameProviderGameID(t *testing.T, pool *db.Pool, gameID uuid.UUID) string {
	t.Helper()
	return mustGetGameField(t, pool, gameID, "provider_game_id")
}

func mustGetGameField(t *testing.T, pool *db.Pool, gameID uuid.UUID, column string) string {
	t.Helper()
	var value string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT `+column+` FROM casino_games WHERE id = $1`, gameID).Scan(&value)
	})
	if err != nil {
		t.Fatalf("read casino_games.%s: %v", column, err)
	}
	return value
}
