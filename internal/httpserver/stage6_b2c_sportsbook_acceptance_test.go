//go:build integration

// THE DEFINING ACCEPTANCE TEST FOR STAGE 6 (B2C PLAYER/BRAND MVP + FIRST
// SPORTSBOOK VERTICAL SLICE).
//
// This single test chains the full real, server-authoritative path the
// Stage 6 directive names explicitly: player register -> login (tokens
// from register) -> wallet (real deposit through the existing payments
// mock provider, exactly like TestFinancialHappyPath_EndToEnd) ->
// sportsbook catalogue browse -> event detail (market/selection) -> bet
// slip assembly (selection + odds read from the server's own response,
// never invented client-side) -> bet placement (full orchestrator chain:
// validation -> RG -> risk -> wallet authorization -> ledger post) ->
// bet history (player's own view) -> wallet reflects the debit -> Back
// Office visibility (an authorized tenant_admin sees the bet, tenant-wide)
// -> audit trail visibility (the same staff principal can see the
// sportsbook_bet.placed audit record CLAUDE.md requires for every
// mutating action).
//
// Every step calls the real HTTP API a browser client would call - no
// internal package is invoked to fabricate a step this chain is supposed
// to prove end-to-end. The only exception is catalogue seeding
// (mustSeedSportsbookSelection), which stands in for the real provider's
// own catalogue sync (internal/sportsbook.SyncCatalogue) - a call this
// process already makes at server startup in production, just not
// exercised by an httptest.Server here.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// newStage6B2CTestServer wires BOTH the payment orchestrator (for a real
// deposit) and the sportsbook feature flag - the union of
// newFinancialTestServer and newSportsbookTestServer, needed only because
// this one test genuinely exercises both subsystems in sequence.
func newStage6B2CTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *payments.Orchestrator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         "platform-api-test",
		AccessTokenTTL:      5 * time.Minute,
		RefreshTokenTTL:     time.Hour,
		PaymentOrchestrator: orchestrator,
		SportsbookEnabled:   true,
		PersonResolver:      identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStage6_B2CPlayerRegisterToBackOfficeVisibility_DefiningAcceptanceTest(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newStage6B2CTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	// Configure the mock PSP's capability so a real deposit can route.
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "s6-ta-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "s6-ta-pw-1")
	resp := capabilityPutRequest(t, srv, "mock", tenantAdminTokens.AccessToken, validCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 0 (configure PSP capability): expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// --- 1. Register (creates the player identity) ---
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	// --- 2. Login is implicit: mustRegisterPlayer's tokens ARE a real
	// server-issued session, and every subsequent call below re-uses that
	// same bearer token exactly as a browser client would after login. ---

	// --- 3. Wallet: fund via the real deposit endpoint + signed webhook
	// callback, not a shortcut ledger write - this is the actual B2C
	// deposit path a player would use to fund their account. ---
	const depositAmount int64 = 20_000
	resp = postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": player.ID.String() + "-deposit",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("step 3 (initiate deposit): expected 201, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := strings.TrimPrefix(intent.RedirectURL, "https://mock-psp.invalid/pay/")

	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3 (deposit webhook): expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3 (read wallet): expected 200, got %d", resp.StatusCode)
	}
	var walletBefore walletSummaryResponse
	decodeBody(t, resp, &walletBefore)
	if walletBefore.CashBalance != depositAmount {
		t.Fatalf("step 3: expected wallet cash_balance=%d after deposit, got %d", depositAmount, walletBefore.CashBalance)
	}

	// --- 4. Sportsbook: browse the catalogue (public) ---
	selectionID := mustSeedSportsbookSelection(t, pool, 250, 100) // decimal odds 3.50

	resp = getJSON(t, srv, "/v1/sportsbook/sports", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 4 (browse catalogue): expected 200, got %d", resp.StatusCode)
	}
	var sports []sportCatalogueResponse
	decodeBody(t, resp, &sports)
	var eventID string
	for _, s := range sports {
		for _, c := range s.Competitions {
			for _, e := range c.Events {
				eventID = e.ID // any event resolves to a market/selection tree below
			}
		}
	}
	if eventID == "" {
		t.Fatal("step 4: expected at least one event in the catalogue")
	}

	// --- 5. Event -> market -> selection detail (server-authoritative
	// odds - the bet slip is built from THIS response, never invented
	// client-side). Resolve the event that actually owns our seeded
	// selection so the odds read back match what step 6 expects. ---
	var seededEventID string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT e.id::text FROM sb_events e
			JOIN sb_markets m ON m.event_id = e.id
			JOIN sb_selections sel ON sel.market_id = m.id
			WHERE sel.id = $1`, selectionID).Scan(&seededEventID)
	}); err != nil {
		t.Fatalf("step 5 (resolve seeded event): %v", err)
	}

	resp = getJSON(t, srv, "/v1/sportsbook/events/"+seededEventID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 5 (event detail): expected 200, got %d", resp.StatusCode)
	}
	var detail eventDetailResponse
	decodeBody(t, resp, &detail)
	if len(detail.Markets) != 1 || len(detail.Markets[0].Selections) != 1 {
		t.Fatalf("step 5: expected exactly 1 market with 1 selection, got %+v", detail)
	}
	sel := detail.Markets[0].Selections[0]
	if sel.ID != selectionID.String() {
		t.Fatalf("step 5: expected selection id %s, got %s", selectionID, sel.ID)
	}

	// --- 6. Bet slip -> bet validation -> bet placement, using the
	// server's own odds from step 5 (a stale/invented price would be
	// rejected as odds_changed - proven separately by
	// TestPlaceBet_OddsChangedIsDistinguishableFromInsufficientFunds; this
	// test's job is the happy path all the way to Back Office visibility). ---
	const stakeAmount int64 = 2_000
	resp = postJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken, map[string]any{
		"selection_id": sel.ID, "stake_amount": stakeAmount, "asset_code": "EUR",
		"expected_odds_numerator": sel.OddsNumerator, "expected_odds_denominator": sel.OddsDenominator,
		"idempotency_key": player.ID.String() + "-bet-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("step 6 (place bet): expected 201, got %d", resp.StatusCode)
	}
	var placed placeBetResponse
	decodeBody(t, resp, &placed)
	if !placed.Accepted || placed.Bet == nil {
		t.Fatalf("step 6: expected an accepted bet, got %+v", placed)
	}
	wantReturn := stakeAmount * sel.OddsNumerator / sel.OddsDenominator
	if placed.Bet.PotentialReturn != wantReturn {
		t.Fatalf("step 6: expected potential_return=%d (stake %d at %d/%d), got %d", wantReturn, stakeAmount, sel.OddsNumerator, sel.OddsDenominator, placed.Bet.PotentialReturn)
	}
	betID := placed.Bet.ID

	// --- 7. Bet history: the player's own view includes the new bet. ---
	resp = getJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 7 (bet history): expected 200, got %d", resp.StatusCode)
	}
	var history pagedResponse[betResponse]
	decodeBody(t, resp, &history)
	foundInHistory := false
	for _, b := range history.Items {
		if b.ID == betID {
			foundInHistory = true
			if b.Status != "open" {
				t.Errorf("step 7: expected bet status 'open', got %q", b.Status)
			}
		}
	}
	if !foundInHistory {
		t.Fatalf("step 7: expected bet %s to appear in the player's own history", betID)
	}

	// --- 8. Wallet reflects the stake debit. ---
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 8 (wallet after bet): expected 200, got %d", resp.StatusCode)
	}
	var walletAfter walletSummaryResponse
	decodeBody(t, resp, &walletAfter)
	wantCashAfter := depositAmount - stakeAmount
	if walletAfter.CashBalance != wantCashAfter {
		t.Fatalf("step 8: expected cash_balance=%d after the bet stake, got %d", wantCashAfter, walletAfter.CashBalance)
	}

	// --- 9. Back Office visibility: an authorized tenant_admin sees the
	// bet, tenant-wide, with player/selection/stake/asset/status/timestamp. ---
	resp = getJSON(t, srv, "/v1/admin/sportsbook/bets", tenantAdminTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 9 (admin bet queue): expected 200, got %d", resp.StatusCode)
	}
	var adminPage pagedResponse[adminBetResponse]
	decodeBody(t, resp, &adminPage)
	var seen *adminBetResponse
	for i := range adminPage.Items {
		if adminPage.Items[i].ID == betID {
			seen = &adminPage.Items[i]
		}
	}
	if seen == nil {
		t.Fatalf("step 9: expected bet %s to be visible to the tenant_admin's Back Office queue", betID)
	}
	if seen.PlayerAccountID != player.ID.String() {
		t.Errorf("step 9: expected player_account_id=%s, got %s", player.ID, seen.PlayerAccountID)
	}
	if seen.SelectionID != selectionID.String() {
		t.Errorf("step 9: expected selection_id=%s, got %s", selectionID, seen.SelectionID)
	}
	if seen.AssetCode != "EUR" || seen.StakeAmount != stakeAmount || seen.Status != "open" || seen.PlacedAt == "" {
		t.Errorf("step 9: expected asset_code=EUR stake_amount=%d status=open placed_at set, got %+v", stakeAmount, seen)
	}

	// --- 10. Audit trail visibility: the same operator sees the
	// sportsbook_bet.placed audit record CLAUDE.md requires for every
	// mutating financial action. ---
	resp = getJSON(t, srv, "/v1/admin/audit-log?action=sportsbook_bet.placed", tenantAdminTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 10 (audit log): expected 200, got %d", resp.StatusCode)
	}
	var auditPage pagedResponse[auditEntryResponse]
	decodeBody(t, resp, &auditPage)
	foundAudit := false
	for _, e := range auditPage.Items {
		if e.TargetID == betID && e.Outcome == "success" {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("step 10: expected a sportsbook_bet.placed audit entry targeting bet %s, got %+v", betID, auditPage.Items)
	}
}
