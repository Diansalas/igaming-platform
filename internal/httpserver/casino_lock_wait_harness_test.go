//go:build integration

// Small harness for gate 10.3-W1 ledger-finance condition C1: the L0.1
// race tests must prove WHERE the loser blocks (the casino_bet_delivery
// advisory lock itself, per ADR 0082 Amendment A6), not merely assert the
// eventual outcome - the same Stage 10.1 rule internal/payments' own
// lockorder_harness_test.go implements for its row-lock races. This is a
// much smaller harness than that one: it needs to prove only "a real
// production caller is genuinely waiting on an advisory lock", not force a
// specific multi-step interleaving, so it polls pg_stat_activity's
// wait_event_type/wait_event columns directly rather than reproducing the
// full loRunABBA machinery.
//
// H1 (ledger-finance re-verification after fix round A, gate 10.3-W1): the
// original poll was cluster-wide and unfiltered - CI runs
// `go test -tags=integration ./...` with packages in parallel against ONE
// database, so another package's unrelated advisory wait (or a stray
// connection from a different database on the same cluster) could satisfy
// it spuriously. The poll now:
//   - filters on datname = current_database(), so it can never observe a
//     waiter connected to a different database on the same cluster;
//   - excludes the blocker's OWN backend pid, so the harness never mistakes
//     the blocker holding the lock for a caller waiting on it;
//   - requires the waiting backend's own query text to match the
//     casino_bet_delivery advisory-lock statement specifically (the exact
//     literal embedded in acquireProviderTxDeliveryLock/
//     casHoldProviderTxDeliveryLock), so an unrelated advisory wait
//     elsewhere in the codebase (e.g. reconciliation's own advisory lock)
//     can never satisfy this check.
//
// Only ONE waiter is required (not two): the test shape here is always
// "the blocker holds the lock externally, and exactly one of two
// concurrent racers queues behind it" - the other racer either has not
// reached the lock yet or already returned, so requiring 2 simultaneous
// waiters would not match how these tests actually race.
package httpserver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const casLockWaitTimeout = 10 * time.Second

// casBlocker holds a manually-acquired L0.1 casino_bet_delivery advisory
// lock until release() is called. pid is this blocker's OWN backend pid
// (pg_backend_pid(), captured on the same connection/transaction that
// holds the lock) - casWaitForAdvisoryWaiter excludes it explicitly, so the
// blocker itself is never mistaken for a caller waiting on the lock it
// holds.
type casBlocker struct {
	pid     int32
	release func()
}

// casHoldProviderTxDeliveryLock acquires, from OUTSIDE production code,
// the EXACT SAME advisory lock acquireProviderTxDeliveryLock takes
// (orchestrator.go) for (tenantID, providerID, ref) - the identical
// `hashtextextended('casino_bet_delivery:' || tenant || ':' || provider ||
// ':' || ref, 0)` key - so a test can force a real HTTP delivery of
// postBet/postWin/postRollback to queue at a known point and prove it from
// pg_stat_activity, rather than merely asserting the eventual outcome.
func casHoldProviderTxDeliveryLock(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID, ref string) *casBlocker {
	t.Helper()
	proceed := make(chan struct{})
	ready := make(chan error, 1)
	pidCh := make(chan int32, 1)
	done := make(chan error, 1)
	go func() {
		done <- pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`SELECT pg_advisory_xact_lock(hashtextextended('casino_bet_delivery:' || $1::text || ':' || $2 || ':' || $3, 0))`,
				tenantID, providerID, ref,
			); err != nil {
				ready <- err
				return err
			}
			var pid int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				ready <- err
				return err
			}
			pidCh <- pid
			ready <- nil
			<-proceed
			return nil
		})
	}()
	if err := <-ready; err != nil {
		t.Fatalf("blocker failed to acquire L0.1 for ref %q: %v", ref, err)
	}
	pid := <-pidCh
	var once sync.Once
	b := &casBlocker{pid: pid}
	b.release = func() {
		once.Do(func() {
			close(proceed)
			if err := <-done; err != nil {
				t.Errorf("blocker transaction for ref %q: %v", ref, err)
			}
		})
	}
	t.Cleanup(b.release)
	return b
}

// casWaitForAdvisoryWaiter polls pg_stat_activity for a backend genuinely
// blocked waiting to ACQUIRE the casino_bet_delivery advisory lock
// (wait_event_type='Lock', wait_event='advisory' - Postgres' own,
// unambiguous classification of "this backend is queued on an advisory
// lock someone else holds", per the documentation for
// pg_stat_activity.wait_event), scoped so it can only ever observe the
// delivery this test is actually racing (H1, gate 10.3-W1 re-
// verification):
//   - datname = current_database() - never a waiter on some other database
//     sharing this cluster;
//   - pid <> excludePID - never the blocker's own backend, which holds
//     (not waits on) the lock;
//   - query ILIKE '%casino_bet_delivery%' - the literal embedded in both
//     acquireProviderTxDeliveryLock and casHoldProviderTxDeliveryLock's own
//     SQL text, so an unrelated advisory wait elsewhere in the codebase can
//     never satisfy this check.
//
// Returns true as soon as such a backend is observed, or false if done
// fires first (both racers finished without ever queuing - the
// interleaving this test depends on did not happen) or the timeout
// elapses.
func casWaitForAdvisoryWaiter(t *testing.T, pool *db.Pool, done <-chan struct{}, excludePID int32) bool {
	t.Helper()
	deadline := time.Now().Add(casLockWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false
		default:
		}
		var found bool
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					 WHERE datname = current_database()
					   AND pid <> $1
					   AND wait_event_type = 'Lock' AND wait_event = 'advisory'
					   AND query ILIKE '%casino_bet_delivery%'
				)`,
				excludePID,
			).Scan(&found)
		}); err != nil {
			t.Fatalf("poll pg_stat_activity for an advisory waiter: %v", err)
		}
		if found {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
