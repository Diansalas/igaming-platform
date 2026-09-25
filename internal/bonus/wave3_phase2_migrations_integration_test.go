//go:build integration

// Migration mechanics for Stage 4H-B1 Wave 3 Phase 2 (backend, schema
// only, ledger-accounting-model.md §7.18): 0068 (bonus_ledger_sweep_
// watermarks), 0069 (bonus_cashback_schedule_watermarks), 0070
// (bonus_grants.expires_at). Unlike migration 0048's tangled dependency
// web (internal/ledger/migration_0048_integration_test.go), none of
// these three has any conditional/guarded down-migration behavior and
// nothing after them in the chain depends on their prior state - a
// straightforward full-chain up/down/up round trip against a throwaway
// database is sufficient to exercise them for real, per this project's
// established migration-testing discipline.
package bonus

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const (
	migration0068Version = int64(68)
	migration0069Version = int64(69)
	migration0070Version = int64(70)
	// migration0071Version (Stage 4I, jurisdiction_resolution_foundation)
	// is now the chain's tip. This test's own round-trip only exercises
	// 0068-0070's behavior, but MigrateDown/MigrateUp operate on the
	// chain's most-recently-applied end - rolling back "the 3 most
	// recent" would silently start rolling back a DIFFERENT stage's
	// migration the moment a new one lands after 0070, which is exactly
	// what happened here. Roll back 4 and account for 0071 explicitly
	// rather than re-introducing that fragility.
	migration0071Version = int64(71)
	// migration0072Version (Stage 4I security review, SEC-4I-F4:
	// per-command RLS policies on jurisdiction_resolution_active) is now
	// the chain's tip, for the same reason 0071 was: this test rolls back
	// from the most-recently-applied end, so every migration that lands
	// after 0070 has to be accounted for here explicitly.
	migration0072Version = int64(72)
	// migration0073Version (Stage 4I final security certification,
	// SEC-4I-F8: the BEFORE TRUNCATE deny trigger on
	// jurisdiction_resolution_active) is now the chain's tip, for the
	// same reason 0071 and 0072 each were in turn. Every migration that
	// lands after 0070 has to be accounted for here explicitly, because
	// this test rolls back from the most-recently-applied end.
	migration0073Version = int64(73)
	// migration0074Version (Stage 4I Phase B: player_accounts/
	// kyc_verifications residence columns plus
	// jurisdiction_evidence_collection_active) is now the chain's tip, for
	// the same reason 0071/0072/0073 each were in turn.
	migration0074Version = int64(74)
	// migration0075Version (Stage 4I Phase D: widens
	// jurisdiction_precedence_configs into the evaluation-policy config
	// table) is now the chain's tip, for the same reason
	// 0071/0072/0073/0074 each were in turn.
	migration0075Version = int64(75)
	// migration0076Version (Stage 4I Phase E: jurisdictions.country_code,
	// platform_operations, licence_country_ceilings,
	// operating_country_policies) is now the chain's tip, for the same
	// reason 0071/0072/0073/0074/0075 each were in turn.
	migration0076Version = int64(76)
	// migration0077Version (Stage 4I Phase E-SECURITY: tenant/licence/
	// jurisdiction registry RLS) is now the chain's tip, for the same
	// reason 0071/0072/0073/0074/0075/0076 each were in turn. Its own down
	// migration is unconditionally reversible in this test's scenario
	// (this test's up-migration run never writes to tenants/licences/
	// jurisdictions in a way that would trip any guard - it has none
	// anyway, per migration 0077's own down.sql header comment).
	migration0077Version = int64(77)
	// migration0078Version (Stage 6: sportsbook foundation - sb_sports/
	// sb_competitions/sb_events/sb_markets/sb_selections,
	// sportsbook_bets, plus the ledger_transactions_transaction_type_check
	// widening for 'sportsbook_bet') is now the chain's tip, for the same
	// reason 0071-0077 each were in turn.
	migration0078Version = int64(78)
	// migration0079Version (Stage 7: casino_launch_sessions brand-pinning
	// fix) is now the chain's tip, for the same reason 0071-0078 each were
	// in turn - it only replaces one FK constraint (no data dependency), so
	// it is unconditionally reversible in this test's scenario.
	migration0079Version = int64(79)
	// migration0080Version (Stage 8: casino_provider_rounds, ADR 0080
	// Decision 1) is now the chain's tip, for the same reason 0071-0079
	// each were in turn - it only creates a new, empty additive table, so
	// it is unconditionally reversible in this test's scenario.
	migration0080Version = int64(80)
	// migration0081Version (Stage 8: sportsbook_bets provider reference
	// columns, ADR 0080 Decision 2) is now the chain's tip, for the same
	// reason 0071-0080 each were in turn - it only adds two nullable
	// columns and a partial unique index (no data dependency), so it is
	// unconditionally reversible in this test's scenario.
	migration0081Version = int64(81)
	// migration0082Version (Stage 9: the bundled DB-hardening migration -
	// immutability/TRUNCATE-deny triggers, ARCH-DB-3 composite brand
	// pinning, and three pagination indexes) is now the chain's tip, for
	// the same reason 0071-0081 each were in turn - it only adds
	// triggers, indexes and foreign keys (no data dependency), so it is
	// unconditionally reversible in this test's scenario.
	migration0082Version = int64(82)
	// migration0083Version (Stage 9.1, PLAT-MIGDRIFT-1: adds
	// schema_migrations.checksum) is now the chain's tip, for the same
	// reason 0071-0082 each were in turn - it only adds a single nullable
	// column to schema_migrations itself (no data dependency on anything
	// this test touches), so it is unconditionally reversible in this
	// test's scenario.
	migration0083Version = int64(83)
	// migration0084Version (Stage 9.1, ARCH-DB-2: RLS + immutable-identity
	// + deny-delete/truncate triggers on the six platform catalogue
	// tables) is now the chain's tip, for the same reason 0071-0083 each
	// were in turn - it only adds RLS policies, triggers and a shared
	// trigger function to tables this test's own up-migration run never
	// writes to, so it is unconditionally reversible in this test's
	// scenario.
	migration0084Version = int64(84)
	// migration0085Version (Stage 9.1, SEC-S91-3: casino_games write
	// policies now require app.platform_admin_principal_id to resolve to a
	// real platform-scoped staff_users row) is now the chain's tip, for the
	// same reason 0071-0084 each were in turn - it only adds one trigger
	// function and one trigger on casino_games, a table this test's own
	// up-migration run never writes to, so it is unconditionally reversible
	// in this test's scenario.
	migration0085Version = int64(85)
	// migration0086Version (Stage 9.2, casino-catalogue-dual-control:
	// four-eyes governance for casino_games jurisdiction_blocklist
	// removal/status reactivation) is now the chain's tip, for the same
	// reason 0071-0085 each were in turn - it only adds a new table
	// (casino_catalogue_change_requests) and triggers on casino_games, a
	// table this test's own up-migration run never writes to, so it is
	// unconditionally reversible in this test's scenario.
	migration0086Version = int64(86)
	// migration0087Version (Stage 9.2, ADR 0083 Part C: sportsbook
	// jurisdiction/market gating) is now the chain's tip, for the same
	// reason 0071-0086 each were in turn - it only adds a new table
	// (sb_jurisdiction_restrictions) and a nullable column on
	// sportsbook_bets, neither of which this test's own up-migration run
	// writes to, so it is unconditionally reversible in this test's
	// scenario.
	migration0087Version = int64(87)
	// migration0088Version (Stage 9.2, ADR 0083 Part B2/Wave 3: sportsbook
	// cross-player book-exposure limits) is now the chain's tip, for the
	// same reason 0071-0087 each were in turn - it only adds a new table
	// (sb_exposure_limits) and a partial index on sportsbook_bets, neither
	// of which this test's own up-migration run writes to, so it is
	// unconditionally reversible in this test's scenario.
	migration0088Version = int64(88)
	// migration0089Version (Stage 9.2 fix round, casino specialist:
	// SEC-S92-1's casino_catalogue_change_requests/_approvals principal-
	// eligibility hardening to migration 0047's shape) is now the chain's
	// tip, for the same reason 0071-0088 each were in turn - it only
	// replaces two trigger function bodies on casino_games-adjacent
	// tables, neither of which this test's own up-migration run writes
	// to, so it is unconditionally reversible in this test's scenario.
	// Landed concurrently with migration0090Version below by a parallel
	// Stage 9.2 fix-round workstream - not this package's own subject.
	migration0089Version = int64(89)
	// migration0090Version (Stage 9.2 fix round, SEC-S92-2: defense-in-depth
	// staff-principal-resolution trigger on sb_jurisdiction_restrictions,
	// mirroring migration 0085's identical shape for casino_games) is now
	// the chain's tip, for the same reason 0071-0089 each were in turn - it
	// only adds one trigger function and one trigger on sb_jurisdiction_
	// restrictions, a table this test's own up-migration run never writes
	// to, so it is unconditionally reversible in this test's scenario.
	migration0090Version = int64(90)
)

// migrationsDir resolves the real migrations directory relative to this
// package (internal/bonus -> ../../migrations), mirroring internal/
// ledger/migration_0048_integration_test.go's own helper. Read only,
// never written.
func wave3MigrationsDir(t *testing.T) string {
	t.Helper()
	dir := "../../migrations"
	if _, err := os.Stat(dir + "/0068_bonus_ledger_sweep_watermarks.up.sql"); err != nil {
		t.Fatalf("migration 0068 not found relative to internal/bonus: %v", err)
	}
	return dir
}

// wave3ScratchDatabase creates an empty scratch database and returns its
// URL, dropping it on cleanup - delegates to the shared
// internal/testsupport/scratchdb helper (Stage 10 W0).
func wave3ScratchDatabase(t *testing.T) string {
	t.Helper()
	return scratchdb.New(t, "bonus_w3p2_")
}

func wave3ScratchPool(t *testing.T, databaseURL string) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), databaseURL, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func wave3AppliedVersions(t *testing.T, pool *db.Pool) map[int64]bool {
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

// wave3RegclassExists reports whether to_regclass resolves objectName
// (a table) to something real - NULL means "does not exist", the
// standard way to check object existence without erroring on absence.
func wave3RegclassExists(t *testing.T, pool *db.Pool, objectName string) bool {
	t.Helper()
	var exists bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, objectName).Scan(&exists)
	})
	if err != nil {
		t.Fatalf("check existence of %s: %v", objectName, err)
	}
	return exists
}

func wave3ColumnExists(t *testing.T, pool *db.Pool, table, column string) bool {
	t.Helper()
	var exists bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`,
			table, column).Scan(&exists)
	})
	if err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}
	return exists
}

func wave3EqualVersions(got, want []int64) bool {
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

// TestWave3Phase2Migrations_FullChainUpDownUpRoundTrip is the round-trip
// (up/down/up) test the dispatch requires for 0068/0069/0070: apply the
// ENTIRE migration chain (0001..0070, exactly what a fresh deployment
// does - these three add no guard requiring anything but a normal
// forward run), assert all three new schema objects exist in the shapes
// this dispatch specifies, roll back exactly the three most recently
// applied migrations, assert they are cleanly gone (including the
// trigger functions, not just the tables/column), then re-apply and
// assert they are back - the "up/down/up" this project's established
// discipline (internal/ledger/migration_0048_integration_test.go)
// requires for every migration that touches structure, even one with no
// tricky conditional behavior.
func TestWave3Phase2Migrations_FullChainUpDownUpRoundTrip(t *testing.T) {
	scratchURL := wave3ScratchDatabase(t)
	pool := wave3ScratchPool(t, scratchURL)
	dir := wave3MigrationsDir(t)

	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("expected the full chain to apply on a fresh database")
	}
	versions := wave3AppliedVersions(t, pool)
	for _, v := range []int64{migration0068Version, migration0069Version, migration0070Version} {
		if !versions[v] {
			t.Fatalf("expected migration %d to be applied, schema_migrations: %v", v, versions)
		}
	}

	// (a) 0068: bonus_ledger_sweep_watermarks exists with the expected shape.
	if !wave3RegclassExists(t, pool, "bonus_ledger_sweep_watermarks") {
		t.Fatal("bonus_ledger_sweep_watermarks does not exist after migrating up")
	}
	for _, col := range []string{"tenant_id", "consumer_name", "last_processed_ledger_transaction_id", "last_processed_posted_at", "updated_at"} {
		if !wave3ColumnExists(t, pool, "bonus_ledger_sweep_watermarks", col) {
			t.Errorf("bonus_ledger_sweep_watermarks missing column %s", col)
		}
	}

	// (b) 0069: bonus_cashback_schedule_watermarks exists with the expected shape.
	if !wave3RegclassExists(t, pool, "bonus_cashback_schedule_watermarks") {
		t.Fatal("bonus_cashback_schedule_watermarks does not exist after migrating up")
	}
	for _, col := range []string{"tenant_id", "campaign_id", "player_account_id", "asset_code", "last_processed_window_end", "updated_at"} {
		if !wave3ColumnExists(t, pool, "bonus_cashback_schedule_watermarks", col) {
			t.Errorf("bonus_cashback_schedule_watermarks missing column %s", col)
		}
	}

	// (c) 0070: bonus_grants.expires_at exists.
	if !wave3ColumnExists(t, pool, "bonus_grants", "expires_at") {
		t.Fatal("bonus_grants.expires_at does not exist after migrating up")
	}

	// Roll back exactly the twenty-three most recently applied migrations
	// (0090, 0089, 0088, 0087, 0086, 0085, 0084, 0083, 0082, 0081, 0080,
	// 0079, 0078, 0077, 0076, 0075, 0074, 0073, 0072, 0071, 0070, 0069,
	// 0068, in that order - MigrateDown orders by applied_at DESC).
	// 0071/0072/0073/0074/0075/0076/0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090
	// are not this test's own subject (Stage 4I, jurisdiction resolution
	// foundation plus its security-review follow-ups, Phase B's evidence
	// foundation, Phase D's evaluation-policy config widening, Phase E's
	// operating-market/country-policy foundation, Phase E-SECURITY's
	// tenant/licence/jurisdiction registry RLS, Stage 6's sportsbook
	// foundation, Stage 7's casino_launch_sessions brand-pinning fix,
	// Stage 8's casino_provider_rounds table plus sportsbook_bets provider
	// reference columns, Stage 9's bundled DB-hardening migration 0082,
	// Stage 9.1's schema_migrations checksum column (0083) plus
	// catalogue-write-authorization RLS (0084) plus casino_games write
	// principal hardening (0085), Stage 9.2's casino-catalogue-dual-
	// control (0086) plus sportsbook jurisdiction/market gating (0087) plus
	// sportsbook cross-player book-exposure limits (0088), and the Stage
	// 9.2 fix round's two parallel workstreams - casino's SEC-S92-1
	// principal-eligibility hardening (0089) and sportsbook's
	// sb_jurisdiction_restrictions staff-principal-resolution trigger
	// (0090))
	// but all of them are unconditionally reversible (Phase E's down
	// migration refuses only on a non-empty policy/ceiling table, which
	// this test's up-migration run never populates; the others carry no
	// such guard at all) and sit directly on top of 0070 in the chain, so
	// they must be rolled back first for 0070's own down migration to run
	// at all.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 23)
	if err != nil {
		t.Fatalf("migrate down 23 (0090/0089/0088/0087/0086/0085/0084/0083/0082/0081/0080/0079/0078/0077/0076/0075/0074/0073/0072/0071/0070/0069/0068): %v", err)
	}
	wantDown := []int64{migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version, migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version, migration0077Version, migration0076Version, migration0075Version, migration0074Version, migration0073Version, migration0072Version, migration0071Version, migration0070Version, migration0069Version, migration0068Version}
	if !wave3EqualVersions(rolledBack, wantDown) {
		t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
	}

	if wave3RegclassExists(t, pool, "bonus_ledger_sweep_watermarks") {
		t.Fatal("bonus_ledger_sweep_watermarks still exists after rolling back migration 0068")
	}
	if wave3RegclassExists(t, pool, "bonus_cashback_schedule_watermarks") {
		t.Fatal("bonus_cashback_schedule_watermarks still exists after rolling back migration 0069")
	}
	if wave3ColumnExists(t, pool, "bonus_grants", "expires_at") {
		t.Fatal("bonus_grants.expires_at still exists after rolling back migration 0070")
	}
	// The down migrations must leave no dangling trigger functions behind
	// (0068/0069 each define their own immutable-identity function; 0070
	// restores bonus_grants_enforce_immutable_fields to its pre-0070 body
	// rather than dropping it, since bonus_grants' own trigger still
	// needs it).
	for _, fn := range []string{
		"bonus_ledger_sweep_watermarks_enforce_immutable_identity",
		"bonus_cashback_schedule_watermarks_enforce_immutable_identity",
	} {
		var exists bool
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = $1)`, fn).Scan(&exists)
		})
		if err != nil {
			t.Fatalf("check function %s: %v", fn, err)
		}
		if exists {
			t.Errorf("function %s still exists after its owning migration was rolled back", fn)
		}
	}
	// bonus_grants_enforce_immutable_fields itself must survive (owned by
	// migration 0057, only its BODY was restored by 0070's down).
	var fnExists bool
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'bonus_grants_enforce_immutable_fields')`).Scan(&fnExists)
	})
	if err != nil {
		t.Fatalf("check bonus_grants_enforce_immutable_fields: %v", err)
	}
	if !fnExists {
		t.Fatal("bonus_grants_enforce_immutable_fields must survive migration 0070's down migration (owned by 0057)")
	}

	// Round trip: up again, cleanly.
	reapplied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("re-applying migrations 0068/0069/0070/0071/0072/0073/0074/0075/0076/0077/0078/0079/0080/0081/0082/0083/0084/0085/0086/0087/0088/0089/0090: %v", err)
	}
	wantUp := []int64{
		migration0068Version, migration0069Version, migration0070Version, migration0071Version, migration0072Version, migration0073Version,
		migration0074Version, migration0075Version, migration0076Version, migration0077Version, migration0078Version, migration0079Version,
		migration0080Version, migration0081Version, migration0082Version, migration0083Version, migration0084Version, migration0085Version,
		migration0086Version, migration0087Version, migration0088Version, migration0089Version, migration0090Version,
	}
	if !wave3EqualVersions(reapplied, wantUp) {
		t.Fatalf("expected exactly migrations %v to be re-applied in that order, got %v", wantUp, reapplied)
	}
	if !wave3RegclassExists(t, pool, "bonus_ledger_sweep_watermarks") {
		t.Fatal("bonus_ledger_sweep_watermarks missing after re-applying migration 0068")
	}
	if !wave3RegclassExists(t, pool, "bonus_cashback_schedule_watermarks") {
		t.Fatal("bonus_cashback_schedule_watermarks missing after re-applying migration 0069")
	}
	if !wave3ColumnExists(t, pool, "bonus_grants", "expires_at") {
		t.Fatal("bonus_grants.expires_at missing after re-applying migration 0070")
	}
}
