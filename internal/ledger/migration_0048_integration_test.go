//go:build integration

// Migration mechanics for 0048 - §6.5.8 items 6 and 7, the two cases that
// cannot be tested against an already-migrated database because they are
// about the migration's own pre-state and roll-back behavior:
//
//	item 6: the up-migration's pre-flight guard FIRES when a bare
//	        player_locked row exists, with its named exception, and leaves
//	        the schema - and ledger_accounts' FORCE RLS flag - untouched.
//	item 7: the down-migration succeeds on a clean database and FAILS with
//	        SQLSTATE 23514 on one holding a player_locked_cash account -
//	        the rehearsal §6.3.2 requires, mirroring migration 0035's
//	        documented behavior.
//
// Both run against their own throwaway database built from the real
// migrations directory, so they exercise the actual 0001..0049 chain
// (0048 filling its reserved gap) and never touch the shared test
// database.
package ledger

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

const migration0048Version = int64(48)

// migrationsDependentOn0048Prefixes are the migration files whose up.sql
// performs a DROP CONSTRAINT/ADD CONSTRAINT on ledger_accounts_
// account_type_check assuming migration 0048's exact prior state (its
// twelve-value list) as the baseline to widen from - migrations 0050
// (bonus_expense) and 0052 (player_bonus_held), added in this same Stage
// 4H-B1 Wave 2 dispatch. Migration 0051 (the bonus_* transaction types)
// touches ledger_transactions, not ledger_accounts, and has no such
// dependency, so it is deliberately NOT included here and stays applied
// normally even when 0048 is held back.
//
// Holding ONLY 0048 back while still running 0050/0052 would leave the
// chain in an incoherent state no real deployment could reach: 0050/0052
// would each successfully DROP+ADD the constraint using their own
// hard-coded value lists, silently producing a schema that excludes bare
// 'player_locked' even though 0048 - the migration that is SUPPOSED to be
// the one removing it - never ran. Holding all three back together and
// re-applying them together (MigrateUp sorts by version and applies
// unapplied versions in ascending order, so 0048 lands before 0050 lands
// before 0052 regardless of what else is already applied in between) is
// what actually reproduces "0048 is a not-yet-applied reserved gap" -
// the real scenario doc 27's migration-order notes describe, and the one
// these tests are about.
var migrationsDependentOn0048Prefixes = []string{"0048_", "0050_", "0052_"}

// migrationsDir is the real migrations directory, relative to this
// package. Read (never written) by these tests.
func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0048_ledger_locked_account_origin_split.up.sql")); err != nil {
		t.Fatalf("migration 0048 not found in %s: %v", dir, err)
	}
	return dir
}

// stagedMigrations copies the real migrations into a temp directory,
// optionally holding migration 0048 (and the later migrations that
// structurally depend on its prior state,
// migrationsDependentOn0048Prefixes) back, and returns the directory plus
// a function that adds them all back in later, in one MigrateUp call.
// Copying (rather than pointing the migrator at the repo) is what lets a
// test run the chain WITHOUT 0048 and then add it, which is the only way
// to observe 0048's pre-flight guard against a genuinely pre-0048
// database.
func stagedMigrations(t *testing.T, includeMigration0048 bool) (dir string, addHeld func()) {
	t.Helper()
	src := migrationsDir(t)
	dir = t.TempDir()

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var held []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		heldBack := false
		if !includeMigration0048 {
			for _, prefix := range migrationsDependentOn0048Prefixes {
				if strings.HasPrefix(e.Name(), prefix) {
					heldBack = true
					break
				}
			}
		}
		if heldBack {
			held = append(held, e.Name())
			continue
		}
		copyMigrationFile(t, src, dir, e.Name())
	}
	wantHeld := 2 * len(migrationsDependentOn0048Prefixes)
	if len(held) != wantHeld && !includeMigration0048 {
		t.Fatalf("expected to hold back exactly %d files (up+down for each of %v), held %v", wantHeld, migrationsDependentOn0048Prefixes, held)
	}
	return dir, func() {
		for _, name := range held {
			copyMigrationFile(t, src, dir, name)
		}
	}
}

func copyMigrationFile(t *testing.T, src, dst, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(src, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dst, name), content, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// scratchDatabase creates an empty database next to TEST_DATABASE_URL and
// returns a URL for it, dropping it on cleanup. Used only by these
// migration tests: everything else runs against the shared, already
// migrated test database.
func scratchDatabase(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	name := "ledger48_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

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

func scratchPool(t *testing.T, databaseURL string) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), databaseURL, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedLockedAccountRow builds the minimum tenant/person/brand/player/
// wallet chain and inserts one ledger_accounts row of accountType,
// returning the TENANT id - which the caller needs in order to read the
// row back at all: ledger_accounts carries FORCE ROW LEVEL SECURITY
// (migration 0020), so an unscoped connection sees zero rows even as the
// table's owner. That property is exactly what made §6.5.2's designed
// pre-flight guard inert, and a test that counted rows unscoped would
// have reported "the row vanished" for the same reason.
//
// accountType is interpolated as a literal because these tests
// deliberately insert values no Go const expresses any more (bare
// player_locked); it is never caller-supplied data.
func seedLockedAccountRow(t *testing.T, pool *db.Pool, accountType string) uuid.UUID {
	t.Helper()
	tenantID, brandID, playerID, personID, walletID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Migration Test Tenant', 'under_platform_licence')`,
			tenantID, "t-"+tenantID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Migration Test Brand')`,
			brandID, tenantID, "b-"+brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, tenantID, brandID, personID, playerID.String()+"@example.com"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			walletID, tenantID, brandID, playerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			fmt.Sprintf(`INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code)
			             VALUES ($1, $2, $3, '%s', 'EUR')`, accountType),
			uuid.New(), tenantID, walletID)
		return err
	})
	if err != nil {
		t.Fatalf("seed %s ledger account: %v", accountType, err)
	}
	return tenantID
}

func accountTypeCheckDef(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var def string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, accountTypeConstraint).Scan(&def)
	})
	if err != nil {
		t.Fatalf("read %s definition: %v", accountTypeConstraint, err)
	}
	return def
}

// ledgerAccountsForcesRLS reports whether ledger_accounts still carries
// FORCE ROW LEVEL SECURITY. Migration 0048 must not touch that flag at
// all: an earlier revision lifted it around a row count and restored it
// transaction-locally, which left FORCE permanently OFF when the file was
// run standalone under psql (security finding S-1, Stage 4H-B0-R7). The
// toggle was removed; this helper is the regression guard that keeps it
// removed.
func ledgerAccountsForcesRLS(t *testing.T, pool *db.Pool) bool {
	t.Helper()
	var enabled, forced bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'ledger_accounts'`).Scan(&enabled, &forced)
	})
	if err != nil {
		t.Fatalf("read ledger_accounts RLS flags: %v", err)
	}
	if !enabled {
		t.Fatal("ledger_accounts no longer has ROW LEVEL SECURITY enabled at all")
	}
	return forced
}

func appliedVersions(t *testing.T, pool *db.Pool) map[int64]bool {
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

// TestMigration0048_PreflightGuardFiresOnBarePlayerLocked is §6.5.8
// item 6 and invariant L1 layer 3. The guard exists because the remedy
// for a bare player_locked row is an authorized backfill DECISION, not a
// retry, and a raw SQLSTATE 23514 from the ADD CONSTRAINT would not say
// so (§6.5.2).
//
// The guard has exactly one mechanism: the ADD CONSTRAINT is wrapped so
// that the check violation is re-raised with a legible, remedy-naming
// message. Constraint validation evaluates every row, so - unlike the
// SELECT count(*) §6.5.2 originally specified - it cannot be blinded by
// ledger_accounts' FORCE ROW LEVEL SECURITY, and it needs no privileged
// toggle to see past it. A second mechanism that counted rows with FORCE
// RLS lifted was removed at security review (S-1); this test therefore
// runs the REAL, unmodified migration file and asserts the constraint-
// violation message, with no count in it.
func TestMigration0048_PreflightGuardFiresOnBarePlayerLocked(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)
	dir, addHeld := stagedMigrations(t, false)

	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up without 0048/0050/0052: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("expected the pre-0048 chain to apply")
	}
	if appliedVersions(t, pool)[migration0048Version] {
		t.Fatal("migration 0048 must not have been applied yet")
	}

	// Legal under migration 0020's constraint, which is still in force.
	seedLockedAccountRow(t, pool, "player_locked")
	before := accountTypeCheckDef(t, pool)

	addHeld()
	// MigrateUp stops at the first failing migration (internal/db/
	// migrate.go), so 0048 failing here means 0050/0052 are never even
	// attempted - the assertions below need nothing extra for that.
	_, err = pool.MigrateUp(context.Background(), dir)
	if err == nil {
		t.Fatal("migration 0048 must refuse to run while a bare player_locked account exists")
	}
	for _, want := range []string{
		"migration 0048",
		"bare player_locked account(s) exist",
		"constraint validation",
		"row-level security cannot filter",
		"backfill",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the guard's exception must mention %q, got: %v", want, err)
		}
	}

	// ...and nothing changed: the whole file ran inside one transaction
	// that rolled back before the version was recorded.
	if appliedVersions(t, pool)[migration0048Version] {
		t.Fatal("a failed migration 0048 must not be recorded in schema_migrations")
	}
	if after := accountTypeCheckDef(t, pool); after != before {
		t.Fatalf("a failed migration 0048 must leave the constraint untouched:\nbefore: %s\nafter:  %s", before, after)
	}
	if !strings.Contains(before, "'player_locked'::text") {
		t.Fatalf("expected migration 0020's constraint to still be in force, got: %s", before)
	}
	if strings.Contains(before, "player_locked_cash") {
		t.Fatalf("the new values must NOT be admitted after a failed 0048, got: %s", before)
	}
	// Migration 0048 must not touch FORCE ROW LEVEL SECURITY on any path
	// (security S-1). There is no longer a lift to restore; this asserts
	// the toggle has not come back.
	if !ledgerAccountsForcesRLS(t, pool) {
		t.Fatal("migration 0048 left ledger_accounts without FORCE ROW LEVEL SECURITY - tenant isolation weakened")
	}
	// Belt and braces on the same point, read off the migration text
	// itself: no forward migration may toggle FORCE RLS.
	raw, err := os.ReadFile(filepath.Join(migrationsDir(t), "0048_ledger_locked_account_origin_split.up.sql"))
	if err != nil {
		t.Fatalf("read migration 0048: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		// Comments may DISCUSS the flag (the file explains at length why
		// it must not be toggled); only executable lines are in scope.
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		if strings.Contains(line, "FORCE ROW LEVEL SECURITY") {
			t.Fatalf("migration 0048 must not execute any FORCE ROW LEVEL SECURITY statement (security finding "+
				"S-1): the restore is transaction-local, so a standalone `psql -f` run of a firing guard leaves "+
				"tenant isolation off permanently. Offending line: %s", strings.TrimSpace(line))
		}
	}
}

// migration0050Version and migration0052Version are this dispatch's own
// account-type-check widenings, chained onto migration 0048's exact prior
// state (see migrationsDependentOn0048Prefixes).
const (
	migration0050Version = int64(50)
	migration0052Version = int64(52)
)

// TestMigration0048_DownMigrationCleanThenFailsOnDirtyDatabase is §6.5.8
// item 7 plus the up/down/up round trip: 0048 fills its reserved gap in
// the existing chain, rolls back cleanly while no locked account exists,
// re-applies, and then REFUSES to roll back once one does - loudly, with
// SQLSTATE 23514, leaving the schema intact. That refusal is the correct,
// deliberate behavior for an append-only financial ledger (CLAUDE.md),
// identical to migration 0035's documented position, not a defect.
//
// EXTENDED, Stage 4H-B1 Wave 2: migrations 0050 (bonus_expense) and 0052
// (player_bonus_held) both DROP+ADD the same ledger_accounts_
// account_type_check constraint, chained onto 0048's result
// (migrationsDependentOn0048Prefixes), so they apply and roll back
// TOGETHER with 0048 in this test now, in the version order MigrateUp/
// MigrateDown actually use - up ascending (48, then 50, then 52), down in
// applied-order-descending (52, then 50, then 48). That is the same
// structural relationship migration 0022 has with 0021 (each widening
// assumes its predecessor's exact prior value list), just discovered here
// because 0048/0050/0052 are the reserved-gap migrations this dispatch's
// tests exercise directly rather than through the ordinary "just run the
// whole chain" path every other test in this package uses.
func TestMigration0048_DownMigrationCleanThenFailsOnDirtyDatabase(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)

	// Apply the chain WITHOUT 0048/0050/0052 first, then add all three
	// back, so they are the three most recently applied migrations and
	// MigrateDown(3) targets exactly them (MigrateDown orders by
	// applied_at). This also reproduces how 0048 lands on an environment
	// already at 0049+, which is the real deployment shape - 0048's
	// number is a reserved gap, not the tip.
	dir, addHeld := stagedMigrations(t, false)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up without 0048/0050/0052: %v", err)
	}
	addHeld()
	rolledUp, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up 0048/0050/0052 into the existing chain: %v", err)
	}
	wantUp := []int64{migration0048Version, migration0050Version, migration0052Version}
	if !equalVersions(rolledUp, wantUp) {
		t.Fatalf("expected exactly migrations %v to be applied in that order, got %v", wantUp, rolledUp)
	}
	afterUp := accountTypeCheckDef(t, pool)
	for _, want := range []string{"player_locked_cash", "player_locked_bonus", "bonus_expense", "player_bonus_held"} {
		if !strings.Contains(afterUp, want) {
			t.Fatalf("expected the widened constraint to admit %s, got: %s", want, afterUp)
		}
	}
	// The success path must leave FORCE ROW LEVEL SECURITY exactly as it
	// found it - none of the three declares an RLS change, and, since
	// security finding S-1, none executes a statement touching it.
	if !ledgerAccountsForcesRLS(t, pool) {
		t.Fatal("migrations 0048/0050/0052 left ledger_accounts without FORCE ROW LEVEL SECURITY - tenant isolation weakened")
	}

	// (a) Clean database: rolling all three back succeeds and restores
	// migration 0020's exact eleven values - the state before any of
	// 0048/0050/0052 ever ran.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 3)
	if err != nil {
		t.Fatalf("down migration must succeed on a database with no locked/bonus account: %v", err)
	}
	wantDown := []int64{migration0052Version, migration0050Version, migration0048Version}
	if !equalVersions(rolledBack, wantDown) {
		t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
	}
	restored := accountTypeCheckDef(t, pool)
	for _, want := range []string{"'player_locked'::text", "'player_cash'::text", "'manual_adjustment'::text"} {
		if !strings.Contains(restored, want) {
			t.Fatalf("restored constraint must contain %s, got: %s", want, restored)
		}
	}
	for _, unwanted := range []string{"player_locked_cash", "player_locked_bonus", "bonus_expense", "player_bonus_held"} {
		if strings.Contains(restored, unwanted) {
			t.Fatalf("restored constraint must not contain %s, got: %s", unwanted, restored)
		}
	}

	// Round trip: up again.
	rolledUpAgain, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-applying migrations 0048/0050/0052 after a rollback: %v", err)
	}
	if !equalVersions(rolledUpAgain, wantUp) {
		t.Fatalf("expected exactly migrations %v to be re-applied in that order, got %v", wantUp, rolledUpAgain)
	}
	if !strings.Contains(accountTypeCheckDef(t, pool), "player_bonus_held") {
		t.Fatal("re-applied migrations did not widen the constraint again")
	}

	// (b) Dirty database: a real player_locked_cash account now exists.
	// 0052 and 0050's down migrations succeed on their own (neither
	// widening's value is the one this row holds); only 0048's - the one
	// that actually admits player_locked_cash - must refuse, loudly, and
	// leave the schema exactly as it found it. Rolling back one step at a
	// time (rather than 3 in one call) makes that boundary explicit
	// rather than relying on MigrateDown's internal stop-on-first-error
	// behavior to prove it.
	dirtyTenantID := seedLockedAccountRow(t, pool, "player_locked_cash")
	widenedWithDirtyRow := accountTypeCheckDef(t, pool)

	for _, step := range []struct {
		version int64
		name    string
	}{
		{migration0052Version, "player_bonus_held"},
		{migration0050Version, "bonus_expense"},
	} {
		rolledBack, err := pool.MigrateDown(context.Background(), dir, 1)
		if err != nil {
			t.Fatalf("rolling back migration %d (%s) must succeed - it does not touch player_locked_cash: %v",
				step.version, step.name, err)
		}
		if len(rolledBack) != 1 || rolledBack[0] != step.version {
			t.Fatalf("expected exactly migration %d to be rolled back, got %v", step.version, rolledBack)
		}
	}

	_, err = pool.MigrateDown(context.Background(), dir, 1)
	if err == nil {
		t.Fatal("migration 0048's down migration must FAIL once a player_locked_cash account exists - silently " +
			"narrowing the constraint while violating rows remain is exactly what NOT VALID would have allowed")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %T: %v", err, err)
	}
	if pgErr.Code != pgCheckViolation {
		t.Fatalf("expected SQLSTATE %s, got %s: %v", pgCheckViolation, pgErr.Code, err)
	}

	// The failed rollback left everything intact: migration 0048 (and
	// only it - 0050/0052 were already rolled back above) still applied,
	// the constraint still at 0048's exact widened state, and the row
	// still there (ledger data is append-only and was never at risk).
	if !appliedVersions(t, pool)[migration0048Version] {
		t.Fatal("a failed rollback must leave migration 0048 recorded as applied")
	}
	if appliedVersions(t, pool)[migration0050Version] || appliedVersions(t, pool)[migration0052Version] {
		t.Fatal("migrations 0050/0052 must remain rolled back - only 0048's own down migration failed")
	}
	after := accountTypeCheckDef(t, pool)
	if strings.Contains(after, "bonus_expense") || strings.Contains(after, "player_bonus_held") {
		t.Fatalf("the constraint must reflect ONLY migration 0048's widening after 0050/0052 were rolled back, got: %s", after)
	}
	if !strings.Contains(after, "player_locked_cash") {
		t.Fatalf("a failed 0048 rollback must leave its own widening (player_locked_cash) in place, got: %s", after)
	}
	_ = widenedWithDirtyRow // documents the pre-rollback-attempt state; not compared field-by-field since 0050/0052's rollback intentionally changes it first.
	var lockedCount int
	err = pool.WithTenant(context.Background(), dirtyTenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_accounts WHERE account_type = 'player_locked_cash'`).Scan(&lockedCount)
	})
	if err != nil {
		t.Fatalf("count locked accounts: %v", err)
	}
	if lockedCount != 1 {
		t.Fatalf("expected the player_locked_cash account to survive the failed rollback, found %d", lockedCount)
	}
}

// equalVersions reports whether got and want name the same migration
// versions in the same order - used instead of reflect.DeepEqual so a
// mismatch's failure message can show both slices directly at the call
// site.
func equalVersions(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
