//go:build integration

package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Security r2 (MEDIUM): an executor whose transaction snapshot was taken BEFORE another approver's
// 'reject' committed must not be able to approve, execute and commit. The approvals guard only
// row-locks the request, so snapshot isolation does not abort this; the database therefore refuses
// the approval-based pending -> executing move unless the transaction is READ COMMITTED (LA011),
// where the reject check sees the committed reject. Two sessions: the executor snapshots, the
// rejecter commits, the executor then approves and executes.
func TestLaunchGov_R2_StaleSnapshotExecutorCannotBypassACommittedReject(t *testing.T) {
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		t.Run(string(iso), func(t *testing.T) {
			w := newLG(t)
			f := seedBrandPinTenant(t, w.owner)
			b := w.pendingBrand(t, f.tenantID)
			var reqID uuid.UUID
			var hash string
			if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				reqID, hash, err = lgRequest(ctx, tx, f.tenantID, &b, "activate")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			exec, err := w.rt.Raw().BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = exec.Rollback(ctx) }()
			// The executor's snapshot is fixed by its first statement.
			if _, err := exec.Exec(ctx, `SELECT count(*) FROM launch_authorisation_approvals`); err != nil {
				t.Fatal(err)
			}
			if err := lgSetPlatform(ctx, exec, w.ops.Approver2); err != nil {
				t.Fatal(err)
			}
			// A different approver's reject commits after the executor's snapshot.
			if err := w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
				return lgApprove(ctx, tx, reqID, hash, "reject")
			}); err != nil {
				t.Fatalf("reject: %v", err)
			}
			err = lgApprove(ctx, exec, reqID, hash, "approve")
			if err == nil {
				err = lgExecuteBrand(ctx, exec, reqID, f.tenantID, b, "pending_launch", "active")
			}
			if err == nil {
				err = exec.Commit(ctx)
			}
			if err == nil {
				t.Fatalf("a stale-snapshot executor activated the brand despite a committed reject (brand now %s)", w.brandStatus(t, f.tenantID, b))
			}
			if code := pgCodeOf(err); code != "LA011" && code != "40001" {
				t.Fatalf("want LA011 (or a serialization failure), got %q (%v)", code, err)
			}
			if got := w.brandStatus(t, f.tenantID, b); got != "pending_launch" {
				t.Fatalf("brand status %s", got)
			}
		})
	}
}
