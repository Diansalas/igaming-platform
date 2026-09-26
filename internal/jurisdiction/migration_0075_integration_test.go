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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
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

// migration0078Version (Stage 6: sportsbook foundation) is now the chain's
// tip, for the same reason 0076/0077 each were in turn - unconditionally
// reversible in this test's scenario (it never posts a sportsbook_bet
// transaction), so it never blocks the round-trip this file exercises.
const migration0078Version = int64(78)

// migration0079Version (Stage 7: casino_launch_sessions brand-pinning fix)
// is now the chain's tip, for the same reason 0076/0077/0078 each were in
// turn - it only replaces one FK constraint (no data dependency), so it is
// unconditionally reversible in this test's scenario and never blocks the
// round-trip this file exercises.
const migration0079Version = int64(79)

// migration0080Version (Stage 8: casino_provider_rounds, ADR 0080
// Decision 1) is now the chain's tip, for the same reason 0076-0079 each
// were in turn - it only creates a new, empty additive table, so it is
// unconditionally reversible in this test's scenario and never blocks the
// round-trip this file exercises.
const migration0080Version = int64(80)

// migration0081Version (Stage 8: sportsbook_bets provider reference
// columns, ADR 0080 Decision 2) is now the chain's tip, for the same
// reason 0076-0080 each were in turn - it only adds two nullable columns
// and a partial unique index (no data dependency), so it is
// unconditionally reversible in this test's scenario and never blocks the
// round-trip this file exercises.
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
// reversible in this test's scenario and never blocks the round-trip this
// file exercises.
const migration0083Version = int64(83)

// migration0084Version (Stage 9.1, ARCH-DB-2: RLS + immutable-identity +
// deny-delete/truncate triggers on the six platform catalogue tables) is
// now the chain's tip, for the same reason 0076-0083 each were in turn -
// it only adds RLS policies, triggers and a shared trigger function to
// casino_games/sb_* tables, which this test never writes to, so it is
// unconditionally reversible in this test's scenario and never blocks the
// round-trip this file exercises.
const migration0084Version = int64(84)

// migration0085Version (Stage 9.1, SEC-S91-3: casino_games write policies
// now require app.platform_admin_principal_id to resolve to a real
// platform-scoped staff_users row) is now the chain's tip, for the same
// reason 0076-0084 each were in turn - it only adds one trigger function
// and one trigger on casino_games, which this test never writes to, so it
// is unconditionally reversible in this test's scenario and never blocks
// the round-trip this file exercises.
const migration0085Version = int64(85)

// migration0086Version (Stage 9.2, casino-catalogue-dual-control:
// four-eyes governance for casino_games) is now the chain's tip, for the
// same reason 0076-0085 each were in turn - it only adds a new table
// (casino_catalogue_change_requests) and triggers on casino_games, which
// this test never writes to, so it is unconditionally reversible in this
// test's scenario and never blocks the round-trip this file exercises.
const migration0086Version = int64(86)

// migration0087Version (Stage 9.2, ADR 0083 Part C: sportsbook
// jurisdiction/market gating) is now the chain's tip, for the same reason
// 0076-0086 each were in turn - it only adds a new table
// (sb_jurisdiction_restrictions) and a nullable column on
// sportsbook_bets, neither of which this test writes to, so it is
// unconditionally reversible in this test's scenario and never blocks the
// round-trip this file exercises.
const migration0087Version = int64(87)

// migration0088Version (Stage 9.2, ADR 0083 Part B2/Wave 3: sportsbook
// cross-player book-exposure limits) is now the chain's tip, for the same
// reason 0076-0087 each were in turn - it only adds a new table
// (sb_exposure_limits) and a partial index on sportsbook_bets, neither of
// which this test writes to, so it is unconditionally reversible in this
// test's scenario and never blocks the round-trip this file exercises.
const migration0088Version = int64(88)

// migration0089Version (Stage 9.2 fix round, casino specialist: SEC-S92-1's
// casino_catalogue_change_requests/_approvals principal-eligibility
// hardening to migration 0047's shape) is now the chain's tip, for the
// same reason 0076-0088 each were in turn - it only replaces two trigger
// function bodies on casino_games-adjacent tables, which this test never
// writes to, so it is unconditionally reversible in this test's scenario
// and never blocks the round-trip this file exercises. Landed concurrently
// with migration0090Version below by a parallel Stage 9.2 fix-round
// workstream - not this package's own subject.
const migration0089Version = int64(89)

// migration0090Version (Stage 9.2 fix round, SEC-S92-2: defense-in-depth
// staff-principal-resolution trigger on sb_jurisdiction_restrictions,
// mirroring migration 0085's identical shape for casino_games) is now the
// chain's tip, for the same reason 0076-0089 each were in turn - it only
// adds one trigger function and one trigger on sb_jurisdiction_
// restrictions, which this test never writes to, so it is unconditionally
// reversible in this test's scenario and never blocks the round-trip this
// file exercises.
const migration0090Version = int64(90)

// migration0091Version (Stage 10 W1, ADR 0088 §12: sportsbook settlement
// lifecycle). Its down migration refuses once any sportsbook settlement
// evidence exists, but this test never places or settles a bet, so it is
// reversible in this test's scenario and never blocks the round-trip this
// file exercises.
const migration0091Version = int64(91)

// migration0092Version (Stage 10.1: one deposit_reversal per original
// deposit) only adds a partial unique index on ledger_transactions; its
// down just drops the index, so it is unconditionally reversible in this
// test's scenario and never blocks the round-trip this file exercises.
const migration0092Version = int64(92)

// migration0093Version (Stage 10.1: sportsbook settlement causation check
// via xact status). It only replaces a trigger function body and its down
// restores the prior body, so it is unconditionally reversible in this
// test's scenario and never blocks the round-trip this file exercises.
const migration0093Version = int64(93)

// migration0094Version (Stage 10.3, CAS-CAP-ROLLBACK-1/M-CAS-1: the
// casino_provider_capabilities settlement-completeness CHECK). It only
// adds one CHECK constraint on a table this test's own scenario never
// writes a row to, so it is unconditionally reversible here too.
const migration0094Version = int64(94)

// migration0095Version (Stage 10.3, KYC-REASON-BOUND-1: the
// kyc_verifications.reason CHECK plus its pre-flight normalization). It
// only adds one CHECK constraint on a table this test's own scenario never
// writes a row to, so it is unconditionally reversible here too.
const migration0095Version = int64(95)

// migration0096Version (Stage 10.3 W2a, provider credential handles and
// four-eyes governance). Its down migration refuses only once provider
// credential history exists, which this test's own scenario never
// creates, so it is unconditionally reversible here.
const migration0096Version = int64(96)

// migration0097Version (Stage 10.3 W2b, CAS-RECON-1: the append-only
// casino_callback_rejections table plus the casino_consistency mismatch
// kinds). This test's scenario never writes a rejection row or a casino
// mismatch, so its evidence-refusing down is unconditionally reversible
// here.
const migration0097Version = int64(97)

// migration0098Version (Stage 10.3 W3a, CAS-RECON-STMT-1: the
// casino_statement mismatch kind) is now the chain's tip. Its down refuses
// only once a cas_mock_statement_mismatch row exists, which this scenario
// never creates, so it is unconditionally reversible here too.
const migration0098Version = int64(98)

// migrationFileVersion parses the 4-digit numeric version prefix off a
// migration filename (e.g. "0098_casino_statement_mismatch_kind.up.sql" ->
// 98). Mirrors internal/ledger/migration_0092_integration_test.go's own
// helper of the same name exactly (a separate copy, since these two test
// files live in different packages) - a malformed/misnamed file fails
// loudly here rather than being silently treated as version 0.
func migrationFileVersion(name string) (int64, error) {
	if len(name) < 4 {
		return 0, fmt.Errorf("migration filename %q is too short to carry a version prefix", name)
	}
	v, err := strconv.ParseInt(name[:4], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration filename %q has no numeric version prefix: %w", name, err)
	}
	return v, nil
}

// migrationsFromVersion counts the distinct migration versions >= from that
// exist as an up-migration in dir. Used against stagedMigrations0075's
// self-contained staged directory (never the real migrations directory
// directly), so it always returns the same count - the depth to request in
// pool.MigrateDown to roll back "from 0075's own version up to 0098" -
// regardless of how many migrations accumulate in the real directory above
// 0098 later. It still fails loudly (via migrationFileVersion) on a
// malformed filename rather than silently miscounting.
func migrationsFromVersion(t *testing.T, dir string, from int64) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	seen := map[int64]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		v, err := migrationFileVersion(e.Name())
		if err != nil {
			t.Fatalf("%v", err)
		}
		if v >= from {
			seen[v] = true
		}
	}
	return len(seen)
}

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

// stagedMigrations0075 copies the real migrations into a fresh temp
// directory, holding back every migration numbered ABOVE migration0098Version
// - this file's own fix for code review finding F-1 (16-code-hygiene-
// review.md): deriving MigrateDown's depth from the migrations directory
// (migrationsFromVersion) is not enough on its own, because these tests'
// wantDown/wantDirty assertions describe an EXACT ordered list of versions
// (0098 down through 0075/0076) that is only valid for a chain that ends at
// 0098. Migrating up the full REAL chain (as this file did before the F-1
// fix) means that list silently goes stale the moment a 0099 lands. Staging
// a directory that never contains anything above 0098 - exactly
// internal/ledger/migration_0092_integration_test.go's stagedMigrations0092
// pattern (itself following migration_0048_integration_test.go's
// stagedMigrations/copyMigrationFile shape) - keeps wantDown/wantDirty (and
// every per-migration reversibility comment they depend on) correct
// indefinitely: a 0099 is simply never copied into dir, so it can never
// appear in MigrateUp/MigrateDown's return value here, no matter how many
// such migrations land in the real directory later. Ordering detection
// itself is untouched - MigrateDown's actual returned order is still
// compared, element by element, against wantDown/wantDirty.
func stagedMigrations0075(t *testing.T) string {
	t.Helper()
	src := migration0075MigrationsDir(t)
	dir := t.TempDir()

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var copied int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := migrationFileVersion(e.Name())
		if err != nil {
			t.Fatalf("%v", err)
		}
		if v > migration0098Version {
			continue // held back: this test's scenario ends at 0098
		}
		copyMigrationFile0075(t, src, dir, e.Name())
		copied++
	}
	if copied == 0 {
		t.Fatal("expected to stage at least one migration file, staged 0")
	}
	return dir
}

func copyMigrationFile0075(t *testing.T, src, dst, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(src, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dst, name), content, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func migration0075ScratchDatabase(t *testing.T) string {
	t.Helper()
	return scratchdb.New(t, "jur0075_")
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
	dir := stagedMigrations0075(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the staged chain (0075 through 0098): %v", err)
	}
	if !migration0075AppliedVersions(t, pool)[migration0075Version] {
		t.Fatal("expected migration 0075 to be applied")
	}

	// (a) Clean database: rolling back every migration from the chain's
	// current tip down through 0075 itself (0098, then 0097, then 0096,
	// then 0095, then 0094, then 0093, then 0092, then 0091, then 0090,
	// then 0089, then 0088, then 0087, then 0086, then 0085, then 0084,
	// then 0083, then 0082, then 0081, then 0080, then 0079, then 0078,
	// then 0077, then 0076, then 0075) succeeds. None of 0096, 0095, 0094,
	// 0093, 0092, 0091, 0090, 0089, 0088, 0087, 0086, 0085, 0084, 0083,
	// 0082, 0081, 0080, 0079, 0078, 0077, nor 0076 is this test's own
	// subject (Stage 10.3 x4, Stage 10.1 x2, Stage 10 W1, Stage 9.2 fix
	// round x2, Stage 9.2, Stage 9.1, Stage 9, Stage 8, Stage 7, Stage 6,
	// Stage 4I Phase E/E-SECURITY) but every one of them sits directly on
	// top of 0075 in the chain and is reversible in this scenario (0091
	// refuses only once sportsbook settlement evidence exists, which this
	// test never creates), so they must be rolled back first for 0075's
	// own down migration to run at all. dir is stagedMigrations0075's
	// staged directory, which never contains anything above 0098, so the
	// count (migrationsFromVersion) is always 24 here regardless of how
	// many migrations exist in the real directory - this and wantDown
	// below stay correct indefinitely, rather than going stale the moment
	// a 0099 lands (CODE-HYGIENE-10.3-1 item 4; code review F-1). It still
	// fails loudly, via migrationFileVersion, on a malformed filename
	// rather than silently miscounting.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, migrationsFromVersion(t, dir, migration0075Version))
	if err != nil {
		t.Fatalf("down migration must succeed on a database with zero jurisdiction_precedence_configs rows: %v", err)
	}
	wantDown := []int64{migration0098Version, migration0097Version, migration0096Version, migration0095Version, migration0094Version, migration0093Version, migration0092Version, migration0091Version, migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version, migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version, migration0077Version, migration0076Version, migration0075Version}
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
	if migration0075AppliedVersions(t, pool)[migration0078Version] {
		t.Fatal("migration 0078 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0079Version] {
		t.Fatal("migration 0079 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0080Version] {
		t.Fatal("migration 0080 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0081Version] {
		t.Fatal("migration 0081 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0082Version] {
		t.Fatal("migration 0082 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0083Version] {
		t.Fatal("migration 0083 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0084Version] {
		t.Fatal("migration 0084 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0085Version] {
		t.Fatal("migration 0085 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0086Version] {
		t.Fatal("migration 0086 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0087Version] {
		t.Fatal("migration 0087 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0088Version] {
		t.Fatal("migration 0088 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0089Version] {
		t.Fatal("migration 0089 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0090Version] {
		t.Fatal("migration 0090 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0091Version] {
		t.Fatal("migration 0091 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0092Version] {
		t.Fatal("migration 0092 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0093Version] {
		t.Fatal("migration 0093 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0094Version] {
		t.Fatal("migration 0094 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0095Version] {
		t.Fatal("migration 0095 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0096Version] {
		t.Fatal("migration 0096 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0097Version] {
		t.Fatal("migration 0097 must no longer be recorded as applied after a successful rollback")
	}
	if migration0075AppliedVersions(t, pool)[migration0098Version] {
		t.Fatal("migration 0098 must no longer be recorded as applied after a successful rollback")
	}

	// Round trip: up again.
	rolledUpAgain, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-applying migrations 0075/0076/0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093/0094/0095/0096/0097/0098 after a clean rollback: %v", err)
	}
	wantUp := []int64{
		migration0075Version, migration0076Version, migration0077Version, migration0078Version, migration0079Version, migration0080Version,
		migration0081Version, migration0082Version, migration0083Version, migration0084Version, migration0085Version, migration0086Version, migration0087Version,
		migration0088Version, migration0089Version, migration0090Version, migration0091Version,
		migration0092Version, migration0093Version, migration0094Version, migration0095Version, migration0096Version, migration0097Version, migration0098Version,
	}
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
	if !migration0075AppliedVersions(t, pool)[migration0078Version] {
		t.Fatal("expected migration 0078 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0079Version] {
		t.Fatal("expected migration 0079 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0080Version] {
		t.Fatal("expected migration 0080 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0081Version] {
		t.Fatal("expected migration 0081 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0083Version] {
		t.Fatal("expected migration 0083 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0084Version] {
		t.Fatal("expected migration 0084 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0085Version] {
		t.Fatal("expected migration 0085 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0086Version] {
		t.Fatal("expected migration 0086 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0087Version] {
		t.Fatal("expected migration 0087 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0088Version] {
		t.Fatal("expected migration 0088 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0089Version] {
		t.Fatal("expected migration 0089 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0090Version] {
		t.Fatal("expected migration 0090 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0091Version] {
		t.Fatal("expected migration 0091 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0092Version] {
		t.Fatal("expected migration 0092 to be recorded as applied again")
	}
	if !migration0075AppliedVersions(t, pool)[migration0093Version] {
		t.Fatal("expected migration 0093 to be recorded as applied again")
	}

	// (b) Dirty database: one real row now exists.
	jurisdictionID := seedMinimalJurisdiction(t, pool)
	platformAdmin := uuid.New()
	insertOnePolicyRow(t, pool, platformAdmin, jurisdictionID)
	if got := countPolicyRowsUnscoped(t, pool); got != 1 {
		t.Fatalf("expected exactly 1 row before the rollback attempt, got %d", got)
	}

	// Requesting the same depth as above (every migration from the chain's
	// tip down through 0075) rolls back 0098 through 0076 (none holds any
	// rows of its own in this scenario - 0092, 0093, 0094, 0095, and 0096
	// carry no such guard, 0097 and 0098 refuse only once casino
	// reconciliation evidence exists, 0091 refuses only once sportsbook
	// settlement evidence exists, and the rest carry no "refuse if rows
	// exist" guard at all, per each one's own down.sql header comment),
	// and the overall call then fails once it reaches 0075's own guard -
	// MigrateDown processes one migration per transaction and returns the
	// partial rolledBack list plus the error from whichever one failed, so
	// everything from 0098 down through 0076 stays rolled back while 0075
	// stays applied.
	rolledBackDirty, err := pool.MigrateDown(context.Background(), dir, migrationsFromVersion(t, dir, migration0075Version))
	if err == nil {
		t.Fatal("migration 0075's down migration must FAIL once a jurisdiction_precedence_configs row exists - silently " +
			"dropping columns that hold policy-authoring history would destroy audit-relevant provenance")
	}
	wantDirty := []int64{
		migration0098Version, migration0097Version, migration0096Version, migration0095Version, migration0094Version, migration0093Version, migration0092Version, migration0091Version, migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version,
		migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version, migration0077Version, migration0076Version,
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
		t.Fatalf("re-applying migrations 0076/0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090/0091/0092/0093/0094/0095/0096/0097/0098 after the aborted rollback: %v", err)
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
	dir := stagedMigrations0075(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the staged chain (0075 through 0098): %v", err)
	}
	// Roll back every migration from the chain's tip down through 0075
	// itself (0098, then 0097, then 0096, then 0095, then 0094, then 0093,
	// then 0092, then 0091, then 0090, then 0089, then 0088, then 0087,
	// then 0086, then 0085, then 0084, then 0083, then 0082, then 0081,
	// then 0080, then 0079, then 0078, then 0077, then 0076, then 0075) -
	// see this file's own header/migration0076Version through
	// migration0098Version comments for why every one of them must be
	// accounted for explicitly here. dir is stagedMigrations0075's staged
	// directory (never anything above 0098), so this count is always 24
	// and does not go stale as new migrations land above 0098 in the real
	// directory (CODE-HYGIENE-10.3-1 item 4; code review F-1).
	if _, err := pool.MigrateDown(context.Background(), dir, migrationsFromVersion(t, dir, migration0075Version)); err != nil {
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
