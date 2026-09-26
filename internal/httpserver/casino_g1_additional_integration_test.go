//go:build integration

// Stage 10.3 G-1 additional cases (04-review-qa.md §4 W1c "G-1:" bullet,
// gate 10.3-W1 ledger-finance condition C3): the binding test plan names
// four G-1 cases beyond the normal-settlement and two-wallet-collision
// tests already in casino_multibet_win_prefix_test.go - `_Replay`,
// `_ConcurrentWins`, `_RollbackOneThenWin` (this file), and the HTTP-level
// 409 mappings (casino_multibet_win_prefix_test.go's
// TestCasMultiBetWin_G1_HTTPLevel409Mappings).
package httpserver

import (
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
)

// TestCasMultiBetWin_G1_Replay: a G-1 win (two cash bets sharing one
// wallet, one win) is redelivered with the IDENTICAL provider_tx_id after
// its first successful post - the ordinary sequential-replay idempotency
// guarantee, exercised specifically on the G-1 code path
// (classifyDirectOriginRows), not just the pre-G-1 single-bet path.
func TestCasMultiBetWin_G1_Replay(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	const roundID = "round-g1-replay"
	bet1 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-g1-replay-bet-1", "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	bet2 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-g1-replay-bet-2", "", roundID, game.ProviderGameID,
		500, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp1 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bet1, got %d", resp1.StatusCode)
	}
	resp1.Body.Close()
	resp2 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bet2, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	const winTxID = "cas-g1-replay-win"
	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)

	firstResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the first win delivery, got %d", firstResp.StatusCode)
	}
	var firstBody map[string]any
	decodeBody(t, firstResp, &firstBody)
	balanceAfterFirstWin := walletCashBalance(t, srv, player.Tokens.AccessToken)

	replayResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the replayed (identical) win delivery, got %d", replayResp.StatusCode)
	}
	var replayBody map[string]any
	decodeBody(t, replayResp, &replayBody)

	if firstBody["ledger_transaction_id"] != replayBody["ledger_transaction_id"] {
		t.Fatalf("expected the replay to return the SAME ledger_transaction_id, got first=%v replay=%v",
			firstBody["ledger_transaction_id"], replayBody["ledger_transaction_id"])
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 1 {
		t.Fatalf("expected exactly 1 ledger_transactions row for the win despite a replay, got %d", got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != balanceAfterFirstWin {
		t.Fatalf("expected the replay to be a no-op (balance unchanged at %d), got %d", balanceAfterFirstWin, balance)
	}
}

// TestCasMultiBetWin_G1_ConcurrentWins fires N concurrent deliveries of the
// IDENTICAL win callback (same provider_tx_id) against a G-1 two-cash-bet
// round, proving postWin's own round lock (`FOR UPDATE ... ORDER BY id` on
// the round's bet transactions) serializes the classifyDirectOriginRows
// resolution itself under concurrency, not merely the L0.1 delivery lock -
// exactly one win posts.
func TestCasMultiBetWin_G1_ConcurrentWins(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	const roundID = "round-g1-concurrent-wins"
	bet1 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-g1-cw-bet-1", "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	bet2 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-g1-cw-bet-2", "", roundID, game.ProviderGameID,
		500, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp1 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bet1, got %d", resp1.StatusCode)
	}
	resp1.Body.Close()
	resp2 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bet2, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	const winTxID = "cas-g1-cw-win"
	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		}()
	}
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("delivery %d: expected 200, got %d", i, status)
		}
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 1 {
		t.Fatalf("expected exactly 1 win ledger_transactions row despite %d concurrent identical G-1 win deliveries, got %d", n, got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != 10_000-1500+2500 {
		t.Fatalf("expected exactly one win's worth credited, got %d", balance)
	}
}

// TestCasMultiBetWin_G1_RollbackOneThenWin: a two-cash-bet round has ONE
// of its two bets rolled back before the round's win arrives - the
// ledger-finance gate review's own point (§1.2): "rollback-one-then-win
// resolves to the remaining bets" because classifyDirectOriginRows'
// caller (queryOriginRows) excludes any bet with
// `NOT EXISTS (reverses_transaction_id)`. The win must still settle
// (200), crediting the SAME wallet the surviving bet used.
func TestCasMultiBetWin_G1_RollbackOneThenWin(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	const roundID = "round-g1-rollback-then-win"
	const bet1TxID = "cas-g1-rtw-bet-1"
	const bet2TxID = "cas-g1-rtw-bet-2"
	bet1 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, bet1TxID, "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	bet2 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, bet2TxID, "", roundID, game.ProviderGameID,
		500, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp1 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bet1, got %d", resp1.StatusCode)
	}
	resp1.Body.Close()
	resp2 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bet2, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	rollbackOfBet2 := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, "cas-g1-rtw-rollback-bet2", bet2TxID, roundID, "game-1",
		0, "EUR", "", "", player.ID, uuid.Nil)
	rbResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackOfBet2)
	if rbResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 rolling back bet2, got %d", rbResp.StatusCode)
	}
	rbResp.Body.Close()

	balanceAfterRollback := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balanceAfterRollback != 10_000-1000 {
		t.Fatalf("expected only bet1's stake to remain taken (10000-1000=9000), got %d", balanceAfterRollback)
	}

	const winTxID = "cas-g1-rtw-win"
	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		1800, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
	winResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	defer winResp.Body.Close()
	if winResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200: the win must resolve to the SURVIVING bet's wallet after the OTHER bet in the round was rolled back, got %d", winResp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 1 {
		t.Fatalf("expected the win to post exactly once, got %d", got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != balanceAfterRollback+1800 {
		t.Fatalf("expected the win to credit the surviving bet's own wallet (balance %d), got %d", balanceAfterRollback+1800, balance)
	}
}
