//go:build integration

package payments

// ADR 0119 S4b section 9.5 / ADR 0082 Amendment A9 (ported from the ledger-finance review): K3 (M2 force resolution)
// re-assertion of the acting approver in BOTH orders for the acting (platform principal) family and the tenant family,
// a tenant role change (finance -> support) refused with nothing written, and the writer-starvation bound of the
// two-phase suspension: an execution holding its A8 share locks longer than T2's lock_timeout makes T2 fail 55P03
// (lifecycle_busy in S5 + S6) while the execution still commits cleanly - no deadlock, no partial money outcome.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func s4bSetStaff(t *testing.T, pool *db.Pool, s k3Staff, sql string, lockTimeout string) error {
	t.Helper()
	fn := func(ctx context.Context, tx pgx.Tx) error {
		if lockTimeout != "" {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+lockTimeout+`'`); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, sql, s.ID)
		return err
	}
	if s.TenantID == uuid.Nil {
		if err := plantSparePlatformAdmin(pool); err != nil { // ADR 0119 S5 + S6: SF020
			return err
		}
		return pool.WithoutTenant(context.Background(), fn)
	}
	return pool.WithTenant(context.Background(), s.TenantID, fn)
}

func TestS4b_K3_ActingActorSuspendedBeforeA8_IsRefusedAndNothingWritten(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	ledgerBefore := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID)
	testHookResolutionBeforeShareLocks = func(ctx context.Context, id uuid.UUID) {
		if id == r.ID {
			if err := s4bSetStaff(t, w.pool, w.acting2, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, ""); err != nil {
				t.Errorf("suspend: %v", err)
			}
		}
	}
	defer func() { testHookResolutionBeforeShareLocks = nil }()
	_, err := w.decide(w.acting2, r, ResolutionApprove)
	if !errors.Is(err, db.ErrActorStaffNotLive) {
		t.Fatalf("want db.ErrActorStaffNotLive, got %v", err)
	}
	if got := w.resolution(r.ID); got.State != r.State {
		t.Fatalf("resolution changed %s -> %s", r.State, got.State)
	}
	if n := w.countRows(`SELECT count(*) FROM payment_manual_resolution_approvals WHERE resolution_id = $1`, r.ID); n != 0 {
		t.Fatalf("approvals written: %d", n)
	}
	if n := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID); n != ledgerBefore {
		t.Fatalf("ledger transactions %d -> %d", ledgerBefore, n)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("withdrawal moved to %s", got.State)
	}
	w.assertInvariants()
}

func TestS4b_K3_TenantActorRoleChangedBeforeA8_IsRefusedAndNothingWritten(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	testHookResolutionBeforeShareLocks = func(ctx context.Context, id uuid.UUID) {
		if id == r.ID {
			if err := s4bSetStaff(t, w.pool, w.f2, `UPDATE staff_users SET role = 'support' WHERE id = $1`, ""); err != nil {
				t.Errorf("role change: %v", err)
			}
		}
	}
	defer func() { testHookResolutionBeforeShareLocks = nil }()
	_, err := w.decide(w.f2, r, ResolutionApprove)
	if !errors.Is(err, db.ErrActorStaffNotLive) {
		t.Fatalf("want db.ErrActorStaffNotLive, got %v", err)
	}
	if n := w.countRows(`SELECT count(*) FROM payment_manual_resolution_approvals WHERE resolution_id = $1`, r.ID); n != 0 {
		t.Fatalf("approvals written: %d", n)
	}
	w.assertInvariants()
}

// holdFor: how long the execution holds its A8 locks; lockTimeout: the suspension T2's lock_timeout.
func s4bK3HoldingOrder(t *testing.T, acting bool, holdFor time.Duration, lockTimeout string, expectEarly bool) (execErr, suspErr error, executed bool) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	requester, approver := w.f1, w.f2
	if acting {
		requester, approver = w.acting, w.acting2
	}
	r := w.mustRequest(requester, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	testHookResolutionAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
		if id == r.ID {
			close(reached)
			<-release
		}
	}
	defer func() { testHookResolutionAfterShareLocks = nil }()
	defer once.Do(func() { close(release) })
	execDone := make(chan error, 1)
	var out ResolutionOutcome
	go func() {
		var err error
		out, err = w.decide(approver, r, ResolutionApprove)
		execDone <- err
	}()
	<-reached
	// Phase T1 (sessions) takes no staff lock: not modelled. Phase T2:
	suspDone := make(chan error, 1)
	go func() {
		suspDone <- s4bSetStaff(t, w.pool, approver, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, lockTimeout)
	}()
	select {
	case err := <-suspDone:
		if !expectEarly {
			once.Do(func() { close(release) })
			<-execDone
			t.Fatalf("suspension finished while A8 held (err %v)", err)
		}
		suspErr = err
	case <-time.After(holdFor):
	}
	once.Do(func() { close(release) })
	execErr = <-execDone
	if suspErr == nil {
		suspErr = <-suspDone
	}
	w.assertInvariants()
	return execErr, suspErr, out.Executed
}

func TestS4b_K3_ExecutionHoldingA8_SuspensionWaits_BothFamilies(t *testing.T) {
	for _, acting := range []bool{false, true} {
		name := "tenant"
		if acting {
			name = "acting"
		}
		t.Run(name, func(t *testing.T) {
			execErr, suspErr, executed := s4bK3HoldingOrder(t, acting, 700*time.Millisecond, "5s", false)
			if execErr != nil || !executed {
				t.Fatalf("execution: %v executed=%v", execErr, executed)
			}
			if suspErr != nil {
				t.Fatalf("T2 within lock_timeout should succeed after the execution: %v", suspErr)
			}
		})
	}
}

// Writer starvation bound: an execution holding A8 longer than T2's lock_timeout makes T2 fail 55P03
// (lifecycle_busy in S5/S6), and the execution still completes - no deadlock, no partial money outcome.
func TestS4b_K3_ExecutionHoldingA8_T2LockTimeoutIs55P03_ExecutionCommits(t *testing.T) {
	execErr, suspErr, executed := s4bK3HoldingOrder(t, true, 1500*time.Millisecond, "500ms", true)
	if execErr != nil || !executed {
		t.Fatalf("execution: %v executed=%v", execErr, executed)
	}
	var pg *pgconn.PgError
	if !errors.As(suspErr, &pg) || pg.Code != "55P03" {
		t.Fatalf("want 55P03 lock_not_available, got %v", suspErr)
	}
}
