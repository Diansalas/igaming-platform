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
	"fmt"
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

// mustSimulateRequest posts a raw JSON body against the simulate-settlement
// route with an explicit X-Request-Id, so a caller can force two otherwise-
// identical requests to echo the same request_id back (P3-7's
// byte-identical-404-body assertion needs this - postJSON alone lets the
// server mint a fresh, differing id per call).
func mustSimulateRequest(t *testing.T, srv *httptest.Server, betID, bearerToken, requestID, rawBody string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+simulatePath(betID), strings.NewReader(rawBody))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
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

	// Both requests below carry the SAME X-Request-Id, so the response
	// body's echoed request_id cannot itself make an otherwise-identical
	// body differ (P3-7's "byte-identical" requirement is about the error
	// shape, not a coincidentally-matching random request id).
	const fixedRequestID = "settlement-cross-tenant-p3-7-fixed-request-id"
	resp := mustSimulateRequest(t, srv, bet.ID, tokensB.AccessToken, fixedRequestID,
		`{"event_type":"void","void_reason":"market_cancelled"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's bet id, got %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	resp.Body.Close()
	var body apierror.Error
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body.Code != apierror.CodeSettlementNotFound {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementNotFound, body.Code)
	}

	// No settlement history row exists for tenant B's own bets (there are
	// none), and tenant A's bet is untouched.
	var count int
	err = pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bet_settlements WHERE bet_id = $1`, uuid.MustParse(bet.ID)).Scan(&count)
	})
	if err != nil {
		t.Fatalf("failed to count settlement rows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no settlement rows written for the cross-tenant request, got %d", count)
	}

	// Security review P3-7: the OWNING tenant's ledger is untouched - only
	// the one placement transaction for this bet exists, no
	// sportsbook_settlement/_void/_rollback/tombstone posting was made.
	var ledgerTxCount int
	err = pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE correlation_id = $1`, uuid.MustParse(bet.ID)).Scan(&ledgerTxCount)
	})
	if err != nil {
		t.Fatalf("failed to count ledger transactions: %v", err)
	}
	if ledgerTxCount != 1 {
		t.Errorf("expected exactly the one placement ledger transaction for bet %s, got %d", bet.ID, ledgerTxCount)
	}

	// P3-7: no rejection/lifecycle audit row lands in the OWNING tenant
	// either - the request never reached tenant A's own scope at all.
	var ownerAuditCount int
	err = pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE target_type = 'sportsbook_bet' AND target_id = $1
			   AND action IN ('sportsbook_bet.settled', 'sportsbook_bet.rolled_back', 'sportsbook_bet.voided',
			                   'sportsbook_bet.rollback_tombstoned', 'sportsbook_bet.settlement_rejected')`,
			bet.ID).Scan(&ownerAuditCount)
	})
	if err != nil {
		t.Fatalf("failed to count owning-tenant audit rows: %v", err)
	}
	if ownerAuditCount != 0 {
		t.Errorf("expected no settlement-lifecycle audit row in the OWNING tenant, got %d", ownerAuditCount)
	}

	// P3-7: the 404 body for a cross-tenant bet id must be byte-identical
	// to the 404 body for a bet id that does not exist at all - otherwise
	// the response itself would let a caller distinguish "exists in another
	// tenant" from "never existed" (a cross-tenant enumeration oracle).
	respNonexistent := mustSimulateRequest(t, srv, uuid.NewString(), tokensB.AccessToken, fixedRequestID,
		`{"event_type":"void","void_reason":"market_cancelled"}`)
	if respNonexistent.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a nonexistent bet id, got %d", respNonexistent.StatusCode)
	}
	nonexistentBytes, err := io.ReadAll(respNonexistent.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	respNonexistent.Body.Close()
	if string(bodyBytes) != string(nonexistentBytes) {
		t.Errorf("cross-tenant 404 body differs from nonexistent-id 404 body:\ncross-tenant: %s\nnonexistent:  %s",
			bodyBytes, nonexistentBytes)
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

// --- Decoder strictness (code review #8) ---

// TestSettlementSimulate_ValidationFailed_QuotedPayoutAmountRejected pins
// code review #8: encoding/json's json.Number accepts a QUOTED JSON string
// into a json.Number-typed field with no validation at all (verified: a
// struct field of type json.Number has Kind() == reflect.String, so the
// decoder's string-literal path stores it directly). PayoutAmount is
// json.RawMessage precisely so a quoted "2500" is rejected as not being a
// JSON number token, never silently accepted as a claim.
func TestSettlementSimulate_ValidationFailed_QuotedPayoutAmountRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	body := fmt.Sprintf(`{"event_type":"settle","generation":1,"outcome":"won","asset_code":"EUR","payout_amount":"%d"}`,
		bet.PotentialReturn)
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
		t.Fatalf("expected 400 for a QUOTED payout_amount, got %d", resp.StatusCode)
	}
	var respBody apierror.Error
	decodeBody(t, resp, &respBody)
	if respBody.Code != apierror.CodeSettlementValidationFailed {
		t.Errorf("expected code %q, got %q", apierror.CodeSettlementValidationFailed, respBody.Code)
	}

	// Nothing was posted: the bet is still open.
	status := betStatusHTTP(t, pool, tenant.ID, uuid.MustParse(bet.ID))
	if status != "open" {
		t.Errorf("expected the bet to remain open, got %q", status)
	}
}

// TestSettlementSimulate_ValidationFailed_VoidWithExplicitZeroGeneration
// pins code review #8: ADR 0088 §9.2 makes "generation" FORBIDDEN on void
// - not merely ignored - and an explicit "generation": 0 must be
// distinguished from the field being absent (both are the zero value of a
// plain int). Generation is *int precisely so presence, not just value,
// can be checked.
func TestSettlementSimulate_ValidationFailed_VoidWithExplicitZeroGeneration(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)

	body := `{"event_type":"void","void_reason":"market_cancelled","generation":0}`
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
		t.Fatalf("expected 400 for void with an explicit generation:0, got %d", resp.StatusCode)
	}

	status := betStatusHTTP(t, pool, tenant.ID, uuid.MustParse(bet.ID))
	if status != "open" {
		t.Errorf("expected the bet to remain open, got %q", status)
	}
}

// TestSettlementSimulate_DeactivatedActor_403AndAuditRecorded pins security
// review P3-2: a still-valid access token for a staff account that has
// since been deactivated must not just 403 silently - it is a
// security-relevant event and must leave its own rejection audit row
// (rejection_code = ACTOR_NOT_ACTIVE), in its own transaction (the failed
// SimulateSettlementEvent transaction rolled back and cannot carry it).
func TestSettlementSimulate_DeactivatedActor_403AndAuditRecorded(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "risk-deactivate-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "risk-deactivate-pw-1")

	// Deactivate AFTER the token was issued - the token itself is still
	// valid and carries the risk_manager role claim, so it clears
	// auth.RequirePermission; only the in-transaction staff-active check
	// (identity.GetStaffUserByID) can catch this.
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, staff.ID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to suspend staff user: %v", err)
	}

	resp := postJSON(t, srv, simulatePath(bet.ID), tokens.AccessToken,
		map[string]any{"event_type": "void", "void_reason": "market_cancelled"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a deactivated staff actor, got %d", resp.StatusCode)
	}

	var count int
	var rejectionCode string
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*), COALESCE(MAX(metadata->>'rejection_code'), '')
			   FROM audit_log
			  WHERE tenant_id = $1 AND target_type = 'sportsbook_bet' AND target_id = $2
			    AND action = 'sportsbook_bet.settlement_rejected'`,
			tenant.ID, bet.ID).Scan(&count, &rejectionCode)
	})
	if err != nil {
		t.Fatalf("failed to read rejection audit row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one settlement_rejected audit row, got %d", count)
	}
	if rejectionCode != "ACTOR_NOT_ACTIVE" {
		t.Errorf("expected rejection_code=ACTOR_NOT_ACTIVE, got %q", rejectionCode)
	}

	// Nothing was posted: the bet is still open.
	status := betStatusHTTP(t, pool, tenant.ID, uuid.MustParse(bet.ID))
	if status != "open" {
		t.Errorf("expected the bet to remain open, got %q", status)
	}
}

// betStatusHTTP reads a bet's status straight from the database under
// staff tenant scope, for tests in this package that do not want to depend
// on internal/sportsbook's own test helpers.
func betStatusHTTP(t *testing.T, pool *db.Pool, tenantID, betID uuid.UUID) string {
	t.Helper()
	var status string
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM sportsbook_bets WHERE id = $1`, betID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read bet status for %s: %v", betID, err)
	}
	return status
}
