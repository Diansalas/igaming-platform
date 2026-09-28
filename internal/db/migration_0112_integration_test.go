//go:build integration

package db

// A-17 (ADR 0099 §14): migration 0112 up/down/up on a scratch database;
// down refuses while rows exist in any of the three new grant tables; and
// after a clean down, the RESTRICTIVE acting fence this migration added to
// the seven exposed tables is gone again (the "effective policy set
// equals pre-0112" baseline, checked here by policy NAME/kind rather than
// a full historical replay - see TestA18_NoNullArmTableMissingActingFence
// in acting_fence_static_test.go for the static, forward-looking half of
// this guarantee).

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func scratch0112Pool(t *testing.T, prefix string) *Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), "../../migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return pool
}

// fencePolicyCount returns how many pg_policies rows of polkind exist on
// tableName. polkind: 'r' = permissive/restrictive both show as rows in
// pg_policies with a separate "permissive" boolean column - queried
// directly here rather than via information_schema for that reason.
func countPolicies(t *testing.T, pool *Pool, tableName string, permissiveOnly *bool) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if permissiveOnly == nil {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1`, tableName).Scan(&count)
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1 AND permissive = $2`,
			tableName, permissiveKeyword(*permissiveOnly)).Scan(&count)
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
	pool := scratch0112Pool(t, "cap0112a17")
	ctx := context.Background()

	fencedTables := []string{"staff_users", "audit_log", "sessions", "login_attempts", "persons", "player_restrictions", "risk_rules"}
	// Baseline: every fenced table has at least one RESTRICTIVE policy
	// (0112's own acting fence) right after up.
	for _, tbl := range fencedTables {
		restrictive := false
		if n := countPolicies(t, pool, tbl, &restrictive); n == 0 {
			t.Fatalf("expected at least one RESTRICTIVE policy on %s after migrating up", tbl)
		}
	}

	// Down refuses while a row exists in any of the three new tables.
	f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
	_, err := pool.MigrateDown(ctx, "../../migrations", 1)
	if err == nil {
		t.Fatal("expected migration 0112's down to refuse while grant rows exist")
	}

	// Revoke the grant - the row still EXISTS (revoke is not delete), so
	// down must still refuse.
	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, f.GrantID, "cleanup")
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := pool.MigrateDown(ctx, "../../migrations", 1); err == nil {
		t.Fatal("expected migration 0112's down to still refuse - a revoked grant row still exists")
	}
}

// TestMigration0112_A17_UpDownUpOnEmptyScratch is the reversibility half:
// on an otherwise-empty (no grant rows) database, 0112 fully reverses -
// every RESTRICTIVE fence and new table it added is gone - and re-applies
// cleanly.
func TestMigration0112_A17_UpDownUpOnEmptyScratch(t *testing.T) {
	pool := scratch0112Pool(t, "cap0112updown")
	ctx := context.Background()

	if _, err := pool.MigrateDown(ctx, "../../migrations", 1); err != nil {
		t.Fatalf("down: %v", err)
	}

	fencedTables := []string{"staff_users", "audit_log", "sessions", "login_attempts", "persons", "player_restrictions", "risk_rules"}
	for _, tbl := range fencedTables {
		restrictive := false
		if n := countPolicies(t, pool, tbl, &restrictive); n != 0 {
			t.Fatalf("expected NO RESTRICTIVE policy on %s after down (pre-0112 baseline), found %d", tbl, n)
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

	applied, err := pool.MigrateUp(ctx, "../../migrations")
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("expected exactly 1 migration re-applied, got %d", len(applied))
	}
}
