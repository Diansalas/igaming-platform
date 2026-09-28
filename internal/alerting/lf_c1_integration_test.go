//go:build integration

package alerting

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLFC1_a_BusinessWritesBeforeAndAfterSwallowedRaiseSurviveCommit is LF
// test 5(a): a business write before AND after a swallowed RaiseGuarded
// call both commit - the swallow never poisons or truncates the
// transaction.
func TestLFC1_a_BusinessWritesBeforeAndAfterSwallowedRaiseSurviveCommit(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	var beforeID, afterID uuid.UUID
	pending, err := InTx(context.Background(), NewTenantRunner(pool, tenantA), func(ctx context.Context, tx pgx.Tx) error {
		beforeID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			beforeID, tenantA, "before-"+beforeID.String()+"@tenant.test"); err != nil {
			return fmt.Errorf("business write before: %w", err)
		}

		// A tenant session raising a meta-Kind directly is refused by RLS
		// with 42501 on every attempt (the same deterministic swallow
		// this package's other tests use) - RaiseGuarded must swallow it
		// without touching the surrounding transaction at all.
		if err := RaiseGuarded(ctx, tx, Alert{
			Kind:          KindAlertingUnrouted,
			Discriminator: "severity:p1",
			Attributes:    map[string]AttrValue{},
		}); err != nil {
			t.Fatalf("RaiseGuarded must never propagate: %v", err)
		}

		afterID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			afterID, tenantA, "after-"+afterID.String()+"@tenant.test"); err != nil {
			return fmt.Errorf("business write after: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a non-nil Pending for a committed transaction")
	}

	var count int
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE id IN ($1, $2)`, beforeID, afterID).Scan(&count)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected both business writes to have committed, got %d of 2", count)
	}
}

// TestLFC1_b_25P02Propagates is LF test 5(b): once the outer transaction
// is already aborted (25P02, "current transaction is aborted"),
// RaiseGuarded cannot even open its savepoint and must propagate; InTx
// must then return that error (the transaction rolls back).
func TestLFC1_b_25P02Propagates(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	var raiseErr error
	_, intxErr := InTx(context.Background(), NewTenantRunner(pool, tenantA), func(ctx context.Context, tx pgx.Tx) error {
		// Poison the transaction directly (division by zero: 22012,
		// class 22) - this is NOT run through RaiseGuarded's own
		// savepoint, so it aborts the OUTER transaction outright, exactly
		// like a real business-statement failure would.
		_, _ = tx.Exec(ctx, `SELECT 1/0`)

		raiseErr = RaiseGuarded(ctx, tx, Alert{
			Kind:          KindAlertingUnrouted,
			Discriminator: "severity:p1",
			Attributes:    map[string]AttrValue{},
		})
		if raiseErr == nil {
			t.Fatal("expected RaiseGuarded to propagate once the outer transaction is aborted (25P02)")
		}
		return raiseErr
	})
	if intxErr == nil {
		t.Fatal("expected InTx to return the propagated error")
	}
}

// TestLFC1_c_55P03Propagates is LF test 5(c): a second, concurrent
// session holds an UNCOMMITTED raise of the same dedup key; the business
// transaction sets a short SET LOCAL lock_timeout, and the conflicting
// alert INSERT inside RaiseGuarded's savepoint must wait, then fail with
// 55P03 (lock_not_available) once the timeout elapses. 55P03 is NOT in
// the swallow allowlist, so it must propagate, InTx must error, and the
// business row inserted in the same transaction must not be committed.
func TestLFC1_c_55P03Propagates(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	discriminator := "switch:" + uuid.NewString()

	ctx := context.Background()

	// Session A: opens a transaction, raises the alert, and holds it
	// uncommitted for the rest of the test - a second, distinct pooled
	// connection from the lock-holder used below.
	txA, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatalf("begin session A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if _, err := txA.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
		t.Fatalf("set session A tenant: %v", err)
	}
	if err := Raise(ctx, txA, Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: tenantA,
		Discriminator:   discriminator,
		Attributes:      map[string]AttrValue{"reason_code": "lock-holder"},
	}); err != nil {
		t.Fatalf("session A raise: %v", err)
	}
	// txA is now an uncommitted holder of this exact dedup key.

	var raiseErr error
	var businessRowID uuid.UUID
	_, intxErr := InTx(ctx, NewTenantRunner(pool, tenantA), func(ctx context.Context, tx pgx.Tx) error {
		businessRowID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			businessRowID, tenantA, "lockcontention-"+businessRowID.String()+"@tenant.test"); err != nil {
			return fmt.Errorf("business write: %w", err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}
		raiseErr = RaiseGuarded(ctx, tx, Alert{
			Kind:            KindPaymentKillSwitchEngaged,
			SubjectTenantID: tenantA,
			Discriminator:   discriminator,
			Attributes:      map[string]AttrValue{"reason_code": "lock-contender"},
		})
		if raiseErr == nil {
			t.Fatal("expected RaiseGuarded to propagate a lock_timeout failure (55P03)")
		}
		return raiseErr
	})

	// Release the lock holder now that session B's attempt is resolved.
	_ = txA.Rollback(ctx)

	if intxErr == nil {
		t.Fatal("expected InTx to return the propagated error")
	}
	class, swallow := classifySQLState(raiseErr)
	if swallow {
		t.Fatalf("55P03 must never be swallowed, got class %q swallow=true", class)
	}
	if class != "55" {
		t.Fatalf("expected sqlstate class 55 (lock_not_available), got %q (error: %v)", class, raiseErr)
	}

	var count int
	if err := pool.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE id = $1`, businessRowID).Scan(&count)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("expected the business row to NOT be committed when the alert statement propagates 55P03")
	}
}
