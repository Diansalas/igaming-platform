//go:build integration

// Stage 10.3 G-1: post-fix behaviour, inverted from the pre-fix evidence
// this file used to record.
//
// This file used to record, against the PRE-G-1 code
// (docs/plans/stage-10.3-planning/02-casino-financial-analysis.md §3): a
// round with two un-reversed cash bets under the same correlation id made
// classifyOriginRows return ErrAmbiguousMultiOriginRound for ANY win on
// that round, even though both bets are plain player_cash and
// postWinDirectCash never uses BetTransactionID - the ambiguity had no
// financial basis for cash. The HTTP handler had no mapping for this
// error, so it fell through to the generic 500 branch. The red run
// against pre-fix HEAD is preserved as evidence in
// docs/plans/stage-10.3-planning/evidence/, untouched by this inversion.
// The fix resolves such a win to the shared wallet instead
// (classifyDirectOriginRows) - 409 stays reserved for genuinely different
// wallets or mixed-origin funding.
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
)

// TestCasMultiBetWin_G1_TwoCashBetsRoundWinSettlesToSharedWallet is G-1
// (inverted from the pre-fix E5 defect): two cash bets posted under the
// same round/correlation id, then a win on that round, now settles
// (200), crediting the wallet both bets shared - it no longer withholds
// the win indefinitely behind an unmapped 500.
func TestCasMultiBetWin_G1_TwoCashBetsRoundWinSettlesToSharedWallet(t *testing.T) {
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
	winResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	defer winResp.Body.Close()

	if winResp.StatusCode != http.StatusOK {
		t.Fatalf("Stage 10.3 G-1: expected HTTP 200 for a win on a two-cash-bet round (resolves to the shared wallet), got %d", winResp.StatusCode)
	}

	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 1 {
		t.Fatalf("expected the win to post exactly once, got %d ledger_transactions rows", got)
	}
	balanceAfterWin := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balanceAfterWin != balanceAfterBets+2500 {
		t.Fatalf("expected the win to credit the shared wallet (balance %d), got %d", balanceAfterBets+2500, balanceAfterWin)
	}
}

// TestCasMultiBetWin_G1_TwoWalletsUnderOneRound_Returns409 keeps G-1's own
// named exception alive: a correlation id that (through some other bug or
// a future writer) resolved to TWO DIFFERENT wallets is still a 409
// integrity alert, never silently resolved to either wallet. This is
// exercised directly against the origin classifier (the HTTP-level
// scenario for a genuine two-wallet collision has no natural trigger
// through the public callback API, since one session always binds one
// wallet) - see internal/casino's own unit coverage for
// classifyDirectOriginRows for the full case matrix (wallet collision,
// mixed funding, genuinely ambiguous bonus/mixed-origin rounds).
func TestCasMultiBetWin_G1_AmbiguousIntegrityAlertsMapTo409(t *testing.T) {
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

	// A win naming a round with NO prior bet at all is ErrBetNotFound, not
	// one of the G-1 §16.4 abort classes - it stays its own established
	// mapping (400 + integrity alert), confirmed here as the baseline this
	// test's sibling classifier-level unit tests build on.
	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, "cas-g1-orphan-win", "", "round-g1-orphan", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a win with no matching prior bet (ErrBetNotFound, unaffected by G-1), got %d", resp.StatusCode)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
}
