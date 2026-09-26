//go:build integration

// Stage 10.3 pre-fix evidence; inverted by the fix.
//
// Records G-1 from docs/plans/stage-10.3-planning/02-casino-financial-
// analysis.md §3: a round with two un-reversed cash bets under the same
// correlation id makes classifyOriginRows return
// ErrAmbiguousMultiOriginRound for ANY win on that round, even though
// both bets are plain player_cash and postWinDirectCash never uses
// BetTransactionID - the ambiguity has no financial basis for cash. The
// HTTP handler has no mapping for this error, so it falls through to the
// generic 500 branch. This test PASSES today because it demonstrates the
// defect (500, win never posted); the fix is expected to resolve such a
// win to the shared wallet instead (409 is reserved for genuinely
// different wallets/mixed funding).
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
)

// TestCasMultiBetWin_PreFix_TwoCashBetsRoundWinIs500 is E5 (G-1): two cash
// bets posted under the same round/correlation id, then a win on that
// round, returns HTTP 500 today via ErrAmbiguousMultiOriginRound - and the
// win is not posted (no ledger row for its provider_tx_id), the stakes
// from both bets stay debited.
func TestCasMultiBetWin_PreFix_TwoCashBetsRoundWinIs500(t *testing.T) {
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

	const roundID = "round-g1-multibet"
	const bet1TxID = "cas-g1-bet-1"
	const bet2TxID = "cas-g1-bet-2"
	const winTxID = "cas-g1-win-1"

	bet1 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, bet1TxID, "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp1 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the first cash bet on the round, got %d", resp1.StatusCode)
	}
	resp1.Body.Close()

	bet2 := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, bet2TxID, "", roundID, game.ProviderGameID,
		500, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp2 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", bet2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the second cash bet on the SAME round (platform already accepts multi-bet cash rounds), got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	balanceAfterBets := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balanceAfterBets != 8500 {
		t.Fatalf("expected 10000-1000-500=8500 after both stakes were taken, got %d", balanceAfterBets)
	}

	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
	winResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	defer winResp.Body.Close()

	if winResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PRE-FIX EVIDENCE: expected HTTP 500 for a win on a two-cash-bet round (ErrAmbiguousMultiOriginRound is unmapped, G-1), got %d - if this changed, invert this test rather than widen it", winResp.StatusCode)
	}

	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 0 {
		t.Fatalf("expected the win to NOT be posted (withheld indefinitely, per G-1's own framing), got %d ledger_transactions rows", got)
	}
	balanceAfterBlockedWin := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balanceAfterBlockedWin != balanceAfterBets {
		t.Fatalf("expected both stakes to stay debited and the win withheld (balance still %d), got %d", balanceAfterBets, balanceAfterBlockedWin)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
}
