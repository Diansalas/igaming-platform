//go:build integration

package payments

// ADR 0119 S4b section 9.5 (ADR 0082 Amendment A9): the acting approver's in-transaction re-assertion folded into the
// existing A8 staff FOR SHARE statement of K3 (M2 force resolution, tenant session) and HSEC (hold resolution,
// platform-acting session). A suspension committed between the L1 locks and the A8 statement is seen and nothing is
// written; with the A8 locks held first, a suspension of the (platform) actor waits for the execution.

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

func suspendStaff(t *testing.T, pool *db.Pool, s k3Staff) error {
	t.Helper()
	fn := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, s.ID)
		return err
	}
	if s.TenantID == uuid.Nil {
		return pool.WithoutTenant(context.Background(), fn)
	}
	return pool.WithTenant(context.Background(), s.TenantID, fn)
}

func TestS4b_K3_ActorSuspendedBeforeA8_IsRefusedAndNothingWritten(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	testHookResolutionBeforeShareLocks = func(ctx context.Context, id uuid.UUID) {
		if id == r.ID {
			if err := suspendStaff(t, w.pool, w.f2); err != nil {
				t.Errorf("suspend: %v", err)
			}
		}
	}
	defer func() { testHookResolutionBeforeShareLocks = nil }()
	_, err := w.decide(w.f2, r, ResolutionApprove)
	if !errors.Is(err, db.ErrActorStaffNotLive) {
		t.Fatalf("want db.ErrActorStaffNotLive, got %v", err)
	}
	if got := w.resolution(r.ID); got.State != r.State {
		t.Fatalf("the refused final approval changed the resolution: %s -> %s", r.State, got.State)
	}
	if n := w.countRows(`SELECT count(*) FROM payment_manual_resolution_approvals WHERE resolution_id = $1`, r.ID); n != 0 {
		t.Fatalf("the refused final approval wrote %d approval rows", n)
	}
}

func TestS4b_HSEC_PlatformActorReasserted_BothOrders(t *testing.T) {
	t.Run("suspended_before_A8", func(t *testing.T) {
		h := newHSR(t, 1)
		wr := h.hold(400)
		h.suspendTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		testHookHoldResolutionBeforeShareLocks = func(ctx context.Context, id uuid.UUID) {
			if id == r.ID {
				if err := suspendStaff(t, h.pool, h.apprB); err != nil {
					t.Errorf("suspend: %v", err)
				}
			}
		}
		defer func() { testHookHoldResolutionBeforeShareLocks = nil }()
		_, err := h.decide(h.apprB, r, ResolutionApprove)
		if !errors.Is(err, db.ErrActorStaffNotLive) {
			t.Fatalf("want db.ErrActorStaffNotLive, got %v", err)
		}
		if got := h.res(r.ID); got.State != r.State {
			t.Fatalf("the refused execution changed the resolution: %s -> %s", r.State, got.State)
		}
		if n := h.governedTxCount(wr.ID); n != 0 {
			t.Fatalf("the refused execution posted %d governed transactions", n)
		}
	})
	t.Run("execution_holds_A8_suspension_waits", func(t *testing.T) {
		h := newHSR(t, 1)
		wr := h.hold(400)
		h.suspendTenant()
		r := h.mustRequest(h.reqA, wr.ID)
		reached, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		testHookHoldResolutionAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
			if id == r.ID {
				close(reached)
				<-release
			}
		}
		defer func() { testHookHoldResolutionAfterShareLocks = nil }()
		defer once.Do(func() { close(release) })
		execDone := make(chan error, 1)
		go func() { _, err := h.decide(h.apprB, r, ResolutionApprove); execDone <- err }()
		<-reached
		suspDone := make(chan error, 1)
		go func() { suspDone <- suspendStaff(t, h.pool, h.apprB) }()
		select {
		case err := <-suspDone:
			once.Do(func() { close(release) })
			t.Fatalf("the platform actor's suspension committed while the execution held its row FOR SHARE (err %v)", err)
		case <-time.After(700 * time.Millisecond):
		}
		once.Do(func() { close(release) })
		if err := <-execDone; err != nil {
			t.Fatalf("execution: %v", err)
		}
		if err := <-suspDone; err != nil {
			t.Fatalf("suspension after the execution: %v", err)
		}
		if got := h.res(r.ID); got.State == r.State {
			t.Fatal("the execution must have completed")
		}
	})
}
