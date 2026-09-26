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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
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

// migration0078Version (Stage 6: sportsbook foundation) now sits directly
// on top of 0077 in the chain, for the same reason 0077 sits on top of
// 0076 - unconditionally reversible in every scenario this file exercises
// (it never posts a sportsbook_bet transaction), so it never blocks the
// round-trips below.
const migration0078Version = int64(78)

// migration0079Version (Stage 7: casino_launch_sessions brand-pinning fix)
// now sits directly on top of 0078 in the chain, for the same reason 0078
// sits on top of 0077 - it only replaces one FK constraint (no data
// dependency), so it is unconditionally reversible in every scenario this
// file exercises and never blocks the round-trips below.
const migration0079Version = int64(79)

// migration0080Version (Stage 8: casino_provider_rounds, ADR 0080
// Decision 1) now sits directly on top of 0079 in the chain - it only
// creates a new, empty additive table, so it is unconditionally
// reversible in every scenario this file exercises and never blocks the
// round-trips below.
const migration0080Version = int64(80)

// migration0081Version (Stage 8: sportsbook_bets provider reference
// columns, ADR 0080 Decision 2) now sits directly on top of 0080 in the
// chain - it only adds two nullable columns and a partial unique index
// (no data dependency), so it is unconditionally reversible in every
// scenario this file exercises and never blocks the round-trips below.
const migration0081Version = int64(81)

// migration0082Version (Stage 9: the bundled DB-hardening migration -
// immutability/TRUNCATE-deny triggers, ARCH-DB-3 composite brand pinning,
// and three pagination indexes) is now the chain's tip, for the same
// reason 0076-0081 each were in turn - it only adds triggers, indexes and
// foreign keys, so it is unconditionally reversible in this test's
// scenario and never blocks the round-trip this file exercises.
const migration0082Version = int64(82)

// migration0083Version (Stage 9.1, PLAT-MIGDRIFT-1: adds
// schema_migrations.checksum) is now the chain's tip, for the same reason
// 0076-0082 each were in turn - it only adds a single nullable column to
// schema_migrations itself (no data dependency), so it is unconditionally
// reversible in every scenario this file exercises and never blocks the
// round-trips below.
const migration0083Version = int64(83)

// migration0084Version (Stage 9.1, ARCH-DB-2: RLS + immutable-identity +
// deny-delete/truncate triggers on the six platform catalogue tables) is
// now the chain's tip, for the same reason 0076-0083 each were in turn -
// it only adds RLS policies, triggers and a shared trigger function to
// casino_games/sb_* tables, which this file never writes to, so it is
// unconditionally reversible in every scenario this file exercises and
// never blocks the round-trips below.
const migration0084Version = int64(84)

// migration0085Version (Stage 9.1, SEC-S91-3: casino_games write policies
// now require app.platform_admin_principal_id to resolve to a real
// platform-scoped staff_users row) is now the chain's tip, for the same
// reason 0076-0084 each were in turn - it only adds one trigger function
// and one trigger on casino_games, which this file never writes to, so it
// is unconditionally reversible in every scenario this file exercises and
// never blocks the round-trips below.
const migration0085Version = int64(85)

// migration0086Version (Stage 9.2, casino-catalogue-dual-control:
// four-eyes governance for casino_games) is now the chain's tip, for the
// same reason 0076-0085 each were in turn - it only adds a new table
// (casino_catalogue_change_requests) and triggers on casino_games, which
// this file never writes to, so it is unconditionally reversible in every
// scenario this file exercises and never blocks the round-trips below.
const migration0086Version = int64(86)

// migration0087Version (Stage 9.2, ADR 0083 Part C: sportsbook
// jurisdiction/market gating) is now the chain's tip, for the same reason
// 0076-0086 each were in turn - it only adds a new table
// (sb_jurisdiction_restrictions) and a nullable column on
// sportsbook_bets, neither of which this file writes to, so it is
// unconditionally reversible in every scenario this file exercises and
// never blocks the round-trips below.
const migration0087Version = int64(87)

// migration0088Version (Stage 9.2, ADR 0083 Part B2/Wave 3: sportsbook
// cross-player book-exposure limits) is now the chain's tip, for the same
// reason 0076-0087 each were in turn - it only adds a new table
// (sb_exposure_limits) and a partial index on sportsbook_bets, neither of
// which this file writes to, so it is unconditionally reversible in every
// scenario this file exercises and never blocks the round-trips below.
const migration0088Version = int64(88)

// migration0089Version (Stage 9.2 fix round, casino specialist: SEC-S92-1's
// casino_catalogue_change_requests/_approvals principal-eligibility
// hardening to migration 0047's shape) is now the chain's tip, for the
// same reason 0076-0088 each were in turn - it only replaces two trigger
// function bodies on casino_games-adjacent tables, which this file never
// writes to, so it is unconditionally reversible in every scenario this
// file exercises and never blocks the round-trips below. Landed
// concurrently with migration0090Version below by a parallel Stage 9.2
// fix-round workstream - not this package's own subject.
const migration0089Version = int64(89)

// migration0090Version (Stage 9.2 fix round, SEC-S92-2: defense-in-depth
// staff-principal-resolution trigger on sb_jurisdiction_restrictions,
// mirroring migration 0085's identical shape for casino_games) is now the
// chain's tip, for the same reason 0076-0089 each were in turn - it only
// adds one trigger function and one trigger on sb_jurisdiction_
// restrictions, which this file never writes to, so it is unconditionally
// reversible in every scenario this file exercises and never blocks the
// round-trips below.
const migration0090Version = int64(90)

// migration0091Version (Stage 10 W1, ADR 0088 §12: sportsbook settlement
// lifecycle). Its down migration refuses once any sportsbook settlement
// evidence exists, but no scenario in this file places or settles a bet,
// so it is reversible in every scenario this file exercises and never
// blocks the round-trips below.
const migration0091Version = int64(91)

// migration0092Version (Stage 10.1: one deposit_reversal per original
// deposit) only adds a partial unique index on ledger_transactions; its
// down just drops the index, so it is unconditionally reversible in every
// scenario this file exercises and never blocks the round-trips below.
const migration0092Version = int64(92)

// migration0093Version (Stage 10.1: sportsbook settlement causation check
// via xact status). It only replaces a trigger function body and its down
// restores the prior body, so it is unconditionally reversible in every
// scenario this file exercises and never blocks the round-trips below.
const migration0093Version = int64(93)

// migration0094Version (Stage 10.3, CAS-CAP-ROLLBACK-1/M-CAS-1: the
// casino_provider_capabilities settlement-completeness CHECK) is now the
// chain's tip. It only adds one CHECK constraint on a table no scenario in
// this file writes a row to, so it is unconditionally reversible here too.
const migration0094Version = int64(94)

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
	return scratchdb.New(t, "om0076_")
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

	// (a) Clean database: rolling migrations 0093, 0092, 0091, 0090, 0089, 0088, 0087,
	// 0086, 0085, 0084, 0083, 0082, 0081, 0080, 0079, 0078, 0077, then 0076
	// back succeeds.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 19)
	if err != nil {
		t.Fatalf("down migration must succeed on an empty database: %v", err)
	}
	wantDown := []int64{
		migration0094Version, migration0093Version, migration0092Version, migration0091Version, migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version,
		migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version,
		migration0077Version, migration0076Version,
	}
	if len(rolledBack) != len(wantDown) {
		t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
	}
	for i, v := range wantDown {
		if rolledBack[i] != v {
			t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
		}
	}

	rolledUpAgain, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-applying migrations 0076/0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093 after a clean rollback: %v", err)
	}
	wantUp := []int64{
		migration0076Version, migration0077Version, migration0078Version, migration0079Version, migration0080Version,
		migration0081Version, migration0082Version, migration0083Version, migration0084Version, migration0085Version,
		migration0086Version, migration0087Version, migration0088Version, migration0089Version, migration0090Version,
		migration0091Version, migration0092Version, migration0093Version, migration0094Version,
	}
	if len(rolledUpAgain) != len(wantUp) {
		t.Fatalf("expected exactly migrations %v to be re-applied in that order, got %v", wantUp, rolledUpAgain)
	}
	for i, v := range wantUp {
		if rolledUpAgain[i] != v {
			t.Fatalf("expected exactly migrations %v to be re-applied in that order, got %v", wantUp, rolledUpAgain)
		}
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
	// Roll back 18: 0093, 0092, 0091, 0090, 0089, 0088, 0087, 0086, 0085, 0084, 0083,
	// 0082, 0081, 0080, 0079, 0078, and 0077 all succeed on their own (none
	// holds any rows of its own in this scenario; 0091 refuses only once
	// sportsbook settlement evidence exists), and the overall call
	// then fails once it reaches 0076's own guard.
	rolledBackDirty, err := pool.MigrateDown(context.Background(), dir, 19)
	if err == nil {
		t.Fatal("migration 0076's down migration must FAIL once a licence_country_ceilings row exists")
	}
	wantDirty := []int64{
		migration0094Version, migration0093Version, migration0092Version, migration0091Version, migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version,
		migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version, migration0077Version,
	}
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
	if pgErr.Code != "P0001" {
		t.Fatalf("expected SQLSTATE P0001, got %s: %v", pgErr.Code, err)
	}
	if !strings.Contains(err.Error(), "licence_country_ceilings is append-only") {
		t.Fatalf("expected the guard's exception to mention licence_country_ceilings, got: %v", err)
	}
	if !migration0076AppliedVersions(t, pool)[migration0076Version] {
		t.Fatal("a failed rollback must leave migration 0076 recorded as applied")
	}
	// Re-apply 0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093
	// so this scratch database ends in a consistent, fully-migrated state.
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migrations 0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093 after the aborted rollback: %v", err)
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
	// Roll back 0093, 0092, 0091, 0090, 0089, 0088, 0087, 0086, 0085, 0084, 0083, 0082,
	// 0081, 0080, 0079, 0078, 0077, then 0076 - see migration0077Version's
	// through migration0093Version's own comments for why all seventeen
	// must be accounted for explicitly here.
	if _, err := pool.MigrateDown(context.Background(), dir, 19); err != nil {
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
		t.Fatalf("re-apply migrations 0076/0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093: %v", err)
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

	// Roll back 18: 0093, 0092, 0091, 0090, 0089, 0088, 0087, 0086, 0085, 0084, 0083,
	// 0082, 0081, 0080, 0079, 0078, and 0077 all succeed on their own, and
	// the overall call then fails once it reaches 0076's own guard.
	rolledBackDirty, err := pool.MigrateDown(context.Background(), dir, 19)
	if err == nil {
		t.Fatal("migration 0076's down migration must FAIL once a jurisdictions row has a non-NULL country_code")
	}
	wantDirty := []int64{
		migration0094Version, migration0093Version, migration0092Version, migration0091Version, migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version,
		migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version, migration0077Version,
	}
	if len(rolledBackDirty) != len(wantDirty) {
		t.Fatalf("expected exactly migrations %v to have been rolled back before the failure, got %v", wantDirty, rolledBackDirty)
	}
	for i, v := range wantDirty {
		if rolledBackDirty[i] != v {
			t.Fatalf("expected exactly migrations %v to have been rolled back before the failure, got %v", wantDirty, rolledBackDirty)
		}
	}
	if !strings.Contains(err.Error(), "country_code") {
		t.Fatalf("expected the guard's exception to mention country_code, got: %v", err)
	}
	// Re-apply 0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093
	// so this scratch database ends in a consistent, fully-migrated state.
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migrations 0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093 after the aborted rollback: %v", err)
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
