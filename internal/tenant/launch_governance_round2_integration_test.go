//go:build integration

// ADR 0112 slice 1, review round 2: security S-1 (fail-closed deferred check), S-2 (supersede
// proof), code-review C-3(a) (a recorded reject blocks execution).
package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func lgClearSession(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true), set_config('app.principal_id', '', true),
		set_config('app.platform_admin_principal_id', '', true)`)
	return err
}

func (w *lgWorld) requestStatus(t *testing.T, id uuid.UUID) (string, bool) {
	t.Helper()
	var st string
	found := true
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT status FROM launch_authorisation_requests WHERE id = $1`, id).Scan(&st)
		if err == pgx.ErrNoRows {
			found = false
			return nil
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return st, found
}

// S-1: a session whose GUCs no longer let it see its own request at COMMIT is REFUSED (LA030), not
// passed: an invisible 'executing' request must never be able to commit and stay stuck.
func TestLaunchGov_S1_DeferredCheckFailsClosed_WhenSessionClearsItsScopeBeforeCommit(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)

	// 1 approval (brand activation): request + approval + executing in one platform transaction,
	// then the session scope is cleared before COMMIT.
	b1 := w.pendingBrand(t, f.tenantID)
	var r1 uuid.UUID
	err := w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		var h string
		var err error
		r1, h, err = lgRequest(ctx, tx, f.tenantID, &b1, "activate")
		if err != nil {
			return err
		}
		if err := lgSetPlatform(ctx, tx, w.ops.Approver2); err != nil {
			return err
		}
		if err := lgApprove(ctx, tx, r1, h, "approve"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, r1); err != nil {
			return err
		}
		return lgClearSession(ctx, tx)
	})
	wantCode(t, "1-approval request left executing with a cleared session", err, "LA030")
	if _, found := w.requestStatus(t, r1); found {
		t.Fatal("the refused commit must leave no request behind")
	}

	// 2 approvals (brand close): the first approval is committed earlier; the second approves and
	// executes, then clears the scope.
	var r2 uuid.UUID
	var h2 string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r2, h2, err = lgRequest(ctx, tx, f.tenantID, &f.brandID, "close")
		if err != nil {
			return err
		}
		if err := lgSetPlatform(ctx, tx, w.ops.Approver1); err != nil {
			return err
		}
		return lgApprove(ctx, tx, r2, h2, "approve")
	}); err != nil {
		t.Fatal(err)
	}
	err = w.platformRT(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
		if err := lgApprove(ctx, tx, r2, h2, "approve"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, r2); err != nil {
			return err
		}
		return lgClearSession(ctx, tx)
	})
	wantCode(t, "2-approval close left executing with a cleared session", err, "LA030")
	if st, found := w.requestStatus(t, r2); !found || st != "pending" {
		t.Fatalf("the refused commit must leave the close request pending (not stuck executing): %q found=%v", st, found)
	}
	if got := w.brandStatus(t, f.tenantID, f.brandID); got != "active" {
		t.Fatalf("brand status %s", got)
	}

	// A tenant session (S8, own-brand suspension) clearing its scope before COMMIT.
	staff := w.tenantStaff(t, f.tenantID)
	var r3 uuid.UUID
	err = w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r3, _, err = lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, r3); err != nil {
			return err
		}
		return lgClearSession(ctx, tx)
	})
	wantCode(t, "tenant session left executing with a cleared scope", err, "LA030")
	if _, found := w.requestStatus(t, r3); found {
		t.Fatal("the refused commit must leave no request behind")
	}
}

// S-2: 'superseded' is admitted only once the suspension's governed transition exists in this
// transaction. A tenant session that merely sets its own suspend request 'executing' cannot use it
// to kill a pending request of the subject (activation / closure) it wants out of the way.
func TestLaunchGov_S2_SupersedeNeedsTheSuspensionsGovernedTransition(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)
	staff := w.tenantStaff(t, f.tenantID)
	var pendingClose uuid.UUID
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pendingClose, _, err = lgRequest(ctx, tx, f.tenantID, &f.brandID, "close")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The tenant's own suspend request is only 'executing'; no transition yet.
	err := w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'superseded' WHERE id = $1`, pendingClose)
		return err
	})
	wantCode(t, "supersede on the strength of an executing suspend with no transition", err, "LA011")
	if st, _ := w.requestStatus(t, pendingClose); st != "pending" {
		t.Fatalf("the platform's pending close was killed: %q", st)
	}
}

// C-3(a): a recorded 'reject' decision blocks S1 execution of that request for good, even when
// enough approvals exist and one is decided in the executing transaction.
func TestLaunchGov_C3a_RecordedRejectBlocksExecution(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)
	b := w.pendingBrand(t, f.tenantID)
	var reqID uuid.UUID
	var hash string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, hash, err = lgRequest(ctx, tx, f.tenantID, &b, "activate")
		if err != nil {
			return err
		}
		if err := lgSetPlatform(ctx, tx, w.ops.Approver1); err != nil {
			return err
		}
		return lgApprove(ctx, tx, reqID, hash, "reject") // recorded, request NOT moved to rejected
	}); err != nil {
		t.Fatal(err)
	}
	err := w.platformRT(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
		if err := lgApprove(ctx, tx, reqID, hash, "approve"); err != nil {
			return err
		}
		return lgExecuteBrand(ctx, tx, reqID, f.tenantID, b, "pending_launch", "active")
	})
	wantCode(t, "execution after a recorded reject", err, "LA011")
	if got := w.brandStatus(t, f.tenantID, b); got != "pending_launch" {
		t.Fatalf("brand status %s", got)
	}
	// pending -> rejected still needs the same-transaction reject (already covered); with it, it works.
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, reqID)
		return err
	}); err != nil {
		t.Fatalf("the requester can still cancel: %v", err)
	}
}
