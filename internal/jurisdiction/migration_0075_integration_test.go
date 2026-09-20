//go:build integration

// Migration mechanics for 0075 - the down-migration's own refusal
// behavior (fix #2 of the Stage 4I Phase D fix round): a clean database
// rolls back and re-applies cleanly, but a database holding even one
// jurisdiction_precedence_configs row must refuse to roll back, loudly,
// leaving the schema and the row both intact. Mirrors
// internal/ledger/migration_0048_integration_test.go's own
// scratch-database pattern exactly, since this table's own
// TestJurisdictionPrecedenceConfigs_Immutability/RLS tests all run against
// the SHARED test database, which may already hold residue rows from
// other tests in this package - this test needs a database it can prove
// is genuinely empty at the point it exercises the clean-rollback path.
package jurisdiction

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const migration0075Version = int64(75)

// migration0076Version (Stage 4I Phase E: jurisdictions.country_code,
// platform_operations, licence_country_ceilings,
// operating_country_policies) is now the chain's tip, for the same reason
// this file's own header already documents for prior migrations landing
// on top of 0075 - this test rolls back from the most-recently-applied
// end, so it must roll back 0076 before 0075's own down migration can run
// at all. Migration 0076's own down migration is unconditionally
// reversible in this test's scenario (it never inserts a
// licence_country_ceilings/operating_country_policies row, and never
// sets jurisdictions.country_code), so it never blocks the round-trip
// this file exercises.
const migration0076Version = int64(76)

// migration0077Version (Stage 4I Phase E-SECURITY: tenant/licence/
// jurisdiction registry RLS) is now the chain's tip. Its own down
// migration is unconditionally reversible in this test's scenario -
// unlike migration 0075's own down migration, it carries no "refuse if
// rows exist" guard at all (tenants/licences/jurisdictions are core
// tables that will always hold rows; see migration 0077's own down.sql
// header comment for why that guard shape does not apply to them) - so it
// never blocks the round-trip this file exercises, exactly like 0076
// before it.
const migration0077Version = int64(77)

func migration0075MigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0075_jurisdiction_evaluation_policy_config.up.sql")); err != nil {
		t.Fatalf("migration 0075 not found in %s: %v", dir, err)
	}
	return dir
}

func migration0075ScratchDatabase(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	name := "jur0075_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

	admin, err := pgx.Connect(context.Background(), baseURL)
	if err != nil {
		t.Fatalf("connect to base database: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), baseURL)
		if err != nil {
			t.Logf("scratch database %s left behind (connect failed: %v)", name, err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
			t.Logf("scratch database %s left behind (drop failed: %v)", name, err)
		}
	})

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func migration0075ScratchPool(t *testing.T, databaseURL string) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), databaseURL, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func migration0075AppliedVersions(t *testing.T, pool *db.Pool) map[int64]bool {
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

// seedMinimalJurisdiction inserts just enough platform-wide reference data
// (a jurisdiction row) for jurisdiction_precedence_configs' own FK on
// licensing_jurisdiction_id, without needing the full tenant/brand/player
// chain seedFixture builds - this test never resolves a policy, it only
// needs one legal row to insert into jurisdiction_precedence_configs.
// Stage 4I Phase E-SECURITY (migration 0077): `jurisdictions` writes now
// require a genuinely platform-admin-scoped transaction.
func seedMinimalJurisdiction(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Migration Test Jurisdiction')`,
			id, "MJ-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}
	return id
}

// insertOnePolicyRow inserts one row directly (the sanctioned Go write
// path also works, but a raw insert keeps this test independent of
// evaluation_policy_admin.go's own behavior).
func insertOnePolicyRow(t *testing.T, pool *db.Pool, platformAdmin, jurisdictionID uuid.UUID) {
	t.Helper()
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO jurisdiction_precedence_configs (
				licensing_jurisdiction_id, operation_class, precedence, status,
				location_requirement, resolver_policy_version, precedence_policy_version,
				reason_code, created_by_actor_type, created_by_actor_id
			) VALUES ($1, $2, '[]'::jsonb, 'draft', 'unset', $3, $4, 'migration-test-insert', 'staff', $5)`,
			jurisdictionID, string(OperationPlay), PolicyVersion, PrecedencePolicyVersion, platformAdmin)
		return err
	})
	if err != nil {
		t.Fatalf("insert one policy row: %v", err)
	}
}

func countPolicyRowsUnscoped(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_precedence_configs`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count policy rows: %v", err)
	}
	return count
}

// TestMigration0075_DownMigrationCleanThenFailsOnDirtyDatabase is fix #2's
// own validation requirement: on an empty table, down->up round-trips
// cleanly; after inserting one row via the sanctioned path's own shape (a
// minimal raw insert satisfying every NOT NULL/CHECK column), down must
// fail with the exact RAISE message, and the row must survive untouched.
func TestMigration0075_DownMigrationCleanThenFailsOnDirtyDatabase(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	if !migration0075AppliedVersions(t, pool)[migration0075Version] {
		t.Fatal("expected migration 0075 to be applied")
	}

	// (a) Clean database: rolling back the three most recently applied
	// migrations (0077, then 0076, then 0075) succeeds. Neither 0077 nor
	// 0076 is this test's own subject (Stage 4I Phase E/E-SECURITY) but
	// both sit directly on top of 0075 in the chain and are unconditionally
	// reversible in this scenario, so they must be rolled back first for
	// 0075's own down migration to run at all.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 3)
	if err != nil {
		t.Fatalf("down migration must succeed on a database with zero jurisdiction_precedence_configs rows: %v", err)
	}
	wantDown := []int64{migration0077Version, migration0076Version, migration0075Version}
	if len(rolledBack) != len(wantDown) {
		t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
	}
	for i, v := range wantDown {
		if rolledBack[i] != v {
			t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
		}
	}
	if migration0075AppliedVersions(t, pool)[migration0075Version] {
		t.Fatal("migration 0075 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0076Version] {
		t.Fatal("migration 0076 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0077Version] {
		t.Fatal("migration 0077 must no longer be recorded as applied after a successful rollback")
	}

	// Round trip: up again.
	rolledUpAgain, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-applying migrations 0075/0076/0077 after a clean rollback: %v", err)
	}
	wantUp := []int64{migration0075Version, migration0076Version, migration0077Version}
	if len(rolledUpAgain) != len(wantUp) {
		t.Fatalf("expected exactly migrations %v to be re-applied in that order, got %v", wantUp, rolledUpAgain)
	}
	for i, v := range wantUp {
		if rolledUpAgain[i] != v {
			t.Fatalf("expected exactly migrations %v to be re-applied in that order, got %v", wantUp, rolledUpAgain)
		}
	}
	if !migration0075AppliedVersions(t, pool)[migration0075Version] {
		t.Fatal("expected migration 0075 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0076Version] {
		t.Fatal("expected migration 0076 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0077Version] {
		t.Fatal("expected migration 0077 to be recorded as applied again")
	}

	// (b) Dirty database: one real row now exists.
	jurisdictionID := seedMinimalJurisdiction(t, pool)
	platformAdmin := uuid.New()
	insertOnePolicyRow(t, pool, platformAdmin, jurisdictionID)
	if got := countPolicyRowsUnscoped(t, pool); got != 1 {
		t.Fatalf("expected exactly 1 row before the rollback attempt, got %d", got)
	}

	// Roll back 3 (0077, then 0076, then 0075): 0077 and 0076 both succeed
	// (neither holds any rows of its own in this scenario - 0077 carries
	// no "refuse if rows exist" guard at all, per its own down.sql header
	// comment), and the overall call then fails once it reaches 0075's own
	// guard - MigrateDown processes one migration per transaction and
	// returns the partial rolledBack list plus the error from whichever
	// one failed, so 0077/0076 stay rolled back while 0075 stays applied.
	rolledBackDirty, err := pool.MigrateDown(context.Background(), dir, 3)
	if err == nil {
		t.Fatal("migration 0075's down migration must FAIL once a jurisdiction_precedence_configs row exists - silently " +
			"dropping columns that hold policy-authoring history would destroy audit-relevant provenance")
	}
	wantDirty := []int64{migration0077Version, migration0076Version}
	if len(rolledBackDirty) != len(wantDirty) {
		t.Fatalf("expected exactly migrations %v to have been rolled back before the failure, got %v", wantDirty, rolledBackDirty)
	}
	for i, v := range wantDirty {
		if rolledBackDirty[i] != v {
			t.Fatalf("expected exactly migrations %v to have been rolled back before the failure, got %v", wantDirty, rolledBackDirty)
		}
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %T: %v", err, err)
	}
	if pgErr.Code != pgRaisedError {
		t.Fatalf("expected SQLSTATE %s, got %s: %v", pgRaisedError, pgErr.Code, err)
	}
	for _, want := range []string{
		"jurisdiction_precedence_configs is append-only",
		"refusing to roll back migration 0075",
		"restore from backup",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the guard's exception must mention %q, got: %v", want, err)
		}
	}

	// The failed rollback left everything intact: migration 0075 still
	// applied, and the row still there, byte for byte. Re-apply 0076 so
	// this scratch database ends in a consistent, fully-migrated state.
	if !migration0075AppliedVersions(t, pool)[migration0075Version] {
		t.Fatal("a failed rollback must leave migration 0075 recorded as applied")
	}
	if got := countPolicyRowsUnscoped(t, pool); got != 1 {
		t.Fatalf("expected the row to survive the failed rollback untouched, got %d rows", got)
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migrations 0076/0077 after the aborted rollback: %v", err)
	}
}

// TestMigration0075_DownMigrationRestoresPreMigrationRLSPosture is the
// security P4-1 finding: the down migration must leave the table's RLS
// flags matching its EXACT pre-0075 state (relrowsecurity=false,
// relforcerowsecurity=false), not FORCE=true with RLS disabled (which a
// DISABLE ROW LEVEL SECURITY with no preceding NO FORCE would produce).
func TestMigration0075_DownMigrationRestoresPreMigrationRLSPosture(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	// Roll back 3 (0077, then 0076, then 0075) - see this file's own header/
	// migration0076Version/migration0077Version comments for why both must
	// be accounted for explicitly here.
	if _, err := pool.MigrateDown(context.Background(), dir, 3); err != nil {
		t.Fatalf("down migration on a clean database: %v", err)
	}

	var enabled, forced bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'jurisdiction_precedence_configs'`).Scan(&enabled, &forced)
	})
	if err != nil {
		t.Fatalf("read jurisdiction_precedence_configs RLS flags: %v", err)
	}
	if enabled {
		t.Error("expected relrowsecurity=false after rolling back migration 0075 (matching migration 0071's original posture)")
	}
	if forced {
		t.Error("expected relforcerowsecurity=false after rolling back migration 0075 - a bare DISABLE ROW LEVEL SECURITY with no preceding NO FORCE would leave this true")
	}
}
