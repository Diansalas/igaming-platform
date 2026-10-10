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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// rawPostCasinoCallback posts a webhookauth.Inbound's body to the public
// casino webhook route with its own headers (X-Casino-Signature/
// X-Casino-Key-Id) - CAS-WH-TENANT-1 moved the signature out of the JSON
// body and into these headers, mirroring rawPostCallback's identical
// payments-domain helper.
func rawPostCasinoCallback(t *testing.T, srv *httptest.Server, path string, inbound webhookauth.Inbound) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(inbound.Body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range inbound.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func newCasinoTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *casino.Orchestrator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:                      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                          pool,
		AuthIssuer:                  issuer,
		ServiceName:                 "platform-api-test",
		AccessTokenTTL:              5 * time.Minute,
		RefreshTokenTTL:             time.Hour,
		CasinoOrchestrator:          orchestrator,
		CasinoOutboundCredentials:   casino.NewMockOutboundResolver(),
		CasinoPlaySimulationEnabled: true,
		PersonResolver:              identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newMockCasinoOrchestrator() (*casino.Orchestrator, *casino.MockCasinoProvider) {
	mock := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	return casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock)), mock
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

// mustSeedPlatformAdminStaffPrincipal inserts a genuine platform-scoped
// (tenant_id IS NULL) staff_users row and returns its id. Migration 0085
// (SEC-S91-3) added a trigger requiring app.platform_admin_principal_id
// to resolve to a REAL staff_users row before casino_games can be
// written, so WithPlatformAdmin(ctx, uuid.New(), ...) alone no longer
// suffices for a legitimate casino_games write in this suite. Deliberately
// a lightweight raw-SQL insert rather than mustCreateStaff (which hashes
// a real password via Argon2id) - these principals are never used to log
// in, only to satisfy the trigger's staff_users lookup, so paying that
// cost on every casino_games fixture write would be pure overhead.
func mustSeedPlatformAdminStaffPrincipal(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "platform-admin-"+id.String()+"@test.example")
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin staff principal: %v", err)
	}
	return id
}

func mustSeedCasinoGame(t *testing.T, pool *db.Pool, providerID string, assetCodes ...string) casino.Game {
	t.Helper()
	var g casino.Game
	// Migration 0084 (ADR 0081): casino_games writes now require a
	// genuinely platform-admin-scoped transaction. Migration 0085
	// (SEC-S91-3) further requires that transaction's principal to
	// resolve to a real staff_users row.
	err := pool.WithPlatformAdmin(context.Background(), mustSeedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
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
// uniform 401 "callback rejected" response from the casino webhook
// (enumeration resistance) - Stage 10.2 (CAS-WH-TENANT-1, ADR 0091, design
// §C6): the former 404 is now folded into the shared webhookPreamble's
// uniform pre-verification rejection, exactly like the payments webhook. ---

func TestCasinoWebhook_UnknownAndSuspendedTenantIdenticalNotFound(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	unknownResp := rawPostJSON(t, srv, "/v1/webhooks/casino/does-not-exist-"+uuid.NewString()+"/mock-casino", []byte(`{}`))
	defer unknownResp.Body.Close()
	if unknownResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unknown tenant slug, got %d", unknownResp.StatusCode)
	}

	tenant := mustCreateTenant(t, pool)
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction - a
	// WithoutTenant UPDATE here would be a silent zero-row no-op, not an
	// error, so rows-affected is checked explicitly.
	// ADR 0112: the status moves through the governed fixture (real guards); it returns an
	// error unless exactly the tenant's own row changed.
	err := launchfix.TrySetTenantStatus(context.Background(), tenant.ID, "suspended")
	if err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}
	suspendedResp := rawPostJSON(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", []byte(`{}`))
	defer suspendedResp.Body.Close()
	if suspendedResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a suspended tenant, got %d", suspendedResp.StatusCode)
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
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unsigned callback, got %d", resp.StatusCode)
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

// --- 6b. Stage 8 review fix (P1): a provider_round_id already bound to a
// DIFFERENT player is a 409, not a 5xx, and the response never echoes the
// round id or either player's identity (enumeration-resistance) ---

func TestCasinoWebhook_ProviderRoundOwnershipConflict_Returns409WithoutLeakingIdentity(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	playerA := mustRegisterPlayer(t, srv, brand.Slug)
	playerB := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, playerA.ID)
	mustActivatePlayer(t, pool, tenant.ID, playerB.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, playerA.ID, "EUR", 10_000)
	fundWallet(t, pool, tenant.ID, brand.ID, playerB.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launchedA := mustLaunchCasinoGame(t, srv, playerA.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	launchedB := mustLaunchCasinoGame(t, srv, playerB.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionA := uuid.MustParse(launchedA.SessionID)
	sessionB := uuid.MustParse(launchedB.SessionID)

	const conflictRoundID = "round-http-ownership-conflict"

	payloadA := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "bet-http-conflict-a", "", conflictRoundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", playerA.ID, sessionA)
	respA := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payloadA)
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("expected player A's first bet on the round to succeed, got %d", respA.StatusCode)
	}

	payloadB := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "bet-http-conflict-b", "", conflictRoundID, game.ProviderGameID,
		750, "EUR", casino.OutcomeSucceeded, "", playerB.ID, sessionB)
	respB := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payloadB)
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusConflict {
		t.Fatalf("expected a 409 for player B's collision with an already-bound round id, got %d", respB.StatusCode)
	}
	errBody := decodeAPIError(t, respB)
	if strings.Contains(errBody.Message, conflictRoundID) {
		t.Fatalf("response message must never echo the round id (enumeration oracle), got %q", errBody.Message)
	}
	if strings.Contains(errBody.Message, playerA.ID.String()) || strings.Contains(errBody.Message, playerB.ID.String()) {
		t.Fatalf("response message must never echo either player's identity, got %q", errBody.Message)
	}

	var count int
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'bet-http-conflict-b'`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero ledger_transactions rows for player B's rejected bet, got %d", count)
	}
}

// --- 7. SEC-4I-F3: casino_game.upserted audit gap fix ---

// mustGetLatestAuditMetadata reads the most recent audit_log row for
// (action, targetID) and decodes its metadata JSONB - platform-level rows
// only (tenant_id IS NULL), matching newUpsertCasinoGameHandler's own
// db.Pool.WithPlatformAdmin scope (migration 0084 / ADR 0081), which -
// like WithoutTenant before it - never sets app.tenant_id.
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
