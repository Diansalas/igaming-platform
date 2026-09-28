//go:build integration

package alerting

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedTenant creates a tenant row (as a platform-admin session, since
// tenants' own RLS requires one).
func seedTenant(t *testing.T, pool *db.Pool, adminID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'own_licence')`,
			id, "alerting-test-"+id.String()[:8], "alerting-test-"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// seedPlatformAdmin creates a platform-scoped staff_users row and returns
// its id, without going through any tenant/RLS-restricted path (the
// dual_scope_isolation policy on staff_users allows this with no GUC
// set).
func seedPlatformAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "admin-"+id.String()+"@platform.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

// seedTenantStaff creates a tenant_admin staff_users row for tenantID.
func seedTenantStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			id, tenantID, "staff-"+id.String()+"@tenant.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant staff: %v", err)
	}
	return id
}
