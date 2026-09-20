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

	// The down-migration must fail (dirty database).
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err == nil {
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
