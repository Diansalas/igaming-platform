//go:build integration

// C11 (ledger-finance re-verification after fix round A, gate 10.3-W1):
// postRollback's generic (non-tombstone) entry-inversion path gates its
// `casino_bet.rolled_back`/`casino_win.rolled_back` audit write on
// `!postResult.AlreadyPosted` (orchestrator.go, near the rollbackTxType
// posting) - the SAME one-row-per-fact guard postWin's direct-cash path
// already has, killed by TestPostWin_ConcurrentIdenticalWinCallbacks_
// PostsExactlyOnce. Until this file, NO test exercised a redelivery of the
// GENERIC rollback path (only the tombstone path, which short-circuits
// before ever reaching this guard, had a concurrent-identical test -
// TestPostRollback_ConcurrentIdenticalRollbackCallbacks_TombstonesExactlyOnce).
// Removing the guard would therefore survive the suite. This test posts a
// REAL bet, rolls it back (a genuine reversal of an existing casino_bet,
// not a never-seen original), then redelivers the identical rollback both
// sequentially and concurrently (N=8), and asserts exactly one reversal
// row, the same ledger_transaction_id in every response, and exactly one
// casino_bet.rolled_back audit row.
//
// Run as the NOBYPASSRLS runtime role (igaming_runtime /
// TEST_RUNTIME_DATABASE_URL), per the binding test plan (04-review-qa.md
// §3, §1.9) - see docs/plans/stage-10.3-planning/evidence/
// w1c-c11-runtime-role.txt for the recorded run.
package httpserver

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
)

func TestPostRollback_C11_GenericPathAuditGate_RedeliverySequentialThenConcurrent(t *testing.T) {
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

	const betTxID = "cas-c11-generic-rb-bet-1"
	const roundID = "round-c11-generic-rb"
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betTxID, "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting the bet, got %d", betResp.StatusCode)
	}
	betResp.Body.Close()

	const rollbackTxID = "cas-c11-generic-rb-rollback-1"
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackTxID, betTxID, roundID, game.ProviderGameID,
		0, "EUR", "", "", player.ID, uuid.Nil)

	postRollbackOnce := func() (status int, ledgerTxID string) {
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if id, ok := body["ledger_transaction_id"].(string); ok {
			ledgerTxID = id
		}
		return resp.StatusCode, ledgerTxID
	}

	// First delivery: the GENUINE, first rollback of a real posted bet -
	// takes the generic entry-inversion path (never the tombstone path,
	// since the original bet genuinely exists).
	status, firstLedgerTxID := postRollbackOnce()
	if status != http.StatusOK {
		t.Fatalf("expected 200 posting the first rollback, got %d", status)
	}
	if firstLedgerTxID == "" {
		t.Fatal("expected the first rollback response to carry a ledger_transaction_id")
	}

	// One sequential redelivery: the E4-style, non-concurrent replay this
	// package's own idempotency short-circuit design otherwise favors -
	// proves the generic path's AlreadyPosted gate holds even outside a
	// race.
	status, sequentialLedgerTxID := postRollbackOnce()
	if status != http.StatusOK {
		t.Fatalf("expected 200 on the sequential redelivery, got %d", status)
	}
	if sequentialLedgerTxID != firstLedgerTxID {
		t.Fatalf("expected the sequential redelivery to return the SAME ledger_transaction_id %q, got %q", firstLedgerTxID, sequentialLedgerTxID)
	}

	// N=8 concurrent redeliveries of the IDENTICAL rollback reference.
	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	ledgerTxIDs := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			statuses[i], ledgerTxIDs[i] = postRollbackOnce()
		}()
	}
	wg.Wait()

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("concurrent redelivery %d: expected 200, got %d", i, s)
		}
	}
	for i, id := range ledgerTxIDs {
		if id != firstLedgerTxID {
			t.Fatalf("concurrent redelivery %d: expected the SAME ledger_transaction_id %q as the first delivery, got %q", i, firstLedgerTxID, id)
		}
	}

	// Exactly one reversal row overall, despite 1 (first) + 1 (sequential) +
	// 8 (concurrent) = 10 total deliveries of the identical rollback
	// reference.
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, rollbackTxID); got != 1 {
		t.Fatalf("expected exactly 1 reversal ledger_transactions row despite 10 deliveries of the identical rollback, got %d", got)
	}
	// This is C11's own binding assertion: exactly one casino_bet.rolled_back
	// audit row - the one this generic-path gate exists to guarantee. Before
	// this test, removing `!postResult.AlreadyPosted` at that gate would
	// have survived the whole suite (see the mutation-kill record).
	if got := countCasinoAuditRowsForActionMetadataKey(t, pool, tenant.ID, "casino_bet.rolled_back", "rollback_provider_tx_id", rollbackTxID); got != 1 {
		t.Fatalf("expected exactly 1 casino_bet.rolled_back audit row despite 10 deliveries of the identical rollback, got %d", got)
	}
}
