//go:build integration

// Deterministic lock-interleaving harness for the ADR 0082 (canonical
// financial lock ordering) test suite.
//
// WHY A HARNESS AND NOT A GOROUTINE RACE. An ABBA deadlock test that just
// starts two goroutines and hopes they interleave proves nothing: it
// passes on a broken build roughly as often as it fails, and it passes on
// a fixed build for the wrong reason. Every deadlock-freedom test in this
// dispatch is instead driven to a KNOWN state before anything is
// released, using this codebase's own established technique (an
// uncommitted blocker transaction holding a lock the racing calls must
// also take, plus polling of the server's own lock graph) - the same
// shape internal/casino/stage9_concurrency_integration_test.go and
// internal/jurisdiction/tenant_licence_admin_integration_test.go use.
//
// Two refinements over the existing helpers, both needed here:
//
//  1. pg_blocking_pids(pid) instead of a bare
//     "count(*) WHERE wait_event_type = 'Lock'" poll. The bare count
//     cannot tell WHICH lock a backend is waiting on, and this suite has
//     to release blockers in a specific order relative to a specific
//     racer. pg_blocking_pids also covers ADVISORY locks, which the
//     LOCK-1d test needs and a row-lock-only poll would miss.
//  2. Each racing goroutine reports its own backend pid
//     (SELECT pg_backend_pid()) as the first statement of its
//     transaction, so the harness never has to guess which backend is
//     which - and so a concurrently-running test in another package
//     cannot be mistaken for one of ours.
//
// The canonical sequence every deadlock test below follows:
//
//  1. blockers take and hold BOTH contended locks
//  2. start racer A; wait until A is genuinely blocked
//  3. start racer B; wait until B is genuinely blocked
//  4. release exactly the blocker(s) A is waiting on
//  5. wait until A has moved on (it is now either finished, or blocked
//     on something else - typically the OTHER blocker, holding the lock
//     it acquired in step 4)
//  6. release every remaining blocker
//
// On a build with the ABBA defect, step 6 hands the second lock to B,
// which then wants the lock A acquired in step 4 while A wants the one B
// just took: Postgres detects the cycle and aborts one of them with
// SQLSTATE 40P01. On a build with ADR 0082's canonical ordering, both
// racers want the same lock first, so step 6 simply lets them run in
// series. That is why these tests are required to be demonstrated FAILING
// on HEAD before the fix: a test that cannot fail proves nothing.
package ledger

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
)

// loLockWaitTimeout bounds every poll below. Generous relative to
// Postgres's own deadlock_timeout (1s by default) so a genuine deadlock is
// always detected and reported by the server before this fires - a
// timeout here means the harness failed to reach the state it wanted, not
// that a deadlock was missed.
const loLockWaitTimeout = 20 * time.Second

// loIsDeadlock reports whether err is (or wraps) Postgres SQLSTATE 40P01,
// "deadlock detected" - the exact production symptom ADR 0082 exists to
// eliminate. Nothing is mis-posted when this fires (the whole transaction
// rolls back, so SUM(debits) == SUM(credits) still holds); it aborts a
// bet, a win, a deposit or a withdrawal transition under precisely the
// concurrent load where that is least acceptable.
func loIsDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// loDescribeDeadlock renders err for a failure message, naming 40P01
// explicitly when present so a failure reads as "the lock ordering
// regressed", not as a generic database error.
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

// loBlockingPIDs returns the server's own answer to "which backends is
// pid waiting on", across every lock type (row locks, index insertion
// waits and advisory locks alike).
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

// loWaitBlocked polls until pid is blocked by at least one other backend,
// and returns the blockers. Returns ok == false if the racer finished
// (done fired) or the timeout elapsed - both of which mean the harness
// never reached the state it needed and the test must fail loudly rather
// than continue against an unknown interleaving.
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

// loBlocker is one held lock plus the means to let it go.
type loBlocker struct {
	name    string
	pid     int
	release func()
}

// loContains reports whether pid appears in pids.
func loContains(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// loHoldProjectionRow opens a transaction that takes a FOR UPDATE row
// lock on ledgerAccountID's wallet_balance_projection row and holds it
// until release() is called.
//
// This is a TEST blocker, deliberately issuing the projection FOR UPDATE
// that ADR 0082 R4 forbids in production code outside internal/ledger:
// R4 is a constraint on the production lock graph, and
// TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage (which enforces
// it permanently) scopes itself to non-test files for exactly this
// reason. Holding the row from outside is the only way to force a
// production caller to queue at a known point.
func loHoldProjectionRow(t *testing.T, pool *db.Pool, tenantID, ledgerAccountID uuid.UUID, name string) *loBlocker {
	t.Helper()
	return loHoldWith(t, pool, tenantID, name, func(ctx context.Context, tx pgx.Tx) error {
		var d, c int64
		err := tx.QueryRow(ctx,
			`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
			ledgerAccountID).Scan(&d, &c)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("blocker %s: no projection row for %s to lock - seed one first", name, ledgerAccountID)
		}
		return err
	})
}

var errLoBlockerRollback = errors.New("lockorder test blocker: deliberate rollback")

// loHoldWith opens a tenant-scoped transaction, runs acquire, and holds
// the transaction open (so every lock acquire took stays held) until the
// returned release() is called, at which point it commits.
func loHoldWith(t *testing.T, pool *db.Pool, tenantID uuid.UUID, name string,
	acquire func(ctx context.Context, tx pgx.Tx) error) *loBlocker {
	t.Helper()
	return loHold(t, pool, tenantID, name, acquire, false)
}

// loHoldWithRollback is loHoldWith but rolls the blocker transaction back
// instead of committing, so anything it inserted is gone by the time the
// racers proceed.
func loHoldWithRollback(t *testing.T, pool *db.Pool, tenantID uuid.UUID, name string,
	acquire func(ctx context.Context, tx pgx.Tx) error) *loBlocker {
	t.Helper()
	return loHold(t, pool, tenantID, name, acquire, true)
}

func loHold(t *testing.T, pool *db.Pool, tenantID uuid.UUID, name string,
	acquire func(ctx context.Context, tx pgx.Tx) error, rollback bool) *loBlocker {
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
			if rollback {
				return errLoBlockerRollback
			}
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
			if err := <-done; err != nil && !errors.Is(err, errLoBlockerRollback) {
				t.Errorf("blocker %q transaction: %v", name, err)
			}
		})
	}
	t.Cleanup(b.release)
	return b
}

// loRacer is one of the two concurrent money-path calls under test.
type loRacer struct {
	name string
	pid  int
	done chan struct{}
	err  error
}

// loStartRacer runs body in its own tenant-scoped transaction, publishing
// the backend pid before body's first statement so the harness can watch
// exactly this backend in the server's lock graph.
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

// loRunABBA executes the canonical six-step sequence documented at the
// top of this file against two already-started racers and a set of held
// blockers, then waits for both racers and returns their errors.
//
// It deliberately does NOT assert anything: each test asserts its own
// financial properties on top of "neither racer deadlocked", because
// "no 40P01" alone is not evidence that the right thing was posted.
//
// startA and startB are FUNCTIONS, not already-started racers, and that is
// load-bearing: A must be started and observed blocked BEFORE B is started
// at all. Postgres hands a released lock to the FIRST waiter in its queue,
// so if both racers were launched together and happened to want the same
// row first, B could win the queue and the interleaving the test depends
// on would never occur. Starting them in a defined order makes A's
// position in every wait queue deterministic.
func loRunABBA(t *testing.T, pool *db.Pool, startA, startB func() *loRacer, blockers []*loBlocker) (a, b *loRacer, errA, errB error) {
	t.Helper()

	a = startA()
	blockedA, ok := loWaitBlocked(t, pool, a.pid, a.done)
	if !ok {
		releaseAll(blockers)
		t.Fatalf("racer %q never blocked on a held lock; the interleaving this test depends on did not happen (err=%v)",
			a.name, a.wait())
	}
	b = startB()
	if _, ok := loWaitBlocked(t, pool, b.pid, b.done); !ok {
		releaseAll(blockers)
		_, _ = a.wait(), b.wait()
		t.Fatalf("racer %q never blocked on a held lock; the interleaving this test depends on did not happen (b.err=%v)",
			b.name, b.err)
	}

	// Step 4: release exactly the blockers A is waiting on, so A - and
	// only A - moves forward and takes the first lock of the prospective
	// cycle.
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
		releaseAll(blockers)
		_, _ = a.wait(), b.wait()
		t.Fatalf("racer %q was blocked by %v, none of which is one of this test's blockers - the harness cannot "+
			"sequence an interleaving it does not control", a.name, blockedA)
	}

	// Step 5: wait for A to actually acquire it.
	// Wait until A has not merely stopped waiting on the released blocker
	// but has ACQUIRED it and queued on the next one (or finished).
	//
	// The weaker condition - "no longer blocked by the released pid" - has
	// a real window in it: between acquiring the first lock and requesting
	// the second, A is blocked by nothing at all, and a poll landing in
	// that window would release the remaining blockers before A is in
	// their wait queues. That makes the interleaving timing-dependent,
	// which is exactly what this harness exists to avoid; it was observed
	// producing an intermittent false PASS against a deliberately un-fixed
	// build.
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
			releaseAll(remaining)
			_, _ = a.wait(), b.wait()
			t.Fatalf("racer %q never reached the next lock in the sequence after %v were released "+
				"(expected it to queue on one of %v)", a.name, releasedPIDs, wantPIDs)
		}
	}

	// Step 6: let everything else go. On a defective build this is where
	// the cycle closes.
	releaseAll(remaining)
	return a, b, a.wait(), b.wait()
}

func releaseAll(blockers []*loBlocker) {
	for _, b := range blockers {
		b.release()
	}
}

// loAssertNoDeadlock is the shared assertion for every group-A test: a
// 40P01 from either racer is the exact defect ADR 0082 closes, and is
// reported as such rather than as an anonymous error.
func loAssertNoDeadlock(t *testing.T, cycle string, results map[string]error) {
	t.Helper()
	for name, err := range results {
		if loIsDeadlock(err) {
			t.Fatalf("%s: %q aborted with a deadlock - the canonical lock order (ADR 0082) is not being "+
				"honoured on this path: %s", cycle, name, loDescribeDeadlock(err))
		}
	}
}

// loSumDebitsCredits is the invariant assertion every concurrency test in
// this dispatch makes afterwards: SUM(debits) == SUM(credits), per asset,
// tenant-wide. A deadlock never breaks it (the whole transaction rolls
// back), so this is not what detects the defect - it is what proves the
// FIX did not introduce one.
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

// loAssertProjectionMatchesRebuild proves the projection is still exactly
// reproducible from ledger_entries alone for every account in the tenant
// - the property R6's zero-totals rows must not disturb.
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
			rebuilt, err := RebuildBalance(ctx, tx, id)
			if err != nil {
				return err
			}
			projected, err := GetProjectedBalance(ctx, tx, id)
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
			if rebuilt.DebitTotal != projected.DebitTotal || rebuilt.CreditTotal != projected.CreditTotal {
				return fmt.Errorf("account %s projection drifted: rebuilt d=%d c=%d, projected d=%d c=%d",
					id, rebuilt.DebitTotal, rebuilt.CreditTotal, projected.DebitTotal, projected.CreditTotal)
			}
			if rebuilt.AssetCode != projected.AssetCode || rebuilt.AccountType != projected.AccountType {
				return fmt.Errorf("account %s projection identity drifted: rebuilt %s/%s, projected %s/%s",
					id, rebuilt.AssetCode, rebuilt.AccountType, projected.AssetCode, projected.AccountType)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("projection vs. rebuild: %v", err)
	}
}
