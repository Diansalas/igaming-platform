//go:build integration

// Stage 7 HTTP-layer tests for the casino play-simulation endpoints
// (casino_play_handlers.go) and the round history/Back-Office visibility
// endpoints (casino_history_handlers.go) - mirrors casino_flow_integration_
// test.go's and sportsbook_flow_integration_test.go's own conventions
// exactly (testEnv/newCasinoTestServer/mustCreateTenant/mustRegisterPlayer/
// fundWallet/mustCreateStaff/mustLoginStaff/postJSON/getJSON/decodeBody).
//
// Four independent specialist reviews (architect, security, ledger-
// finance, database/RLS) converged on the same P0 during Stage 7's review
// round: the rollback endpoint let a player name ANY provider_tx_id as
// the reversal target, since the mock adapter's signature is self-issued
// by the platform on the player's own behalf and therefore authenticates
// nothing about which transaction they name (unlike a real, independently
// signed provider webhook). Several tests below exist specifically to
// prove the fix (requireRollbackTargetOwnedByRound in
// casino_play_handlers.go) actually closes each exploit path the reviews
// demonstrated live over HTTP: cross-player reversal, cross-round
// reversal (same player, different round), and tombstone-poisoning DoS.
package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// mustEnableCasinoCapability registers the mock-casino provider capability
// for tenant via the real PUT /v1/admin/casino/providers/{id}/capability
// HTTP endpoint (a tenant_admin action) - without this, LaunchGame's own
// capability check rejects every launch with ErrProviderUnavailable,
// exactly like a real, never-yet-configured provider would.
func mustEnableCasinoCapability(t *testing.T, srv *httptest.Server, pool *db.Pool, tenant identity.Tenant) {
	t.Helper()
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "casino-capability-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "casino-capability-pw-1")
	resp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling the mock-casino capability for tenant %s, got %d", tenant.Slug, resp.StatusCode)
	}
	resp.Body.Close()
}

// mustLaunchCasinoGame drives the real player launch HTTP endpoint and
// returns the parsed response - the same server-authoritative path a real
// player session would use, never a direct casino.CreateLaunchSession
// call, so these tests exercise the exact code path Stage 7's brand-
// pinning fix (migration 0079) protects.
func mustLaunchCasinoGame(t *testing.T, srv *httptest.Server, bearerToken, gameID, assetCode, mode string) launchCasinoGameResponse {
	t.Helper()
	resp := postJSON(t, srv, "/v1/me/casino/games/"+gameID+"/launch", bearerToken, map[string]string{
		"asset_code": assetCode, "mode": mode,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 launching the game, got %d", resp.StatusCode)
	}
	var launched launchCasinoGameResponse
	decodeBody(t, resp, &launched)
	return launched
}

func wagerBody(stake int64) map[string]any {
	return map[string]any{"stake_amount": stake, "idempotency_key": uuid.NewString()}
}

func winBody(amount int64) map[string]any {
	return map[string]any{"win_amount": amount, "idempotency_key": uuid.NewString()}
}

func countLedgerTransactions(t *testing.T, pool *db.Pool, tenantID uuid.UUID, whereExtra string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino'`+whereExtra).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions: %v", err)
	}
	return count
}

func walletCashBalance(t *testing.T, srv *httptest.Server, bearerToken string) int64 {
	t.Helper()
	resp := getJSON(t, srv, "/v1/me/wallets/EUR", bearerToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reading wallet, got %d", resp.StatusCode)
	}
	var w walletSummaryResponse
	decodeBody(t, resp, &w)
	return w.CashBalance
}

// TestCasinoPlay_WagerWinRollbackHappyPath drives the full mock-provider
// round-trip through the new HTTP endpoints and confirms the player's own
// history reflects it with the exact posted amounts at every step -
// Stage 7's defining vertical-slice financial flow.
func TestCasinoPlay_WagerWinRollbackHappyPath(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	// Wager.
	wagerResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(1_000))
	if wagerResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the wager, got %d", wagerResp.StatusCode)
	}
	var wagerOut map[string]any
	decodeBody(t, wagerResp, &wagerOut)
	if wagerOut["outcome"] != "succeeded" {
		t.Fatalf("expected outcome=succeeded for the wager, got %+v", wagerOut)
	}
	betProviderTxID, _ := wagerOut["provider_tx_id"].(string)
	if betProviderTxID == "" {
		t.Fatal("expected a non-empty provider_tx_id on the wager response")
	}

	// History after the wager: exactly 1 round, status wagered, bet_amount 1000.
	histResp := getJSON(t, srv, "/v1/me/casino/rounds", player.Tokens.AccessToken)
	if histResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing own rounds, got %d", histResp.StatusCode)
	}
	var histPage pagedResponse[roundResponse]
	decodeBody(t, histResp, &histPage)
	if histPage.Total != 1 || len(histPage.Items) != 1 {
		t.Fatalf("expected exactly 1 round after the wager, got total=%d items=%d", histPage.Total, len(histPage.Items))
	}
	if histPage.Items[0].Status != "wagered" {
		t.Fatalf("expected status=wagered, got %q", histPage.Items[0].Status)
	}
	if histPage.Items[0].BetAmount == nil || *histPage.Items[0].BetAmount != 1_000 {
		t.Fatalf("expected bet_amount=1000, got %+v", histPage.Items[0].BetAmount)
	}

	// Win.
	winResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/win", player.Tokens.AccessToken, winBody(2_500))
	if winResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the win, got %d", winResp.StatusCode)
	}
	var winOut map[string]any
	decodeBody(t, winResp, &winOut)
	if winOut["outcome"] != "succeeded" {
		t.Fatalf("expected outcome=succeeded for the win, got %+v", winOut)
	}

	histResp = getJSON(t, srv, "/v1/me/casino/rounds", player.Tokens.AccessToken)
	decodeBody(t, histResp, &histPage)
	if histPage.Items[0].Status != "won" {
		t.Fatalf("expected status=won after the win, got %q", histPage.Items[0].Status)
	}
	if histPage.Items[0].WinAmount == nil || *histPage.Items[0].WinAmount != 2_500 {
		t.Fatalf("expected win_amount=2500, got %+v", histPage.Items[0].WinAmount)
	}

	// Rollback the bet (the compensating-entry model, never a mutation).
	rollbackResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": betProviderTxID})
	if rollbackResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the rollback, got %d", rollbackResp.StatusCode)
	}
	var rollbackOut map[string]any
	decodeBody(t, rollbackResp, &rollbackOut)
	if rollbackOut["outcome"] != "succeeded" {
		t.Fatalf("expected outcome=succeeded for the rollback, got %+v", rollbackOut)
	}

	histResp = getJSON(t, srv, "/v1/me/casino/rounds", player.Tokens.AccessToken)
	decodeBody(t, histResp, &histPage)
	if histPage.Items[0].Status != "rolled_back" {
		t.Fatalf("expected status=rolled_back after the rollback, got %q", histPage.Items[0].Status)
	}
	if histPage.Items[0].RollbackAmount == nil || *histPage.Items[0].RollbackAmount != 1_000 {
		t.Fatalf("expected rollback_amount=1000 (the bet's own stake), got %+v", histPage.Items[0].RollbackAmount)
	}

	// A duplicate rollback of the same original tx must be rejected
	// (idempotent/no double reversal), never silently accepted again.
	dupResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": betProviderTxID})
	if dupResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a duplicate rollback of the same original tx, got %d", dupResp.StatusCode)
	}
}

// TestCasinoPlay_WagerRetryWithSameIdempotencyKeyIsIdempotent proves a
// client retry (e.g. after a lost response) never posts a second
// financial effect - ledger-finance/security review finding: minting a
// fresh provider_tx_id per call would make a retry a genuinely new
// transaction, defeating the entire purpose of an idempotency key
// (exactly the Stage 6.1 B2C bet-slip/deposit finding, applied here).
func TestCasinoPlay_WagerRetryWithSameIdempotencyKeyIsIdempotent(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	body := wagerBody(1_000)
	first := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, body)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the first wager, got %d", first.StatusCode)
	}
	var firstOut map[string]any
	decodeBody(t, first, &firstOut)

	// Retry with the IDENTICAL body (same idempotency_key) - simulates a
	// client that never saw the first response.
	second := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, body)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the retried wager, got %d", second.StatusCode)
	}
	var secondOut map[string]any
	decodeBody(t, second, &secondOut)

	if firstOut["provider_tx_id"] != secondOut["provider_tx_id"] {
		t.Fatalf("expected the retry to derive the SAME provider_tx_id, got %v vs %v", firstOut["provider_tx_id"], secondOut["provider_tx_id"])
	}
	if firstOut["ledger_transaction_id"] != secondOut["ledger_transaction_id"] {
		t.Fatalf("expected the retry to resolve the SAME ledger_transaction_id, got %v vs %v", firstOut["ledger_transaction_id"], secondOut["ledger_transaction_id"])
	}

	balance := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balance != 9_000 {
		t.Fatalf("expected the stake to be debited exactly ONCE (cash_balance=9000), got %d", balance)
	}
	if n := countLedgerTransactions(t, pool, tenant.ID, " AND transaction_type = 'casino_bet'"); n != 1 {
		t.Fatalf("expected exactly 1 casino_bet ledger transaction after a retried wager, got %d", n)
	}
}

// TestCasinoPlay_CrossPlayerSessionDenied proves player B cannot wager,
// win, or roll back against player A's own launch session, even within
// the same tenant/brand - the exact cross-player integrity property
// Stage 7 §2's brand-pinning fix and §12's callback-security requirement
// both protect, now exercised end to end over HTTP.
func TestCasinoPlay_CrossPlayerSessionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
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

	launched := mustLaunchCasinoGame(t, srv, playerA.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", playerB.Tokens.AccessToken, wagerBody(500))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for player B wagering on player A's own session, got %d", resp.StatusCode)
	}
	resp2 := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/win", playerB.Tokens.AccessToken, winBody(500))
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for player B settling a win on player A's own session, got %d", resp2.StatusCode)
	}
	resp3 := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", playerB.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": "forged"})
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for player B rolling back player A's own session, got %d", resp3.StatusCode)
	}

	// Player A's own wager still succeeds - proving the 404s above are a
	// genuine ownership check, not a broken session.
	okResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", playerA.Tokens.AccessToken, wagerBody(500))
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for player A's own wager on their own session, got %d", okResp.StatusCode)
	}
}

// TestCasinoPlay_RollbackCannotTargetAnotherPlayersTransaction is the
// direct regression test for the Stage 7 review-round P0 (architect,
// security, ledger-finance, database/RLS all independently reproduced
// this live over HTTP): player B, using B's OWN launch session (so
// resolvePlayerOwnedSession's ownership check alone would pass), names
// player A's own bet provider_tx_id in a rollback request. Before the fix
// (requireRollbackTargetOwnedByRound), this reversed player A's stake
// debit, crediting A's wallet with no relationship between B's session and
// A's round. Byte-for-byte reproduction of the reviews' own exploit,
// turned into a permanent regression test.
func TestCasinoPlay_RollbackCannotTargetAnotherPlayersTransaction(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
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

	// A wagers and wins.
	wagerA := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedA.SessionID+"/wager", playerA.Tokens.AccessToken, wagerBody(1_000))
	if wagerA.StatusCode != http.StatusOK {
		t.Fatalf("expected player A's wager to succeed, got %d", wagerA.StatusCode)
	}
	var wagerAOut map[string]any
	decodeBody(t, wagerA, &wagerAOut)
	aBetProviderTxID, _ := wagerAOut["provider_tx_id"].(string)

	winA := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedA.SessionID+"/win", playerA.Tokens.AccessToken, winBody(2_500))
	if winA.StatusCode != http.StatusOK {
		t.Fatalf("expected player A's win to succeed, got %d", winA.StatusCode)
	}
	var winAOut map[string]any
	decodeBody(t, winA, &winAOut)
	aWinProviderTxID, _ := winAOut["provider_tx_id"].(string)

	balanceABefore := walletCashBalance(t, srv, playerA.Tokens.AccessToken)

	// B wagers on B's OWN session (a genuine session B owns), then attempts
	// to roll back A's WIN by naming A's provider_tx_id.
	wagerB := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedB.SessionID+"/wager", playerB.Tokens.AccessToken, wagerBody(500))
	if wagerB.StatusCode != http.StatusOK {
		t.Fatalf("expected player B's own wager to succeed, got %d", wagerB.StatusCode)
	}

	crossRollback := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedB.SessionID+"/rollback", playerB.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": aWinProviderTxID})
	if crossRollback.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for player B naming player A's win in a rollback on B's OWN session, got %d", crossRollback.StatusCode)
	}
	crossRollbackBet := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedB.SessionID+"/rollback", playerB.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": aBetProviderTxID})
	if crossRollbackBet.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for player B naming player A's bet in a rollback on B's OWN session, got %d", crossRollbackBet.StatusCode)
	}

	balanceAAfter := walletCashBalance(t, srv, playerA.Tokens.AccessToken)
	if balanceAAfter != balanceABefore {
		t.Fatalf("expected player A's balance to be UNCHANGED by B's cross-player rollback attempts, was %d now %d", balanceABefore, balanceAAfter)
	}
}

// TestCasinoPlay_RollbackCannotTargetAnotherRoundOfTheSamePlayer proves
// the fix is scoped by ROUND, not merely by player: the SAME player
// cannot use one launch session (round) to roll back a transaction that
// belongs to a DIFFERENT one of their own rounds.
func TestCasinoPlay_RollbackCannotTargetAnotherRoundOfTheSamePlayer(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	round1 := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	round2 := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	wager1 := postJSON(t, srv, "/v1/me/casino/sessions/"+round1.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(1_000))
	if wager1.StatusCode != http.StatusOK {
		t.Fatalf("expected round 1's wager to succeed, got %d", wager1.StatusCode)
	}
	var wager1Out map[string]any
	decodeBody(t, wager1, &wager1Out)
	round1BetProviderTxID, _ := wager1Out["provider_tx_id"].(string)

	wager2 := postJSON(t, srv, "/v1/me/casino/sessions/"+round2.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(500))
	if wager2.StatusCode != http.StatusOK {
		t.Fatalf("expected round 2's wager to succeed, got %d", wager2.StatusCode)
	}

	// From round 2's own session, attempt to roll back round 1's bet.
	crossRoundRollback := postJSON(t, srv, "/v1/me/casino/sessions/"+round2.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": round1BetProviderTxID})
	if crossRoundRollback.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 rolling back round 1's bet from round 2's session, got %d", crossRoundRollback.StatusCode)
	}

	// Round 1's own rollback of its own bet still works.
	ownRollback := postJSON(t, srv, "/v1/me/casino/sessions/"+round1.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": round1BetProviderTxID})
	if ownRollback.StatusCode != http.StatusOK {
		t.Fatalf("expected round 1's own rollback of its own bet to succeed, got %d", ownRollback.StatusCode)
	}
}

// TestCasinoPlay_RollbackForgedProviderTxIDRejectedWithoutTombstone proves
// the fix also closes the tombstone-poisoning denial-of-service every
// review reproduced: a rollback naming a provider_tx_id that was never
// posted at all must be rejected BEFORE reaching postRollback's own
// tombstone-writing path, since a player-written tombstone would
// permanently occupy that reference in the shared (tenant_id, provider_id,
// provider_tx_id) unique index, later making a genuine bet/win that
// happens to mint the exact same reference fail outright with no possible
// recovery (the ledger is append-only).
func TestCasinoPlay_RollbackForgedProviderTxIDRejectedWithoutTombstone(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": "never-issued-reference-12345"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a rollback naming a never-issued provider_tx_id, got %d", resp.StatusCode)
	}
	if n := countLedgerTransactions(t, pool, tenant.ID, " AND transaction_type = 'tombstone'"); n != 0 {
		t.Fatalf("expected ZERO tombstone rows from a rejected forged rollback (would permanently poison that reference), got %d", n)
	}
}

// TestCasinoPlay_RollbackRejectedOnDemoSession proves a demo-mode session
// cannot drive a rollback either (wager/win already had this check;
// rollback was the one endpoint review found missing it).
func TestCasinoPlay_RollbackRejectedOnDemoSession(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "demo")

	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": "irrelevant"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a rollback against a demo-mode session, got %d", resp.StatusCode)
	}
}

// TestCasinoPlay_DemoModeSessionRejected proves a demo-mode launch session
// can never post a real financial effect via the wager endpoint.
func TestCasinoPlay_DemoModeSessionRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "demo")

	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(500))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a wager against a demo-mode session, got %d", resp.StatusCode)
	}
	if n := countLedgerTransactions(t, pool, tenant.ID, ""); n != 0 {
		t.Fatalf("expected zero ledger effect from a rejected demo-mode wager, got %d rows", n)
	}
}

// TestCasinoPlay_AmountAboveMaximumRejected proves the play-simulation
// amount cap (defense in depth: even gated to non-production, a player
// declares their own win amount with no real game outcome behind it).
func TestCasinoPlay_AmountAboveMaximumRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/win", player.Tokens.AccessToken, winBody(1_000_000_000))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a win_amount above the simulation cap, got %d", resp.StatusCode)
	}
	if n := countLedgerTransactions(t, pool, tenant.ID, ""); n != 0 {
		t.Fatalf("expected zero ledger effect from a rejected over-cap win, got %d rows", n)
	}
}

// TestListMyCasinoRounds_CrossPlayerIsolation proves a player's own round
// history never exposes another player's rounds, even within the same
// tenant/brand - database/RLS review finding: this endpoint's entire
// authorization boundary is one application-level WHERE clause
// (ledger_transactions carries no player-scope RLS policy at all), so it
// deserves its own explicit regression test rather than relying on the
// financial-flow test's own single-player assertions.
func TestListMyCasinoRounds_CrossPlayerIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	playerA := mustRegisterPlayer(t, srv, brand.Slug)
	playerB := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, playerA.ID)
	mustActivatePlayer(t, pool, tenant.ID, playerB.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, playerA.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launchedA := mustLaunchCasinoGame(t, srv, playerA.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	if resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedA.SessionID+"/wager", playerA.Tokens.AccessToken, wagerBody(500)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected player A's wager to succeed, got %d", resp.StatusCode)
	}

	// Player B has placed NO wager - their own history must be empty,
	// never showing player A's round.
	resp := getJSON(t, srv, "/v1/me/casino/rounds", playerB.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var pageB pagedResponse[roundResponse]
	decodeBody(t, resp, &pageB)
	if pageB.Total != 0 || len(pageB.Items) != 0 {
		t.Fatalf("expected player B to see zero rounds (never player A's), got total=%d items=%d", pageB.Total, len(pageB.Items))
	}

	// Player A's own history does show it.
	respA := getJSON(t, srv, "/v1/me/casino/rounds", playerA.Tokens.AccessToken)
	var pageA pagedResponse[roundResponse]
	decodeBody(t, respA, &pageA)
	if pageA.Total != 1 {
		t.Fatalf("expected player A to see exactly 1 round, got %d", pageA.Total)
	}
}

// --- Back Office round visibility ---

func TestAdminListCasinoRounds_RoleWithoutPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "a-decent-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/casino/rounds", tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a role with no casino_transaction:read grant, got %d", resp.StatusCode)
	}
}

// TestAdminListCasinoRounds_TenantAdminAuthorizedAndCrossTenantIsolated
// mirrors TestAdminListBets_TenantAdminAuthorizedAndCrossTenantIsolated's
// identical shape for casino rounds.
func TestAdminListCasinoRounds_TenantAdminAuthorizedAndCrossTenantIsolated(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)

	playerA := mustRegisterPlayer(t, srv, brandA.Slug)
	playerB := mustRegisterPlayer(t, srv, brandB.Slug)
	mustActivatePlayer(t, pool, tenantA.ID, playerA.ID)
	mustActivatePlayer(t, pool, tenantB.ID, playerB.ID)
	fundWallet(t, pool, tenantA.ID, brandA.ID, playerA.ID, "EUR", 10_000)
	fundWallet(t, pool, tenantB.ID, brandB.ID, playerB.ID, "EUR", 10_000)

	gameA := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenantA.ID, gameA.ID)
	mustEnableCasinoCapability(t, srv, pool, tenantA)
	gameB := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenantB.ID, gameB.ID)
	mustEnableCasinoCapability(t, srv, pool, tenantB)

	launchedA := mustLaunchCasinoGame(t, srv, playerA.Tokens.AccessToken, gameA.ID.String(), "EUR", "real")
	launchedB := mustLaunchCasinoGame(t, srv, playerB.Tokens.AccessToken, gameB.ID.String(), "EUR", "real")

	if resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedA.SessionID+"/wager", playerA.Tokens.AccessToken, wagerBody(500)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected player A's wager to succeed, got %d", resp.StatusCode)
	}
	if resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedB.SessionID+"/wager", playerB.Tokens.AccessToken, wagerBody(700)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected player B's wager to succeed, got %d", resp.StatusCode)
	}

	staffA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokensA := mustLoginStaff(t, srv, tenantA.Slug, staffA.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/casino/rounds", tokensA.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a tenant_admin with casino_transaction:read, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminRoundResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected tenant A's admin to see exactly its own tenant's 1 round, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].PlayerAccountID != playerA.ID.String() {
		t.Fatalf("expected the visible round to belong to player A, got player_account_id=%s", page.Items[0].PlayerAccountID)
	}
	if page.Items[0].BrandID != brandA.ID.String() {
		t.Fatalf("expected the visible round's brand_id to be player A's own brand %s, got %s", brandA.ID, page.Items[0].BrandID)
	}
	if page.Items[0].DecimalExponent != 2 {
		t.Fatalf("expected decimal_exponent=2 for EUR, got %d", page.Items[0].DecimalExponent)
	}
	if page.Items[0].BetAmount == nil || *page.Items[0].BetAmount != 500 {
		t.Fatalf("expected bet_amount=500, got %+v", page.Items[0].BetAmount)
	}
	if len(page.Items[0].LedgerTransactionIDs) != 1 {
		t.Fatalf("expected exactly 1 audit-linkage ledger_transaction_id, got %v", page.Items[0].LedgerTransactionIDs)
	}
}
