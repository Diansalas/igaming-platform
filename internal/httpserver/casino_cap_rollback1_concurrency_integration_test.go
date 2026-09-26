//go:build integration

// Stage 10.3 CAS-CAP-ROLLBACK-1 / G-1: the concurrency cases the binding
// test plan (04-review-qa.md §2, condition 1) requires beyond the ones
// already named by the analysis paper (§1.11) - "idempotency UNDER
// CONCURRENCY" (the identical callback delivered twice concurrently, for
// bet, win AND rollback), distinct from ordinary sequential replay and
// from the distinct-reference races already covered elsewhere. Fired
// against the ACTUAL inbound HTTP path (the webhook handler), not the
// internal Go function directly (04-review-qa.md §2 item 9).
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
)

// TestPostBet_G1_ConcurrentIdenticalBetCallbacks_PostsExactlyOnce fires
// the SAME signed bet payload (same provider_tx_id) from N concurrent
// goroutines against the webhook HTTP handler, and asserts exactly one
// ledger transaction posts and every response is the SAME successful
// result - never a 500, never a second posting.
func TestPostBet_G1_ConcurrentIdenticalBetCallbacks_PostsExactlyOnce(t *testing.T) {
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

	const betTxID = "cas-conc-bet-identical-1"
	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betTxID, "", "round-conc-bet", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	ledgerTxIDs := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
			statuses[i] = resp.StatusCode
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if id, ok := body["ledger_transaction_id"].(string); ok {
				ledgerTxIDs[i] = id
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("delivery %d: expected 200, got %d", i, status)
		}
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, betTxID); got != 1 {
		t.Fatalf("expected exactly 1 ledger_transactions row despite %d concurrent identical bet deliveries, got %d", n, got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != 9000 {
		t.Fatalf("expected exactly one bet's worth taken (10000-1000=9000), got %d", balance)
	}
	// gate 10.3-W1 QA condition 1 / ledger-finance C4: every loser must
	// have received the ORIGINAL result - the SAME ledger_transaction_id,
	// never a distinct one and never an empty body.
	first := ledgerTxIDs[0]
	if first == "" {
		t.Fatalf("expected every response to carry a ledger_transaction_id, got empty for delivery 0")
	}
	for i, id := range ledgerTxIDs {
		if id != first {
			t.Fatalf("delivery %d: expected the SAME ledger_transaction_id %q as every other concurrent delivery, got %q", i, first, id)
		}
	}
	if got := countCasinoAuditRowsForAction(t, pool, tenant.ID, "casino_bet.posted", betTxID); got != 1 {
		t.Fatalf("expected exactly 1 casino_bet.posted audit row despite %d concurrent identical bet deliveries, got %d", n, got)
	}
}

// TestPostWin_ConcurrentIdenticalWinCallbacks_PostsExactlyOnce is the win
// half of the same idempotency-under-concurrency requirement.
func TestPostWin_ConcurrentIdenticalWinCallbacks_PostsExactlyOnce(t *testing.T) {
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

	const roundID = "round-conc-win"
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-conc-win-bet-1", "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting the bet, got %d", betResp.StatusCode)
	}
	betResp.Body.Close()

	const winTxID = "cas-conc-win-identical-1"
	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	ledgerTxIDs := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
			statuses[i] = resp.StatusCode
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if id, ok := body["ledger_transaction_id"].(string); ok {
				ledgerTxIDs[i] = id
			}
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
		t.Fatalf("expected exactly 1 ledger_transactions row despite %d concurrent identical win deliveries, got %d", n, got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != 10_000-1000+2500 {
		t.Fatalf("expected exactly one win's worth credited, got %d", balance)
	}
	first := ledgerTxIDs[0]
	if first == "" {
		t.Fatalf("expected every response to carry a ledger_transaction_id, got empty for delivery 0")
	}
	for i, id := range ledgerTxIDs {
		if id != first {
			t.Fatalf("delivery %d: expected the SAME ledger_transaction_id %q as every other concurrent delivery, got %q", i, first, id)
		}
	}
	if got := countCasinoAuditRowsForAction(t, pool, tenant.ID, "casino_win.posted", winTxID); got != 1 {
		t.Fatalf("expected exactly 1 casino_win.posted audit row despite %d concurrent identical win deliveries, got %d", n, got)
	}
}

// TestPostRollback_ConcurrentIdenticalRollbackCallbacks_TombstonesExactlyOnce
// is the rollback half: N concurrent deliveries of the IDENTICAL rollback
// reference for a never-seen original must write exactly one tombstone.
func TestPostRollback_ConcurrentIdenticalRollbackCallbacks_TombstonesExactlyOnce(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	const originalTxID = "cas-conc-rb-unseen-original"
	const rollbackTxID = "cas-conc-rb-identical-1"
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackTxID, originalTxID, "round-conc-rb", "game-1",
		0, "EUR", "", "", uuid.New(), uuid.Nil)

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	ledgerTxIDs := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
			statuses[i] = resp.StatusCode
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if id, ok := body["ledger_transaction_id"].(string); ok {
				ledgerTxIDs[i] = id
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("delivery %d: expected 200, got %d", i, status)
		}
	}
	if got := countCasinoTombstonesForTx(t, pool, tenant.ID, originalTxID); got != 1 {
		t.Fatalf("expected exactly 1 tombstone despite %d concurrent identical rollback deliveries, got %d", n, got)
	}
	first := ledgerTxIDs[0]
	if first == "" {
		t.Fatalf("expected every response to carry a ledger_transaction_id (the tombstone's own id), got empty for delivery 0")
	}
	for i, id := range ledgerTxIDs {
		if id != first {
			t.Fatalf("delivery %d: expected the SAME ledger_transaction_id %q (the one tombstone) as every other concurrent delivery, got %q", i, first, id)
		}
	}
	if got := countCasinoAuditRowsForActionMetadataKey(t, pool, tenant.ID, "casino_rollback.tombstoned", "rollback_provider_tx_id", rollbackTxID); got != 1 {
		t.Fatalf("expected exactly 1 casino_rollback.tombstoned audit row despite %d concurrent identical rollback deliveries, got %d", n, got)
	}
}

// TestPostRollback_LateOriginalRacesRollback_SerializesOnL0_1 proves the
// L0.1 serialization point itself (ADR 0082 Amendment A6), not merely the
// outcome: a late-arriving original bet and a rollback naming that SAME
// reference, fired concurrently, must NEVER both succeed in a way that
// leaves two distinct financial states (a posted-then-unreversed bet AND
// a tombstone both existing for the same reference) - exactly one of the
// two deterministic outcomes holds: {bet posted, then reversed} or
// {tombstone written, bet rejected as E3}. Run repeatedly to surface a
// race if the lock were ever removed.
func TestPostRollback_LateOriginalRacesRollback_SerializesOnL0_1(t *testing.T) {
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

	// gate 10.3-W1 ledger-finance condition C1: 50 iterations (was 20),
	// per ADR 0082 A6/QA's requirement.
	const iterations = 50
	for iter := 0; iter < iterations; iter++ {
		launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
		sessionID := uuid.MustParse(launched.SessionID)

		originalTxID := "cas-race-original-" + uuid.NewString()
		rollbackTxID := "cas-race-rollback-" + uuid.NewString()
		roundID := "round-race-" + uuid.NewString()

		betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, originalTxID, "", roundID, game.ProviderGameID,
			500, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
		rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackTxID, originalTxID, roundID, "game-1",
			0, "EUR", "", "", player.ID, uuid.Nil)

		// gate 10.3-W1 condition C1: on the FIRST iteration only, prove
		// WHERE the loser of this race blocks - the L0.1
		// casino_bet_delivery advisory lock on originalTxID itself, not
		// merely the eventual {bet-posted, tombstoned} outcome the rest of
		// this loop already asserts. Manually holding the SAME lock
		// production code takes forces whichever of postBet/postRollback
		// starts second to queue on it, visible in pg_stat_activity as an
		// 'advisory' wait_event, before this test ever releases the
		// blocker and lets the real race proceed.
		var blocker *casBlocker
		var doneCh chan struct{}
		if iter == 0 {
			blocker = casHoldProviderTxDeliveryLock(t, pool, tenant.ID, "mock-casino", originalTxID)
			doneCh = make(chan struct{})
		}

		var wg sync.WaitGroup
		var betStatus, rollbackStatus int
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
			betStatus = resp.StatusCode
			resp.Body.Close()
		}()
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
			rollbackStatus = resp.StatusCode
			resp.Body.Close()
		}()
		if iter == 0 {
			go func() { wg.Wait(); close(doneCh) }()
			if !casWaitForAdvisoryWaiter(t, pool, doneCh) {
				blocker.release()
				<-doneCh
				t.Fatalf("iteration 0: expected one of the two concurrent deliveries to queue on the L0.1 advisory lock (pg_stat_activity wait_event='advisory') while it was held externally, but neither ever did")
			}
			blocker.release()
			<-doneCh
		} else {
			wg.Wait()
		}

		if betStatus != http.StatusOK || rollbackStatus != http.StatusOK {
			t.Fatalf("iteration %d: expected both deliveries to return 200 (a bet+reversal or a tombstone+declined-bet are both 200 shapes), got bet=%d rollback=%d", iter, betStatus, rollbackStatus)
		}

		// countCasinoLedgerRowsForTx counts ANY row for this provider_tx_id
		// - including the tombstone itself, since a tombstone's OWN
		// provider_tx_id IS the original reference (F7). betPosted must
		// therefore check the transaction_type specifically, not just "a
		// row exists", or the two outcomes below could never be told
		// apart.
		var betPosted, tombstoned bool
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2 AND transaction_type = 'casino_bet')`,
				tenant.ID, originalTxID).Scan(&betPosted); err != nil {
				return err
			}
			return tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2 AND transaction_type = 'tombstone')`,
				tenant.ID, originalTxID).Scan(&tombstoned)
		}); err != nil {
			t.Fatalf("iteration %d: query outcome: %v", iter, err)
		}

		if betPosted == tombstoned {
			t.Fatalf("iteration %d: L0.1 must serialize this race deterministically into EXACTLY ONE of {bet posted} or {tombstone written}, got posted=%v tombstoned=%v", iter, betPosted, tombstoned)
		}

		if betPosted {
			// The bet won the race - its own row must be reversed by this
			// SAME rollback reference (a normal E7 settlement), never left
			// standing un-reversed.
			var reversalCount int
			if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT count(*) FROM ledger_transactions lt
					   JOIN ledger_transactions orig ON orig.id = lt.reverses_transaction_id
					  WHERE lt.tenant_id = $1 AND orig.provider_id = 'mock-casino' AND orig.provider_tx_id = $2 AND lt.provider_tx_id = $3`,
					tenant.ID, originalTxID, rollbackTxID).Scan(&reversalCount)
			}); err != nil {
				t.Fatalf("iteration %d: query reversal: %v", iter, err)
			}
			if reversalCount != 1 {
				t.Fatalf("iteration %d: the bet won the race but was not reversed by its own concurrent rollback (reversalCount=%d)", iter, reversalCount)
			}
		}
	}
}

// TestPostWin_LateWinRacesItsOwnRollback_SerializesOnL0_1 is E10's own
// concurrency proof (ADR 0082 Amendment A6's extension to postWin, per
// the architect's W0 code check): a win and a rollback that names that
// SAME win's provider_tx_id as its OWN OriginalProviderTxID (a rollback
// of a win the ledger has not posted yet), fired concurrently, must
// serialize deterministically into exactly one of {win posted, then
// reversed by this rollback} or {tombstone written first, win rejected
// as E10} - never both, never neither, never a 500.
func TestPostWin_LateWinRacesItsOwnRollback_SerializesOnL0_1(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	// gate 10.3-W1 condition C1 fix: unlike the postRollback/bet race above
	// (where every outcome either reverses the bet or never posts it, so
	// the wallet balance never permanently drops), THIS test's bet always
	// stays posted every iteration - only the WIN is raced - so 500/
	// iteration is a genuine, permanent stake. Raising iterations from 20
	// to 50 at the OLD 10,000 funding exhausted the wallet at iteration 20
	// (10000/500), turning every later bet into an insufficient-funds
	// DECLINE (still HTTP 200) and then the win into a spurious
	// ErrBetNotFound 400 - a pre-existing test-fixture bug this raise
	// exposed, not a production defect. Funded generously above 50*500.
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 1_000_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	// gate 10.3-W1 ledger-finance condition C1: 50 iterations (was 20).
	const iterations = 50
	for iter := 0; iter < iterations; iter++ {
		launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
		sessionID := uuid.MustParse(launched.SessionID)

		betTxID := "cas-e10-bet-" + uuid.NewString()
		winTxID := "cas-e10-win-" + uuid.NewString()
		rollbackOfWinTxID := "cas-e10-rollback-" + uuid.NewString()
		roundID := "round-e10-" + uuid.NewString()

		betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betTxID, "", roundID, game.ProviderGameID,
			500, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
		betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
		if betResp.StatusCode != http.StatusOK {
			t.Fatalf("iteration %d: expected 200 posting the bet, got %d", iter, betResp.StatusCode)
		}
		betResp.Body.Close()

		winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
			900, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
		rollbackOfWinPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackOfWinTxID, winTxID, roundID, "game-1",
			0, "EUR", "", "", player.ID, uuid.Nil)

		// gate 10.3-W1 condition C1: on the FIRST iteration, prove the E10
		// waiter blocks on the L0.1 advisory lock itself (keyed on winTxID,
		// which BOTH postWin - its own reference - and postRollback - its
		// OriginalProviderTxID - take here), exactly like the postRollback
		// race test above.
		var blocker *casBlocker
		var doneCh chan struct{}
		if iter == 0 {
			blocker = casHoldProviderTxDeliveryLock(t, pool, tenant.ID, "mock-casino", winTxID)
			doneCh = make(chan struct{})
		}

		var wg sync.WaitGroup
		var winStatus, rollbackStatus int
		var winBody map[string]any
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
			winStatus = resp.StatusCode
			_ = json.NewDecoder(resp.Body).Decode(&winBody)
			resp.Body.Close()
		}()
		go func() {
			defer wg.Done()
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackOfWinPayload)
			rollbackStatus = resp.StatusCode
			resp.Body.Close()
		}()
		if iter == 0 {
			go func() { wg.Wait(); close(doneCh) }()
			if !casWaitForAdvisoryWaiter(t, pool, doneCh) {
				blocker.release()
				<-doneCh
				t.Fatalf("iteration 0: expected one of the two concurrent deliveries to queue on the L0.1 advisory lock (pg_stat_activity wait_event='advisory') while it was held externally, but neither ever did")
			}
			blocker.release()
			<-doneCh
		} else {
			wg.Wait()
		}

		if rollbackStatus != http.StatusOK {
			t.Fatalf("iteration %d: expected the rollback delivery to return 200 (a tombstone or a real reversal), got %d", iter, rollbackStatus)
		}
		if winStatus != http.StatusOK && winStatus != http.StatusConflict {
			t.Fatalf("iteration %d: expected the win delivery to return 200 (posted) or 409 (E10, rejected as tombstoned) - never a 500, got %d", iter, winStatus)
		}

		var winPosted, tombstoned bool
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2 AND transaction_type = 'casino_win')`,
				tenant.ID, winTxID).Scan(&winPosted); err != nil {
				return err
			}
			return tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2 AND transaction_type = 'tombstone')`,
				tenant.ID, winTxID).Scan(&tombstoned)
		}); err != nil {
			t.Fatalf("iteration %d: query outcome: %v", iter, err)
		}

		if winPosted == tombstoned {
			t.Fatalf("iteration %d: E10's race must serialize deterministically into EXACTLY ONE of {win posted} or {tombstone written}, got posted=%v tombstoned=%v (winStatus=%d body=%+v)", iter, winPosted, tombstoned, winStatus, winBody)
		}
		if winPosted && winStatus != http.StatusOK {
			t.Fatalf("iteration %d: the win posted but its own HTTP response was %d, not 200", iter, winStatus)
		}
		if tombstoned && winStatus != http.StatusConflict {
			t.Fatalf("iteration %d: the tombstone won but the win's own HTTP response was %d, not 409 (E10)", iter, winStatus)
		}

		if winPosted {
			var reversalCount int
			if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT count(*) FROM ledger_transactions lt
					   JOIN ledger_transactions orig ON orig.id = lt.reverses_transaction_id
					  WHERE lt.tenant_id = $1 AND orig.provider_id = 'mock-casino' AND orig.provider_tx_id = $2 AND lt.provider_tx_id = $3`,
					tenant.ID, winTxID, rollbackOfWinTxID).Scan(&reversalCount)
			}); err != nil {
				t.Fatalf("iteration %d: query win reversal: %v", iter, err)
			}
			if reversalCount != 1 {
				t.Fatalf("iteration %d: the win won the race but was not reversed by its own concurrent rollback (reversalCount=%d)", iter, reversalCount)
			}
		}
	}
}
