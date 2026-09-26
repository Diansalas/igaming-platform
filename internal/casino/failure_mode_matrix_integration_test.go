//go:build integration

// Stage 8 (ADR 0080) financial-failure-mode matrix for the CASINO money
// path - built entirely on the EXISTING deterministic MockCasinoProvider
// and the EXISTING orchestrator/ledger code. No external API is called,
// no new provider exists, and NOTHING in postBet/postWin/postRollback was
// changed to make any test below pass (ADR 0080's "does not change
// postBet/postWin/postRollback's existing account-resolution logic" and
// CLAUDE.md's financial-code rules - a change to that code has to be a
// deliberate, reviewed decision, never a side effect of writing a test).
//
// These are FAILURE-MODE tests against this platform's own callback
// boundary - they are NOT an integration test of any real or "Dummy"
// casino API, and none of them proves any vendor's contract.
//
// The matrix (Stage 8's own lettering), and where each item lives:
//
//	A  provider accepted / platform "timed out" or crashed before commit
//	   -> TestFailureModeMatrix_A_* (two proofs: an explicit rollback, and
//	      a genuine mid-flight context cancellation while the callback is
//	      blocked inside postBet)
//	B  retried request after a "crash" -> TestFailureModeMatrix_B_*
//	C  duplicate webhook delivery      -> TestFailureModeMatrix_C_*
//	D  out-of-order callbacks          -> TestFailureModeMatrix_D_*
//	E  malformed transaction           -> TestFailureModeMatrix_E_*
//	F  provider transport failure      -> TestFailureModeMatrix_F_*
//	H  unknown session/round           -> TestFailureModeMatrix_H_*
//	I  cross-player reference          -> TestFailureModeMatrix_I_*
//	JK cross-tenant/cross-brand ref    -> TestFailureModeMatrix_JK_*
//
// Item G (provider unavailable via a config `Enabled=false` flag) is NOT
// covered here, deliberately. `internal/providers/config.go` (ADR 0080
// Decision 4) exists, but `ProviderConfig.Enabled` is not wired into any
// money path anywhere in the tree - by that package's own doc comment
// ("this package only loads configuration, it never gates behavior") and
// by ADR 0080's own "no specific provider is wired into
// cmd/platform-api/main.go's CasinoProvider/sportsbook.Provider maps this
// stage". There is therefore no reachable path from that flag to a
// ledger effect to test, and writing one would mean inventing the wiring.
// Its own unit tests cover the loader. What DOES gate the money path
// today is the per-tenant CAPABILITY row (ReceiveCallback's own kill
// switch, already proven by
// TestReceiveCallback_DisabledCapabilityBlocksCallback) and the adapter
// registry (ErrUnknownProvider).
//
// Every test asserts the same four properties, not merely "an error came
// back": no duplicate debit, no duplicate credit, SUM(debits) ==
// SUM(credits) for the affected correlation id (and tenant-wide), and no
// cross-player/cross-tenant financial effect.
//
// On item A and torn writes, for the record: ReceiveCallback never opens
// or commits a transaction of its own - it takes the caller's already-open
// pgx.Tx (see its own doc comment) and every effect it produces (the
// ledger_transactions row, its ledger_entries, the audit_log row, the
// wallet_balance_projection trigger update) is written inside that ONE
// transaction. So "half a posting" is not a state this code can reach:
// Postgres either commits all of it or none of it. The two A tests below
// demonstrate that empirically from both ends (a caller-side abort after a
// successful call, and a cancellation that lands while the callback is
// genuinely mid-flight inside postBet), rather than asserting it by
// assertion alone.
package casino

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// --- matrix-local assertion helpers (fm* prefix so they never collide
// with the package's existing test helpers) ---

// fmCountLedgerTx counts ledger_transactions rows for one provider
// reference under tenantID - the "no duplicate posting" assertion, made
// against the table itself rather than inferred from a balance.
func fmCountLedgerTx(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID, providerTxID string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3`,
			tenantID, providerID, providerTxID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count ledger_transactions for %s/%s: %v", providerID, providerTxID, err)
	}
	return count
}

// fmLedgerTxID returns the single ledger_transactions id for one provider
// reference, or uuid.Nil when none exists.
func fmLedgerTxID(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID, providerTxID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3`,
			tenantID, providerID, providerTxID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			id = uuid.Nil
			return nil
		}
		return err
	})
	if err != nil {
		t.Fatalf("read ledger_transactions id for %s/%s: %v", providerID, providerTxID, err)
	}
	return id
}

// fmCorrelation reports how many ledger_transactions and ledger_entries
// rows exist under one correlation id, plus that correlation's own
// debit/credit totals - the per-round form of invariant #1 (CLAUDE.md:
// SUM(DEBITS) == SUM(CREDITS) always holds), which a tenant-wide sum can
// mask if two rounds' errors happen to offset.
func fmCorrelation(t *testing.T, pool *db.Pool, tenantID, correlationID uuid.UUID) (txCount, entryCount int, debits, credits int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND correlation_id = $2`,
			tenantID, correlationID).Scan(&txCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT count(*),
			        COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0),
			        COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)
			   FROM ledger_entries e
			   JOIN ledger_transactions lt ON lt.id = e.ledger_transaction_id
			  WHERE e.tenant_id = $1 AND lt.correlation_id = $2`,
			tenantID, correlationID).Scan(&entryCount, &debits, &credits)
	})
	if err != nil {
		t.Fatalf("read correlation %s: %v", correlationID, err)
	}
	return txCount, entryCount, debits, credits
}

// fmAssertCorrelationBalanced is the invariant assertion every test below
// makes about the round it touched.
func fmAssertCorrelationBalanced(t *testing.T, pool *db.Pool, tenantID, correlationID uuid.UUID) {
	t.Helper()
	_, _, debits, credits := fmCorrelation(t, pool, tenantID, correlationID)
	if debits != credits {
		t.Fatalf("invariant #1 violated for correlation %s: debits=%d credits=%d", correlationID, debits, credits)
	}
}

// fmWalletCashBalance reads any wallet's player_cash net balance from the
// projection - cashBalance (the package's existing helper) only reads the
// fixture's OWN wallet, which is exactly what a cross-player test must not
// rely on.
func fmWalletCashBalance(t *testing.T, pool *db.Pool, tenantID, walletID uuid.UUID) int64 {
	t.Helper()
	var balance int64
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(credit_total - debit_total), 0)::bigint FROM wallet_balance_projection
			  WHERE wallet_id = $1 AND account_type = 'player_cash'`, walletID).Scan(&balance)
	})
	if err != nil {
		t.Fatalf("read wallet %s cash balance: %v", walletID, err)
	}
	return balance
}

// fmWaitForLockWaiters polls pg_stat_activity until at least want backends
// are genuinely blocked, directly or transitively, by the test's OWN
// blocker session (blockerPID) - this codebase's established deterministic
// concurrency technique, used here so the mid-flight cancellation in item
// A fires at a KNOWN point inside postBet rather than at a sleep-guessed
// one.
//
// Scoped to the blocker rather than a database-wide Lock-wait count:
// `go test ./...` runs packages in parallel against one test database, so
// a bare count also sees other packages' lock waits and can return before
// this test's own delivery has reached the lock (Stage 10 W0; the same
// trap is documented in internal/ledger/lockorder_harness_test.go).
// pg_blocking_pids covers advisory locks; the recursive walk also counts a
// racer queued behind another racer that is itself waiting on the blocker.
func fmWaitForLockWaiters(t *testing.T, pool *db.Pool, blockerPID int32, want int) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				WITH RECURSIVE waiters(pid) AS (
					SELECT a.pid FROM pg_stat_activity a
					 WHERE $1::int = ANY(pg_blocking_pids(a.pid))
					UNION
					SELECT a.pid FROM pg_stat_activity a
					  JOIN waiters w ON w.pid = ANY(pg_blocking_pids(a.pid))
				)
				SELECT count(*) FROM waiters`, blockerPID).Scan(&count)
		})
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if count >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// --- A. Provider accepted, platform "timed out"/crashed before commit ---

// TestFailureModeMatrix_A_PlatformAbortsAfterProviderAccepted_NoPartialLedgerWrite
// is the plain, fully-deterministic form of item A: the provider's
// callback is accepted and posted successfully, and then the platform
// fails before the transaction commits (a crash, a timeout at the HTTP
// layer, a panic recovered into an error - all the same thing to
// Postgres). The assertion is not "an error was returned" but "the ledger
// has no trace of it AT ALL": zero transactions, zero entries, zero
// audit rows, an unchanged balance - never a half-posted state.
//
// The second half is the part that actually matters operationally: the
// provider, having seen no acknowledgement, redelivers the same callback,
// and THAT delivery posts exactly once. A platform that lost the write
// must be able to accept the retry - "no partial write" is only safe if
// it is also "no poisoned reference".
func TestFailureModeMatrix_A_PlatformAbortsAfterProviderAccepted_NoPartialLedgerWrite(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const providerTxID = "bet-fm-a-abort"
	const roundID = "round-fm-a-abort"
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", roundID)
	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, providerTxID, "", roundID, "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

	// The callback itself succeeds - the provider has "accepted" the bet
	// and the platform has posted it - and only THEN does the platform
	// fail, before commit.
	errBoom := errors.New("simulated platform failure after the provider's callback was posted")
	var posted ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var callErr error
		posted, callErr = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		if callErr != nil {
			return callErr
		}
		if posted.Outcome != OutcomeSucceeded || posted.LedgerTransactionID == nil {
			return errors.New("fixture precondition failed: the bet did not post before the simulated crash")
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected the simulated platform failure to propagate, got %v", err)
	}

	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", providerTxID); count != 0 {
		t.Fatalf("a rolled-back callback must leave ZERO ledger_transactions rows, got %d", count)
	}
	txCount, entryCount, debits, credits := fmCorrelation(t, pool, f.tenantID, correlationID)
	if txCount != 0 || entryCount != 0 || debits != 0 || credits != 0 {
		t.Fatalf("a rolled-back callback must leave no trace under its correlation id: tx=%d entries=%d debits=%d credits=%d",
			txCount, entryCount, debits, credits)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the funded balance (5000) untouched after the rollback, got %d", balance)
	}
	// The audit record postBet writes lives in the SAME transaction, so it
	// must be gone too - an audit trail claiming a bet posted that the
	// ledger has no record of would be its own integrity failure.
	var auditCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino_bet.posted'
			   AND metadata->>'provider_tx_id' = $2`, f.tenantID, providerTxID).Scan(&auditCount)
	}); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("expected zero casino_bet.posted audit rows after the rollback, got %d", auditCount)
	}

	// The provider retries the identical callback - it must post, exactly
	// once, now that the platform is healthy again.
	var retried ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var callErr error
		retried, callErr = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return callErr
	}); err != nil {
		t.Fatalf("redelivery after the simulated crash: %v", err)
	}
	if retried.Outcome != OutcomeSucceeded || retried.LedgerTransactionID == nil {
		t.Fatalf("expected the redelivered bet to post, got %+v", retried)
	}
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", providerTxID); count != 1 {
		t.Fatalf("expected exactly one ledger_transactions row after the retry, got %d", count)
	}
	txCount, entryCount, debits, credits = fmCorrelation(t, pool, f.tenantID, correlationID)
	if txCount != 1 || entryCount != 2 {
		t.Fatalf("expected exactly one transaction of two entries for the round, got tx=%d entries=%d", txCount, entryCount)
	}
	if debits != credits || debits != 1000 {
		t.Fatalf("expected a balanced 1000/1000 round, got debits=%d credits=%d", debits, credits)
	}
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected exactly one debit applied (5000-1000), got %d", balance)
	}
}

// TestFailureModeMatrix_A_ContextCancelledMidCallback_NoPartialLedgerWrite
// is the harder half of item A: a cancellation that lands while
// ReceiveCallback is GENUINELY MID-FLIGHT inside postBet, not before or
// after it.
//
// It is made deterministic rather than timing-guessed by using postBet's
// own advisory lock as the rendezvous point: a first delivery of the same
// (tenant, provider, provider_tx_id) is left uncommitted in an open
// transaction, so it holds `pg_advisory_xact_lock('casino_bet_delivery:...')`
// for the whole test. A second delivery of the IDENTICAL callback then
// blocks on exactly that lock, inside postBet, before it has resolved a
// session, evaluated RG/Risk, read a balance or posted anything - and its
// context is cancelled right there (confirmed blocked via
// pg_stat_activity, not via a sleep).
//
// The guarantee proven: the cancelled delivery leaves NOTHING behind, and
// the surviving delivery posts exactly one balanced bet. There is no state
// in which the ledger holds a debit without its matching credit.
func TestFailureModeMatrix_A_ContextCancelledMidCallback_NoPartialLedgerWrite(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const providerTxID = "bet-fm-a-cancel"
	const roundID = "round-fm-a-cancel"
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", roundID)
	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, providerTxID, "", roundID, "game-1", 1200, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

	// Delivery 1: posted but deliberately left uncommitted, so it holds
	// postBet's own per-(tenant, provider, tx) advisory lock.
	holderCtx := context.Background()
	holderTx, err := pool.Raw().Begin(holderCtx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = holderTx.Rollback(holderCtx) }()
	if _, err := holderTx.Exec(holderCtx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
		t.Fatalf("set tenant context on holder tx: %v", err)
	}
	holderResult, err := orch.receiveCallbackInTx(holderCtx, holderTx, f.tenantID, "mock-casino", payload)
	if err != nil {
		t.Fatalf("holder delivery: %v", err)
	}
	if holderResult.Outcome != OutcomeSucceeded {
		t.Fatalf("expected the holder delivery to post, got %+v", holderResult)
	}
	var holderPID int32
	if err := holderTx.QueryRow(holderCtx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read holder pg_backend_pid: %v", err)
	}

	// Delivery 2: the same callback again (the provider retrying because
	// the platform has not acknowledged yet). It blocks inside postBet.
	cancelCtx, cancel := context.WithCancel(context.Background())
	deliveryErr := make(chan error, 1)
	go func() {
		deliveryErr <- pool.WithTenant(cancelCtx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
			return err
		})
	}()

	if !fmWaitForLockWaiters(t, pool, holderPID, 1) {
		cancel()
		<-deliveryErr
		t.Fatal("second delivery never blocked on postBet's advisory lock; cannot cancel it mid-flight deterministically")
	}
	// Cancelled WHILE inside ReceiveCallback, holding an open transaction
	// that has already executed statements.
	cancel()
	select {
	case err := <-deliveryErr:
		if err == nil {
			t.Fatal("expected the cancelled delivery to fail, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			// pgx may surface the cancellation as its own wrapped error
			// rather than context.Canceled verbatim; either is fine, an
			// unexpected SUCCESS is not.
			t.Logf("cancelled delivery surfaced as: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled delivery did not return")
	}

	// Nothing is committed yet at all - the surviving delivery is still
	// open - so the ledger must still be empty for this round.
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", providerTxID); count != 0 {
		t.Fatalf("expected no visible ledger_transactions row while delivery 1 is still uncommitted, got %d", count)
	}

	if err := holderTx.Commit(holderCtx); err != nil {
		t.Fatalf("commit holder tx: %v", err)
	}

	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", providerTxID); count != 1 {
		t.Fatalf("expected exactly one ledger_transactions row after a cancelled duplicate delivery, got %d", count)
	}
	txCount, entryCount, debits, credits := fmCorrelation(t, pool, f.tenantID, correlationID)
	if txCount != 1 || entryCount != 2 {
		t.Fatalf("expected one transaction of two entries, got tx=%d entries=%d", txCount, entryCount)
	}
	if debits != credits || debits != 1200 {
		t.Fatalf("expected a balanced 1200/1200 round, got debits=%d credits=%d", debits, credits)
	}
	if balance := cashBalance(t, pool, f); balance != 3800 {
		t.Fatalf("expected exactly one debit applied (5000-1200), got %d", balance)
	}
}

// --- B. Provider accepted, platform "crashed" AFTER the commit ---

// TestFailureModeMatrix_B_RetryAfterCommittedCrashReturnsPriorOutcome
// covers item A's mirror image: the posting DID commit, and the platform
// died before acknowledging it. The provider retries. The platform must
// return the PRIOR outcome (the same ledger transaction id) rather than
// posting a second debit, and must not re-evaluate the bet against
// whatever state is live at retry time.
//
// Distinct from TestReceiveCallback_RedeliveredBetWinRollbackAreIdempotent,
// which asserts only that the wallet BALANCE is unchanged: this asserts
// the structural facts a balance can hide - one transaction row, two entry
// rows, and the SAME transaction id returned to the caller both times, for
// the bet and for the win.
func TestFailureModeMatrix_B_RetryAfterCommittedCrashReturnsPriorOutcome(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const roundID = "round-fm-b"
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", roundID)

	deliver := func(payload webhookauth.Inbound) ReceiveCallbackResult {
		t.Helper()
		var result ReceiveCallbackResult
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
			return err
		}); err != nil {
			t.Fatalf("deliver callback: %v", err)
		}
		return result
	}

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-fm-b", "", roundID, "game-1", 800, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	first := deliver(betPayload)
	if first.Outcome != OutcomeSucceeded || first.LedgerTransactionID == nil {
		t.Fatalf("expected the first bet delivery to post, got %+v", first)
	}
	// "Crash" happens here: committed, never acknowledged. The provider
	// retries the identical raw payload.
	second := deliver(betPayload)
	if second.Outcome != OutcomeSucceeded || second.LedgerTransactionID == nil {
		t.Fatalf("expected the retried bet to report the prior success, got %+v", second)
	}
	if *second.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("a retry must return the ORIGINAL transaction id (%s), got %s", *first.LedgerTransactionID, *second.LedgerTransactionID)
	}
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", "bet-fm-b"); count != 1 {
		t.Fatalf("expected exactly one bet transaction after the retry, got %d", count)
	}

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-fm-b", "", roundID, "game-1", 2000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	firstWin := deliver(winPayload)
	secondWin := deliver(winPayload)
	if firstWin.LedgerTransactionID == nil || secondWin.LedgerTransactionID == nil {
		t.Fatalf("expected both win deliveries to report a transaction id, got %+v / %+v", firstWin, secondWin)
	}
	if *secondWin.LedgerTransactionID != *firstWin.LedgerTransactionID {
		t.Fatalf("a retried win must return the ORIGINAL transaction id (%s), got %s", *firstWin.LedgerTransactionID, *secondWin.LedgerTransactionID)
	}
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", "win-fm-b"); count != 1 {
		t.Fatalf("expected exactly one win transaction after the retry, got %d", count)
	}

	// One debit of 800 and one credit of 2000, each exactly once.
	txCount, entryCount, debits, credits := fmCorrelation(t, pool, f.tenantID, correlationID)
	if txCount != 2 || entryCount != 4 {
		t.Fatalf("expected exactly two transactions of two entries each for the round, got tx=%d entries=%d", txCount, entryCount)
	}
	if debits != credits || debits != 2800 {
		t.Fatalf("expected a balanced round (800 bet + 2000 win on each side), got debits=%d credits=%d", debits, credits)
	}
	if balance := cashBalance(t, pool, f); balance != 6200 {
		t.Fatalf("expected 5000-800+2000=6200 after retried bet and win, got %d", balance)
	}
	fmAssertCorrelationBalanced(t, pool, f.tenantID, correlationID)
}

// --- C. Duplicate callback (the provider's own webhook infrastructure
// retrying an already-delivered event) ---

// TestFailureModeMatrix_C_DuplicateWebhookDeliveryOfFullRound covers item
// C by name, across a COMPLETE round (bet -> win -> rollback of the win),
// with every event delivered twice. Mechanically this shares B's
// idempotency path, but Stage 8 asks for it explicitly and the
// round-level assertions differ: what is checked here is that the round's
// own correlation id ends up holding exactly three transactions and six
// entries with debits == credits - i.e. that duplicate delivery cannot
// inflate the round, not merely that the wallet balance happens to land
// on the right number.
func TestFailureModeMatrix_C_DuplicateWebhookDeliveryOfFullRound(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const roundID = "round-fm-c"
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", roundID)

	deliverTwice := func(label string, payload webhookauth.Inbound) {
		t.Helper()
		var ids []uuid.UUID
		for i := 0; i < 2; i++ {
			var result ReceiveCallbackResult
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			}); err != nil {
				t.Fatalf("%s delivery %d: %v", label, i+1, err)
			}
			if result.LedgerTransactionID == nil {
				t.Fatalf("%s delivery %d reported no ledger transaction id: %+v", label, i+1, result)
			}
			ids = append(ids, *result.LedgerTransactionID)
		}
		if ids[0] != ids[1] {
			t.Fatalf("%s: duplicate delivery produced a DIFFERENT transaction id (%s vs %s)", label, ids[0], ids[1])
		}
	}

	deliverTwice("bet", provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-fm-c", "", roundID, "game-1", 600, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID))
	deliverTwice("win", provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-fm-c", "", roundID, "game-1", 1500, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil))
	deliverTwice("rollback", provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-fm-c", "win-fm-c", roundID, "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil))

	for _, ref := range []string{"bet-fm-c", "win-fm-c", "rollback-fm-c"} {
		if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", ref); count != 1 {
			t.Fatalf("expected exactly one ledger_transactions row for %s, got %d", ref, count)
		}
	}
	txCount, entryCount, debits, credits := fmCorrelation(t, pool, f.tenantID, correlationID)
	if txCount != 3 || entryCount != 6 {
		t.Fatalf("expected three transactions / six entries for a duplicated bet+win+rollback round, got tx=%d entries=%d", txCount, entryCount)
	}
	if debits != credits {
		t.Fatalf("invariant #1 violated for the duplicated round: debits=%d credits=%d", debits, credits)
	}
	// 5000 - 600 (bet) + 1500 (win) - 1500 (win rolled back) = 4400.
	if balance := cashBalance(t, pool, f); balance != 4400 {
		t.Fatalf("expected 4400 after a duplicated bet/win/rollback round, got %d", balance)
	}
	tenantDebits, tenantCredits := sumDebitsCredits(t, pool, f.tenantID)
	if tenantDebits != tenantCredits {
		t.Fatalf("tenant-wide invariant #1 violated: debits=%d credits=%d", tenantDebits, tenantCredits)
	}
}

// --- D. Out-of-order callbacks ---

// TestFailureModeMatrix_D_WinBeforeBetIsRejectedAndTheRoundStillRecovers
// documents what postWin ACTUALLY does today with an out-of-order win,
// verified rather than assumed:
//
//   - postWin never looks at event.PlayerAccountID to decide where a win
//     goes. It derives the round's correlation id from (tenant, provider,
//     round) via roundCorrelationID and asks resolveWinOrigin to find the
//     round's own prior BET entries in the ledger.
//   - With no prior bet, resolveWinOrigin returns ErrBetNotFound, which
//     ReceiveCallback surfaces as a Go error (aborting the transaction),
//     NOT as a standalone win and NOT as a declined result. A win with no
//     bet is treated as a provider protocol violation / integrity alert,
//     which is the documented Flow 6 behavior.
//
// The genuinely new assertion here (beyond
// TestReceiveCallback_WinWithNoPriorBetIsIntegrityAlert, which proves the
// rejection itself) is the RECOVERY half: the rejected win leaves no
// tombstone, no poisoned provider reference and no ledger trace, so when
// the delayed bet finally arrives and the provider redelivers the same
// win, the round settles correctly and exactly once. An out-of-order
// delivery must be recoverable, not terminal.
func TestFailureModeMatrix_D_WinBeforeBetIsRejectedAndTheRoundStillRecovers(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const roundID = "round-fm-d"
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", roundID)
	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-fm-d", "", roundID, "game-1", 2500, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if !errors.Is(err, ErrBetNotFound) {
		t.Fatalf("expected ErrBetNotFound for a win that arrives before its bet, got %v", err)
	}
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", "win-fm-d"); count != 0 {
		t.Fatalf("a rejected out-of-order win must post nothing, got %d ledger_transactions rows", count)
	}
	txCount, entryCount, _, _ := fmCorrelation(t, pool, f.tenantID, correlationID)
	if txCount != 0 || entryCount != 0 {
		t.Fatalf("expected no ledger trace for the rejected win, got tx=%d entries=%d", txCount, entryCount)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the balance untouched by a rejected out-of-order win, got %d", balance)
	}

	// The delayed bet finally arrives...
	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-fm-d", "", roundID, "game-1", 900, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	}); err != nil {
		t.Fatalf("late bet: %v", err)
	}
	// ...and the SAME win payload, redelivered, now settles.
	var winResult ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		winResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	}); err != nil {
		t.Fatalf("redelivered win after the late bet: %v", err)
	}
	if winResult.Outcome != OutcomeSucceeded {
		t.Fatalf("expected the redelivered win to settle, got %+v", winResult)
	}
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", "win-fm-d"); count != 1 {
		t.Fatalf("expected exactly one win transaction after recovery, got %d", count)
	}
	txCount, entryCount, debits, credits := fmCorrelation(t, pool, f.tenantID, correlationID)
	if txCount != 2 || entryCount != 4 {
		t.Fatalf("expected two transactions / four entries for the recovered round, got tx=%d entries=%d", txCount, entryCount)
	}
	if debits != credits {
		t.Fatalf("invariant #1 violated for the recovered round: debits=%d credits=%d", debits, credits)
	}
	if balance := cashBalance(t, pool, f); balance != 6600 {
		t.Fatalf("expected 5000-900+2500=6600 after recovery, got %d", balance)
	}
}

// TestFailureModeMatrix_D_LateOriginalAfterTombstoneIsRejectedWithNoLedgerEffect
// is the other out-of-order case CLAUDE.md names explicitly: "A rollback
// for a transaction never seen writes a tombstone so a late-arriving
// original is rejected." The rollback arrives first (tombstone written),
// then the bet it reverses finally shows up.
//
// Stage 10.3 CAS-CAP-ROLLBACK-1 (E3, §1.3): this used to surface as an
// undifferentiated wrapped error from ledger.Post's conflict path (F8) -
// diagnosability-poor and, at the HTTP layer, an untyped 500 inviting
// endless provider retries. It is now a NAMED, deterministic decline
// (postBet's own established decline-without-error convention, the same
// shape as an insufficient-funds decline): ReceiveCallback returns
// (ReceiveCallbackResult{Outcome: OutcomeDeclined, DeclineReason:
// "original_rolled_back"}, nil), not a Go error - checked BEFORE ever
// reaching ledger.Post, via the tombstone check postBet now runs right
// after its idempotency short-circuit. The financial outcome (zero
// effect) is unchanged; only the outcome's SHAPE improved.
func TestFailureModeMatrix_D_LateOriginalAfterTombstoneIsRejectedWithNoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const roundID = "round-fm-d-tombstone"
	const betRef = "bet-fm-d-late"

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-fm-d-early", betRef, roundID, "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	var rollbackResult ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rollbackResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	}); err != nil {
		t.Fatalf("early rollback: %v", err)
	}
	if !rollbackResult.Tombstoned {
		t.Fatalf("expected a tombstone for a rollback of a never-seen original, got %+v", rollbackResult)
	}

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, betRef, "", roundID, "game-1", 700, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var lateResult ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		lateResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	}); err != nil {
		t.Fatalf("a late-arriving original whose rollback already tombstoned it must be a named DECLINE, not a Go error: %v", err)
	}
	if lateResult.Outcome != OutcomeDeclined || lateResult.DeclineReason != "original_rolled_back" {
		t.Fatalf("expected outcome=declined, decline_reason=original_rolled_back for a late-arriving tombstoned original, got %+v", lateResult)
	}

	// The only row for this reference is the tombstone itself - no bet was
	// posted, and the tombstone carries no entries, so there is no debit
	// and no credit anywhere.
	var rowCount, entryCount int
	var txType string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2`,
			f.tenantID, betRef).Scan(&rowCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT transaction_type FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2`,
			f.tenantID, betRef).Scan(&txType); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_entries e
			   JOIN ledger_transactions lt ON lt.id = e.ledger_transaction_id
			  WHERE lt.tenant_id = $1 AND lt.provider_id = 'mock-casino' AND lt.provider_tx_id = $2`,
			f.tenantID, betRef).Scan(&entryCount)
	}); err != nil {
		t.Fatalf("inspect tombstoned reference: %v", err)
	}
	if rowCount != 1 || txType != string(ledger.TxTombstone) || entryCount != 0 {
		t.Fatalf("expected exactly one entryless tombstone row for %s, got rows=%d type=%q entries=%d", betRef, rowCount, txType, entryCount)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the balance untouched by the rejected late original, got %d", balance)
	}
	tenantDebits, tenantCredits := sumDebitsCredits(t, pool, f.tenantID)
	if tenantDebits != tenantCredits {
		t.Fatalf("tenant-wide invariant #1 violated: debits=%d credits=%d", tenantDebits, tenantCredits)
	}
}

// --- E. Malformed transaction ---

// TestFailureModeMatrix_E_MalformedAmountsAndUnknownAssetRejectedCleanly
// covers item E: a zero amount, a negative amount, and an asset code that
// does not exist in the `assets` registry. Each must be a clean rejection
// with zero ledger effect - never a panic, never a partial write, never a
// zero-value posting.
//
// Worth recording explicitly, because it is a property of the ORDER of
// checks rather than of the asset registry: an unknown asset code on a bet
// never reaches the ledger at all - postBet compares the event's asset
// against the LAUNCH SESSION's own asset first, so a bogus asset is
// rejected as a session mismatch. The registry foreign key
// (ledger_accounts.asset_code REFERENCES assets(code), migration 0020) is
// the second line of defence, proven separately below by trying to create
// a house account in an unregistered asset directly.
func TestFailureModeMatrix_E_MalformedAmountsAndUnknownAssetRejectedCleanly(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	cases := []struct {
		name         string
		providerTxID string
		roundID      string
		amount       int64
		assetCode    string
	}{
		{"zero amount", "bet-fm-e-zero", "round-fm-e-zero", 0, "EUR"},
		{"negative amount", "bet-fm-e-negative", "round-fm-e-negative", -750, "EUR"},
		{"unregistered asset", "bet-fm-e-asset", "round-fm-e-asset", 500, "ZZZ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, tc.providerTxID, "", tc.roundID, "game-1", tc.amount, tc.assetCode, OutcomeSucceeded, "", f.playerAccountID, sessionID)
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
			if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", tc.providerTxID); count != 0 {
				t.Fatalf("a malformed callback must post nothing, got %d ledger_transactions rows", count)
			}
			correlationID := roundCorrelationID(f.tenantID, "mock-casino", tc.roundID)
			txCount, entryCount, _, _ := fmCorrelation(t, pool, f.tenantID, correlationID)
			if txCount != 0 || entryCount != 0 {
				t.Fatalf("expected no ledger trace, got tx=%d entries=%d", txCount, entryCount)
			}
			if balance := cashBalance(t, pool, f); balance != 5000 {
				t.Fatalf("expected the balance untouched, got %d", balance)
			}
		})
	}

	// A win in an asset the round's own bet never used is rejected by the
	// same class of cross-check (postWin compares the event's asset to the
	// WALLET the round's bet actually debited).
	t.Run("win in a mismatched asset", func(t *testing.T) {
		betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-fm-e-winasset", "", "round-fm-e-winasset", "game-1", 400, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
			return err
		}); err != nil {
			t.Fatalf("bet: %v", err)
		}
		before := cashBalance(t, pool, f)

		winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-fm-e-asset", "", "round-fm-e-winasset", "game-1", 900, "ZZZ", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", winPayload)
			return err
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for a win in a foreign asset, got %v", err)
		}
		if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", "win-fm-e-asset"); count != 0 {
			t.Fatalf("a mismatched-asset win must post nothing, got %d rows", count)
		}
		if balance := cashBalance(t, pool, f); balance != before {
			t.Fatalf("expected the balance unchanged at %d, got %d", before, balance)
		}
	})

	// Second line of defence: even if a caller somehow reached the ledger
	// with an unregistered asset, the assets-registry foreign key rejects
	// the account before any entry can exist.
	t.Run("assets registry foreign key rejects an unregistered asset", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountHouseGaming, "ZZZ")
			return err
		})
		if err == nil {
			t.Fatal("expected creating a ledger account in an unregistered asset to fail")
		}
		var count int
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts WHERE tenant_id = $1 AND asset_code = 'ZZZ'`, f.tenantID).Scan(&count)
		}); err != nil {
			t.Fatalf("count ledger_accounts: %v", err)
		}
		if count != 0 {
			t.Fatalf("expected zero ledger_accounts rows in an unregistered asset, got %d", count)
		}
	})

	tenantDebits, tenantCredits := sumDebitsCredits(t, pool, f.tenantID)
	if tenantDebits != tenantCredits {
		t.Fatalf("tenant-wide invariant #1 violated: debits=%d credits=%d", tenantDebits, tenantCredits)
	}
}

// --- F. Provider failure (the mock's own failure modes) ---

// TestFailureModeMatrix_F_ProviderTransportFailureAtLaunchLeavesNoTrace
// covers item F as far as MockCasinoProvider can actually express it.
//
// What the mock CAN simulate: a transport-level failure of an OUTBOUND
// call (FailNextCall, one-shot, on Launch/Bet/Win/Rollback) and an
// unavailable provider (SetHealth with CircuitOpen). The latter is already
// covered by TestLaunchGame_UnhealthyProviderCircuitOpenRejected; this
// test covers the former at the ORCHESTRATOR level (the adapter-level
// assertion lives in conformance_test.go's "a simulated transport failure
// surfaces as an error, not a result").
//
// What the mock CANNOT simulate, and this file deliberately does not
// invent: an HTTP 5xx on the INBOUND callback path. There is no such
// thing - a callback is the provider calling US; a 5xx would be the
// platform's own response, and retrying it is the provider's business.
// MockCasinoProvider has no error-injection hook on HandleCallback at all
// (only signature/parse rejection), so there is no way to make a delivered
// callback fail "with a 5xx" without adding a mechanism to the mock, which
// this stage's scope forbids. Recorded as a gap in the report rather than
// papered over.
func TestFailureModeMatrix_F_ProviderTransportFailureAtLaunchLeavesNoTrace(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	provider.FailNextCall()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected a provider transport failure at launch to surface as an error")
	}

	// The launch transaction rolled back entirely, so not even the session
	// row survives - a failed launch leaves no session a later callback
	// could name, and no financial effect of any kind.
	var sessionCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM casino_launch_sessions WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerAccountID).Scan(&sessionCount)
	}); err != nil {
		t.Fatalf("count launch sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Fatalf("expected zero launch sessions after a failed launch, got %d", sessionCount)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the balance untouched by a failed launch, got %d", balance)
	}

	// FailNextCall is one-shot: the very next launch succeeds, proving the
	// failure above was the injected one and not a broken fixture.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		if err != nil {
			return err
		}
		if result.SessionID == uuid.Nil {
			return errors.New("expected a session id from the retried launch")
		}
		return nil
	}); err != nil {
		t.Fatalf("retried launch after a one-shot transport failure: %v", err)
	}
}

// --- H. Transaction naming an unknown session ---

// TestFailureModeMatrix_H_BetNamingAnUnknownSessionRejectedCleanly
// complements TestReceiveCallback_BetWithNoSessionRejected (which covers a
// MISSING session id) with the adjacent case: a well-formed session id
// that simply does not exist. Both must be a clean, specific rejection
// (ErrLaunchSessionRequired) with no ledger effect - never a
// generic not-found the caller cannot distinguish, and never a bet
// attributed to some other session.
func TestFailureModeMatrix_H_BetNamingAnUnknownSessionRejectedCleanly(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-fm-h", "", "round-fm-h", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("expected ErrLaunchSessionRequired for an unknown session id, got %v", err)
	}
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", "bet-fm-h"); count != 0 {
		t.Fatalf("expected no ledger_transactions row, got %d", count)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the balance untouched, got %d", balance)
	}
}

// --- I. Cross-player transaction reference (orchestrator level) ---

// TestFailureModeMatrix_I_BetPayloadNamingAnotherPlayerDebitsOnlyTheSessionOwner
// is the BET-side counterpart to the existing
// TestReceiveCallback_WinCreditsBettorsWalletNeverPayloadPlayer (which
// proves the same property for wins). A validly-signed callback names
// player B in its payload while carrying player A's session id: the debit
// must land on A (the session's own owner, resolved from the platform's
// casino_launch_sessions row), and B must be financially untouched.
//
// Note this is the ORCHESTRATOR-level seam, distinct from the
// play-simulation seam Stage 7 already covers in
// internal/httpserver/casino_play_flow_integration_test.go
// (TestCasinoPlay_CrossPlayerSessionDenied and friends), which test the
// HTTP boundary's own player-scope check rather than postBet's resolution.
func TestFailureModeMatrix_I_BetPayloadNamingAnotherPlayerDebitsOnlyTheSessionOwner(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	otherPlayerID, otherWalletID := seedSecondPlayerWallet(t, pool, f)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const roundID = "round-fm-i"
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", roundID)
	// Signed correctly, but naming the OTHER player in the payload.
	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-fm-i", "", roundID, "game-1", 1100, "EUR", OutcomeSucceeded, "", otherPlayerID, sessionID)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	}); err != nil {
		t.Fatalf("bet naming another player: %v", err)
	}

	if balance := cashBalance(t, pool, f); balance != 3900 {
		t.Fatalf("expected the SESSION OWNER debited (5000-1100=3900), got %d", balance)
	}
	if balance := fmWalletCashBalance(t, pool, f.tenantID, otherWalletID); balance != 0 {
		t.Fatalf("the payload-named player must be financially untouched, got balance %d", balance)
	}
	// And the entries themselves name the session owner's wallet only.
	var otherEntryCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_entries e
			   JOIN ledger_accounts la ON la.id = e.ledger_account_id
			  WHERE e.tenant_id = $1 AND la.wallet_id = $2`, f.tenantID, otherWalletID).Scan(&otherEntryCount)
	}); err != nil {
		t.Fatalf("count entries against the other player's wallet: %v", err)
	}
	if otherEntryCount != 0 {
		t.Fatalf("expected zero ledger entries against the payload-named player's wallet, got %d", otherEntryCount)
	}
	fmAssertCorrelationBalanced(t, pool, f.tenantID, correlationID)
}

// --- J/K. Cross-tenant (and therefore cross-brand) reference ---

// TestFailureModeMatrix_JK_CrossTenantRollbackCannotReachAnotherTenantsTransaction
// proves item J/K at the ORCHESTRATOR level for the ROLLBACK path, which
// is the one case the existing TestReceiveCallback_CrossTenantBetIsInvisible
// does not cover (it proves the WIN path). Tenant A posts a bet; a
// validly-signed rollback naming that exact provider_tx_id is then
// delivered under tenant B.
//
// The property is structural, not a new check: postRollback's
// `SELECT ... FROM ledger_transactions WHERE tenant_id = $1 AND
// provider_id = $2 AND provider_tx_id = $3 FOR UPDATE` runs inside tenant
// B's RLS-scoped transaction, so tenant A's row is not merely filtered by
// the WHERE clause - it is invisible to the query. The lookup therefore
// takes the "original never seen" branch and writes tenant B's OWN
// tombstone, which is correct and carries no entries: tenant A's bet is
// not reversed, tenant A's balance does not move, and no entry exists in
// either tenant's ledger as a result.
//
// Cross-brand (item K) collapses into this same proof plus the existing
// brand-pinning suite (launch_session_brand_pinning_test.go): a session
// carries its own brand_id, and postBet resolves the wallet from the
// session, so a callback can never move money into a different brand's
// wallet than the one the round was launched under.
func TestFailureModeMatrix_JK_CrossTenantRollbackCannotReachAnotherTenantsTransaction(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	fundWallet(t, pool, fA, 5000)
	fundWallet(t, pool, fB, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, fA, provider, 100)
	registerCasinoCapability(t, pool, fB, provider, 100)
	sessionID := mintSession(t, pool, fA, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const betRef = "bet-fm-jk"
	const roundID = "round-fm-jk"
	betPayload := provider.CallbackPayload(fA.tenantID, CallbackEventBet, betRef, "", roundID, "game-1", 1300, "EUR", OutcomeSucceeded, "", fA.playerAccountID, sessionID)
	if err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, fA.tenantID, "mock-casino", betPayload)
		return err
	}); err != nil {
		t.Fatalf("tenant A bet: %v", err)
	}
	tenantABetTxID := fmLedgerTxID(t, pool, fA.tenantID, "mock-casino", betRef)
	if tenantABetTxID == uuid.Nil {
		t.Fatal("fixture precondition failed: tenant A's bet did not post")
	}

	// Tenant B's callback names tenant A's provider_tx_id.
	rollbackPayload := provider.CallbackPayload(fB.tenantID, CallbackEventRollback, "rollback-fm-jk", betRef, roundID, "game-1", 0, "EUR", "", "", fB.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, fB.tenantID, "mock-casino", rollbackPayload)
		return err
	}); err != nil {
		t.Fatalf("tenant B rollback naming tenant A's reference: %v", err)
	}
	if !result.Tombstoned {
		t.Fatalf("expected tenant B's rollback to tombstone (tenant A's original is invisible to it), got %+v", result)
	}

	// Tenant A is completely unaffected: balance intact, bet not reversed.
	if balance := cashBalance(t, pool, fA); balance != 3700 {
		t.Fatalf("tenant A's balance must be unchanged by tenant B's rollback (5000-1300=3700), got %d", balance)
	}
	var reversalCount int
	if err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND reverses_transaction_id = $2`,
			fA.tenantID, tenantABetTxID).Scan(&reversalCount)
	}); err != nil {
		t.Fatalf("count reversals of tenant A's bet: %v", err)
	}
	if reversalCount != 0 {
		t.Fatalf("tenant A's bet must NOT have been reversed by tenant B's callback, found %d reversal(s)", reversalCount)
	}

	// Tenant B gained nothing: its tombstone carries no entries at all.
	if balance := cashBalance(t, pool, fB); balance != 5000 {
		t.Fatalf("tenant B's balance must be unchanged by its own tombstoned rollback, got %d", balance)
	}
	var tenantBEntryCount int
	if err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_entries e
			   JOIN ledger_transactions lt ON lt.id = e.ledger_transaction_id
			  WHERE lt.tenant_id = $1 AND lt.provider_id = 'mock-casino' AND lt.provider_tx_id = $2`,
			fB.tenantID, betRef).Scan(&tenantBEntryCount)
	}); err != nil {
		t.Fatalf("count tenant B entries: %v", err)
	}
	if tenantBEntryCount != 0 {
		t.Fatalf("expected zero entries under tenant B for the tombstoned reference, got %d", tenantBEntryCount)
	}

	for _, f := range []casinoFixture{fA, fB} {
		debits, credits := sumDebitsCredits(t, pool, f.tenantID)
		if debits != credits {
			t.Fatalf("invariant #1 violated for tenant %s: debits=%d credits=%d", f.tenantID, debits, credits)
		}
	}
}
