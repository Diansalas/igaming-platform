//go:build integration

// Migration 0094 mechanics (Stage 10.3 CAS-CAP-ROLLBACK-1, M-CAS-1;
// docs/plans/stage-10.3-planning/02-casino-financial-analysis.md §1.5):
// the up-migration's refusal when a supports_bet-without-settlement row
// already exists, its success on a clean database, and its down
// migration's exact restoration - all against throwaway scratch
// databases (never the shared TEST_DATABASE_URL one), mirroring
// internal/ledger/migration_0092_integration_test.go's own pattern.
package casino

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func migration0094MigrationsDir(t *testing.T) string {
	t.Helper()
	dir := "../../migrations"
	if _, err := os.Stat(dir + "/0094_casino_capability_bet_requires_settlement.up.sql"); err != nil {
		t.Fatalf("migration 0094 not found relative to internal/casino: %v", err)
	}
	return dir
}

// migration0094DirThroughSelf copies the real migrations directory into a
// fresh t.TempDir(), INCLUDING 0094's own up/down files but EXCLUDING any
// migration numbered ABOVE 94 (e.g. Stage 10.3's own concurrent 0095) -
// i.e. "the chain exactly as it stood the moment 0094 landed, and no
// later". This is what makes "roll back exactly 1 step" in
// TestMigration0094_UpDownUpRoundTrip deterministically mean "roll back
// 0094 itself", regardless of how many unrelated migrations have landed on
// top of it since - mirrors internal/sportsbook/settlement_migration_0093_
// integration_test.go's own migration0093DirThroughSelf helper.
func migration0094DirThroughSelf(t *testing.T) string {
	t.Helper()
	realDir := migration0094MigrationsDir(t)
	entries, err := os.ReadDir(realDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	out := t.TempDir()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		if len(name) < 4 {
			continue
		}
		ver, err := strconv.Atoi(name[:4])
		if err != nil {
			continue
		}
		if ver > 94 {
			continue
		}
		content, err := os.ReadFile(realDir + "/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(out+"/"+name, content, 0o600); err != nil {
			t.Fatalf("write %s into temp migrations dir: %v", name, err)
		}
	}
	return out
}

func migration0094ScratchPool(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func migration0094AppliedVersions(t *testing.T, pool *db.Pool) map[int64]bool {
	t.Helper()
	applied := map[int64]bool{}
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				return err
			}
			applied[v] = true
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return applied
}

func migration0094ConstraintExists(t *testing.T, pool *db.Pool) bool {
	t.Helper()
	var exists bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'casino_provider_capabilities_bet_requires_settlement')`,
		).Scan(&exists)
	})
	if err != nil {
		t.Fatalf("check constraint existence: %v", err)
	}
	return exists
}

// migration0094SeedTenant seeds the minimum tenants row a
// casino_provider_capabilities insert needs to satisfy its FOREIGN KEY -
// the same platform-admin-scoped pattern migration_0092_integration_test.go
// uses for ledger_transactions.
func migration0094SeedTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'Migration 0094 Test Tenant', 'under_platform_licence', 'active')`,
			tenantID, "t94-"+tenantID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenantID
}

// migration0094SeedViolatingCapability inserts a tenant-wide
// casino_provider_capabilities row with supports_bet=true and
// supports_win=false/supports_rollback=false DIRECTLY BY SQL (bypassing
// WriteCapability's own application-level check entirely) - this test is
// about the DATABASE's own backstop, independent of the Go-level fix,
// exactly like migration 0092's own seeding helper. Inserted inside
// pool.WithTenant, so FORCE ROW LEVEL SECURITY's tenant_isolation policy
// is honoured like every other write in this codebase - never bypassed to
// seed data.
func migration0094SeedViolatingCapability(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_provider_capabilities
				(id, tenant_id, brand_id, provider_id, supports_bet, supports_win, supports_rollback, status)
			 VALUES ($1, $2, NULL, 'mock-casino', true, false, false, 'active')`,
			uuid.New(), tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("seed violating capability row: %v", err)
	}
}

// TestMigration0094_PreflightRefusesViolatingCapabilityRow proves the
// pre-flight IS the validating ALTER TABLE ... ADD CONSTRAINT itself (not
// a `SELECT count(*)` pre-check, which migration 0048's own lesson - and
// this file's docs/comment - says would see zero rows under FORCE RLS as
// a non-bypass role and let a violating database through silently).
func TestMigration0094_PreflightRefusesViolatingCapabilityRow(t *testing.T) {
	pool := migration0094ScratchPool(t, "cas0094pre_")
	dir := migration0094MigrationsDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	staged := t.TempDir()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if strings.HasPrefix(e.Name(), "0094_") {
			continue
		}
		copyMigration0094File(t, dir, staged, e.Name())
	}
	if _, err := pool.MigrateUp(context.Background(), staged); err != nil {
		t.Fatalf("migrate up through 0093 (0094 held back): %v", err)
	}
	if migration0094AppliedVersions(t, pool)[94] {
		t.Fatal("migration 0094 must not have been applied yet")
	}

	tenantID := migration0094SeedTenant(t, pool)
	migration0094SeedViolatingCapability(t, pool, tenantID)

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0094_") {
			copyMigration0094File(t, dir, staged, e.Name())
		}
	}
	_, err = pool.MigrateUp(context.Background(), staged)
	if err == nil {
		t.Fatal("migration 0094 must refuse to run while a supports_bet-without-settlement row exists")
	}
	for _, want := range []string{"migration 0094", "supports_bet = true", "supports_win", "supports_rollback"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must mention %q, got: %v", want, err)
		}
	}
	if migration0094AppliedVersions(t, pool)[94] {
		t.Fatal("a failed migration 0094 must not be recorded in schema_migrations")
	}
	if migration0094ConstraintExists(t, pool) {
		t.Fatal("a failed migration 0094 must leave no partial constraint behind")
	}
}

func copyMigration0094File(t *testing.T, srcDir, dstDir, name string) {
	t.Helper()
	data, err := os.ReadFile(srcDir + "/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := os.WriteFile(dstDir+"/"+name, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestMigration0094_SucceedsOnCleanDatabaseThenEnforces proves the
// up-migration applies cleanly on a fresh database and that the resulting
// CHECK constraint is real: a subsequent raw-SQL insert violating it is
// rejected by Postgres itself, independent of WriteCapability's own
// application-level guard.
func TestMigration0094_SucceedsOnCleanDatabaseThenEnforces(t *testing.T) {
	pool := migration0094ScratchPool(t, "cas0094clean_")
	dir := migration0094MigrationsDir(t)

	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up the full chain including 0094: %v", err)
	}
	found := false
	for _, v := range applied {
		if v == 94 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected migration 94 among applied versions, got %v", applied)
	}
	if !migration0094ConstraintExists(t, pool) {
		t.Fatal("expected the settlement-completeness CHECK to exist after migration 0094")
	}

	tenantID := migration0094SeedTenant(t, pool)
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_provider_capabilities
				(id, tenant_id, brand_id, provider_id, supports_bet, supports_win, supports_rollback, status)
			 VALUES ($1, $2, NULL, 'mock-casino', true, false, false, 'active')`,
			uuid.New(), tenantID)
		return err
	})
	if err == nil {
		t.Fatal("expected the CHECK constraint to reject supports_bet=true without supports_win/supports_rollback")
	}
	if !strings.Contains(err.Error(), "casino_provider_capabilities_bet_requires_settlement") {
		t.Fatalf("expected the violation to name the CHECK constraint, got: %v", err)
	}

	// A row that DOES satisfy the invariant is accepted.
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_provider_capabilities
				(id, tenant_id, brand_id, provider_id, supports_bet, supports_win, supports_rollback, status)
			 VALUES ($1, $2, NULL, 'mock-casino', true, true, true, 'active')`,
			uuid.New(), tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("expected a compliant row to be accepted, got: %v", err)
	}
}

// TestMigration0094_UpDownUpRoundTrip: the down migration drops only the
// CHECK constraint, is safe at any time, and a subsequent up re-creates
// it. Uses migration0094DirThroughSelf so "roll back 1 step" targets 0094
// itself, not whatever migration (e.g. Stage 10.3's concurrent 0095) now
// sits on top of it in the real chain at HEAD.
func TestMigration0094_UpDownUpRoundTrip(t *testing.T) {
	pool := migration0094ScratchPool(t, "cas0094rt_")
	dir := migration0094DirThroughSelf(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up through 0094: %v", err)
	}
	if !migration0094ConstraintExists(t, pool) {
		t.Fatal("expected the constraint to exist before rolling back")
	}

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("migrate down 0094: %v", err)
	}
	if migration0094AppliedVersions(t, pool)[94] {
		t.Fatal("migration 0094 must no longer be recorded after rolling it back")
	}
	if migration0094ConstraintExists(t, pool) {
		t.Fatal("expected the constraint to be gone after rolling back migration 0094")
	}

	// Down is safe even when a (now unconstrained) violating row exists.
	tenantID := migration0094SeedTenant(t, pool)
	migration0094SeedViolatingCapability(t, pool, tenantID)

	if _, err := pool.MigrateUp(context.Background(), dir); err == nil {
		t.Fatal("re-applying migration 0094 must refuse while the violating row (inserted after down) still exists")
	}

	// Clean up the violator and confirm the round trip completes.
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM casino_provider_capabilities WHERE tenant_id = $1`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("clean up violating row: %v", err)
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-apply migration 0094 after removing the violator: %v", err)
	}
	if !migration0094ConstraintExists(t, pool) {
		t.Fatal("expected the constraint to exist again after re-applying migration 0094")
	}
}
