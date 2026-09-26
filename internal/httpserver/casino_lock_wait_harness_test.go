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
// lock until release() is called.
type casBlocker struct {
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
			ready <- nil
			<-proceed
			return nil
		})
	}()
	if err := <-ready; err != nil {
		t.Fatalf("blocker failed to acquire L0.1 for ref %q: %v", ref, err)
	}
	var once sync.Once
	b := &casBlocker{}
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

// casWaitForAdvisoryWaiter polls pg_stat_activity for ANY backend
// genuinely blocked waiting to ACQUIRE an advisory lock
// (wait_event_type='Lock', wait_event='advisory' - Postgres' own,
// unambiguous classification of "this backend is queued on an advisory
// lock someone else holds", per the documentation for
// pg_stat_activity.wait_event). Returns true as soon as such a backend is
// observed, or false if done fires first (both racers finished without
// ever queuing - the interleaving this test depends on did not happen) or
// the timeout elapses.
func casWaitForAdvisoryWaiter(t *testing.T, pool *db.Pool, done <-chan struct{}) bool {
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
				`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND wait_event = 'advisory')`,
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
