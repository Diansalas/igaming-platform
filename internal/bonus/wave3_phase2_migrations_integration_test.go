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
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
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

// wave3ScratchDatabase creates an empty database next to TEST_DATABASE_URL
// and returns its URL, dropping it on cleanup - the same pattern internal/
// ledger's migration tests use, duplicated here (not imported) since the
// ledger package's helpers are unexported.
func wave3ScratchDatabase(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	name := "bonus_w3p2_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

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

	// Roll back exactly the seven most recently applied migrations (0074,
	// 0073, 0072, 0071, 0070, 0069, 0068, in that order - MigrateDown
	// orders by applied_at DESC). 0071/0072/0073/0074 are not this test's
	// own subject (Stage 4I, jurisdiction resolution foundation plus its
	// security-review follow-ups and Phase B's evidence foundation) but
	// all four are unconditionally reversible and sit directly on top of
	// 0070 in the chain, so they must be rolled back first for 0070's own
	// down migration to run at all.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 7)
	if err != nil {
		t.Fatalf("migrate down 7 (0074/0073/0072/0071/0070/0069/0068): %v", err)
	}
	wantDown := []int64{migration0074Version, migration0073Version, migration0072Version, migration0071Version, migration0070Version, migration0069Version, migration0068Version}
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
		t.Fatalf("re-applying migrations 0068/0069/0070/0071/0072/0073/0074: %v", err)
	}
	wantUp := []int64{migration0068Version, migration0069Version, migration0070Version, migration0071Version, migration0072Version, migration0073Version, migration0074Version}
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
