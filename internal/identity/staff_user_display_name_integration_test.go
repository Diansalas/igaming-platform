//go:build integration

package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestUpdateStaffDisplayName_SetsAndReturnsPrior exercises the CTE-based
// before-value capture UpdateStaffDisplayName relies on: unset -> a value
// -> a different value, checking the returned "previous" each time.
func TestUpdateStaffDisplayName_SetsAndReturnsPrior(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	var staffID = createTestStaffUser(t, pool, tenant)

	var prior *string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		prior, err = UpdateStaffDisplayName(ctx, tx, staffID, "Alice Admin")
		return err
	})
	if err != nil {
		t.Fatalf("first rename: %v", err)
	}
	if prior != nil {
		t.Fatalf("expected nil prior value on first rename, got %v", *prior)
	}

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		prior, err = UpdateStaffDisplayName(ctx, tx, staffID, "Alice A. Admin")
		return err
	})
	if err != nil {
		t.Fatalf("second rename: %v", err)
	}
	if prior == nil || *prior != "Alice Admin" {
		t.Fatalf("expected prior=%q, got %v", "Alice Admin", prior)
	}

	var current *string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT display_name FROM staff_users WHERE id = $1`, staffID).Scan(&current)
	}); err != nil {
		t.Fatal(err)
	}
	if current == nil || *current != "Alice A. Admin" {
		t.Fatalf("expected current display_name=%q, got %v", "Alice A. Admin", current)
	}
}

// TestUpdateStaffDisplayName_RejectsInvalidBeforeAnySQL confirms Go-level
// validation runs before any UPDATE - an invalid name never reaches the
// database at all, matching the DB's own CHECK exactly (defense in depth).
func TestUpdateStaffDisplayName_RejectsInvalidBeforeAnySQL(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	staffID := createTestStaffUser(t, pool, tenant)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpdateStaffDisplayName(ctx, tx, staffID, strings.Repeat("a", 101))
		return err
	})
	if !errors.Is(err, ErrInvalidDisplayName) {
		t.Fatalf("expected ErrInvalidDisplayName, got %v", err)
	}

	// Confirm nothing was written.
	var current *string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT display_name FROM staff_users WHERE id = $1`, staffID).Scan(&current)
	}); err != nil {
		t.Fatal(err)
	}
	if current != nil {
		t.Fatalf("expected display_name to remain unset, got %v", *current)
	}
}

// TestUpdateStaffDisplayName_NotFound confirms a staff id outside the
// caller's own scope (or simply absent) returns ErrNotFound, never a
// silent no-op success.
func TestUpdateStaffDisplayName_NotFound(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	otherTenant := createTestTenant(t, pool)
	staffID := createTestStaffUser(t, pool, otherTenant)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpdateStaffDisplayName(ctx, tx, staffID, "Should Not Apply")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a staff id outside the tenant's own RLS scope, got %v", err)
	}
}

// createTestStaffUser is a small local helper (this package has no
// existing one) - hashing is irrelevant to these tests, so a fixed
// placeholder is used.
func createTestStaffUser(t *testing.T, pool *db.Pool, tenant Tenant) uuid.UUID {
	t.Helper()
	email := "staff-" + uuid.NewString() + "@example.com"
	var staff StaffUser
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		staff, err = CreateStaffUser(ctx, tx, tenant.ID, email, "x", StaffRoleTenantAdmin, nil)
		return err
	})
	if err != nil {
		t.Fatalf("create staff user: %v", err)
	}
	return staff.ID
}
