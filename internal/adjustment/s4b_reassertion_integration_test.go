//go:build integration

package adjustment

// ADR 0119 S4b section 9.5 (ADR 0082 Amendment A9): the scoped in-transaction re-assertion of the ACTING approver in
// K2's existing A8 staff FOR SHARE statement, for the tenant and the acting (platform) family. Both orders of
// "suspend the approver" against "the approver's final approval":
//   - the suspension (or a role change) committed between the request lock and the A8 statement: the re-assertion
//     sees it, nothing is written (the approval insert rolls back with the transaction), db.ErrActorStaffNotLive;
//   - the approval holds its A8 locks first: the suspension waits for it (the actor's own row is now in the A8 set,
//     made visible for a platform actor by migration 0151's staff_actor_self_* pair), the approval commits, then the
//     suspension completes. No deadlock in either order.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func (w *world) s4bSetStaff(s staffMember, sql string) error {
	fn := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, s.ID)
		return err
	}
	if s.TenantID == uuid.Nil {
		return w.pool.WithoutTenant(context.Background(), fn)
	}
	return w.pool.WithTenant(context.Background(), s.TenantID, fn)
}

func (w *world) approvalsBy(r Request, s staffMember) int {
	w.t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_approvals WHERE request_id = $1 AND decided_by = $2`, r.ID, s.ID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestS4b_K2_ActorChangedBeforeA8_IsRefusedAndNothingWritten(t *testing.T) {
	for _, c := range []struct {
		family, change string
	}{
		{"tenant", "suspend"}, {"acting", "suspend"}, {"tenant", "role"}, // a platform row holds platform_admin only (staff_users_check)
	} {
		t.Run(c.family+"_"+c.change, func(t *testing.T) {
			w := newWorld(t, worldOpts{base: 1})
			approver := w.F2
			if c.family == "acting" {
				approver = w.Acting
			}
			r, err := w.submit(w.F1, w.credit(250, ReasonOperationalErrorCorrection))
			if err != nil {
				t.Fatal(err)
			}
			sql := `UPDATE staff_users SET status = 'suspended' WHERE id = $1`
			if c.change == "role" {
				sql = `UPDATE staff_users SET role = 'support' WHERE id = $1`
			}
			testHookBeforeShareLocks = func(ctx context.Context, id uuid.UUID) {
				if id == r.ID {
					if err := w.s4bSetStaff(approver, sql); err != nil {
						t.Errorf("change the approver: %v", err)
					}
				}
			}
			defer func() { testHookBeforeShareLocks = nil }()
			_, err = w.decide(approver, r, DecisionApprove)
			if !errors.Is(err, db.ErrActorStaffNotLive) {
				t.Fatalf("want db.ErrActorStaffNotLive, got %v", err)
			}
			if ClassifyError(err) != ErrClassSessionInvalid {
				t.Fatalf("class %q", ClassifyError(err))
			}
			if n := w.approvalsBy(r, approver); n != 0 {
				t.Fatalf("the refused final approval wrote %d approval rows", n)
			}
			if got := w.request(r.ID); got.State != StatePending {
				t.Fatalf("request after the refusal: %+v", got)
			}
		})
	}
}

func TestS4b_K2_ApprovalHoldingA8_SuspensionOfTheActorWaits(t *testing.T) {
	for _, family := range []string{"tenant", "acting"} {
		t.Run(family, func(t *testing.T) {
			w := newWorld(t, worldOpts{base: 1})
			approver := w.F2
			if family == "acting" {
				approver = w.Acting
			}
			r, err := w.submit(w.F1, w.credit(250, ReasonOperationalErrorCorrection))
			if err != nil {
				t.Fatal(err)
			}
			locked, proceed := make(chan struct{}), make(chan struct{})
			var release sync.Once
			testHookAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
				if id == r.ID {
					close(locked)
					<-proceed
				}
			}
			defer release.Do(func() { close(proceed) })
			defer func() { testHookAfterShareLocks = nil }()
			var (
				wg                  sync.WaitGroup
				out                 Outcome
				execErr, suspendErr error
				execDone, suspDone  time.Time
			)
			wg.Add(1)
			go func() { defer wg.Done(); out, execErr = w.decide(approver, r, DecisionApprove); execDone = time.Now() }()
			<-locked
			wg.Add(1)
			go func() {
				defer wg.Done()
				suspendErr = w.s4bSetStaff(approver, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`)
				suspDone = time.Now()
			}()
			w.waitForLockWaiter(t, "%UPDATE staff_users SET status%")
			releasedAt := time.Now()
			release.Do(func() { close(proceed) })
			wg.Wait()
			if execErr != nil || !out.Executed {
				t.Fatalf("the approval that held its A8 locks first must stand: %v %+v", execErr, out)
			}
			if suspendErr != nil || suspDone.Before(releasedAt) || suspDone.Before(execDone.Add(-time.Second)) {
				t.Fatalf("the suspension must wait for the approval's FOR SHARE: err %v", suspendErr)
			}
		})
	}
}
