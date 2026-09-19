//go:build integration

// Stage 4A HTTP-layer authorization/tenant-isolation/webhook-authentication
// tests - the gap API/HTTP and security specialist review both flagged as
// a completion-blocking condition: casino_handlers.go/casino_admin_
// handlers.go/casino_routes.go had zero HTTP-level tests before this file,
// unlike every sibling financial route (financial_flow_integration_test.go).
// Follows that file's own conventions: fixtures created directly via the
// internal packages, a bearer token minted through the real staff-login/
// player-register HTTP flow.
package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
)

func newCasinoTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *casino.Orchestrator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:             slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                 pool,
		AuthIssuer:         issuer,
		ServiceName:        "platform-api-test",
		AccessTokenTTL:     5 * time.Minute,
		RefreshTokenTTL:    time.Hour,
		CasinoOrchestrator: orchestrator,
		PersonResolver:     identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newMockCasinoOrchestrator() (*casino.Orchestrator, *casino.MockCasinoProvider) {
	mock := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	return casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}), mock
}

func putJSON(t *testing.T, srv *httptest.Server, path, bearerToken string, body any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, srv.URL+path, jsonBody(t, body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func mustSeedCasinoGame(t *testing.T, pool *db.Pool, providerID string, assetCodes ...string) casino.Game {
	t.Helper()
	var g casino.Game
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = casino.UpsertGame(ctx, tx, casino.UpsertGameInput{
			ProviderID: providerID, ProviderGameID: "game-" + uuid.New().String()[:8],
			Name: "Test Game", GameType: "slot", SupportedAssets: assetCodes, Status: casino.GameStatusActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed casino game: %v", err)
	}
	return g
}

func mustEnableCasinoGameForTenant(t *testing.T, pool *db.Pool, tenantID, gameID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.SetGameAvailability(ctx, tx, tenantID, nil, gameID, true)
		return err
	})
	if err != nil {
		t.Fatalf("enable casino game for tenant: %v", err)
	}
}

func validCasinoCapabilityBody() map[string]any {
	return map[string]any{
		"supports_catalogue":   true,
		"supports_launch":      true,
		"supports_balance":     true,
		"supports_bet":         true,
		"supports_win":         true,
		"supports_rollback":    true,
		"supported_assets":     []string{"EUR", "USD"},
		"supported_game_types": []string{"slot", "table", "live"},
		"priority":             0,
		"status":               "active",
	}
}

// --- 1. A player token is denied on every casino admin route ---

func TestCasinoAdminRoutes_PlayerTokenDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	token := player.Tokens.AccessToken
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")

	assertDenied := func(t *testing.T, resp *http.Response, label string) {
		t.Helper()
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: expected 403 or 401, got %d", label, resp.StatusCode)
		}
	}

	assertDenied(t, putJSON(t, srv, "/v1/admin/casino/games", token, map[string]any{
		"provider_id": "mock-casino", "provider_game_id": "x", "name": "X", "game_type": "slot",
	}), "upsert platform catalogue game")
	assertDenied(t, putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", token, validCasinoCapabilityBody()), "write provider capability")
	assertDenied(t, putJSON(t, srv, "/v1/admin/casino/games/"+game.ID.String()+"/availability", token, map[string]any{"enabled": true}), "set game availability")
}

// --- 2. Platform-only catalogue management: tenant_admin denied,
// platform_admin allowed ---

func TestCasinoCatalogueManage_OnlyPlatformAdminAllowed(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-casino-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-casino-pw-1")

	body := map[string]any{
		"provider_id": "mock-casino", "provider_game_id": "game-" + uuid.NewString()[:8],
		"name": "Catalogue Test Game", "game_type": "slot",
	}
	resp := putJSON(t, srv, "/v1/admin/casino/games", tenantAdminTokens.AccessToken, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin managing the platform catalogue, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-casino-pw-1")
	adminTokens := mustLoginStaff(t, srv, "", admin.Email, "admin-casino-pw-1")
	resp = putJSON(t, srv, "/v1/admin/casino/games", adminTokens.AccessToken, body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for platform_admin managing the platform catalogue, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 3. Tenant-scoped casino config: platform_admin's nil-tenant token is
// denied by RequireTenantScope; tenant_admin succeeds ---

func TestCasinoTenantConfigRoutes_PlatformAdminDeniedTenantAdminAllowed(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-casino-cfg-pw-1")
	adminTokens := mustLoginStaff(t, srv, "", admin.Email, "admin-casino-cfg-pw-1")

	resp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", adminTokens.AccessToken, validCasinoCapabilityBody())
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for platform_admin's nil-tenant token writing capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = putJSON(t, srv, "/v1/admin/casino/games/"+game.ID.String()+"/availability", adminTokens.AccessToken, map[string]any{"enabled": true})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for platform_admin's nil-tenant token setting availability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-casino-cfg-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-casino-cfg-pw-1")

	resp = putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tenantAdminTokens.AccessToken, validCasinoCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for tenant_admin writing its own tenant's capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = putJSON(t, srv, "/v1/admin/casino/games/"+game.ID.String()+"/availability", tenantAdminTokens.AccessToken, map[string]any{"enabled": true})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for tenant_admin setting its own tenant's game availability, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 4. A tenant's player never sees a game only a DIFFERENT tenant
// opted into ---

func TestCasinoGamesList_CrossTenantAvailabilityInvisible(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenantB.ID, game.ID) // only tenant B opts in

	playerA := mustRegisterPlayer(t, srv, brandA.Slug)

	resp := getJSON(t, srv, "/v1/me/casino/games", playerA.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing tenant A's own (empty) catalogue, got %d", resp.StatusCode)
	}
	var games []map[string]any
	decodeBody(t, resp, &games)
	for _, g := range games {
		if g["id"] == game.ID.String() {
			t.Fatalf("tenant A's player must never see a game only tenant B opted into, got %+v", games)
		}
	}

	resp = postJSON(t, srv, "/v1/me/casino/games/"+game.ID.String()+"/launch", playerA.Tokens.AccessToken, map[string]string{
		"asset_code": "EUR", "mode": "real",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 launching a game only a different tenant opted into, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 5. Unknown tenant slug and a suspended tenant get the identical
// not-found response from the casino webhook (enumeration resistance) ---

func TestCasinoWebhook_UnknownAndSuspendedTenantIdenticalNotFound(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	unknownResp := rawPostJSON(t, srv, "/v1/webhooks/casino/does-not-exist-"+uuid.NewString()+"/mock-casino", []byte(`{}`))
	defer unknownResp.Body.Close()
	if unknownResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown tenant slug, got %d", unknownResp.StatusCode)
	}

	tenant := mustCreateTenant(t, pool)
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, tenant.ID)
		return err
	})
	if err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}
	suspendedResp := rawPostJSON(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", []byte(`{}`))
	defer suspendedResp.Body.Close()
	if suspendedResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a suspended tenant, got %d", suspendedResp.StatusCode)
	}

	unknownBody := decodeAPIError(t, unknownResp)
	suspendedBody := decodeAPIError(t, suspendedResp)
	if unknownBody.Code != suspendedBody.Code || unknownBody.Message != suspendedBody.Message {
		t.Errorf("expected byte-identical responses for unknown vs. suspended tenant, got %+v vs %+v", unknownBody, suspendedBody)
	}
}

// --- 6. An unsigned casino webhook payload is rejected, with zero
// ledger effect ---

func TestCasinoWebhook_UnsignedPayloadRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)

	unsigned := []byte(`{"event_type":"bet","provider_tx_id":"http-unsigned-1","amount":1000,"asset_code":"EUR","outcome":"succeeded","player_account_id":"` + uuid.NewString() + `"}`)
	resp := rawPostJSON(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", unsigned)
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("expected a 4xx for an unsigned callback, got %d", resp.StatusCode)
	}

	var count int
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'http-unsigned-1'`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero ledger_transactions rows for an unsigned callback, got %d", count)
	}
}

// --- 7. SEC-4I-F3: casino_game.upserted audit gap fix ---

// mustGetLatestAuditMetadata reads the most recent audit_log row for
// (action, targetID) and decodes its metadata JSONB - platform-level rows
// only (tenant_id IS NULL), matching newUpsertCasinoGameHandler's own
// db.Pool.WithoutTenant scope.
func mustGetLatestAuditMetadata(t *testing.T, pool *db.Pool, action, targetID string) map[string]any {
	t.Helper()
	var raw []byte
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata FROM audit_log WHERE tenant_id IS NULL AND action = $1 AND target_id = $2
			 ORDER BY created_at DESC LIMIT 1`,
			action, targetID,
		).Scan(&raw)
	})
	if err != nil {
		t.Fatalf("query audit_log metadata for %s/%s: %v", action, targetID, err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("unmarshal audit metadata: %v", err)
	}
	return metadata
}

// TestUpsertCasinoGame_AuditRecordsBeforeAfterJurisdictionBlocklist proves
// SEC-4I-F3's fix (docs/governance/stage-4i-canonical-model.md §9.5,
// confirmed a HARD PREREQUISITE of K-3's own remediation): the
// casino_game.upserted audit entry now captures before/after state for
// jurisdiction_blocklist (and the other enforcement-relevant fields),
// where the pre-fix entry recorded only provider_id/provider_game_id/
// status and omitted jurisdiction_blocklist entirely even though the same
// call writes it. A brand-new game records before=nil (no prior row to
// diff against); an update to an EXISTING game's blocklist - the exact
// act that arms K-3's fail-closed control platform-wide - must leave a
// reconstructable before/after diff.
func TestUpsertCasinoGame_AuditRecordsBeforeAfterJurisdictionBlocklist(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-casino-audit-pw-1")
	adminTokens := mustLoginStaff(t, srv, "", admin.Email, "admin-casino-audit-pw-1")

	providerGameID := "game-" + uuid.NewString()[:8]
	createBody := map[string]any{
		"provider_id": "mock-casino", "provider_game_id": providerGameID,
		"name": "Audit Test Game", "game_type": "slot",
	}
	resp := putJSON(t, srv, "/v1/admin/casino/games", adminTokens.AccessToken, createBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create game: expected 200, got %d", resp.StatusCode)
	}
	var created casinoGameResponse
	decodeBody(t, resp, &created)

	metadata := mustGetLatestAuditMetadata(t, pool, "casino_game.upserted", created.ID)
	if before, ok := metadata["before"]; !ok || before != nil {
		t.Fatalf("expected an explicit before=nil for a brand-new game, got %+v (present=%v)", before, ok)
	}
	after, ok := metadata["after"].(map[string]any)
	if !ok {
		t.Fatalf("expected an 'after' object, got %+v", metadata["after"])
	}
	if bl, _ := after["jurisdiction_blocklist"].([]any); len(bl) != 0 {
		t.Fatalf("expected an empty after jurisdiction_blocklist, got %+v", after["jurisdiction_blocklist"])
	}

	// Update: add a jurisdiction to the blocklist - the exact act that
	// arms K-3's fail-closed control for this game platform-wide.
	updateBody := map[string]any{
		"provider_id": "mock-casino", "provider_game_id": providerGameID,
		"name": "Audit Test Game", "game_type": "slot",
		"jurisdiction_blocklist": []string{"KM-ANJ"},
	}
	resp2 := putJSON(t, srv, "/v1/admin/casino/games", adminTokens.AccessToken, updateBody)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("update game: expected 200, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	metadata2 := mustGetLatestAuditMetadata(t, pool, "casino_game.upserted", created.ID)
	before2, ok := metadata2["before"].(map[string]any)
	if !ok {
		t.Fatalf("expected a 'before' object on the update, got %+v", metadata2["before"])
	}
	if bl, _ := before2["jurisdiction_blocklist"].([]any); len(bl) != 0 {
		t.Fatalf("expected the pre-update jurisdiction_blocklist to be empty, got %+v", before2["jurisdiction_blocklist"])
	}
	after2, ok := metadata2["after"].(map[string]any)
	if !ok {
		t.Fatalf("expected an 'after' object on the update, got %+v", metadata2["after"])
	}
	afterBlocklist, _ := after2["jurisdiction_blocklist"].([]any)
	if len(afterBlocklist) != 1 || afterBlocklist[0] != "KM-ANJ" {
		t.Fatalf("expected the post-update jurisdiction_blocklist to be [KM-ANJ], got %+v", after2["jurisdiction_blocklist"])
	}
}
