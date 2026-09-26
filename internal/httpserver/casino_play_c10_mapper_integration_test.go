//go:build integration

// Stage 10.3 gate 10.3-W1 ledger-finance condition C10 / code review
// finding #6 (writeCasinoCallbackError): the player-facing play-simulation
// error mapper must mirror the public webhook mapper's ErrOriginalTombstoned
// (E10) and G-1 §16.4 abort-class 409 mappings, never falling through to
// the generic 500 branch these two cases used to reach on this route.
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// TestCasinoPlaySimulation_C10_OriginalTombstoned_MapsTo409 reproduces E10
// on the PLAYER-FACING simulated-win route: a rollback naming the win's own
// deterministic provider_tx_id tombstones it (via the real provider
// webhook - the simulation route's own rollback endpoint cannot target a
// never-seen reference, see requireRollbackTargetOwnedByRound), and the
// later simulated win attempt for that same reference must be 409, not the
// 500 it fell through to before C10.
func TestCasinoPlaySimulation_C10_OriginalTombstoned_MapsTo409(t *testing.T) {
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

	const idempotencyKey = "c10-tombstone-idem-1"
	winProviderTxID := deterministicSimulatedProviderTxID(sessionID, "win", idempotencyKey)

	// Tombstone the win's own future reference FIRST, via the real provider
	// webhook (a genuine rollback of a never-seen original).
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, "c10-rb-of-future-win", winProviderTxID,
		"round-c10-tombstone", game.ProviderGameID, 0, "EUR", "", "", player.ID, uuid.Nil)
	rbResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	if rbResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 tombstoning the never-seen win reference, got %d", rbResp.StatusCode)
	}
	rbResp.Body.Close()

	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+sessionID.String()+"/win", player.Tokens.AccessToken, map[string]any{
		"win_amount": 500, "idempotency_key": idempotencyKey,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("Stage 10.3 C10: expected 409 for a simulated win whose own reference is already tombstoned, got %d", resp.StatusCode)
	}
}

// TestCasinoPlaySimulation_C10_G1AbortClass_MapsTo409 reproduces a G-1
// §16.4 abort class (wallet collision) reachable from the simulated-win
// route's own round id (the launch session id) by seeding a second,
// different-wallet direct cash bet leg under that SAME correlation id
// directly via internal/ledger - the abort class the mapper must return
// 409 for, not 500.
func TestCasinoPlaySimulation_C10_G1AbortClass_MapsTo409(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	_ = mock

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	otherPlayer := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, otherPlayer.ID)
	otherWallet := fundWallet(t, pool, tenant.ID, brand.ID, otherPlayer.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	// The player's own real bet on this session/round (round id == session
	// id, per newWagerCasinoRoundHandler/newWinCasinoRoundHandler).
	wagerResp := postJSON(t, srv, "/v1/me/casino/sessions/"+sessionID.String()+"/wager", player.Tokens.AccessToken, map[string]any{
		"stake_amount": 1000, "idempotency_key": "c10-g1-wager-1",
	})
	if wagerResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the simulated wager, got %d", wagerResp.StatusCode)
	}
	wagerResp.Body.Close()

	// A SECOND direct cash bet leg, on a DIFFERENT player's wallet, seeded
	// directly under the SAME correlation id (round id == session id) - the
	// wallet-collision shape the public-API bet path cannot itself produce
	// (one session always binds one wallet).
	seedDirectCasinoBetLeg(t, pool, tenant.ID, "mock-casino", "c10-g1-other-wallet-bet", sessionID.String(), otherWallet.ID, "EUR", ledger.AccountPlayerCash, 500)

	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+sessionID.String()+"/win", player.Tokens.AccessToken, map[string]any{
		"win_amount": 500, "idempotency_key": "c10-g1-win-1",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("Stage 10.3 C10: expected 409 for a simulated win whose round resolves to a wallet collision (G-1 abort class), got %d", resp.StatusCode)
	}
}
