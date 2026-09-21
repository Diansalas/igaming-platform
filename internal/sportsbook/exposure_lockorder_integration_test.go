//go:build integration

// Stage 9.2 (ADR 0083 §12.2 item 30) - the composed lock-acquisition-order
// proof for `sportsbook.PlaceBet`'s FULL sequence (§7.2):
// L0.4 -> L0.5 -> L0.6 -> L3 -> L4 -> E-3, with the jurisdiction gate
// (Wave 2, steps 5-7) confirmed to take no lock at all before any of them.
//
// A local, minimal copy of internal/casino/lockorder_harness_test.go's
// blocking-PID technique (deliberately duplicated, not shared - see that
// file's own doc comment for why this codebase keeps lock-order test
// helpers per-package) - only the pieces this file's own proof needs:
// a way to hold a named lock via a blocker transaction, and a way to poll
// which pid(s) a given backend is currently blocked by
// (pg_blocking_pids), which is precise enough to prove ORDER between two
// specific locks, unlike the coarser wait_event_type='Lock' COUNT this
// package's own waitForBlockedCount already uses elsewhere.
package sportsbook

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

const sbLockOrderWaitTimeout = 20 * time.Second

func sbLockOrderBlockingPIDs(t *testing.T, pool *db.Pool, pid int) []int {
	t.Helper()
	var pids []int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(pg_blocking_pids($1), '{}')`, pid).Scan(&pids)
	})
	if err != nil {
		t.Fatalf("read pg_blocking_pids(%d): %v", pid, err)
	}
	return pids
}

func sbLockOrderContains(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

func sbLockOrderWaitBlockedBy(t *testing.T, pool *db.Pool, pid int, want int, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(sbLockOrderWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false
		default:
		}
		if sbLockOrderContains(sbLockOrderBlockingPIDs(t, pool, pid), want) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

type sbBlocker struct {
	pid     int
	release func()
}

// sbHoldWith opens a tenant-scoped transaction, runs acquire, and holds
// every lock acquire took until release() is called.
func sbHoldWith(t *testing.T, pool *db.Pool, tenantID uuid.UUID, acquire func(ctx context.Context, tx pgx.Tx) error) *sbBlocker {
	t.Helper()
	type ready struct {
		pid int
		err error
	}
	readyCh := make(chan ready, 1)
	proceed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				readyCh <- ready{err: err}
				return err
			}
			if err := acquire(ctx, tx); err != nil {
				readyCh <- ready{err: err}
				return err
			}
			readyCh <- ready{pid: pid}
			<-proceed
			return nil
		})
	}()
	r := <-readyCh
	if r.err != nil {
		<-done
		t.Fatalf("blocker failed to acquire its lock: %v", r.err)
	}
	var once sync.Once
	b := &sbBlocker{pid: r.pid}
	b.release = func() {
		once.Do(func() {
			close(proceed)
			if err := <-done; err != nil {
				t.Errorf("blocker transaction: %v", err)
			}
		})
	}
	t.Cleanup(b.release)
	return b
}

func sbHoldExposureAdvisory(t *testing.T, pool *db.Pool, tenantID, eventID uuid.UUID) *sbBlocker {
	return sbHoldWith(t, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sb_exposure:' || $1::text || ':' || $2::text, 0))`, tenantID, eventID)
		return err
	})
}

func sbHoldProjectionRow(t *testing.T, pool *db.Pool, tenantID, ledgerAccountID uuid.UUID) *sbBlocker {
	return sbHoldWith(t, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var d, c int64
		return tx.QueryRow(ctx,
			`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
			ledgerAccountID).Scan(&d, &c)
	})
}

// TestLockOrder_SportsbookPlaceBetAcquiresLocksInCanonicalOrder proves the
// §7.2 sequence directly: with an ARMED, generously-sized event-level
// exposure limit (so PlaceBet reaches and takes L0.6) and no RG/risk
// configuration at all (so L0.4/L0.5 are the RG person lock and a no-op
// respectively, neither of which anything else in this test holds), a
// fresh PlaceBet call must block FIRST on a held L0.6 (event-exposure
// advisory) and ONLY THEN, once L0.6 is released, on a held L3
// (wallet_balance_projection) row - never the other way around, and never
// on neither (which would mean the jurisdiction gate, RG or risk
// themselves are unexpectedly holding something incompatible before
// reaching L0.6, contradicting Wave 2's own "the jurisdiction gate takes
// no lock at all" design and this ADR's own step ordering).
func TestLockOrder_SportsbookPlaceBetAcquiresLocksInCanonicalOrder(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel, eventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	seedExposureLimit(t, pool, f.tenantID, nil, "event", "EUR", 1_000_000_000)

	cashAccountID := resolveCashAccountID(t, pool, f)

	l06 := sbHoldExposureAdvisory(t, pool, f.tenantID, eventID)
	l3 := sbHoldProjectionRow(t, pool, f.tenantID, cashAccountID)

	done := make(chan struct{})
	var result PlaceBetResult
	var placeErr error
	pidCh := make(chan int, 1)
	go func() {
		defer close(done)
		placeErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				pidCh <- 0
				return err
			}
			pidCh <- pid
			var err error
			result, err = PlaceBet(ctx, tx, PlaceBetParams{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
				SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1_000,
				ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
				IdempotencyKey: "lockorder-canonical",
			})
			return err
		})
	}()
	pid := <-pidCh
	if pid == 0 {
		<-done
		t.Fatalf("PlaceBet racer never reported a backend pid: %v", placeErr)
	}

	if !sbLockOrderWaitBlockedBy(t, pool, pid, l06.pid, done) {
		l06.release()
		l3.release()
		<-done
		t.Fatalf("expected PlaceBet to block on the held L0.6 (event-exposure) advisory lock FIRST - it never did (result=%+v err=%v)", result, placeErr)
	}

	l06.release()

	if !sbLockOrderWaitBlockedBy(t, pool, pid, l3.pid, done) {
		l3.release()
		<-done
		t.Fatalf("expected PlaceBet to proceed to the L3 wallet_balance_projection lock immediately after L0.6 was released - it never did (result=%+v err=%v)", result, placeErr)
	}

	l3.release()
	<-done
	if placeErr != nil {
		t.Fatalf("place bet: %v", placeErr)
	}
	if !result.Accepted {
		t.Fatalf("expected the bet to be accepted once both blockers released, got rejection %q/%q", result.RejectionCategory, result.RejectionCode)
	}
}

func resolveCashAccountID(t *testing.T, pool *db.Pool, f sbFixture) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("resolve player_cash account id: %v", err)
	}
	return id
}

// TestLockOrder_ConcurrentSportsbookBetsOnSameEventNoDeadlock proves two
// concurrent PlaceBet calls on the SAME event (an armed event-level
// exposure limit, so both genuinely contend for the SAME L0.6 key) never
// deadlock - they serialize cleanly at L0.6, exactly the accepted
// consequence ADR 0083 §6.2.5 documents ("concurrent bets on the same
// event serialize at this lock when a limit is configured").
func TestLockOrder_ConcurrentSportsbookBetsOnSameEventNoDeadlock(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 1_000_000)
	second := seedSecondPlayer(t, pool, f)
	fundWallet(t, pool, second, 1_000_000)
	sel, _, marketID := seedSelectionWithContext(t, pool, seedSelectionParams{})
	sibling := seedSiblingSelection(t, pool, marketID)
	// Generous enough that BOTH bets fit - this test is about absence of
	// deadlock under genuine L0.6 contention, not about which one wins.
	seedExposureLimit(t, pool, f.tenantID, nil, "event", "EUR", 1_000_000_000)

	const n = 2
	type res struct {
		result PlaceBetResult
		err    error
	}
	results := make([]res, n)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0].result, results[0].err = placeBet(t, pool, f, sel, 1_000, "lockorder-same-event-1")
	}()
	go func() {
		defer wg.Done()
		results[1].result, results[1].err = placeBet(t, pool, second, sibling, 1_000, "lockorder-same-event-2")
	}()
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			if isDeadlock(r.err) {
				t.Fatalf("goroutine %d: deadlock on the shared L0.6 event-exposure advisory lock: %v", i, r.err)
			}
			t.Fatalf("goroutine %d: unexpected error: %v", i, r.err)
		}
		if !r.result.Accepted {
			t.Fatalf("goroutine %d: expected acceptance (limit is generous), got rejection %q/%q", i, r.result.RejectionCategory, r.result.RejectionCode)
		}
	}
	loAssertBalancedSportsbook(t, pool, f.tenantID)
}

// loAssertBalancedSportsbook is a package-local, minimal mirror of
// internal/casino's loAssertBalanced (that package's own copy is
// unexported and this package must not import internal/casino).
func loAssertBalancedSportsbook(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var debits, credits int64
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint,
			        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint
			   FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits)
	})
	if err != nil {
		t.Fatalf("sum debits/credits: %v", err)
	}
	if debits != credits {
		t.Fatalf("invariant SUM(debits)==SUM(credits) violated: debits=%d credits=%d", debits, credits)
	}
}
