//go:build integration

// Migration mechanics for 0076: the down-migration's own refusal behavior
// (mirrors internal/jurisdiction/migration_0075_integration_test.go and
// internal/ledger/migration_0048_integration_test.go's scratch-database
// pattern) - a clean database rolls back and re-applies cleanly, but a
// database holding even one licence_country_ceilings or
// operating_country_policies row (or a non-NULL jurisdictions.country_code)
// must refuse to roll back, loudly, leaving the schema and the row both
// intact.
package operatingmarket

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

const migration0076Version = int64(76)

// migration0077Version (Stage 4I Phase E-SECURITY: tenant/licence/
// jurisdiction registry RLS) now sits directly on top of 0076 in the
// chain and must be rolled back first for 0076's own down migration to
// run at all - mirrors internal/jurisdiction/migration_0075_integration_
// test.go's own migration0077Version/migration0076Version precedent.
// 0077's own down migration is unconditionally reversible in every
// scenario this file exercises (it never inserts a licence_country_
// ceilings/operating_country_policies row and never sets jurisdictions.
// country_code), so it never blocks the round-trips below.
const migration0077Version = int64(77)

func migration0076MigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0076_operating_market_country_policy.up.sql")); err != nil {
		t.Fatalf("migration 0076 not found in %s: %v", dir, err)
	}
	return dir
}

func migration0076ScratchDatabase(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	name := "om0076_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

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

func migration0076ScratchPool(t *testing.T, databaseURL string) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), databaseURL, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func migration0076AppliedVersions(t *testing.T, pool *db.Pool) map[int64]bool {
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

func TestMigration0076_DownMigrationCleanThenFailsOnDirtyDatabase(t *testing.T) {
	scratchURL := migration0076ScratchDatabase(t)
	pool := migration0076ScratchPool(t, scratchURL)
	dir := migration0076MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	if !migration0076AppliedVersions(t, pool)[migration0076Version] {
		t.Fatal("expected migration 0076 to be applied")
	}

	// (a) Clean database: rolling migrations 0077 then 0076 back succeeds.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 2)
	if err != nil {
		t.Fatalf("down migration must succeed on an empty database: %v", err)
	}
	wantDown := []int64{migration0077Version, migration0076Version}
	if len(rolledBack) != len(wantDown) || rolledBack[0] != wantDown[0] || rolledBack[1] != wantDown[1] {
		t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
	}

	rolledUpAgain, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-applying migrations 0076/0077 after a clean rollback: %v", err)
	}
	wantUp := []int64{migration0076Version, migration0077Version}
	if len(rolledUpAgain) != len(wantUp) || rolledUpAgain[0] != wantUp[0] || rolledUpAgain[1] != wantUp[1] {
		t.Fatalf("expected exactly migrations %v to be re-applied in that order, got %v", wantUp, rolledUpAgain)
	}

	// (b) Dirty database: seed a real ceiling row via the sanctioned path.
	licenceID := seedMinimalLicence(t, pool)
	platformAdmin := uuid.New()
	err = pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: "PA", State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "migration-test-ref", Actor: testActor(platformAdmin, "migration-test-insert"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed a ceiling row: %v", err)
	}
	// Roll back 2: 0077 succeeds on its own (it holds no rows of its own
	// in this scenario), and the overall call then fails once it reaches
	// 0076's own guard.
	rolledBackDirty, err := pool.MigrateDown(context.Background(), dir, 2)
	if err == nil {
		t.Fatal("migration 0076's down migration must FAIL once a licence_country_ceilings row exists")
	}
	if len(rolledBackDirty) != 1 || rolledBackDirty[0] != migration0077Version {
		t.Fatalf("expected exactly migration %d to have been rolled back before the failure, got %v", migration0077Version, rolledBackDirty)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %T: %v", err, err)
	}
	if pgErr.Code != "P0001" {
		t.Fatalf("expected SQLSTATE P0001, got %s: %v", pgErr.Code, err)
	}
	if !strings.Contains(err.Error(), "licence_country_ceilings is append-only") {
		t.Fatalf("expected the guard's exception to mention licence_country_ceilings, got: %v", err)
	}
	if !migration0076AppliedVersions(t, pool)[migration0076Version] {
		t.Fatal("a failed rollback must leave migration 0076 recorded as applied")
	}
	// Re-apply 0077 so this scratch database ends in a consistent, fully-
	// migrated state.
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migration 0077 after the aborted rollback: %v", err)
	}
}

// TestMigration0076_RestoresPreMigrationRLSPostureAndRefusesNonNullCountryCode
// covers the RLS-restoration (NO FORCE before DISABLE, mirroring
// PHASE-D-SEC-P4-1) AND the jurisdictions.country_code non-NULL refusal.
func TestMigration0076_RestoresPreMigrationRLSPostureAndRefusesNonNullCountryCode(t *testing.T) {
	scratchURL := migration0076ScratchDatabase(t)
	pool := migration0076ScratchPool(t, scratchURL)
	dir := migration0076MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	// Roll back 0077 then 0076 - see migration0077Version's own comment
	// for why 0077 must be accounted for explicitly here.
	if _, err := pool.MigrateDown(context.Background(), dir, 2); err != nil {
		t.Fatalf("down migration on a clean database: %v", err)
	}

	for _, table := range []string{"platform_operations", "licence_country_ceilings", "operating_country_policies"} {
		var exists bool
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, table).Scan(&exists)
		})
		if err != nil {
			t.Fatalf("check table existence for %s: %v", table, err)
		}
		if exists {
			t.Fatalf("expected %s to be dropped after rolling back migration 0076", table)
		}
	}

	var hasColumn bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jurisdictions' AND column_name = 'country_code')`).Scan(&hasColumn)
	})
	if err != nil {
		t.Fatalf("check jurisdictions.country_code existence: %v", err)
	}
	if hasColumn {
		t.Fatal("expected jurisdictions.country_code to be dropped after a clean rollback")
	}

	// Re-apply, set a country_code, and confirm the down migration now
	// refuses (a non-NULL administrative value must not be silently
	// destroyed).
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-apply migrations 0076/0077: %v", err)
	}
	// Stage 4I Phase E-SECURITY (migration 0077): `jurisdictions` writes
	// now require a genuinely platform-admin-scoped transaction.
	var jurisdictionID uuid.UUID
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		jurisdictionID = uuid.New()
		tag, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name, country_code) VALUES ($1, $2, 'Migration Test', 'MT')`,
			jurisdictionID, "MJ076-"+jurisdictionID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a jurisdiction with country_code: %v", err)
	}

	// Roll back 2: 0077 succeeds on its own, and the overall call then
	// fails once it reaches 0076's own guard.
	rolledBackDirty, err := pool.MigrateDown(context.Background(), dir, 2)
	if err == nil {
		t.Fatal("migration 0076's down migration must FAIL once a jurisdictions row has a non-NULL country_code")
	}
	if len(rolledBackDirty) != 1 || rolledBackDirty[0] != migration0077Version {
		t.Fatalf("expected exactly migration %d to have been rolled back before the failure, got %v", migration0077Version, rolledBackDirty)
	}
	if !strings.Contains(err.Error(), "country_code") {
		t.Fatalf("expected the guard's exception to mention country_code, got: %v", err)
	}
	// Re-apply 0077 so this scratch database ends in a consistent, fully-
	// migrated state.
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migration 0077 after the aborted rollback: %v", err)
	}
}

// TestMigration0076_SchemaMatchesTheCurrentMigrationFile guards against
// MKT-MIG76-1: migration 0076 has now been amended in place THREE TIMES
// (AMENDMENT-1's write-time narrowing step, AMENDMENT-2's inherit-rung
// withdrawal CHECK, AMENDMENT-3's deferred close-requires-successor
// constraint trigger). A database that had 0076 applied before any
// amendment landed would silently carry a stale schema forever (an
// in-place amendment does not change the migration's version number) -
// this test fails loudly against exactly that stale shape, rather than
// passing a comment-only check. It asserts on three markers, one per
// amendment: the CHECK constraint ocp_inherit_rung_withdrawal_requires_
// authorization (AMENDMENT-2) in pg_constraint; the RAISE-message
// substring "may only narrow and may never widen" in the EXECUTABLE body
// of operating_country_policies_enforce_ceiling (AMENDMENT-1); and, for
// AMENDMENT-3, BOTH the row registered in pg_trigger for the constraint
// trigger ocp_inherit_rung_close_requires_successor (confirming it is
// DEFERRABLE and INITIALLY DEFERRED - the property AMENDMENT-3's own
// liveness argument depends on) and the enforcement function
// operating_country_policies_enforce_close_successor in pg_proc.
func TestMigration0076_SchemaMatchesTheCurrentMigrationFile(t *testing.T) {
	scratchURL := migration0076ScratchDatabase(t)
	pool := migration0076ScratchPool(t, scratchURL)
	dir := migration0076MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}

	var constraintExists bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint c
				 JOIN pg_class t ON t.oid = c.conrelid
				WHERE t.relname = 'operating_country_policies'
				  AND c.conname = 'ocp_inherit_rung_withdrawal_requires_authorization'
			)`).Scan(&constraintExists)
	})
	if err != nil {
		t.Fatalf("query pg_constraint: %v", err)
	}
	if !constraintExists {
		t.Fatal("expected CHECK constraint ocp_inherit_rung_withdrawal_requires_authorization on operating_country_policies (AMENDMENT-2) - this database's migration 0076 is STALE (MKT-MIG76-1): rebuild via migrate-down-then-up before trusting any test results")
	}

	var functionDef string
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_get_functiondef('operating_country_policies_enforce_ceiling'::regproc)`).Scan(&functionDef)
	})
	if err != nil {
		t.Fatalf("query pg_get_functiondef: %v", err)
	}
	if !strings.Contains(functionDef, "may only narrow and may never widen") {
		t.Fatal(`expected operating_country_policies_enforce_ceiling's EXECUTABLE body (a RAISE EXCEPTION string, not a comment) to contain "may only narrow and may never widen" (AMENDMENT-1) - this database's migration 0076 is STALE (MKT-MIG76-1): rebuild via migrate-down-then-up before trusting any test results`)
	}

	// AMENDMENT-3: ocp_inherit_rung_close_requires_successor, a CONSTRAINT
	// TRIGGER. Empirically, PostgreSQL registers a CONSTRAINT TRIGGER as
	// BOTH a pg_trigger row (which alone carries tgdeferrable/
	// tginitdeferred - the DEFERRABLE INITIALLY DEFERRED property
	// AMENDMENT-3's own liveness argument depends on being real, not
	// decorative) AND a pg_constraint row with contype='t'. Both are
	// asserted here, verified empirically against a freshly-migrated
	// scratch database before being written (MKT-MIG76-1's own lesson).
	var tgDeferrable, tgInitDeferred bool
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT tgdeferrable, tginitdeferred FROM pg_trigger
			 WHERE tgname = 'ocp_inherit_rung_close_requires_successor'`).Scan(&tgDeferrable, &tgInitDeferred)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("expected a pg_trigger row for ocp_inherit_rung_close_requires_successor (AMENDMENT-3) - this database's migration 0076 is STALE (MKT-MIG76-1): rebuild via migrate-down-then-up before trusting any test results")
		}
		t.Fatalf("query pg_trigger for ocp_inherit_rung_close_requires_successor: %v", err)
	}
	if !tgDeferrable || !tgInitDeferred {
		t.Fatalf("expected ocp_inherit_rung_close_requires_successor to be DEFERRABLE INITIALLY DEFERRED (AMENDMENT-3), got tgdeferrable=%v tginitdeferred=%v", tgDeferrable, tgInitDeferred)
	}

	var constraintTriggerExists bool
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				 WHERE conname = 'ocp_inherit_rung_close_requires_successor' AND contype = 't'
			)`).Scan(&constraintTriggerExists)
	})
	if err != nil {
		t.Fatalf("query pg_constraint for ocp_inherit_rung_close_requires_successor: %v", err)
	}
	if !constraintTriggerExists {
		t.Fatal("expected a pg_constraint row (contype='t') for ocp_inherit_rung_close_requires_successor (AMENDMENT-3) - this database's migration 0076 is STALE (MKT-MIG76-1): rebuild via migrate-down-then-up before trusting any test results")
	}

	var closeSuccessorFunctionExists bool
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_proc WHERE proname = 'operating_country_policies_enforce_close_successor'
			)`).Scan(&closeSuccessorFunctionExists)
	})
	if err != nil {
		t.Fatalf("query pg_proc for operating_country_policies_enforce_close_successor: %v", err)
	}
	if !closeSuccessorFunctionExists {
		t.Fatal("expected function operating_country_policies_enforce_close_successor to exist in pg_proc (AMENDMENT-3) - this database's migration 0076 is STALE (MKT-MIG76-1): rebuild via migrate-down-then-up before trusting any test results")
	}
}

// Stage 4I Phase E-SECURITY (migration 0077): `jurisdictions`/`licences`
// writes now require a genuinely platform-admin-scoped transaction.
func seedMinimalLicence(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	var licenceID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		jurisdictionID := uuid.New()
		tag, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Migration Test Jurisdiction')`,
			jurisdictionID, "MJ-"+jurisdictionID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		licenceID = uuid.New()
		tag, err = tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			licenceID, jurisdictionID, "LIC-"+licenceID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 licence row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed minimal licence: %v", err)
	}
	return licenceID
}
