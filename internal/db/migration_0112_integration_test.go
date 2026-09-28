//go:build integration

package db

// A-17 (ADR 0099 §14): migration 0112 up/down/up on a scratch database;
// down refuses while rows exist in any of the three new grant tables; and
// after a clean down, the RESTRICTIVE acting fence this migration added to
// the seven exposed tables is gone again (the "effective policy set
// equals pre-0112" baseline, checked here by policy NAME/kind rather than
// a full historical replay).
//
// Per the orchestrator's binding note (after PRH-2 B/migration 0111
// merged): any test doing MigrateDown(...,1) or asserting a round trip
// must migrate a scratch DB ONLY through its own migration, using a temp
// dir holding just 0001..0112 - otherwise it breaks (or passes vacuously)
// the moment K2's 0113 lands on top. This mirrors internal/alerting's own
// scratchPoolThrough0110 and internal/casino's migration0099Scratch.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0112Version = int64(112)

// scratchPoolThrough0112 copies only the migration files numbered up to
// and including 0112 into a temp dir and migrates a fresh scratch DB
// through them, so "down one step" always rolls back exactly 0112
// regardless of how many later migrations (K2's 0113, etc.) exist on
// disk. The returned dir must be used for every subsequent
// MigrateDown/MigrateUp/VerifyMigrations call in the same test.
func scratchPoolThrough0112(t *testing.T, prefix string) (*Pool, string) {
	t.Helper()
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < 4 {
			continue
		}
		n, perr := strconv.ParseInt(name[:4], 10, 64)
		if perr != nil || n > migration0112Version {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(src, name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(filepath.Join(dir, name), b, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	url := scratchdb.New(t, prefix)
	pool, err := Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through %d: %v", migration0112Version, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != migration0112Version {
		t.Fatalf("expected %d to be the last applied migration, got %v", migration0112Version, applied)
	}
	return pool, dir
}

// countPolicies returns how many pg_policies rows exist on tableName,
// optionally filtered to PERMISSIVE-only or RESTRICTIVE-only.
func countPolicies(t *testing.T, pool *Pool, tableName string, restrictiveOnly *bool) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if restrictiveOnly == nil {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1`, tableName).Scan(&count)
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1 AND permissive = $2`,
			tableName, permissiveKeyword(!*restrictiveOnly)).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count policies on %s: %v", tableName, err)
	}
	return count
}

func permissiveKeyword(permissive bool) string {
	if permissive {
		return "PERMISSIVE"
	}
	return "RESTRICTIVE"
}

func TestMigration0112_A17_UpDownRefusesWithRowsThenCleanBaseline(t *testing.T) {
	pool, migDir := scratchPoolThrough0112(t, "cap0112a17")
	ctx := context.Background()

	fencedTables := []string{"staff_users", "audit_log", "sessions", "login_attempts", "persons", "player_restrictions", "risk_rules"}
	restrictive := true
	// Baseline: every fenced table has at least one RESTRICTIVE policy
	// (0112's own acting fence) right after up.
	for _, tbl := range fencedTables {
		if n := countPolicies(t, pool, tbl, &restrictive); n == 0 {
			t.Fatalf("expected at least one RESTRICTIVE policy on %s after migrating up", tbl)
		}
	}
	// B's (migration 0111) own tables carry no NULL-arm exposure and no
	// RESTRICTIVE policy of their own (their positive tenant_id equality
	// policies already exclude both acting GUCs explicitly) - 0112 must
	// leave them completely untouched, in either direction.
	for _, tbl := range []string{"casino_launch_bootstraps", "casino_provider_player_refs"} {
		if n := countPolicies(t, pool, tbl, &restrictive); n != 0 {
			t.Fatalf("expected 0112 to add no RESTRICTIVE policy to %s (not a K1-1 NULL-arm table), found %d", tbl, n)
		}
		if n := countPolicies(t, pool, tbl, nil); n < 2 {
			t.Fatalf("expected %s to keep its own (B's) permissive policies untouched, found %d total", tbl, n)
		}
	}

	// Down refuses while a row exists in any of the three new tables.
	f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
	if _, err := pool.MigrateDown(ctx, migDir, 1); err == nil {
		t.Fatal("expected migration 0112's down to refuse while grant rows exist")
	}

	// Revoke the grant - the row still EXISTS (revoke is not delete), so
	// down must still refuse.
	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, f.GrantID, "cleanup")
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := pool.MigrateDown(ctx, migDir, 1); err == nil {
		t.Fatal("expected migration 0112's down to still refuse - a revoked grant row still exists")
	}
}

// TestMigration0112_A17_UpDownUpOnEmptyScratch is the reversibility half:
// on an otherwise-empty (no grant rows) database, 0112 fully reverses -
// every RESTRICTIVE fence and new table it added is gone, and B's own
// (migration 0111) tables/policies are untouched - and re-applies
// cleanly.
func TestMigration0112_A17_UpDownUpOnEmptyScratch(t *testing.T) {
	pool, migDir := scratchPoolThrough0112(t, "cap0112updown")
	ctx := context.Background()

	if _, err := pool.MigrateDown(ctx, migDir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}

	fencedTables := []string{"staff_users", "audit_log", "sessions", "login_attempts", "persons", "player_restrictions", "risk_rules"}
	restrictive := true
	for _, tbl := range fencedTables {
		if n := countPolicies(t, pool, tbl, &restrictive); n != 0 {
			t.Fatalf("expected NO RESTRICTIVE policy on %s after down (pre-0112 baseline), found %d", tbl, n)
		}
	}
	for _, tbl := range []string{"casino_launch_bootstraps", "casino_provider_player_refs"} {
		if n := countPolicies(t, pool, tbl, nil); n < 2 {
			t.Fatalf("expected %s to still have B's own permissive policies after 0112's down, found %d", tbl, n)
		}
	}
	var exists bool
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'staff_capability_grants')`).Scan(&exists)
	}); err != nil {
		t.Fatalf("check table existence: %v", err)
	}
	if exists {
		t.Fatal("expected staff_capability_grants to not exist after down")
	}

	applied, err := pool.MigrateUp(ctx, migDir)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != 1 || applied[0] != migration0112Version {
		t.Fatalf("expected exactly [%d] re-applied, got %v", migration0112Version, applied)
	}

	report, err := pool.VerifyMigrations(ctx, migDir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected a clean migration report after the round trip, got %+v", report)
	}
}
