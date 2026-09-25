//go:build integration

package sportsbook

// Stage 10 F-7 remediation (ADR 0020 amendment 2026-09-25,
// docs/governance/stage-10-f7-ledger-replay-audit.md §6.2/§6.3 site #18):
// ledger.Post now rejects a same-key replay whose correlation differs.
// PlaceBet mints betID (its correlation) per attempt, so the legitimate
// concurrent duplicate now reaches Post as a payload mismatch and must
// be resolved by PlaceBet itself - these tests prove it still returns the
// one bet, with one posting, and never a 500.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// raceSameKeyPlacements holds the player's cash projection row under an
// uncommitted blocker, starts one PlaceBet per stake with the SAME
// idempotency key, waits until both are blocked at L3 (so both have
// already missed findBetByIdempotencyKey), then releases them.
func raceSameKeyPlacements(t *testing.T, pool *db.Pool, f sbFixture, sel Selection, key string, stakes ...int64) ([]PlaceBetResult, []error) {
	t.Helper()
	var cashAccountID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cashAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		return err
	}); err != nil {
		t.Fatalf("resolve cash account: %v", err)
	}

	var blockerPID int32
	blockerReady := make(chan struct{})
	proceed := make(chan struct{})
	blockerErr := make(chan error, 1)
	go func() {
		blockerErr <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var d, c int64
			if err := tx.QueryRow(ctx,
				`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
				cashAccountID).Scan(&d, &c); err != nil {
				return err
			}
			var err error
			if blockerPID, err = backendPID(ctx, tx); err != nil {
				return err
			}
			close(blockerReady)
			<-proceed
			return nil
		})
	}()
	select {
	case <-blockerReady:
	case err := <-blockerErr:
		t.Fatalf("blocker failed before acquiring its lock: %v", err)
	}

	results := make([]PlaceBetResult, len(stakes))
	errs := make([]error, len(stakes))
	var wg sync.WaitGroup
	for i, stake := range stakes {
		wg.Add(1)
		go func(i int, stake int64) {
			defer wg.Done()
			results[i], errs[i] = placeBet(t, pool, f, sel, stake, key)
		}(i, stake)
	}
	if !waitForBlockedCount(t, pool, blockerPID, len(stakes)) {
		close(proceed)
		wg.Wait()
		t.Fatal("timed out waiting for the concurrent PlaceBet calls to block at L3")
	}
	close(proceed)
	wg.Wait()
	if err := <-blockerErr; err != nil {
		t.Fatalf("blocker: %v", err)
	}
	return results, errs
}

func TestPlaceBet_F7_ConcurrentIdenticalDuplicateReturnsOneBet(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	ledgerBefore := countLedgerTransactions(t, pool, f)

	results, errs := raceSameKeyPlacements(t, pool, f, sel, "f7-concurrent-identical", 1_000, 1_000)
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d: legitimate concurrent duplicate failed: %v", i, errs[i])
		}
		if !results[i].Accepted {
			t.Fatalf("attempt %d: not accepted (%q)", i, results[i].RejectionCategory)
		}
	}
	if results[0].Bet.ID != results[1].Bet.ID {
		t.Fatalf("two bet ids for one idempotency key: %s vs %s", results[0].Bet.ID, results[1].Bet.ID)
	}
	if results[0].Bet.LedgerTransactionID != results[1].Bet.LedgerTransactionID {
		t.Fatal("two ledger transactions reported for one bet")
	}
	if got := countBets(t, pool, f); got != 1 {
		t.Fatalf("bets = %d, want 1", got)
	}
	if got := countLedgerTransactions(t, pool, f) - ledgerBefore; got != 1 {
		t.Fatalf("new ledger transactions = %d, want 1", got)
	}
	if got := cashBalance(t, pool, f); got != 9_000 {
		t.Fatalf("player_cash = %d, want 9000 (stake taken once)", got)
	}
}

func TestPlaceBet_F7_ConcurrentSameKeyDifferentStakeRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	results, errs := raceSameKeyPlacements(t, pool, f, sel, "f7-concurrent-different", 1_000, 2_000)
	var accepted, reused int
	var winnerStake int64
	for i := range results {
		switch {
		case errs[i] == nil && results[i].Accepted:
			accepted++
			winnerStake = results[i].Bet.StakeAmount
		case errors.Is(errs[i], ErrBetIdempotencyKeyReused):
			reused++
		default:
			t.Fatalf("attempt %d: unexpected outcome res=%+v err=%v", i, results[i], errs[i])
		}
	}
	if accepted != 1 || reused != 1 {
		t.Fatalf("accepted=%d reused=%d, want 1 and 1", accepted, reused)
	}
	if got := countBets(t, pool, f); got != 1 {
		t.Fatalf("bets = %d, want 1", got)
	}
	if got := cashBalance(t, pool, f); got != 10_000-winnerStake {
		t.Fatalf("player_cash = %d, want %d", got, 10_000-winnerStake)
	}
}

// Audit §3 evidence gap for site #18: a sequential same-key retry with a
// different stake is ErrBetIdempotencyKeyReused, with no second effect.
func TestPlaceBet_F7_SequentialSameKeyDifferentStakeRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	if _, err := placeBet(t, pool, f, sel, 1_000, "f7-sequential-different"); err != nil {
		t.Fatalf("first placement: %v", err)
	}
	ledgerBefore := countLedgerTransactions(t, pool, f)
	if _, err := placeBet(t, pool, f, sel, 1_500, "f7-sequential-different"); !errors.Is(err, ErrBetIdempotencyKeyReused) {
		t.Fatalf("want ErrBetIdempotencyKeyReused, got %v", err)
	}
	if got := countLedgerTransactions(t, pool, f); got != ledgerBefore {
		t.Fatalf("ledger transactions %d -> %d on a rejected retry", ledgerBefore, got)
	}
	if got := cashBalance(t, pool, f); got != 9_000 {
		t.Fatalf("player_cash = %d, want 9000", got)
	}
}
