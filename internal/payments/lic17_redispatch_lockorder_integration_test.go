//go:build integration

// lic17 F-3 / L-1: the sweeper's re-dispatch gate (T2 / T12) takes the tenant status gate key SHARED before it reads the
// withdrawal restriction policy (ADR 0082: status keys first, before L0.4 and every row lock). A policy writer takes the same key
// EXCLUSIVE, so a tick evaluates one consistent policy version: it waits for an in-flight policy change and then sees it, and a
// policy change waits for an in-flight tick. Forced interleavings (the lockorder harness), no goroutine race. MOCK providers only.
package payments

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// l17BlockedBy polls until some backend is blocked by holder (any lock type, advisory included) and its current query contains
// want; it returns that backend's pid, or 0 when done closes first or the wait times out.
func l17BlockedBy(t *testing.T, pool *db.Pool, holder int, want string, done <-chan struct{}) int {
	t.Helper()
	deadline := time.Now().Add(loLockWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return 0
		default:
		}
		var pid int
		var q string
		err := pool.Raw().QueryRow(context.Background(), `SELECT pid, query FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)) ORDER BY pid LIMIT 1`, holder).Scan(&pid, &q)
		if err == nil && strings.Contains(q, want) {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

func TestLic17_F3_RedispatchGate_SerialisesWithPolicyWriters(t *testing.T) {
	ctx := context.Background()
	t.Run("policy_writer_first", func(t *testing.T) {
		w := newF4World(t, "l17-lo-pw")
		l11PaySuspendLicence(t, w.pool, w.f.tenantID)
		wr, attempt := notSentPayoutAttempt(t, w.pool, w.orch, w.f, 500, "l17-lo-pw")
		policyTx, err := w.pool.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer policyTx.Rollback(ctx) //nolint:errcheck
		if _, err := policyTx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err := policyTx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_suspended', 'block', 'lic17_lo')`, w.f.tenantID); err != nil {
			t.Fatal(err)
		}
		var policyPID int
		if err := policyTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&policyPID); err != nil {
			t.Fatal(err)
		}
		done, finished := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(finished)
			done <- w.sweeper().reclaimPayoutCreated(ctx, w.f.tenantID, attempt)
		}()
		if l17BlockedBy(t, w.pool, policyPID, "tenant_status_gate_key", finished) == 0 {
			t.Fatal("the re-dispatch tick must wait on the tenant status key held EXCLUSIVE by the policy writer, before it reads the policy")
		}
		if err := policyTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil || loIsDeadlock(err) {
			t.Fatalf("the tick completes after the policy writer, got %v", err)
		}
		// It saw the committed block: escalated, no resend, one WD056 denying decision.
		after := w.reread(t, attempt.ID)
		if after.State != AttemptCreated || after.EscalatedAt == nil || after.SubmitCount != attempt.SubmitCount {
			t.Fatalf("the tick must evaluate the committed block, got %s escalated=%v", after.State, after.EscalatedAt != nil)
		}
		if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = 'redispatch' AND NOT allowed
			AND gate_code = 'WD056' AND restriction_policy_set_version = 1`, wr.ID); n != 1 {
			t.Fatalf("one WD056 decision recording set version 1, got %d", n)
		}
		loAssertBalanced(t, w.pool, w.f.tenantID)
	})
	t.Run("tick_first", func(t *testing.T) {
		w := newF4World(t, "l17-lo-tk")
		l11PaySuspendLicence(t, w.pool, w.f.tenantID)
		wr, attempt := notSentPayoutAttempt(t, w.pool, w.orch, w.f, 500, "l17-lo-tk")
		// Park the tick at its L1 lock (after the gate evaluation, while it holds the status key SHARED).
		row := loHoldWith(t, w.pool, w.f.tenantID, "withdrawal row", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT 1 FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, wr.ID)
			return err
		})
		tickDone, tickFinished := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(tickFinished)
			tickDone <- w.sweeper().reclaimPayoutCreated(ctx, w.f.tenantID, attempt)
		}()
		tickPID := l17BlockedBy(t, w.pool, row.pid, "withdrawal_requests", tickFinished)
		if tickPID == 0 {
			t.Fatal("the tick never reached its L1 lock")
		}
		var held bool
		if err := w.pool.Raw().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND locktype = 'advisory' AND mode = 'ShareLock' AND granted)`, tickPID).Scan(&held); err != nil || !held {
			t.Fatalf("the tick must hold the tenant status key SHARED while parked at L1: %v %v", held, err)
		}
		policyDone, policyFinished := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(policyFinished)
			policyDone <- w.pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_suspended', 'block', 'lic17_lo')`, w.f.tenantID)
				return err
			})
		}()
		if l17BlockedBy(t, w.pool, tickPID, "withdrawal_restriction_policies", policyFinished) == 0 {
			t.Fatal("the policy writer must wait for the in-flight tick")
		}
		row.release()
		if err := <-tickDone; err != nil || loIsDeadlock(err) {
			t.Fatalf("the tick commits under the version it read, got %v", err)
		}
		if err := <-policyDone; err != nil || loIsDeadlock(err) {
			t.Fatalf("the policy writer completes after the tick, got %v", err)
		}
		if after := w.reread(t, attempt.ID); after.State != AttemptPending {
			t.Fatalf("the tick read the default (no row yet) and re-claimed, got %s", after.State)
		}
		if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = 'redispatch' AND allowed
			AND restriction_policy_source = 'default_unconfigured' AND restriction_policy_set_version = 0`, wr.ID); n != 1 {
			t.Fatalf("the allowing decision records the version the tick read (0), got %d", n)
		}
		if got := w.state(t, wr.ID); got.State != withdrawal.StateSubmitted {
			t.Fatalf("withdrawal state %s", got.State)
		}
		loAssertBalanced(t, w.pool, w.f.tenantID)
	})
}
