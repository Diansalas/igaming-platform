//go:build integration

// Independent QA re-verification of the implementing agent's first
// self-disclosed judgment call: migration 0076's down-migration
// temporarily disables RLS INSIDE its own transaction (to make the
// dirty-database EXISTS checks see real rows despite a migration
// connection carrying none of the tenant/platform-admin GUCs). The
// implementing agent's reasoning is that a RAISE EXCEPTION anywhere in
// that DO block rolls back the ENTIRE transaction, including the ALTER
// TABLE ... DISABLE ROW LEVEL SECURITY statements, leaving RLS posture
// completely untouched on a refused rollback. This test does not take
// that reasoning on trust: it fails a dirty-database check and then
// queries pg_class directly to confirm RLS is still ENABLED and FORCED
// afterward.
package operatingmarket

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestQAAdversarial_FailedDirtyRollbackLeavesRLSEnabledAndForced(t *testing.T) {
	scratchURL := migration0076ScratchDatabase(t)
	pool := migration0076ScratchPool(t, scratchURL)
	dir := migration0076MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}

	readRLSPosture := func(table string) (enabled, forced bool) {
		t.Helper()
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced)
		})
		if err != nil {
			t.Fatalf("read RLS posture for %s: %v", table, err)
		}
		return
	}

	// Baseline: both new tables carry ENABLE + FORCE immediately after
	// migrating up, before any rollback attempt.
	for _, table := range []string{"operating_country_policies", "licence_country_ceilings"} {
		enabled, forced := readRLSPosture(table)
		if !enabled || !forced {
			t.Fatalf("baseline: expected %s to have RLS enabled+forced right after migrating up, got enabled=%v forced=%v", table, enabled, forced)
		}
	}

	// Seed a real row via the sanctioned path so the down-migration's
	// dirty-database guard has something to refuse on.
	licenceID := seedMinimalLicence(t, pool)
	platformAdmin := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: "LC", State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "qa-rls-survives-ref", Actor: testActor(platformAdmin, "qa-rls-survives-seed"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed a ceiling row: %v", err)
	}

	// The down-migration must fail (dirty database). Roll back 19 steps:
	// migrations 0095 (Stage 10.3), 0094 (Stage 10.3), 0093 (Stage 10.1), 0092 (Stage 10.1), 0091 (Stage 10 W1), 0090 (Stage 9.2 fix round), 0089
	// (Stage 9.2 fix round), 0088 (Stage 9.2), 0087 (Stage 9.2), 0086
	// (Stage 9.2), 0085 (Stage 9.1), 0084 (Stage 9.1), 0083 (Stage 9.1),
	// 0082 (Stage 9), 0081 (Stage 8), 0080 (Stage 8), 0079 (Stage 7), 0078
	// (Stage 6), and 0077 (Stage 4I Phase E-SECURITY) now sit on top of
	// 0076 in the chain and are all reversible in this scenario (0091
	// refuses only once sportsbook settlement evidence exists, which this
	// test never creates), so they succeed on their own before the overall
	// call fails once it reaches 0076's own guard - mirrors
	// migration_0076_integration_test.go's own migration0077Version
	// through migration0095Version precedent.
	if _, err := pool.MigrateDown(context.Background(), dir, 20); err == nil {
		t.Fatal("expected migration 0076's down migration to fail on a dirty database")
	}

	// THE ADVERSARIAL CHECK: query pg_class directly (not through the
	// package's own assumption) and confirm RLS was NOT left disabled by
	// the DO block's ALTER TABLE ... DISABLE ROW LEVEL SECURITY statements
	// - i.e. the failed transaction's rollback genuinely undid them.
	for _, table := range []string{"operating_country_policies", "licence_country_ceilings"} {
		enabled, forced := readRLSPosture(table)
		if !enabled {
			t.Fatalf("SECURITY REGRESSION: %s has ROW LEVEL SECURITY DISABLED after a FAILED dirty-database rollback attempt - the down migration's temporary in-transaction RLS disable was NOT correctly rolled back", table)
		}
		if !forced {
			t.Fatalf("SECURITY REGRESSION: %s does not FORCE ROW LEVEL SECURITY after a FAILED dirty-database rollback attempt", table)
		}
	}

	// And confirm the RLS policies themselves are still present (the
	// down-migration's DROP POLICY statements come AFTER the guard block
	// in the file, so they must not have run either).
	var policyCount int
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename IN ('operating_country_policies', 'licence_country_ceilings')`).Scan(&policyCount)
	})
	if err != nil {
		t.Fatalf("count surviving policies: %v", err)
	}
	if policyCount == 0 {
		t.Fatal("expected the RLS policies on both tables to still exist after the failed rollback - the guard's failure must be all-or-nothing")
	}

	// And the underlying data survived too (belt-and-braces: this is what
	// TestMigration0076_DownMigrationCleanThenFailsOnDirtyDatabase already
	// checks via schema_migrations; here we check the row itself).
	var stillThere int
	err = pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1`, licenceID).Scan(&stillThere)
	})
	if err != nil {
		t.Fatalf("verify ceiling row survived: %v", err)
	}
	if stillThere != 1 {
		t.Fatalf("expected the seeded ceiling row to survive the failed rollback, got count %d", stillThere)
	}
}

// TestQAAdversarial_PartialRollbackLeavesRegistryRLSDisabledButReapplyRestoresIt
// is Fix 9 (Stage 4I Phase E-SECURITY fix round, DB/RLS review): the test
// above only asserted that the OVERALL `-steps=2 down` command failed. It
// did NOT assert what state `tenants`/`licences`/`jurisdictions` are left
// in. Since Pool.MigrateDown commits each migration's own down in its own
// transaction, a `-steps=2 down` against migration 0077 can succeed at
// rolling back 0077 (silently disabling the new RLS on the registry
// tables) and only THEN fail at migration 0076's own dirty-database guard
// - meaning an operator who sees only "refusing to roll back migration
// 0076" has no way to know that 0077's RLS was, in fact, already reverted.
// This test makes that currently-undocumented intermediate state explicit
// and confirms a subsequent MigrateUp restores full protection. This is a
// coverage/documentation fix only - Pool.MigrateDown's transaction-per-
// migration behavior itself is unchanged and out of scope.
func TestQAAdversarial_PartialRollbackLeavesRegistryRLSDisabledButReapplyRestoresIt(t *testing.T) {
	scratchURL := migration0076ScratchDatabase(t)
	pool := migration0076ScratchPool(t, scratchURL)
	dir := migration0076MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}

	readRLSPosture := func(table string) (enabled, forced bool) {
		t.Helper()
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced)
		})
		if err != nil {
			t.Fatalf("read RLS posture for %s: %v", table, err)
		}
		return
	}
	countPolicies := func(table string) int {
		t.Helper()
		var n int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1`, table).Scan(&n)
		})
		if err != nil {
			t.Fatalf("count policies for %s: %v", table, err)
		}
		return n
	}
	countDenyTruncateTriggers := func(table string) int {
		t.Helper()
		var n int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM pg_trigger WHERE tgrelid = $1::regclass AND tgname = $2 AND NOT tgisinternal`,
				table, table+"_deny_truncate",
			).Scan(&n)
		})
		if err != nil {
			t.Fatalf("count deny-truncate triggers for %s: %v", table, err)
		}
		return n
	}

	registryTables := []string{"tenants", "licences", "jurisdictions"}

	// Baseline: full protection right after migrating up.
	for _, table := range registryTables {
		enabled, forced := readRLSPosture(table)
		if !enabled || !forced {
			t.Fatalf("baseline: expected %s to have RLS enabled+forced right after migrating up, got enabled=%v forced=%v", table, enabled, forced)
		}
		if n := countPolicies(table); n == 0 {
			t.Fatalf("baseline: expected %s to have at least one RLS policy, found none", table)
		}
		if n := countDenyTruncateTriggers(table); n != 1 {
			t.Fatalf("baseline: expected %s to have exactly 1 deny-truncate trigger, found %d", table, n)
		}
	}

	// Seed a real row so migration 0076's own down-migration dirty-database
	// guard has something to refuse on, exactly as the sibling test above
	// does.
	licenceID := seedMinimalLicence(t, pool)
	platformAdmin := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: "GY", State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "qa-partial-rollback-ref", Actor: testActor(platformAdmin, "qa-partial-rollback-seed"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed a ceiling row: %v", err)
	}

	// `-steps=19 down`: migrations 0095, 0094, 0093, 0092, 0091, 0090, 0089, 0088, 0087, 0086,
	// 0085, 0084, 0083, 0082, 0081, 0080, 0079, 0078, and 0077 all roll
	// back successfully on their own (none carries a "refuse if rows
	// exist" guard that this scenario trips - 0091 refuses only once
	// sportsbook settlement evidence exists - tenants/licences/jurisdictions are core tables that will
	// always hold rows), and the OVERALL call only THEN fails once it
	// reaches migration 0076's own dirty-database guard.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 20)
	if err == nil {
		t.Fatal("expected the -steps=19 down to fail on a dirty database")
	}
	wantDown := []int64{
		migration0095Version, migration0094Version, migration0093Version, migration0092Version, migration0091Version, migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version,
		migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version, migration0077Version,
	}
	if len(rolledBack) != len(wantDown) {
		t.Fatalf("expected exactly migrations %v to have been rolled back before the overall failure, got %v", wantDown, rolledBack)
	}
	for i, v := range wantDown {
		if rolledBack[i] != v {
			t.Fatalf("expected exactly migrations %v to have been rolled back before the overall failure, got %v", wantDown, rolledBack)
		}
	}

	// THE UNDOCUMENTED STATE THIS FIX MAKES EXPLICIT: migration 0077's own
	// rollback succeeded, so tenants/licences/jurisdictions are now WITHOUT
	// row-level security, with zero policies and zero deny-truncate
	// triggers - even though the overall `-steps=18 down` command reported
	// failure. An operator reading only "refusing to roll back migration
	// 0076" would have no way to know this.
	for _, table := range registryTables {
		enabled, forced := readRLSPosture(table)
		if enabled || forced {
			t.Fatalf("expected %s to have RLS DISABLED after migration 0077's own successful partial rollback (even though the overall -steps=18 command failed), got enabled=%v forced=%v", table, enabled, forced)
		}
		if n := countPolicies(table); n != 0 {
			t.Fatalf("expected %s to have 0 policies after migration 0077's own successful partial rollback, found %d", table, n)
		}
		if n := countDenyTruncateTriggers(table); n != 0 {
			t.Fatalf("expected %s to have 0 deny-truncate triggers after migration 0077's own successful partial rollback, found %d", table, n)
		}
	}

	// A subsequent MigrateUp must restore full protection on all three
	// tables.
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migration 0077 after the partial rollback: %v", err)
	}
	for _, table := range registryTables {
		enabled, forced := readRLSPosture(table)
		if !enabled || !forced {
			t.Fatalf("expected %s to have RLS enabled+forced again after MigrateUp, got enabled=%v forced=%v", table, enabled, forced)
		}
		if n := countPolicies(table); n == 0 {
			t.Fatalf("expected %s to have its RLS policies restored after MigrateUp, found none", table)
		}
		if n := countDenyTruncateTriggers(table); n != 1 {
			t.Fatalf("expected %s to have its deny-truncate trigger restored after MigrateUp, found %d", table, n)
		}
	}
}
