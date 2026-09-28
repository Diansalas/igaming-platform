//go:build integration

// A7 shared lock-order test helpers. a7HoldRow and a7WaitAnyLockWaiter used
// to exist as two separate, independently-written local copies: one on this
// (payments/callback) agent's branch, in a7_1a_integration_test.go, and one
// on the payout agent's FH-6 branch, in a7_lockorder_integration_test.go -
// with different argument orders and different waiter-query
// implementations, which would fail to compile once both branches merged
// into the same package. Code review (rv-fh3-code-review.md, 95a1c34)
// resolved this by designating FH-6's versions canonical (copied here
// verbatim from worktree-agent-a5b19b46582e95b75 @ b7f84ec's
// internal/payments/a7_lockorder_integration_test.go, folding in this
// branch's own clearer "no row to lock" error), landing them here on a
// dedicated, collision-free file, and having the payout agent delete its
// own copies from a7_lockorder_integration_test.go once FH-6 merges - this
// file becomes their sole definition.
package payments

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// a7WaitAnyLockWaiter polls pg_locks for ANY not-granted lock request (row,
// advisory, or otherwise) from a backend pid not already in exclude. Needed
// (rather than a relation-scoped join, e.g. `pg_locks JOIN pg_class ON
// relation`) because row-level `FOR UPDATE` contention is represented in
// pg_locks as locktype = 'transactionid' with relation IS NULL - the waiter
// waits on the lock-holder's XID, not a relation-scoped lock row - and
// advisory locks likewise carry relation = NULL.
//
// Scoped to pg_stat_activity.datname = current_database(): this suite runs
// against a private, per-test scratch database on a Postgres CLUSTER that
// may be shared with other agents' concurrent test runs (their own,
// unrelated scratch/private databases on the same instance) - an
// unscoped, cluster-wide `pg_locks` scan can otherwise pick up a
// completely unrelated backend's not-granted lock and misidentify it as
// this test's own racer, an observed source of flakiness once multiple
// agents run concurrently on the same Postgres instance.
func a7WaitAnyLockWaiter(t *testing.T, pool *db.Pool, exclude map[int]bool, timeout time.Duration) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var pid int
		found := false
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx,
				`SELECT l.pid FROM pg_locks l
				 JOIN pg_stat_activity a ON a.pid = l.pid
				 WHERE NOT l.granted AND a.datname = current_database()`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p int
				if err := rows.Scan(&p); err != nil {
					return err
				}
				if !exclude[p] {
					pid = p
					found = true
				}
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("scan pg_locks: %v", err)
		}
		if found {
			return pid, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, false
}

// a7HoldRow holds a FOR UPDATE row lock on table WHERE id = rowID until
// release() is called - the generic parent-row blocker used across the A7
// test files (deposit_intents, withdrawal_requests). Folds in this branch's
// own clearer "no row to lock" error (a bare pgx.ErrNoRows was otherwise
// indistinguishable from any other setup mistake).
func a7HoldRow(t *testing.T, pool *db.Pool, tenantID uuid.UUID, table string, rowID uuid.UUID, name string) *loBlocker {
	t.Helper()
	return loHoldWith(t, pool, tenantID, name, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE id = $1 FOR UPDATE`, table), rowID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("blocker %s: no %s row %s to lock", name, table, rowID)
		}
		return err
	})
}
