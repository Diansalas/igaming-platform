//go:build integration

package stafflifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Ledger-finance review LF-2: ChangeRole waits for a share holder of the target row (a re-asserting transaction)
// for at most lockTimeout, then answers ErrBusy and writes nothing.
func TestFixRound_LF2_ChangeRoleBoundedWait(t *testing.T) {
	w := newLWorld(t)
	w.svc.lockTimeout = 300 * time.Millisecond
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	su := w.staff(w.tenant, RoleSupport, false)
	role := func() string {
		var r string
		if err := w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT role FROM staff_users WHERE id = $1`, su).Scan(&r)
		}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	held := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.owner.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var one int
			err := tx.QueryRow(ctx, `SELECT 1 FROM staff_users WHERE id = $1 FOR SHARE`, su).Scan(&one)
			held <- err
			if err != nil {
				return err
			}
			<-release
			return errRollback
		})
	}()
	if err := <-held; err != nil {
		t.Fatalf("holder: %v", err)
	}
	actor := w.actor(pa, uuid.Nil, RolePlatformAdmin)
	start := time.Now()
	_, err := w.svc.ChangeRole(context.Background(), actor, w.tenant, su, RoleRiskManager, Reason{Code: "role_change_approved"})
	elapsed := time.Since(start)
	close(release)
	<-done
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("a role change behind a share holder must give up with ErrBusy, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the wait was not bounded by lockTimeout: %s", elapsed)
	}
	if r := role(); r != RoleSupport {
		t.Fatalf("a busy role change must write nothing: role %s", r)
	}
	// Control: with the holder gone the same call succeeds.
	if _, err := w.svc.ChangeRole(context.Background(), actor, w.tenant, su, RoleRiskManager, Reason{Code: "role_change_approved"}); err != nil {
		t.Fatalf("role change without a holder: %v", err)
	}
	if r := role(); r != RoleRiskManager {
		t.Fatalf("control: role %s", r)
	}
}
