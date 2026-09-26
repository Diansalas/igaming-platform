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
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
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

// casinoTestRoundCorrelationID mirrors internal/casino's own UNEXPORTED
// roundCorrelationID (orchestrator.go) byte-for-byte - deterministic
// uuid.NewSHA1 over "tenantID:providerID:roundID" - so a test seeding
// ledger_transactions rows DIRECTLY (bypassing postBet, to reach a §16.4
// abort shape postBet's own cash-only bet path (G-6) cannot produce) can
// still land its rows under the EXACT correlation id the real win webhook
// call will resolve against. Kept as a small, duplicated, well-commented
// formula rather than exporting casino's internal function purely for
// tests - the same "per-package test helper, not a shared test import"
// convention this codebase already follows (see
// internal/casino/lockorder_harness_test.go's own file comment).
func casinoTestRoundCorrelationID(tenantID uuid.UUID, providerID, roundID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(tenantID.String()+":"+providerID+":"+roundID))
}

// seedDirectCasinoBetLeg posts a real casino_bet ledger transaction
// DIRECTLY via internal/ledger (never through postBet), debiting exactly
// one direct-origin account (player_cash or player_bonus) on walletID and
// crediting house_gaming, under roundID's own correlation id. This is how
// the G-1 tests below reach the §16.4 abort shapes (wallet collision,
// mixed cash+bonus funding, two-or-more bare bonus debits) that postBet's
// own cash-only bet path (G-6) cannot produce through the public API - the
// ledger-finance gate review's own suggestion ("seeded directly") for
// exercising these mappings at HTTP level: the SEED is direct, but the WIN
// itself still goes through the real webhook HTTP handler below.
func seedDirectCasinoBetLeg(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID, providerTxID, roundID string, walletID uuid.UUID, assetCode string, accountType ledger.AccountType, amount int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
			ledger.AccountSpec{WalletID: &walletID, AccountType: accountType, AssetCode: assetCode},
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: assetCode},
		)
		if err != nil {
			return err
		}
		var bonusCost *ledger.BonusCostAttribution
		if accountType == ledger.AccountPlayerBonus {
			bonusCost = &ledger.BonusCostAttribution{Funding: ledger.FundingOperator}
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: tenantID, TransactionType: ledger.TxCasinoBet,
			IdempotencyKey: providerID + ":" + providerTxID,
			ProviderID:     &providerID, ProviderTxID: &providerTxID,
			CorrelationID: casinoTestRoundCorrelationID(tenantID, providerID, roundID),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[0], Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: accounts[1], Direction: ledger.Credit, Amount: amount},
			},
			BonusCost: bonusCost,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed direct casino bet leg %s: %v", providerTxID, err)
	}
}

// TestCasMultiBetWin_G1_HTTPLevel409Mappings is the binding test plan's
// (04-review-qa.md §2, condition 1/C3) "real HTTP-level 409 test for the
// ambiguity, collision and mixed-origin mappings" - the actual win
// callback for each subtest goes through the real webhook HTTP handler,
// against directly-seeded bet legs that reproduce each §16.4 abort shape.
func TestCasMultiBetWin_G1_HTTPLevel409Mappings(t *testing.T) {
	t.Run("wallet_collision", func(t *testing.T) {
		pool, issuer := testEnv(t)
		orchestrator, mock := newMockCasinoOrchestrator()
		srv := newCasinoTestServer(t, pool, issuer, orchestrator)

		tenant := mustCreateTenant(t, pool)
		mustEnableCasinoCapability(t, srv, pool, tenant)
		brand := mustCreateBrand(t, pool, tenant)
		playerA := mustRegisterPlayer(t, srv, brand.Slug)
		mustActivatePlayer(t, pool, tenant.ID, playerA.ID)
		walletA := fundWallet(t, pool, tenant.ID, brand.ID, playerA.ID, "EUR", 10_000)
		playerB := mustRegisterPlayer(t, srv, brand.Slug)
		mustActivatePlayer(t, pool, tenant.ID, playerB.ID)
		walletB := fundWallet(t, pool, tenant.ID, brand.ID, playerB.ID, "EUR", 10_000)

		const roundID = "round-g1-collision"
		seedDirectCasinoBetLeg(t, pool, tenant.ID, "mock-casino", "cas-g1-coll-bet-a", roundID, walletA.ID, "EUR", ledger.AccountPlayerCash, 1000)
		seedDirectCasinoBetLeg(t, pool, tenant.ID, "mock-casino", "cas-g1-coll-bet-b", roundID, walletB.ID, "EUR", ledger.AccountPlayerCash, 500)

		before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
		winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, "cas-g1-coll-win", "", roundID, "game-1",
			1000, "EUR", casino.OutcomeSucceeded, "", playerA.ID, uuid.Nil)
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("wallet collision: expected 409, got %d", resp.StatusCode)
		}
		noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
	})

	t.Run("mixed_funding", func(t *testing.T) {
		pool, issuer := testEnv(t)
		orchestrator, mock := newMockCasinoOrchestrator()
		srv := newCasinoTestServer(t, pool, issuer, orchestrator)

		tenant := mustCreateTenant(t, pool)
		mustEnableCasinoCapability(t, srv, pool, tenant)
		brand := mustCreateBrand(t, pool, tenant)
		player := mustRegisterPlayer(t, srv, brand.Slug)
		mustActivatePlayer(t, pool, tenant.ID, player.ID)
		wallet := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

		const roundID = "round-g1-mixed"
		seedDirectCasinoBetLeg(t, pool, tenant.ID, "mock-casino", "cas-g1-mixed-bet-cash", roundID, wallet.ID, "EUR", ledger.AccountPlayerCash, 1000)
		seedDirectCasinoBetLeg(t, pool, tenant.ID, "mock-casino", "cas-g1-mixed-bet-bonus", roundID, wallet.ID, "EUR", ledger.AccountPlayerBonus, 500)

		before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
		winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, "cas-g1-mixed-win", "", roundID, "game-1",
			1000, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("mixed funding: expected 409, got %d", resp.StatusCode)
		}
		noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
	})

	t.Run("ambiguous_bare_bonus", func(t *testing.T) {
		pool, issuer := testEnv(t)
		orchestrator, mock := newMockCasinoOrchestrator()
		srv := newCasinoTestServer(t, pool, issuer, orchestrator)

		tenant := mustCreateTenant(t, pool)
		mustEnableCasinoCapability(t, srv, pool, tenant)
		brand := mustCreateBrand(t, pool, tenant)
		player := mustRegisterPlayer(t, srv, brand.Slug)
		mustActivatePlayer(t, pool, tenant.ID, player.ID)
		wallet := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

		const roundID = "round-g1-ambiguous-bonus"
		seedDirectCasinoBetLeg(t, pool, tenant.ID, "mock-casino", "cas-g1-ambig-bet-1", roundID, wallet.ID, "EUR", ledger.AccountPlayerBonus, 500)
		seedDirectCasinoBetLeg(t, pool, tenant.ID, "mock-casino", "cas-g1-ambig-bet-2", roundID, wallet.ID, "EUR", ledger.AccountPlayerBonus, 500)

		before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
		winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, "cas-g1-ambig-win", "", roundID, "game-1",
			1000, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("ambiguous bare-bonus round: expected 409, got %d", resp.StatusCode)
		}
		noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
	})
}

// TestCasMultiBetWin_OrphanWin_MapsTo400NotG1 replaces the mislabelled
// TestCasMultiBetWin_G1_AmbiguousIntegrityAlertsMapTo409 (gate 10.3-W1
// code review finding #12): it never exercised a 409 at all - it asserts
// the PRE-EXISTING orphan-win 400 mapping (ErrBetNotFound, a round with no
// prior bet whatsoever), which is unaffected by G-1 and was never one of
// the §16.4 abort classes G-1 maps to 409
// (TestCasMultiBetWin_G1_HTTPLevel409Mappings above is the real 409
// coverage). Kept as its own test because it is still a useful baseline:
// it proves a genuinely bet-less round is NOT accidentally swept into the
// G-1 "resolve to shared wallet" fix.
func TestCasMultiBetWin_OrphanWin_MapsTo400NotG1(t *testing.T) {
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
