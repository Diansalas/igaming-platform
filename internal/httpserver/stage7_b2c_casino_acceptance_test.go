//go:build integration

// THE DEFINING ACCEPTANCE TEST FOR STAGE 7 (B2C CASINO PLAYER EXPERIENCE +
// CASINO VERTICAL SLICE).
//
// Mirrors stage6_b2c_sportsbook_acceptance_test.go's exact structure and
// discipline: register -> login (tokens from register) -> deposit (real
// payments mock provider round trip) -> wallet -> casino lobby -> select
// game -> launch -> wager -> win -> rollback -> wallet -> player history ->
// Back Office visibility -> audit trail visibility. Every step calls the
// real HTTP API a browser client would call. Exact financial values are
// asserted at every authoritative boundary (CLAUDE.md's ledger-invariant
// discipline, applied to a test assertion, not just production code).
package httpserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// newStage7B2CTestServer wires the payment orchestrator (for a real
// deposit) and the casino orchestrator (for the vertical slice) - the
// union newStage6B2CTestServer already established for payments+sportsbook,
// applied here to payments+casino.
func newStage7B2CTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, paymentOrch *payments.Orchestrator, casinoOrch *casino.Orchestrator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         "platform-api-test",
		AccessTokenTTL:      5 * time.Minute,
		RefreshTokenTTL:     time.Hour,
		PaymentOrchestrator: paymentOrch, PaymentsOutboundCredentials: payments.MockCredentialResolver{},
		CasinoOrchestrator:          casinoOrch,
		CasinoOutboundCredentials:   casino.NewMockOutboundResolver(),
		CasinoPlaySimulationEnabled: true,
		PersonResolver:              identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStage7_B2CPlayerRegisterToBackOfficeVisibility_DefiningAcceptanceTest(t *testing.T) {
	pool, issuer := testEnv(t)
	paymentOrch, mockPaymentProvider := newMockOrchestrator()
	casinoOrch, _ := newMockCasinoOrchestrator()
	srv := newStage7B2CTestServer(t, pool, issuer, paymentOrch, casinoOrch)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "s7-ta-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "s7-ta-pw-1")

	// --- step 0a: configure the mock PSP's capability so a real deposit can route ---
	resp := capabilityPutRequest(t, srv, "mock", tenantAdminTokens.AccessToken, validCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 0a (configure PSP capability): expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// --- step 0b: configure the mock casino provider's capability, and
	// seed+opt-in a catalogue game - the platform-catalogue/tenant-opt-in
	// setup a real deployment would do via the admin API, exercised for
	// real here (never a direct SQL shortcut). ---
	resp = putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tenantAdminTokens.AccessToken, validCasinoCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 0b (configure casino capability): expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	// --- 1. Register (creates the player identity) ---
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	// --- 2. Login is implicit: mustRegisterPlayer's tokens ARE a real
	// server-issued session, reused for every subsequent call below. ---

	// --- 3. Wallet: fund via the real deposit endpoint + signed webhook
	// callback - the actual B2C deposit path, not a shortcut ledger write. ---
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

	depositPayload := mockPaymentProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", depositPayload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3 (deposit webhook): expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3 (read wallet): expected 200, got %d", resp.StatusCode)
	}
	var wallet walletSummaryResponse
	decodeBody(t, resp, &wallet)
	if wallet.CashBalance != depositAmount {
		t.Fatalf("step 3: expected wallet cash_balance=%d after deposit, got %d", depositAmount, wallet.CashBalance)
	}

	// --- 4. Casino lobby: browse the tenant's own opted-in catalogue. ---
	resp = getJSON(t, srv, "/v1/me/casino/games", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 4 (casino lobby): expected 200, got %d", resp.StatusCode)
	}
	var games []casinoGameResponse
	decodeBody(t, resp, &games)
	found := false
	for _, g := range games {
		if g.ID == game.ID.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("step 4: expected the seeded game %s in the lobby, got %+v", game.ID, games)
	}

	// --- 5. Select game -> launch: server resolves player/tenant/brand/
	// wallet/provider - the client only names asset_code/mode. ---
	resp = postJSON(t, srv, "/v1/me/casino/games/"+game.ID.String()+"/launch", player.Tokens.AccessToken,
		map[string]string{"asset_code": "EUR", "mode": "real"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("step 5 (launch): expected 201, got %d", resp.StatusCode)
	}
	var launched launchCasinoGameResponse
	decodeBody(t, resp, &launched)
	if launched.SessionID == "" || launched.LaunchURL == "" {
		t.Fatalf("step 5: expected a session_id and launch_url, got %+v", launched)
	}

	// --- 6. Wager: the mock-provider round-trip through the exact same
	// callback pipeline a real provider's game client would use. ---
	const stakeAmount int64 = 2_000
	resp = postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken,
		map[string]any{"stake_amount": stakeAmount, "idempotency_key": uuid.NewString()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 6 (wager): expected 200, got %d", resp.StatusCode)
	}
	var wagerResult map[string]any
	decodeBody(t, resp, &wagerResult)
	if wagerResult["outcome"] != "succeeded" {
		t.Fatalf("step 6: expected outcome=succeeded, got %+v", wagerResult)
	}
	betProviderTxID, _ := wagerResult["provider_tx_id"].(string)
	if betProviderTxID == "" {
		t.Fatal("step 6: expected a non-empty provider_tx_id")
	}

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	decodeBody(t, resp, &wallet)
	wantAfterWager := depositAmount - stakeAmount
	if wallet.CashBalance != wantAfterWager {
		t.Fatalf("step 6: expected cash_balance=%d after the wager, got %d", wantAfterWager, wallet.CashBalance)
	}

	// --- 7. Win: credits the platform ledger - never a client-supplied
	// final balance. ---
	const winAmount int64 = 5_000
	resp = postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/win", player.Tokens.AccessToken,
		map[string]any{"win_amount": winAmount, "idempotency_key": uuid.NewString()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 7 (win): expected 200, got %d", resp.StatusCode)
	}
	var winResult map[string]any
	decodeBody(t, resp, &winResult)
	if winResult["outcome"] != "succeeded" {
		t.Fatalf("step 7: expected outcome=succeeded, got %+v", winResult)
	}
	winProviderTxID, _ := winResult["provider_tx_id"].(string)
	if winProviderTxID == "" {
		t.Fatal("step 7: expected a non-empty provider_tx_id")
	}

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	decodeBody(t, resp, &wallet)
	wantAfterWin := depositAmount - stakeAmount + winAmount
	if wallet.CashBalance != wantAfterWin {
		t.Fatalf("step 7: expected cash_balance=%d after the win, got %d", wantAfterWin, wallet.CashBalance)
	}

	// --- 8. Rollback: reverse the win via the existing compensating-entry
	// model (never a mutation of the original ledger entries). ---
	resp = postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": winProviderTxID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 8 (rollback): expected 200, got %d", resp.StatusCode)
	}
	var rollbackResult map[string]any
	decodeBody(t, resp, &rollbackResult)
	if rollbackResult["outcome"] != "succeeded" {
		t.Fatalf("step 8: expected outcome=succeeded, got %+v", rollbackResult)
	}

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	decodeBody(t, resp, &wallet)
	wantAfterRollback := depositAmount - stakeAmount
	if wallet.CashBalance != wantAfterRollback {
		t.Fatalf("step 8: expected cash_balance=%d after the rollback (win reversed), got %d", wantAfterRollback, wallet.CashBalance)
	}

	// --- 9. Player history: the player's own round history reflects the
	// exact bet/win/rollback amounts and final status. ---
	resp = getJSON(t, srv, "/v1/me/casino/rounds", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 9 (player history): expected 200, got %d", resp.StatusCode)
	}
	var history pagedResponse[roundResponse]
	decodeBody(t, resp, &history)
	var round *roundResponse
	for i := range history.Items {
		if history.Items[i].SessionID == launched.SessionID {
			round = &history.Items[i]
		}
	}
	if round == nil {
		t.Fatalf("step 9: expected session %s to appear in the player's own round history", launched.SessionID)
	}
	if round.Status != "rolled_back" {
		t.Errorf("step 9: expected status=rolled_back, got %q", round.Status)
	}
	if round.BetAmount == nil || *round.BetAmount != stakeAmount {
		t.Errorf("step 9: expected bet_amount=%d, got %+v", stakeAmount, round.BetAmount)
	}
	if round.WinAmount == nil || *round.WinAmount != winAmount {
		t.Errorf("step 9: expected win_amount=%d, got %+v", winAmount, round.WinAmount)
	}
	if round.RollbackAmount == nil || *round.RollbackAmount != winAmount {
		t.Errorf("step 9: expected rollback_amount=%d (the win being reversed), got %+v", winAmount, round.RollbackAmount)
	}
	if round.BetProviderTxID != betProviderTxID {
		t.Errorf("step 9: expected bet_provider_tx_id=%s, got %s", betProviderTxID, round.BetProviderTxID)
	}

	// --- 10. Back Office visibility: an authorized tenant_admin sees the
	// round, tenant-wide, with player/brand/game/provider/amounts/status. ---
	resp = getJSON(t, srv, "/v1/admin/casino/rounds", tenantAdminTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 10 (admin round queue): expected 200, got %d", resp.StatusCode)
	}
	var adminPage pagedResponse[adminRoundResponse]
	decodeBody(t, resp, &adminPage)
	var seen *adminRoundResponse
	for i := range adminPage.Items {
		if adminPage.Items[i].SessionID == launched.SessionID {
			seen = &adminPage.Items[i]
		}
	}
	if seen == nil {
		t.Fatalf("step 10: expected session %s to be visible to the tenant_admin's Back Office queue", launched.SessionID)
	}
	if seen.PlayerAccountID != player.ID.String() {
		t.Errorf("step 10: expected player_account_id=%s, got %s", player.ID, seen.PlayerAccountID)
	}
	if seen.BrandID != brand.ID.String() {
		t.Errorf("step 10: expected brand_id=%s, got %s", brand.ID, seen.BrandID)
	}
	if seen.DecimalExponent != 2 {
		t.Errorf("step 10: expected decimal_exponent=2 for EUR, got %d", seen.DecimalExponent)
	}
	if len(seen.LedgerTransactionIDs) != 3 {
		t.Errorf("step 10: expected exactly 3 audit-linkage ledger_transaction_ids (bet+win+rollback), got %v", seen.LedgerTransactionIDs)
	}

	// --- 11. Audit trail visibility: the same operator sees the
	// casino_bet.posted, casino_win.posted, and casino_win.rolled_back
	// audit records CLAUDE.md requires for every mutating financial action. ---
	for _, action := range []string{"casino_bet.posted", "casino_win.posted", "casino_win.rolled_back"} {
		resp = getJSON(t, srv, "/v1/admin/audit-log?action="+action, tenantAdminTokens.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("step 11 (audit log %s): expected 200, got %d", action, resp.StatusCode)
		}
		var auditPage pagedResponse[auditEntryResponse]
		decodeBody(t, resp, &auditPage)
		found := false
		for _, e := range auditPage.Items {
			if e.Outcome == "success" {
				found = true
			}
		}
		if !found {
			t.Fatalf("step 11: expected a successful %s audit entry, got %+v", action, auditPage.Items)
		}
	}
}
