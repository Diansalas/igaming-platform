//go:build integration

package httpserver

// Stage 10 F-7 remediation, HTTP layer: on the casino play-simulation
// routes a player reusing an idempotency_key with a different amount used
// to get the ORIGINAL result back as a 200 (audit sites #6 and #9 via the
// sim route). It is now a 409 integrity rejection with no ledger effect;
// the identical retry is still a 200 with the same ledger transaction.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestCasinoPlay_F7_SameIdempotencyKeyDifferentAmountIs409(t *testing.T) {
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
	base := "/v1/me/casino/sessions/" + launched.SessionID

	wagerKey := uuid.NewString()
	first := postJSON(t, srv, base+"/wager", player.Tokens.AccessToken, map[string]any{"stake_amount": 1_000, "idempotency_key": wagerKey})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first wager: %d", first.StatusCode)
	}
	var firstOut map[string]any
	decodeBody(t, first, &firstOut)

	changed := postJSON(t, srv, base+"/wager", player.Tokens.AccessToken, map[string]any{"stake_amount": 3_000, "idempotency_key": wagerKey})
	if changed.StatusCode != http.StatusConflict {
		t.Fatalf("wager replay with a different stake: got %d, want 409", changed.StatusCode)
	}
	_ = changed.Body.Close()

	retry := postJSON(t, srv, base+"/wager", player.Tokens.AccessToken, map[string]any{"stake_amount": 1_000, "idempotency_key": wagerKey})
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("identical wager retry: %d", retry.StatusCode)
	}
	var retryOut map[string]any
	decodeBody(t, retry, &retryOut)
	if retryOut["ledger_transaction_id"] != firstOut["ledger_transaction_id"] {
		t.Fatalf("identical retry resolved to %v, want %v", retryOut["ledger_transaction_id"], firstOut["ledger_transaction_id"])
	}

	winKey := uuid.NewString()
	win := postJSON(t, srv, base+"/win", player.Tokens.AccessToken, map[string]any{"win_amount": 2_000, "idempotency_key": winKey})
	if win.StatusCode != http.StatusOK {
		t.Fatalf("win: %d", win.StatusCode)
	}
	_ = win.Body.Close()
	changedWin := postJSON(t, srv, base+"/win", player.Tokens.AccessToken, map[string]any{"win_amount": 7_000, "idempotency_key": winKey})
	if changedWin.StatusCode != http.StatusConflict {
		t.Fatalf("win replay with a different amount: got %d, want 409", changedWin.StatusCode)
	}
	_ = changedWin.Body.Close()

	if got := walletCashBalance(t, srv, player.Tokens.AccessToken); got != 11_000 {
		t.Fatalf("cash balance = %d, want 11000 (one 1000 stake, one 2000 win)", got)
	}
	if n := countLedgerTransactions(t, pool, tenant.ID, " AND transaction_type IN ('casino_bet','casino_win')"); n != 2 {
		t.Fatalf("casino ledger transactions = %d, want 2", n)
	}
}
