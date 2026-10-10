//go:build integration

package audit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// createTestTenant seeds a tenant. Stage 4I Phase E-SECURITY (migration
// 0077): `tenants` gained RLS with no tenant-scoped/scopeless write policy
// of any kind, so both the INSERT and the DELETE cleanup below now
// require a genuinely platform-admin-scoped transaction
// (db.Pool.WithPlatformAdmin), not WithoutTenant.
func createTestTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, $2, $3, 'under_platform_licence', 'active')`,
			id, "Audit Test Tenant "+id.String(), "audit-test-"+id.String(),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to create test tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				t.Errorf("cleanup: expected to delete 1 tenant row, deleted %d", tag.RowsAffected())
			}
			return nil
		})
	})
	return id
}

func TestRecord_TenantScopedEntryVisibleOnlyToItsTenant(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)
	actorID := uuid.New()

	err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{
			TenantID: tenantA, ActorType: ActorPlayer, ActorID: actorID,
			Action: "test.event", Outcome: OutcomeSuccess,
		})
	})
	if err != nil {
		t.Fatalf("unexpected error recording entry: %v", err)
	}

	var countA, countB int
	_ = pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'test.event' AND actor_id = $1`, actorID).Scan(&countA)
	})
	_ = pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'test.event' AND actor_id = $1`, actorID).Scan(&countB)
	})

	if countA != 1 {
		t.Errorf("expected tenant A to see its own entry, count=%d", countA)
	}
	if countB != 0 {
		t.Errorf("expected tenant B to see none of tenant A's entries, count=%d", countB)
	}
}

func TestRecord_PlatformLevelEntryVisibleOnlyWithoutTenant(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	action := "test.platform_event_" + uuid.NewString()

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorSystem, Action: action, Outcome: OutcomeSuccess})
	})
	if err != nil {
		t.Fatalf("unexpected error recording platform-level entry: %v", err)
	}

	var countTenantScoped, countPlatform int
	_ = pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&countTenantScoped)
	})
	_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&countPlatform)
	})

	if countTenantScoped != 0 {
		t.Errorf("expected a tenant-scoped connection to see 0 platform-level entries, got %d", countTenantScoped)
	}
	if countPlatform != 1 {
		t.Errorf("expected a platform-scoped (WithoutTenant) connection to see the entry, got %d", countPlatform)
	}
}

func TestRecord_RejectsSystemActorWithNonNilID(t *testing.T) {
	pool := testPool(t)
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorSystem, ActorID: uuid.New(), Action: "bad", Outcome: OutcomeSuccess})
	})
	if err == nil {
		t.Fatal("expected an error for a system actor with a non-nil actor id, got nil")
	}
}

func TestRecord_RejectsNonSystemActorWithNilID(t *testing.T) {
	pool := testPool(t)
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorPlayer, Action: "bad", Outcome: OutcomeSuccess})
	})
	if err == nil {
		t.Fatal("expected an error for a player actor with a nil actor id, got nil")
	}
}

// TestAuditLog_ImmutableEvenForOwningRole is the load-bearing proof for
// docs/decisions/0013: the append-only guarantee holds even for the
// application's own (non-superuser, table-owning) database role -
// REVOKE-based protection would not survive that, a trigger does.
func TestAuditLog_ImmutableEvenForOwningRole(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	actorID := uuid.New()

	err := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{TenantID: tenant, ActorType: ActorPlayer, ActorID: actorID, Action: "immutable.test", Outcome: OutcomeSuccess})
	})
	if err != nil {
		t.Fatalf("unexpected error recording entry: %v", err)
	}

	updateErr := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_log SET outcome = 'failure' WHERE actor_id = $1`, actorID)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(updateErr, &pgErr) {
		t.Fatalf("expected UPDATE against audit_log to fail with a Postgres error, got: %v", updateErr)
	}

	deleteErr := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE actor_id = $1`, actorID)
		return err
	})
	if !errors.As(deleteErr, &pgErr) {
		t.Fatalf("expected DELETE against audit_log to fail with a Postgres error, got: %v", deleteErr)
	}

	// Confirm the row is genuinely untouched.
	var outcome string
	err = pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT outcome FROM audit_log WHERE actor_id = $1`, actorID).Scan(&outcome)
	})
	if err != nil {
		t.Fatalf("expected the original row to still exist, got error: %v", err)
	}
	if outcome != string(OutcomeSuccess) {
		t.Errorf("expected outcome to remain %q, got %q", OutcomeSuccess, outcome)
	}
}
