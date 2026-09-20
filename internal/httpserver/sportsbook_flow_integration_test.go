//go:build integration

// Stage 6 sportsbook vertical-slice HTTP-layer tests: catalogue browse
// (public), bet placement/history (player-authenticated, cross-player
// isolation), and Back Office bet visibility (staff-permission-gated,
// cross-tenant isolation). Follows internal/httpserver's own established
// conventions (testEnv/newTestServer/postJSON/getJSON/mustRegisterPlayer/
// mustCreateTenant/mustCreateStaff/mustLoginStaff) exactly - see
// casino_flow_integration_test.go's identical structure for the closest
// precedent.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
)

func newSportsbookTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                pool,
		AuthIssuer:        issuer,
		ServiceName:       "platform-api-test",
		AccessTokenTTL:    5 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		SportsbookEnabled: true,
		PersonResolver:    identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mustSeedSportsbookSelection inserts a full sport->competition->event->
// market->selection chain directly via SQL - the platform-wide, RLS-free
// catalogue tables (migration 0078), mirroring internal/sportsbook's own
// unexported seedSelection test helper (not reusable across packages).
func mustSeedSportsbookSelection(t *testing.T, pool *db.Pool, oddsNumerator, oddsDenominator int64) uuid.UUID {
	t.Helper()
	ref := uuid.NewString()[:8]
	var selectionID uuid.UUID
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var sportID, compID, eventID, marketID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $2, 'HTTP Test Sport') RETURNING id`,
			"http-sport-"+ref, "http-sport-"+ref).Scan(&sportID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ($1, $2, 'HTTP Test Competition') RETURNING id`,
			sportID, "http-comp-"+ref).Scan(&compID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_events (competition_id, external_ref, name, start_time, status) VALUES ($1, $2, 'HTTP Test Event', $3, 'scheduled') RETURNING id`,
			compID, "http-event-"+ref, time.Now().Add(48*time.Hour)).Scan(&eventID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_markets (event_id, external_ref, name, status) VALUES ($1, $2, 'HTTP Test Market', 'open') RETURNING id`,
			eventID, "http-market-"+ref).Scan(&marketID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator, status)
			 VALUES ($1, $2, 'HTTP Test Selection', $3, $4, 'active') RETURNING id`,
			marketID, "http-sel-"+ref, oddsNumerator, oddsDenominator).Scan(&selectionID)
	})
	if err != nil {
		t.Fatalf("seed sportsbook selection: %v", err)
	}
	return selectionID
}

// mustActivatePlayer moves a freshly-registered player out of
// 'pending_verification' (RegisterPlayer's own default status, Stage 4F)
// into 'active' directly - a real deployment does this via the email-
// verification flow, which is out of scope for this test file; rg.
// EvaluateEligibility requires exactly PlayerStatusActive (CodePlayer
// AccountNotActive otherwise), so every bet-placement test needs this
// fixture step exactly like internal/casino/internal/withdrawal's own
// direct-SQL fixtures insert 'active' up front.
func mustActivatePlayer(t *testing.T, pool *db.Pool, tenantID, playerAccountID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, playerAccountID, identity.PlayerStatusActive)
	})
	if err != nil {
		t.Fatalf("activate player: %v", err)
	}
}

func placeBetRequestBody(selectionID uuid.UUID, stake, oddsNum, oddsDen int64, idempotencyKey string) map[string]any {
	return map[string]any{
		"selection_id": selectionID.String(), "stake_amount": stake, "asset_code": "EUR",
		"expected_odds_numerator": oddsNum, "expected_odds_denominator": oddsDen, "idempotency_key": idempotencyKey,
	}
}

// --- Catalogue browse: public, no authentication ---

func TestSportsbookCatalogue_ListSportsRequiresNoAuth(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)
	mustSeedSportsbookSelection(t, pool, 200, 100)

	resp := getJSON(t, srv, "/v1/sportsbook/sports", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with no bearer token, got %d", resp.StatusCode)
	}
	var sports []sportCatalogueResponse
	decodeBody(t, resp, &sports)
	found := false
	for _, s := range sports {
		if s.Code != "" {
			found = true
		}
	}
	if !found || len(sports) == 0 {
		t.Fatal("expected at least one sport in the catalogue")
	}
}

func TestSportsbookCatalogue_GetEventNotFound(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	resp := getJSON(t, srv, "/v1/sportsbook/events/"+uuid.NewString(), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown event id, got %d", resp.StatusCode)
	}
}

func TestSportsbookCatalogue_GetEventDetailIncludesMarketsAndSelections(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	var eventID string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT e.id::text FROM sb_events e
			JOIN sb_markets m ON m.event_id = e.id
			JOIN sb_selections sel ON sel.market_id = m.id
			WHERE sel.id = $1`, mustSeedSportsbookSelection(t, pool, 300, 100)).Scan(&eventID)
	})
	if err != nil {
		t.Fatalf("resolve seeded event id: %v", err)
	}

	resp := getJSON(t, srv, "/v1/sportsbook/events/"+eventID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var detail eventDetailResponse
	decodeBody(t, resp, &detail)
	if len(detail.Markets) != 1 || len(detail.Markets[0].Selections) != 1 {
		t.Fatalf("expected exactly 1 market with 1 selection, got %+v", detail)
	}
	if detail.Markets[0].Selections[0].OddsNumerator != 300 {
		t.Fatalf("expected odds_numerator 300, got %d", detail.Markets[0].Selections[0].OddsNumerator)
	}
}

// --- Bet placement / history: player-authenticated ---

func TestPlaceBet_UnauthenticatedDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)
	selectionID := mustSeedSportsbookSelection(t, pool, 200, 100)

	resp := postJSON(t, srv, "/v1/me/sportsbook/bets", "", placeBetRequestBody(selectionID, 1000, 200, 100, uuid.NewString()))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d", resp.StatusCode)
	}
}

func TestPlaceBet_AuthenticatedSuccess(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	// Resolve the player's own brand_id server-side (needed to fund the
	// right wallet) the same way every other financial flow test does.
	var brandID uuid.UUID
	errBrand := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.GetPlayerAccountByID(ctx, tx, player.ID)
		brandID = a.BrandID
		return err
	})
	if errBrand != nil {
		t.Fatalf("resolve player brand: %v", errBrand)
	}
	fundWallet(t, pool, tenant.ID, brandID, player.ID, "EUR", 10_000)

	selectionID := mustSeedSportsbookSelection(t, pool, 250, 100)

	resp := postJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 1_000, 250, 100, uuid.NewString()))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var placed placeBetResponse
	decodeBody(t, resp, &placed)
	if !placed.Accepted || placed.Bet == nil {
		t.Fatalf("expected an accepted bet, got %+v", placed)
	}
	if placed.Bet.PotentialReturn != 2_500 {
		t.Fatalf("expected potential_return 2500, got %d", placed.Bet.PotentialReturn)
	}

	// The player's own history includes it.
	resp = getJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing own bets, got %d", resp.StatusCode)
	}
	var page pagedResponse[betResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected exactly 1 bet in own history, got total=%d items=%d", page.Total, len(page.Items))
	}
}

func TestPlaceBet_OddsChangedIsDistinguishableFromInsufficientFunds(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	var brandID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.GetPlayerAccountByID(ctx, tx, player.ID)
		brandID = a.BrandID
		return err
	}); err != nil {
		t.Fatalf("resolve player brand: %v", err)
	}
	fundWallet(t, pool, tenant.ID, brandID, player.ID, "EUR", 100)

	selectionID := mustSeedSportsbookSelection(t, pool, 250, 100)

	// Stale odds -> odds_changed, never conflated with insufficient_funds.
	resp := postJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 1_000, 999, 100, uuid.NewString()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (a well-formed rejection, not an HTTP error), got %d", resp.StatusCode)
	}
	var rejected placeBetResponse
	decodeBody(t, resp, &rejected)
	if rejected.Accepted {
		t.Fatal("expected the bet to be rejected")
	}
	if rejected.RejectionCategory != "odds_changed" {
		t.Fatalf("expected rejection_category odds_changed, got %q", rejected.RejectionCategory)
	}

	// Correct odds but insufficient balance -> insufficient_funds, a
	// DIFFERENT category the frontend must distinguish.
	resp = postJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 1_000, 250, 100, uuid.NewString()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	decodeBody(t, resp, &rejected)
	if rejected.Accepted {
		t.Fatal("expected the bet to be rejected")
	}
	if rejected.RejectionCategory != "insufficient_funds" {
		t.Fatalf("expected rejection_category insufficient_funds, got %q", rejected.RejectionCategory)
	}
}

// TestListMyBets_CrossPlayerIsolation proves a player's bet history never
// exposes another player's bets, even within the same tenant/brand.
func TestListMyBets_CrossPlayerIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	playerA := mustRegisterPlayer(t, srv, brand.Slug)
	playerB := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, playerA.ID)
	mustActivatePlayer(t, pool, tenant.ID, playerB.ID)

	var brandIDA, brandIDB uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.GetPlayerAccountByID(ctx, tx, playerA.ID)
		brandIDA = a.BrandID
		return err
	}); err != nil {
		t.Fatalf("resolve player A brand: %v", err)
	}
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := identity.GetPlayerAccountByID(ctx, tx, playerB.ID)
		brandIDB = b.BrandID
		return err
	}); err != nil {
		t.Fatalf("resolve player B brand: %v", err)
	}
	fundWallet(t, pool, tenant.ID, brandIDA, playerA.ID, "EUR", 10_000)
	fundWallet(t, pool, tenant.ID, brandIDB, playerB.ID, "EUR", 10_000)

	selectionID := mustSeedSportsbookSelection(t, pool, 200, 100)

	respA := postJSON(t, srv, "/v1/me/sportsbook/bets", playerA.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 500, 200, 100, uuid.NewString()))
	if respA.StatusCode != http.StatusCreated {
		t.Fatalf("expected player A's bet to succeed, got %d", respA.StatusCode)
	}

	// Player B has placed NO bets - their own history must be empty, never
	// showing player A's bet.
	respB := getJSON(t, srv, "/v1/me/sportsbook/bets", playerB.Tokens.AccessToken)
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", respB.StatusCode)
	}
	var pageB pagedResponse[betResponse]
	decodeBody(t, respB, &pageB)
	if pageB.Total != 0 || len(pageB.Items) != 0 {
		t.Fatalf("expected player B to see zero bets (never player A's), got total=%d items=%d", pageB.Total, len(pageB.Items))
	}
}

// --- Back Office: staff-permission-gated, tenant-wide, cross-tenant isolation ---

func TestAdminListBets_PlayerTokenDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := getJSON(t, srv, "/v1/admin/sportsbook/bets", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a player token on the admin bets route, got %d", resp.StatusCode)
	}
}

func TestAdminListBets_UnauthenticatedDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	resp := getJSON(t, srv, "/v1/admin/sportsbook/bets", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d", resp.StatusCode)
	}
}

// TestAdminListBets_RoleWithoutPermissionDenied proves holding a broad
// staff role alone is not sufficient - risk_manager stands in for "a
// staff role with no sportsbook_bet:read grant at all" (unlike
// tenant_admin/compliance/support/finance, which internal/auth/
// permission.go does grant it to).
func TestAdminListBets_RoleWithoutPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "a-decent-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/sportsbook/bets", tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a role with no sportsbook_bet:read grant, got %d", resp.StatusCode)
	}
}

// TestAdminListBets_TenantAdminAuthorizedAndCrossTenantIsolated is the
// three-case authorization bar in one test: authorized success (a
// tenant_admin, which holds PermSportsbookBetRead), and genuine cross-
// tenant isolation (tenant A's admin never sees tenant B's bets).
func TestAdminListBets_TenantAdminAuthorizedAndCrossTenantIsolated(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)

	playerA := mustRegisterPlayer(t, srv, brandA.Slug)
	playerB := mustRegisterPlayer(t, srv, brandB.Slug)
	mustActivatePlayer(t, pool, tenantA.ID, playerA.ID)
	mustActivatePlayer(t, pool, tenantB.ID, playerB.ID)

	var brandIDA, brandIDB uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.GetPlayerAccountByID(ctx, tx, playerA.ID)
		brandIDA = a.BrandID
		return err
	}); err != nil {
		t.Fatalf("resolve player A brand: %v", err)
	}
	if err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := identity.GetPlayerAccountByID(ctx, tx, playerB.ID)
		brandIDB = b.BrandID
		return err
	}); err != nil {
		t.Fatalf("resolve player B brand: %v", err)
	}
	fundWallet(t, pool, tenantA.ID, brandIDA, playerA.ID, "EUR", 10_000)
	fundWallet(t, pool, tenantB.ID, brandIDB, playerB.ID, "EUR", 10_000)

	selectionID := mustSeedSportsbookSelection(t, pool, 200, 100)

	if resp := postJSON(t, srv, "/v1/me/sportsbook/bets", playerA.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 500, 200, 100, uuid.NewString())); resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected player A's bet to succeed, got %d", resp.StatusCode)
	}
	if resp := postJSON(t, srv, "/v1/me/sportsbook/bets", playerB.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 700, 200, 100, uuid.NewString())); resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected player B's bet to succeed, got %d", resp.StatusCode)
	}

	staffA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokensA := mustLoginStaff(t, srv, tenantA.Slug, staffA.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/sportsbook/bets", tokensA.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a tenant_admin with sportsbook_bet:read, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminBetResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected tenant A's admin to see exactly its own tenant's 1 bet, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].PlayerAccountID != playerA.ID.String() {
		t.Fatalf("expected the visible bet to belong to player A, got player_account_id=%s", page.Items[0].PlayerAccountID)
	}
	// Stage 6.1 QA-review regression: brand_id was write-only (a
	// PlaceBetParams input) with no assertion anywhere and no HTTP field
	// to check it against - a bet misattributed to the wrong brand within
	// tenant A would have passed every existing test silently.
	if page.Items[0].BrandID != brandIDA.String() {
		t.Fatalf("expected the visible bet's brand_id to be player A's own brand %s, got %s", brandIDA, page.Items[0].BrandID)
	}
	// Stage 6.1 QA-review regression: decimal_exponent was populated but
	// never asserted against a known-correct value anywhere - a
	// regression that always returned 0, or looked up the wrong asset,
	// would have passed every existing test. EUR's exponent is 2
	// (migrations/0003_create_assets.up.sql's seed data).
	if page.Items[0].DecimalExponent != 2 {
		t.Fatalf("expected decimal_exponent=2 for EUR, got %d", page.Items[0].DecimalExponent)
	}
}
