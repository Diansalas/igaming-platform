//go:build integration

// Stage 10 W1 (docs/decisions/0088 §9/§14) HTTP-layer tests for the
// non-production test-support sportsbook settlement-simulation route.
// Follows sportsbook_flow_integration_test.go's own conventions
// (testEnv/newTestServer-style server builders, mustSeedSportsbookSelection,
// placeBetRequestBody, mustActivatePlayer, mustCreateStaff/mustLoginStaff)
// exactly.
package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
)

// newSettlementTestServer mirrors newSportsbookTestServer, with the extra
// Stage 10 W1 flag under test control - unlike every other test-support
// flag, ADR 0088 §9.1 requires the ROUTE ITSELF be absent from the mux
// when this is false, not merely 503 from inside a handler, so tests need
// to build two genuinely different servers rather than toggle one Deps
// field per request.
func newSettlementTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, simulationEnabled bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:                                slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                                    pool,
		AuthIssuer:                            issuer,
		ServiceName:                           "platform-api-test",
		AccessTokenTTL:                        5 * time.Minute,
		RefreshTokenTTL:                       time.Hour,
		SportsbookEnabled:                     true,
		SportsbookSettlementSimulationEnabled: simulationEnabled,
		PersonResolver:                        identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mustPlaceBetHTTP funds the player's wallet, activates them, and places a
// real bet through the actual HTTP placement endpoint - the settlement
// route's own §2.2 derivation logic depends on the bet's REAL placement
// posting, so a direct-SQL bet fixture would not exercise it honestly.
func mustPlaceBetHTTP(t *testing.T, srv *httptest.Server, pool *db.Pool, tenant identity.Tenant, brand identity.Brand, accessToken string, playerID uuid.UUID, stake int64) betResponse {
	t.Helper()
	mustActivatePlayer(t, pool, tenant.ID, playerID)
	fundWallet(t, pool, tenant.ID, brand.ID, playerID, "EUR", stake*10)
	selectionID := mustSeedSportsbookSelection(t, pool, 200, 100) // 2x
	resp := postJSON(t, srv, "/v1/me/sportsbook/bets", accessToken,
		placeBetRequestBody(selectionID, stake, 200, 100, "settlement-test-"+uuid.NewString()))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 placing bet, got %d", resp.StatusCode)
	}
	var placed placeBetResponse
	decodeBody(t, resp, &placed)
	if !placed.Accepted || placed.Bet == nil {
		t.Fatalf("expected bet to be accepted, got %+v", placed)
	}
	return *placed.Bet
}

func simulatePath(betID string) string {
	return "/v1/admin/sportsbook/bets/" + betID + "/simulate-settlement-event"
}

// --- Registration gate ---

func TestSettlementSimulate_RouteAbsentWhenFlagOff(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, false)

	resp := postJSON(t, srv, simulatePath(uuid.NewString()), "", map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 when the flag is off, got %d", resp.StatusCode)
	}
}

// --- Authentication / authorization ---

func TestSettlementSimulate_401WithoutToken(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)

	resp := postJSON(t, srv, simulatePath(uuid.NewString()), "", map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_403ForPlayerToken(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, simulatePath(uuid.NewString()), player.Tokens.AccessToken,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a player token, got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_403ForServicePrincipal(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)

	token, err := issuer.Issue(uuid.NewString(), tenant.ID, auth.RoleRiskManager, auth.PrincipalService, time.Hour)
	if err != nil {
		t.Fatalf("failed to issue service token: %v", err)
	}
	resp := postJSON(t, srv, simulatePath(uuid.NewString()), token,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a service-principal token even holding the right role, got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_403ForPlatformAdmin(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-password-1")
	tokens := mustLoginStaff(t, srv, "", admin.Email, "admin-password-1")

	resp := postJSON(t, srv, simulatePath(uuid.NewString()), tokens.AccessToken,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for platform_admin (nil tenant, RequireTenantScope), got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_403ForStaffWithoutPermission(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "staff-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "staff-password-1")

	resp := postJSON(t, srv, simulatePath(uuid.NewString()), tokens.AccessToken,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for tenant_admin (lacks sportsbook_settlement:simulate), got %d", resp.StatusCode)
	}
}

// --- Cross-tenant / not found ---

func TestSettlementSimulate_CrossTenantBetID_404NoRowsWritten(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenantA, brandA, playerA.Tokens.AccessToken, playerA.ID, 1000)

	tenantB := mustCreateTenant(t, pool)
	riskManagerB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleRiskManager, "risk-password-1")
	tokensB := mustLoginStaff(t, srv, tenantB.Slug, riskManagerB.Email, "risk-password-1")

	resp := postJSON(t, srv, simulatePath(bet.ID), tokensB.AccessToken,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's bet id, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeSettlementNotFound {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementNotFound, body.Code)
	}

	// No settlement history row exists for tenant B's own bets (there are
	// none), and tenant A's bet is untouched.
	var count int
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bet_settlements WHERE bet_id = $1`, uuid.MustParse(bet.ID)).Scan(&count)
	})
	if err != nil {
		t.Fatalf("failed to count settlement rows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no settlement rows written for the cross-tenant request, got %d", count)
	}
}

// --- Validation ---

func mustRiskManagerToken(t *testing.T, pool *db.Pool, srv *httptest.Server, tenant identity.Tenant) string {
	t.Helper()
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "risk-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "risk-password-1")
	return tokens.AccessToken
}

func TestSettlementSimulate_ValidationFailed_UnknownField(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled", "unexpected_field": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_ValidationFailed_PayoutAmountOverflow(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	// A literal payload with a payout_amount that overflows int64 -
	// json.Marshal cannot produce this from a Go value, so the raw request
	// is built by hand.
	body := `{"event_type":"settle","generation":1,"outcome":"won","asset_code":"EUR","payout_amount":99999999999999999999999999999}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+simulatePath(bet.ID), strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an overflowing payout_amount, got %d", resp.StatusCode)
	}
	var body2 apierror.Error
	decodeBody(t, resp, &body2)
	if body2.Code != apierror.CodeSettlementValidationFailed {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementValidationFailed, body2.Code)
	}
}

func TestSettlementSimulate_ValidationFailed_NonIntegerPayout(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token,
		map[string]any{"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": 10.5})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-integer payout_amount, got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_ValidationFailed_NegativePayout(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token,
		map[string]any{"event_type": "settle", "generation": 1, "outcome": "lost", "asset_code": "EUR", "payout_amount": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a negative payout_amount, got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_ValidationFailed_FieldMatrix(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	// rollback carrying a forbidden void_reason.
	resp := postJSON(t, srv, simulatePath(bet.ID), token,
		map[string]any{"event_type": "rollback", "generation": 1, "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for rollback carrying a forbidden field, got %d", resp.StatusCode)
	}
}

// --- Happy paths and 409/422 mappings ---

func TestSettlementSimulate_SettleWon_Applied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 settling won, got %d", resp.StatusCode)
	}
	var result simulateSettlementEventResponse
	decodeBody(t, resp, &result)
	if result.Result != "applied" || result.BetStatus != "settled_won" {
		t.Fatalf("expected applied/settled_won, got %+v", result)
	}
	if len(result.LedgerTransactionIDs) == 0 || len(result.SettlementRecordIDs) == 0 {
		t.Fatalf("expected non-empty ledger/settlement ids, got %+v", result)
	}

	// Audit metadata carries the trusted-proxy client IP separately from
	// the raw remote_addr, never a copied X-Forwarded-For (ADR 0088 §10).
	var ipAddress, requestID string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT ip_address::text, request_id FROM audit_log WHERE action = 'sportsbook_bet.settled' AND target_id = $1 ORDER BY created_at DESC LIMIT 1`,
			bet.ID).Scan(&ipAddress, &requestID)
	})
	if err != nil {
		t.Fatalf("failed to read audit record: %v", err)
	}
	if ipAddress == "" {
		t.Error("expected a non-empty audit ip_address")
	}

	// Replay: identical payload is a no-op, replayed=true.
	resp = postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 replaying an identical settle, got %d", resp.StatusCode)
	}
	var replay simulateSettlementEventResponse
	decodeBody(t, resp, &replay)
	if replay.Result != "replayed" {
		t.Errorf("expected replayed result, got %q", replay.Result)
	}
}

func TestSettlementSimulate_SettlePayloadMismatch_409(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 settling won, got %d", resp.StatusCode)
	}

	// Same generation, different outcome: a redelivery with a DIFFERENT
	// payload must be rejected, never silently returned as the original.
	resp = postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "lost", "asset_code": "EUR", "payout_amount": 0,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a payload-mismatched replay, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeSettlementPayloadMismatch {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementPayloadMismatch, body.Code)
	}
}

func TestSettlementSimulate_PayoutInvalid_422(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	// Anti-minting: claiming a payout different from the frozen
	// potential_return must be rejected, never posted.
	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn + 1,
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an anti-minting payout claim, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeSettlementPayoutInvalid {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementPayoutInvalid, body.Code)
	}
}

func TestSettlementSimulate_AssetMismatch_422(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "lost", "asset_code": "USD", "payout_amount": 0,
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for a mismatched asset claim, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeSettlementAssetMismatch {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementAssetMismatch, body.Code)
	}
}

func TestSettlementSimulate_VoidBeforeSettlement_Applied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "void", "void_reason": "data_error",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 voiding an open bet, got %d", resp.StatusCode)
	}
	var result simulateSettlementEventResponse
	decodeBody(t, resp, &result)
	if result.Result != "applied" || result.BetStatus != "void" {
		t.Fatalf("expected applied/void, got %+v", result)
	}

	// Settling a voided bet is rejected.
	resp = postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 settling a voided bet, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeSettlementBetVoided {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementBetVoided, body.Code)
	}
}

func TestSettlementSimulate_RollbackThenVoid(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "lost", "asset_code": "EUR", "payout_amount": 0,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 settling lost, got %d", resp.StatusCode)
	}

	resp = postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{"event_type": "rollback", "generation": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 rolling back, got %d", resp.StatusCode)
	}
	var rb simulateSettlementEventResponse
	decodeBody(t, resp, &rb)
	if rb.Result != "applied" || rb.BetStatus != "open" {
		t.Fatalf("expected applied/open after rollback, got %+v", rb)
	}

	resp = postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{"event_type": "void", "void_reason": "push"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 voiding after rollback, got %d", resp.StatusCode)
	}
}

func TestSettlementSimulate_GenerationOutOfSequence_409(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 2, "outcome": "lost", "asset_code": "EUR", "payout_amount": 0,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for an out-of-sequence generation, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeSettlementGenerationOutOfSequence {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementGenerationOutOfSequence, body.Code)
	}
}

// --- Read surfaces (ADR 0088 §3.4) ---

func TestListMyBets_PlayerSurfaceOmitsStaffAndRequestFields(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 settling won, got %d", resp.StatusCode)
	}

	resp = getJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing own bets, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	raw := string(rawBytes)
	if strings.Contains(raw, "actor_staff_account_id") || strings.Contains(raw, "request_id") {
		t.Fatalf("player bet history must never expose actor_staff_account_id/request_id, got: %s", raw)
	}

	var page pagedResponse[betResponse]
	if err := json.Unmarshal(rawBytes, &page); err != nil {
		t.Fatalf("failed to decode paged response: %v", err)
	}
	var found *betResponse
	for i := range page.Items {
		if page.Items[i].ID == bet.ID {
			found = &page.Items[i]
		}
	}
	if found == nil {
		t.Fatal("expected the settled bet in the player's own history")
	}
	if found.Outcome == nil || *found.Outcome != "won" {
		t.Errorf("expected outcome=won, got %+v", found.Outcome)
	}
	if found.PayoutAmount == nil || *found.PayoutAmount != bet.PotentialReturn {
		t.Errorf("expected payout_amount=%d, got %+v", bet.PotentialReturn, found.PayoutAmount)
	}
	if found.SettledAt == nil {
		t.Error("expected a non-nil settled_at")
	}
}

func TestListAdminBets_LifecycleFieldsPresent(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	riskToken := mustRiskManagerToken(t, pool, srv, tenant)

	resp := postJSON(t, srv, simulatePath(bet.ID), riskToken, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 settling won, got %d", resp.StatusCode)
	}

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "tenant-admin-password-1")
	adminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "tenant-admin-password-1")

	resp = getJSON(t, srv, "/v1/admin/sportsbook/bets", adminTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing admin bets, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminBetResponse]
	decodeBody(t, resp, &page)
	var found *adminBetResponse
	for i := range page.Items {
		if page.Items[i].ID == bet.ID {
			found = &page.Items[i]
		}
	}
	if found == nil {
		t.Fatal("expected the settled bet in the admin bet list")
	}
	if found.CorrelationID != bet.ID {
		t.Errorf("expected correlation_id=%s, got %s", bet.ID, found.CorrelationID)
	}
	if len(found.Lifecycle) == 0 {
		t.Fatal("expected a non-empty lifecycle")
	}
	entry := found.Lifecycle[0]
	if entry.EventKind != "settlement" || entry.LedgerTransactionID == "" || entry.CreatedAt == "" {
		t.Errorf("expected a populated settlement lifecycle entry, got %+v", entry)
	}
}
