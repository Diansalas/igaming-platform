//go:build integration

package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
)

// errK3Rollback is returned by the in-transaction helpers to roll the test
// transaction back after fn ran, so nothing it did persists.
var errK3Rollback = errors.New("k3 test rollback")

// k3Try runs fn in a SAVEPOINT that is always rolled back, returning fn's
// error. An expected-to-fail statement inside a longer test transaction must go
// through it (a failed statement otherwise aborts the whole transaction).
func k3Try(ctx context.Context, tx pgx.Tx, fn func(ctx context.Context, tx pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sp.Rollback(ctx) }()
	return fn(ctx, sp)
}

// insertApproval inserts the actor's approval of r in tx (the actor session is
// the caller's) with r's payload hash.
func k3InsertApproval(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r ManualResolution, decision string) error {
	// SIGNED-ACTOR-PROOF (migration 0120): the server-issued proof for this session's actor.
	if err := prooftest.AttachForSession(ctx, tx, "payment_force_resolve:"+decision, r.ID.String(), r.PayloadHash); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO payment_manual_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, $5, 'tenant', $5, 0, 'k3-test')`,
		tenantID, r.ID, decision, r.PayloadHash, uuid.Nil)
	return err
}

// inExecuting opens a TENANT-scope session as approver, records the approver's
// approval, moves r to `executing` (the guard recounts), runs fn and then ROLLS
// BACK - the direct way to attack the database-side fences and policies with a
// real executing resolution in the transaction. It returns fn's error (nil when
// fn succeeded), or the error of a failing setup step.
func (w *k3World) inExecuting(r ManualResolution, approver k3Staff, fn func(ctx context.Context, tx pgx.Tx) error) error {
	w.t.Helper()
	err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, approver.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := k3InsertApproval(ctx, tx, w.f.tenantID, r, "approve"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return errK3Rollback
	})
	if errors.Is(err, errK3Rollback) {
		return nil
	}
	return err
}

// inExecutingActing is inExecuting through an ACTING session (a platform
// principal acting in the tenant).
func (w *k3World) inExecutingActing(r ManualResolution, approver k3Staff, fn func(ctx context.Context, tx pgx.Tx) error) error {
	w.t.Helper()
	err := w.pool.WithPlatformActingInTenant(context.Background(), approver.ID, w.f.tenantID, r.ID, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, "payment_force_resolve:approve", r.ID.String(), r.PayloadHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_manual_resolution_approvals
				(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'k3-test')`, w.f.tenantID, r.ID, r.PayloadHash, uuid.Nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return errK3Rollback
	})
	if errors.Is(err, errK3Rollback) {
		return nil
	}
	return err
}

// mustRequest requests as actor and fails the test on error.
func (w *k3World) mustRequest(actor k3Staff, in ResolutionRequestInput) ManualResolution {
	w.t.Helper()
	r, err := w.request(actor, in)
	if err != nil {
		w.t.Fatalf("request: %v", err)
	}
	return r
}

func k3RequireNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
