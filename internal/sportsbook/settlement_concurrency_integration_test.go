//go:build integration

// Concurrency safety (ADR 0088 §5, §14): two concurrent settle(1) on one
// bet must apply exactly once; settle racing void must resolve into
// exactly one consistent terminal state with no double-posting; and the
// LOST void-after-settlement racing a PlaceBet on the SAME wallet must not
// deadlock (§5.2's LockProjectionsForPostings union pre-lock is exactly
// what prevents the subset-then-superset deadlock a per-Post lock order
// would risk). All waits are deterministic: a blocker transaction holding
// the real row lock, pg_blocking_pids polled via this package's own
// waitForBlockedCount/backendPID (orchestrator_integration_test.go) -
// never a sleep-based race.
package sportsbook

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// lockBetRowBlocker is the blocker: it takes the SAME FOR UPDATE row lock
// (L1) SimulateSettlementEvent takes, so every settlement/void/rollback
// call against betID genuinely queues behind it until proceed is closed.
func lockBetRowBlocker(pool *db.Pool, tenantID, betID uuid.UUID, ready chan<- int32, proceed <-chan struct{}, done chan<- error) {
	done <- pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var discard uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM sportsbook_bets WHERE id = $1 FOR UPDATE`, betID).Scan(&discard); err != nil {
			return err
		}
		pid, err := backendPID(ctx, tx)
		if err != nil {
			return err
		}
		ready <- pid
		<-proceed
		return nil
	})
}

// lockCashAccountBlocker takes the same wallet_balance_projection row lock
// PlaceBet's and lockAndPost's L3 pre-lock take for f's player_cash
// account.
func lockCashAccountBlocker(pool *db.Pool, f sbFixture, ready chan<- int32, proceed <-chan struct{}, done chan<- error) {
	done <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		var d, c int64
		if err := tx.QueryRow(ctx,
			`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
			cashAccountID).Scan(&d, &c); err != nil {
			return err
		}
		pid, err := backendPID(ctx, tx)
		if err != nil {
			return err
		}
		ready <- pid
		<-proceed
		return nil
	})
}

// waitOrFatal fails the test if ch does not deliver within timeout -
// bounding the test rather than hanging forever if a genuine deadlock (or
// a regression reintroducing one) blocks a goroutine indefinitely.
func waitOrFatal[T any](t *testing.T, ch <-chan T, timeout time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s (possible deadlock)", what)
		var zero T
		return zero
	}
}

// TestSettlementConcurrency_TwoConcurrentSettlesOnlyOneApplies: two
// concurrent settle(1, won) calls against the SAME bet, forced to queue on
// the bet's L1 row lock by an uncommitted blocker holding it. Exactly one
// applies; the other is replayed, returning the SAME settlement record.
func TestSettlementConcurrency_TwoConcurrentSettlesOnlyOneApplies(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	ready := make(chan int32, 1)
	proceed := make(chan struct{})
	blockerDone := make(chan error, 1)
	go lockBetRowBlocker(pool, f.tenantID, betID, ready, proceed, blockerDone)
	blockerPID := waitOrFatal(t, ready, 5*time.Second, "blocker to acquire the bet row lock")

	type callResult struct {
		res SettlementResult
		err error
	}
	results := make(chan callResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
			results <- callResult{res, err}
		}()
	}

	if !waitForBlockedCount(t, pool, blockerPID, 2) {
		t.Fatalf("both settle calls never queued behind the blocker's bet row lock")
	}
	close(proceed)

	if err := waitOrFatal(t, blockerDone, 5*time.Second, "blocker transaction to finish"); err != nil {
		t.Fatalf("blocker transaction failed: %v", err)
	}

	first := waitOrFatal(t, results, 5*time.Second, "first settle call")
	second := waitOrFatal(t, results, 5*time.Second, "second settle call")
	if first.err != nil || second.err != nil {
		t.Fatalf("unexpected Go errors: %v / %v", first.err, second.err)
	}

	var applied, replayed int
	for _, r := range []callResult{first, second} {
		switch r.res.Result {
		case SettlementResultApplied:
			applied++
		case SettlementResultReplayed:
			replayed++
		default:
			t.Fatalf("unexpected result %q (rejected: %s)", r.res.Result, r.res.RejectionCode)
		}
	}
	if applied != 1 || replayed != 1 {
		t.Fatalf("expected exactly one applied and one replayed, got applied=%d replayed=%d", applied, replayed)
	}

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 1 {
		t.Fatalf("expected exactly 1 settlement history row, got %d", len(hist))
	}
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxSportsbookSettlement)); got != 1 {
		t.Fatalf("expected exactly 1 sportsbook_settlement ledger transaction, got %d", got)
	}
}

// TestSettlementConcurrency_SettleRacesVoid: settle(1) and void race on the
// same open bet. Whichever the database serializes first wins outright;
// the other is either rejected (BET_VOIDED / BET_ALREADY_SETTLED, requires
// a rollback first) or - if void loses the race - resolves as a composed
// void-after-settlement. Either way there must be no double-posting and
// the bet ends in exactly one terminal, history-consistent state.
func TestSettlementConcurrency_SettleRacesVoid(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	ready := make(chan int32, 1)
	proceed := make(chan struct{})
	blockerDone := make(chan error, 1)
	go lockBetRowBlocker(pool, f.tenantID, betID, ready, proceed, blockerDone)
	blockerPID := waitOrFatal(t, ready, 5*time.Second, "blocker to acquire the bet row lock")

	settleCh := make(chan struct {
		res SettlementResult
		err error
	}, 1)
	voidCh := make(chan struct {
		res SettlementResult
		err error
	}, 1)
	go func() {
		res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
		settleCh <- struct {
			res SettlementResult
			err error
		}{res, err}
	}()
	go func() {
		res, err := simulateSettlement(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
		voidCh <- struct {
			res SettlementResult
			err error
		}{res, err}
	}()

	if !waitForBlockedCount(t, pool, blockerPID, 2) {
		t.Fatalf("settle and void never both queued behind the blocker's bet row lock")
	}
	close(proceed)
	if err := waitOrFatal(t, blockerDone, 5*time.Second, "blocker transaction to finish"); err != nil {
		t.Fatalf("blocker transaction failed: %v", err)
	}

	settleOut := waitOrFatal(t, settleCh, 5*time.Second, "settle call")
	voidOut := waitOrFatal(t, voidCh, 5*time.Second, "void call")
	if settleOut.err != nil || voidOut.err != nil {
		t.Fatalf("unexpected Go errors: settle=%v void=%v", settleOut.err, voidOut.err)
	}

	finalStatus := betStatus(t, pool, f.tenantID, betID)
	hist := settlementHistory(t, pool, f.tenantID, betID)

	switch {
	case !settleOut.res.Rejected() && !voidOut.res.Rejected():
		// void lost the race against an already-settled bet and became a
		// composed void-after-settlement.
		if finalStatus != BetStatusVoid || len(hist) != 3 {
			t.Fatalf("settle-then-void: status=%q history=%d, want void/3", finalStatus, len(hist))
		}
	case !settleOut.res.Rejected() && voidOut.res.Rejected():
		if voidOut.res.RejectionCode != SettlementRejectBetAlreadySettled && voidOut.res.RejectionCode != SettlementRejectPayloadMismatch {
			t.Fatalf("void rejection code = %q, want BET_ALREADY_SETTLED-family", voidOut.res.RejectionCode)
		}
		if finalStatus != BetStatusSettledLost || len(hist) != 1 {
			t.Fatalf("settle-won-the-race: status=%q history=%d, want settled_lost/1", finalStatus, len(hist))
		}
	case settleOut.res.Rejected() && !voidOut.res.Rejected():
		if settleOut.res.RejectionCode != SettlementRejectBetVoided {
			t.Fatalf("settle rejection code = %q, want BET_VOIDED", settleOut.res.RejectionCode)
		}
		if finalStatus != BetStatusVoid || len(hist) != 1 {
			t.Fatalf("void-won-the-race: status=%q history=%d, want void/1", finalStatus, len(hist))
		}
	default:
		t.Fatalf("both settle and void were rejected: settle=%s void=%s", settleOut.res.RejectionCode, voidOut.res.RejectionCode)
	}
}

// countLedgerTransactionsByType counts ledger_transactions of txType in
// f's tenant - used to prove concurrency never double-posts.
func countLedgerTransactionsByType(t *testing.T, pool *db.Pool, f sbFixture, txType string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = $2`,
			f.tenantID, txType).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count %s ledger transactions: %v", txType, err)
	}
	return count
}

// TestSettlementConcurrency_VoidAfterSettlementRacesPlaceBetOnSameWallet is
// ADR 0088 §14's named test: the LOST void-after-settlement (two postings:
// rollback {LOCKED,HOUSE} then void {LOCKED,CASH}, pre-locked as ONE union
// via LockProjectionsForPostings, §5.2) racing a PlaceBet that also needs
// the wallet's CASH projection row. Both are forced to queue on the SAME
// blocked cash-account row lock; releasing it must let BOTH complete with
// no deadlock (Postgres would surface "deadlock detected" if the fix
// regressed to per-Post locking of subsets).
func TestSettlementConcurrency_VoidAfterSettlementRacesPlaceBetOnSameWallet(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))

	sel2 := seedSelection(t, pool, seedSelectionParams{})

	ready := make(chan int32, 1)
	proceed := make(chan struct{})
	blockerDone := make(chan error, 1)
	go lockCashAccountBlocker(pool, f, ready, proceed, blockerDone)
	blockerPID := waitOrFatal(t, ready, 5*time.Second, "blocker to acquire the cash account row lock")

	voidCh := make(chan struct {
		res SettlementResult
		err error
	}, 1)
	placeCh := make(chan struct {
		res PlaceBetResult
		err error
	}, 1)
	go func() {
		res, err := simulateSettlement(t, pool, f.tenantID, voidEvent(betID, actor, "data_error"))
		voidCh <- struct {
			res SettlementResult
			err error
		}{res, err}
	}()
	go func() {
		res, err := placeBet(t, pool, f, sel2, 500, "race-placebet-"+uuid.NewString())
		placeCh <- struct {
			res PlaceBetResult
			err error
		}{res, err}
	}()

	if !waitForBlockedCount(t, pool, blockerPID, 2) {
		t.Fatalf("void and PlaceBet never both queued behind the blocked cash account row (possible deadlock or a locking regression)")
	}
	close(proceed)
	if err := waitOrFatal(t, blockerDone, 5*time.Second, "blocker transaction to finish"); err != nil {
		t.Fatalf("blocker transaction failed: %v", err)
	}

	voidOut := waitOrFatal(t, voidCh, 10*time.Second, "void-after-settlement call")
	placeOut := waitOrFatal(t, placeCh, 10*time.Second, "PlaceBet call")

	if voidOut.err != nil {
		t.Fatalf("void-after-settlement must not error (deadlock?): %v", voidOut.err)
	}
	if voidOut.res.Rejected() {
		t.Fatalf("void-after-settlement rejected: %s", voidOut.res.RejectionCode)
	}
	if placeOut.err != nil {
		t.Fatalf("PlaceBet must not error (deadlock?): %v", placeOut.err)
	}
	if !placeOut.res.Accepted {
		t.Fatalf("PlaceBet rejected: %s", placeOut.res.RejectionCategory)
	}

	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusVoid {
		t.Fatalf("first bet status = %q, want void", got)
	}
}
