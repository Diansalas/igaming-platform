//go:build integration

// Stage 10.3 gate 10.3-W1 ledger-finance condition C5: the eight missing
// §1.11 cases the binding test plan names. Each subtest/test below is
// named after the F-5 finding's own bullet list.
package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// --- C5 case 1: active capability with bets off (S-nobet) rejects a bet ---

func TestCasCapRollback1_C5_SnobetActiveCapabilityRejectsBet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-c5-snobet-pw")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-c5-snobet-pw")

	snobetBody := validCasinoCapabilityBody()
	snobetBody["supports_bet"] = false
	resp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, snobetBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 registering an S-nobet (active, supports_bet=false) capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	// LaunchGame itself requires supports_bet for a REAL-mode launch
	// (Item 6/code-review #3 below) - so a real session cannot be minted
	// under S-nobet. Use demo mode to obtain a session id shape, then
	// hand-craft a real-looking bet payload against a directly-created
	// REAL launch session instead, isolating this test to postBet's own
	// S-nobet gate rather than LaunchGame's.
	sessionID := mustSeedRealCasinoLaunchSession(t, pool, tenant.ID, brand.ID, player.ID, "mock-casino", game.ID, game.ProviderGameID, "EUR")

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-c5-snobet-bet", "", "round-c5-snobet", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer betResp.Body.Close()
	if betResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("S-nobet: expected 503 for a new bet when the capability is active but supports_bet=false, got %d", betResp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, "cas-c5-snobet-bet"); got != 0 {
		t.Fatalf("expected no ledger effect for the rejected S-nobet bet, got %d rows", got)
	}
}

// mustSeedRealCasinoLaunchSession creates a REAL-mode casino_launch_sessions
// row directly (bypassing LaunchGame, whose own supports_bet requirement
// would prevent minting a real session under S-nobet or S-off) - the
// minimum needed for postBet's OWN session/capability checks to run.
func mustSeedRealCasinoLaunchSession(t *testing.T, pool *db.Pool, tenantID, brandID, playerAccountID uuid.UUID, providerID string, gameID uuid.UUID, providerGameID, assetCode string) uuid.UUID {
	t.Helper()
	var sessionID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		wl, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, playerAccountID, assetCode)
		if err != nil {
			return err
		}
		session, _, err := casino.CreateLaunchSession(ctx, tx, casino.CreateLaunchSessionParams{
			TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerAccountID, WalletID: wl.ID,
			GameID: gameID, ProviderID: providerID, ProviderGameID: providerGameID,
			AssetCode: assetCode, Mode: casino.ModeReal, JurisdictionCode: "",
		})
		if err != nil {
			return err
		}
		sessionID = session.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed real casino launch session: %v", err)
	}
	return sessionID
}

// --- C5 case 2: a brand-B capability does not authorize a brand-A bet ---

func TestCasCapRollback1_C5_BrandBCapabilityDoesNotAuthorizeBrandABet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenant)
	brandB := mustCreateBrand(t, pool, tenant)

	// Only brand B has a capability row.
	provider := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, werr := casino.WriteCapability(ctx, tx, provider, tenant.ID, &brandB.ID, casino.CapabilityConfig{
			SupportsCatalogue: true, SupportsLaunch: true, SupportsBalance: true,
			SupportsBet: true, SupportsWin: true, SupportsRollback: true,
			SupportedAssets: []string{"EUR"}, SupportedGameTypes: []string{"slot", "table", "live"},
			Priority: 100, Status: casino.CapabilityActive,
		})
		return werr
	})
	if err != nil {
		t.Fatalf("write brand-B capability: %v", err)
	}

	player := mustRegisterPlayer(t, srv, brandA.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brandA.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	sessionID := mustSeedRealCasinoLaunchSession(t, pool, tenant.ID, brandA.ID, player.ID, "mock-casino", game.ID, game.ProviderGameID, "EUR")

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-c5-brandb-bet", "", "round-c5-brandb", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503: brand B's capability must not authorize a brand-A session's bet, got %d", resp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, "cas-c5-brandb-bet"); got != 0 {
		t.Fatalf("expected no ledger effect, got %d rows", got)
	}
}

// --- C5 case 3: capability disabled while a bet is in flight ---

// TestCasCapRollback1_C5_CapabilityDisabledConcurrentlyWithInFlightBet
// races a bet delivery against an admin request that disables the
// capability - proving the outcome is always ONE of {the bet's own
// LoadCapability read observed it still active, so it posted normally} or
// {it observed disabled, so it 503s with zero effect} - never a torn or
// undefined state.
func TestCasCapRollback1_C5_CapabilityDisabledConcurrentlyWithInFlightBet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-c5-race-pw")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-c5-race-pw")
	enableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling the capability, got %d", enableResp.StatusCode)
	}
	enableResp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-c5-race-bet", "", "round-c5-race", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)

	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"

	betDone := make(chan int, 1)
	go func() {
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
		betDone <- resp.StatusCode
		resp.Body.Close()
	}()
	disableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	disableResp.Body.Close()
	betStatus := <-betDone

	if betStatus != http.StatusOK && betStatus != http.StatusServiceUnavailable {
		t.Fatalf("expected the raced bet to be 200 (posted, capability read before the disable committed) or 503 (read after), got %d", betStatus)
	}
	got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, "cas-c5-race-bet")
	if betStatus == http.StatusOK && got != 1 {
		t.Fatalf("bet reported 200 but posted %d rows, want 1", got)
	}
	if betStatus == http.StatusServiceUnavailable && got != 0 {
		t.Fatalf("bet reported 503 but posted %d rows, want 0", got)
	}
}

// --- C5 case 4: bet 503 -> re-enable -> retry posts exactly once ---

func TestCasCapRollback1_C5_Bet503ThenReenableThenRetryPostsOnce(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-c5-retry-pw")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-c5-retry-pw")

	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	disableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 registering the disabled capability, got %d", disableResp.StatusCode)
	}
	disableResp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	sessionID := mustSeedRealCasinoLaunchSession(t, pool, tenant.ID, brand.ID, player.ID, "mock-casino", game.ID, game.ProviderGameID, "EUR")

	const betTxID = "cas-c5-retry-bet"
	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betTxID, "", "round-c5-retry", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)

	firstResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	if firstResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while disabled, got %d", firstResp.StatusCode)
	}
	firstResp.Body.Close()

	enableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 re-enabling the capability, got %d", enableResp.StatusCode)
	}
	enableResp.Body.Close()

	retryResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the retry after re-enabling, got %d", retryResp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, betTxID); got != 1 {
		t.Fatalf("expected exactly 1 posting despite the 503-then-retry, got %d", got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != 9000 {
		t.Fatalf("expected exactly one bet's worth taken (9000), got %d", balance)
	}
}

// --- C5 case 5: E2 bet replay while disabled returns the original ---

func TestCasCapRollback1_C5_BetReplayWhileDisabledReturnsOriginal(t *testing.T) {
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

	const betTxID = "cas-c5-e2-disabled-bet"
	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betTxID, "", "round-c5-e2-disabled", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	firstResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting while enabled, got %d", firstResp.StatusCode)
	}
	firstResp.Body.Close()

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-c5-e2-pw")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-c5-e2-pw")
	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	disableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 disabling, got %d", disableResp.StatusCode)
	}
	disableResp.Body.Close()

	// E2 idempotency short-circuit runs BEFORE the capability gate - a
	// replay of an already-posted bet must return the ORIGINAL result even
	// though the capability is now disabled.
	replayResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (the original result) for a replay while disabled, got %d", replayResp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, betTxID); got != 1 {
		t.Fatalf("expected still exactly 1 posting, got %d", got)
	}
}

// --- C5 case 6: rollback of a posted win while disabled ---

func TestCasCapRollback1_C5_RollbackOfPostedWinWhileDisabled(t *testing.T) {
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

	const roundID = "round-c5-win-rollback"
	const betTxID = "cas-c5-win-rollback-bet"
	const winTxID = "cas-c5-win-rollback-win"
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betTxID, "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting the bet, got %d", betResp.StatusCode)
	}
	betResp.Body.Close()

	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
	winResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	if winResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting the win, got %d", winResp.StatusCode)
	}
	winResp.Body.Close()
	balanceAfterWin := walletCashBalance(t, srv, player.Tokens.AccessToken)

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-c5-winrb-pw")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-c5-winrb-pw")
	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	disableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 disabling, got %d", disableResp.StatusCode)
	}
	disableResp.Body.Close()

	rollbackOfWin := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, "cas-c5-win-rollback-rb", winTxID, roundID, "game-1",
		0, "EUR", "", "", player.ID, uuid.Nil)
	rbResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackOfWin)
	defer rbResp.Body.Close()
	if rbResp.StatusCode != http.StatusOK {
		t.Fatalf("Stage 10.3 CAS-CAP-ROLLBACK-1: expected 200 for a rollback of an ALREADY-POSTED win even while the capability is disabled, got %d", rbResp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 1 {
		t.Fatalf("expected the win's single row untouched (un-duplicated), got %d", got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != balanceAfterWin-2500 {
		t.Fatalf("expected the win's payout reversed (balance %d), got %d", balanceAfterWin-2500, balance)
	}
}

// --- C5 case 7: ledger-vs-projection reconciliation sweep is clean ---

func TestCasCapRollback1_C5_ReconciliationSweepClean(t *testing.T) {
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

	const roundID = "round-c5-recon"
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-c5-recon-bet", "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the bet, got %d", betResp.StatusCode)
	}
	betResp.Body.Close()

	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, "cas-c5-recon-win", "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
	winResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	if winResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the win, got %d", winResp.StatusCode)
	}
	winResp.Body.Close()

	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, "cas-c5-recon-rb-unseen", "cas-c5-recon-unseen-original",
		"round-c5-recon-unseen", "game-1", 0, "EUR", "", "", player.ID, uuid.Nil)
	rbResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	if rbResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the tombstoning rollback, got %d", rbResp.StatusCode)
	}
	rbResp.Body.Close()

	var run reconciliation.Run
	var mismatches []reconciliation.Mismatch
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		run, mismatches, rerr = reconciliation.RunLedgerVsProjection(ctx, tx, tenant.ID,
			time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		return rerr
	})
	if err != nil {
		t.Fatalf("run ledger-vs-projection reconciliation: %v", err)
	}
	if run.Status != reconciliation.StatusClean {
		t.Fatalf("expected a CLEAN reconciliation sweep after bet+win+tombstone, got status=%q mismatches=%+v", run.Status, mismatches)
	}
	if len(mismatches) != 0 {
		t.Fatalf("expected zero mismatches, got %+v", mismatches)
	}
}
