//go:build integration

// Deterministic lock-interleaving harness for this package's share of the
// ADR 0082 (canonical financial lock ordering) test plan, §6 group A.
//
// Deliberately a local copy of internal/ledger's own lockorder harness
// rather than a shared helper package: this repository keeps test helpers
// per-package (see internal/casino/stage9_concurrency_integration_test.go's
// own note on deliberately not importing internal/withdrawal's helpers),
// and a shared *test* package would have to live in the production tree.
// The copy is small and its rationale lives in one place - read
// internal/ledger/lockorder_harness_test.go's file comment for the full
// explanation of why these tests use a forced interleaving rather than a
// goroutine race, and for the canonical six-step sequence loRunABBA
// implements.
package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

const loLockWaitTimeout = 20 * time.Second

// loIsDeadlock reports whether err is (or wraps) Postgres SQLSTATE 40P01,
// "deadlock detected" - the exact production symptom ADR 0082 eliminates.
func loIsDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func loDescribeDeadlock(err error) string {
	if err == nil {
		return "<nil>"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("SQLSTATE %s: %s (detail: %s)", pgErr.Code, pgErr.Message, pgErr.Detail)
	}
	return err.Error()
}

// loBlockingPIDs asks the server which backends pid is waiting on, across
// every lock type - row locks, index insertion waits and ADVISORY locks
// alike. The last of those is why a bare "wait_event_type = 'Lock'" count
// is not sufficient for this suite.
func loBlockingPIDs(t *testing.T, pool *db.Pool, pid int) []int {
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

func loContains(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

func loWaitBlocked(t *testing.T, pool *db.Pool, pid int, done <-chan struct{}) ([]int, bool) {
	t.Helper()
	deadline := time.Now().Add(loLockWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return nil, false
		default:
		}
		if pids := loBlockingPIDs(t, pool, pid); len(pids) > 0 {
			return pids, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, false
}

// loWaitBlockedByAnyOf polls until pid is blocked by at least one of want,
// or until the racer finishes. Returns false only on timeout.
func loWaitBlockedByAnyOf(t *testing.T, pool *db.Pool, pid int, want []int, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(loLockWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return true
		default:
		}
		for _, c := range loBlockingPIDs(t, pool, pid) {
			for _, w := range want {
				if c == w {
					return true
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

type loBlocker struct {
	name    string
	pid     int
	release func()
}

// loHoldProjectionRow holds a FOR UPDATE row lock on ledgerAccountID's
// wallet_balance_projection row until release() is called.
//
// A TEST blocker deliberately issuing the projection FOR UPDATE that ADR
// 0082 R4 forbids in PRODUCTION code outside internal/ledger; R4 is a
// constraint on the production lock graph, and
// TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage scopes itself to
// non-test files for exactly this reason. Holding the row from outside is
// the only way to make a production caller queue at a known point.
func loHoldProjectionRow(t *testing.T, pool *db.Pool, tenantID, ledgerAccountID uuid.UUID, name string) *loBlocker {
	t.Helper()
	return loHoldWith(t, pool, tenantID, name, func(ctx context.Context, tx pgx.Tx) error {
		var d, c int64
		err := tx.QueryRow(ctx,
			`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
			ledgerAccountID).Scan(&d, &c)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("blocker %s: no projection row for %s to lock - fund/seed the account first", name, ledgerAccountID)
		}
		return err
	})
}

// loHoldWith opens a tenant-scoped transaction, runs acquire, and holds
// every lock acquire took until the returned release() is called.
func loHoldWith(t *testing.T, pool *db.Pool, tenantID uuid.UUID, name string,
	acquire func(ctx context.Context, tx pgx.Tx) error) *loBlocker {
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
		t.Fatalf("blocker %q failed to acquire its lock: %v", name, r.err)
	}
	var once sync.Once
	b := &loBlocker{name: name, pid: r.pid}
	b.release = func() {
		once.Do(func() {
			close(proceed)
			if err := <-done; err != nil {
				t.Errorf("blocker %q transaction: %v", name, err)
			}
		})
	}
	t.Cleanup(b.release)
	return b
}

type loRacer struct {
	name string
	pid  int
	done chan struct{}
	err  error
}

// loStartRacer runs body in its own tenant-scoped transaction, publishing
// its backend pid before body's first statement so the harness watches
// exactly this backend and can never mistake a concurrently-running test
// in another package for one of ours.
func loStartRacer(t *testing.T, pool *db.Pool, tenantID uuid.UUID, name string,
	body func(ctx context.Context, tx pgx.Tx) error) *loRacer {
	t.Helper()
	r := &loRacer{name: name, done: make(chan struct{})}
	pidCh := make(chan int, 1)
	go func() {
		defer close(r.done)
		r.err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				pidCh <- 0
				return err
			}
			pidCh <- pid
			return body(ctx, tx)
		})
	}()
	r.pid = <-pidCh
	if r.pid == 0 {
		<-r.done
		t.Fatalf("racer %q never reported a backend pid: %v", name, r.err)
	}
	return r
}

func (r *loRacer) wait() error {
	<-r.done
	return r.err
}

// loRunABBA executes the canonical six-step sequence (documented in
// internal/ledger/lockorder_harness_test.go) and returns both racers'
// errors. It asserts nothing itself: "no 40P01" alone is not evidence
// that the right thing was posted, so every caller adds its own financial
// assertions on top.
// startA and startB are FUNCTIONS, not already-started racers, and that
// is load-bearing: A must be started and observed blocked BEFORE B is
// started at all. Postgres hands a released lock to the FIRST waiter in
// its queue, so if both racers were launched together and happened to
// want the same row first, B could win the queue and the interleaving the
// test depends on would never occur. Starting them in a defined order
// makes A's position in every wait queue deterministic.
func loRunABBA(t *testing.T, pool *db.Pool, startA, startB func() *loRacer, blockers []*loBlocker) (a, b *loRacer, errA, errB error) {
	t.Helper()

	a = startA()
	blockedA, ok := loWaitBlocked(t, pool, a.pid, a.done)
	if !ok {
		loReleaseAll(blockers)
		t.Fatalf("racer %q never blocked on a held lock; the interleaving this test depends on did not happen (err=%v)",
			a.name, a.wait())
	}
	b = startB()
	if _, ok := loWaitBlocked(t, pool, b.pid, b.done); !ok {
		loReleaseAll(blockers)
		_, _ = a.wait(), b.wait()
		t.Fatalf("racer %q never blocked on a held lock; the interleaving this test depends on did not happen (err=%v)",
			b.name, b.err)
	}

	var releasedPIDs []int
	var remaining []*loBlocker
	for _, bl := range blockers {
		if loContains(blockedA, bl.pid) {
			releasedPIDs = append(releasedPIDs, bl.pid)
			bl.release()
		} else {
			remaining = append(remaining, bl)
		}
	}
	if len(releasedPIDs) == 0 {
		loReleaseAll(blockers)
		_, _ = a.wait(), b.wait()
		t.Fatalf("racer %q is blocked by %v, none of which is one of this test's blockers - the harness cannot "+
			"sequence an interleaving it does not control", a.name, blockedA)
	}
	// Wait until A has not merely stopped waiting on the released blocker
	// but has ACQUIRED it and queued on the next one (or finished).
	//
	// The weaker condition - "no longer blocked by the released pid" - has
	// a real window in it: between acquiring the first lock and requesting
	// the second, A is blocked by nothing at all, and a poll landing in
	// that window would release the remaining blockers before A is in
	// their wait queues. That makes the interleaving timing-dependent,
	// which is exactly what this harness exists to avoid; it was observed
	// producing an intermittent false PASS against a deliberately
	// un-fixed build.
	if len(remaining) > 0 {
		// B's pid counts as "A reached the next lock" too, and that is not
		// a loosening - it is how Postgres actually reports this state.
		// A row-lock waiter takes the heavyweight TUPLE lock first and
		// only then waits on the holder's transaction id, so while B is
		// queued on the remaining blocker's row, B HOLDS that tuple lock;
		// a second waiter (A) therefore blocks on B, and
		// pg_blocking_pids(A) reports B rather than the blocker. Either
		// answer means the same thing here: A is past its first lock and
		// is now queued on the second one.
		wantPIDs := make([]int, 0, len(remaining)+1)
		for _, bl := range remaining {
			wantPIDs = append(wantPIDs, bl.pid)
		}
		wantPIDs = append(wantPIDs, b.pid)
		if !loWaitBlockedByAnyOf(t, pool, a.pid, wantPIDs, a.done) {
			loReleaseAll(remaining)
			_, _ = a.wait(), b.wait()
			t.Fatalf("racer %q never reached the next lock in the sequence after %v were released "+
				"(expected it to queue on one of %v)", a.name, releasedPIDs, wantPIDs)
		}
	}
	loReleaseAll(remaining)
	return a, b, a.wait(), b.wait()
}

func loReleaseAll(blockers []*loBlocker) {
	for _, b := range blockers {
		b.release()
	}
}

func loAssertNoDeadlock(t *testing.T, cycle string, results map[string]error) {
	t.Helper()
	for name, err := range results {
		if loIsDeadlock(err) {
			t.Fatalf("%s: %q aborted with a deadlock - the canonical lock order (ADR 0082) is not being "+
				"honoured on this path: %s", cycle, name, loDescribeDeadlock(err))
		}
	}
}

// loAssertBalanced is the invariant assertion every concurrency test makes
// afterwards: SUM(debits) == SUM(credits), per asset, tenant-wide. A
// deadlock never breaks it (the whole transaction rolls back), so this is
// not what detects the defect - it is what proves the FIX did not
// introduce one.
func loAssertBalanced(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	type row struct {
		asset           string
		debits, credits int64
	}
	var rows []row
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := tx.Query(ctx,
			`SELECT asset_code,
			        COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint,
			        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint
			   FROM ledger_entries WHERE tenant_id = $1 GROUP BY asset_code`, tenantID)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var x row
			if err := r.Scan(&x.asset, &x.debits, &x.credits); err != nil {
				return err
			}
			rows = append(rows, x)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("sum debits/credits: %v", err)
	}
	for _, x := range rows {
		if x.debits != x.credits {
			t.Fatalf("invariant #1 violated for asset %s: debits=%d credits=%d", x.asset, x.debits, x.credits)
		}
	}
}

// loAssertProjectionMatchesRebuild proves every wallet_balance_projection
// row is still exactly reproducible from ledger_entries alone - the same
// comparison reconciliation.RunLedgerVsProjection makes, and the property
// ADR 0082 R6's zero-totals rows must not disturb.
func loAssertProjectionMatchesRebuild(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM ledger_accounts WHERE tenant_id = $1 ORDER BY id`, tenantID)
		if err != nil {
			return err
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			rebuilt, err := ledger.RebuildBalance(ctx, tx, id)
			if err != nil {
				return err
			}
			projected, err := ledger.GetProjectedBalance(ctx, tx, id)
			if err != nil {
				return err
			}
			if !projected.Found {
				if rebuilt.DebitTotal != 0 || rebuilt.CreditTotal != 0 {
					return fmt.Errorf("account %s has entries (d=%d c=%d) but no projection row",
						id, rebuilt.DebitTotal, rebuilt.CreditTotal)
				}
				continue
			}
			if rebuilt.DebitTotal != projected.DebitTotal || rebuilt.CreditTotal != projected.CreditTotal ||
				rebuilt.AssetCode != projected.AssetCode || rebuilt.AccountType != projected.AccountType {
				return fmt.Errorf("account %s projection drifted from the ledger", id)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("projection vs. rebuild: %v", err)
	}
}
