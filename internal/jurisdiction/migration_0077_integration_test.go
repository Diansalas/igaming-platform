//go:build integration

// Migration mechanics for 0077 (Stage 4I Phase E-SECURITY: row-level
// security on the tenant/licence/jurisdiction ROOT layer). Mirrors
// migration_0075_integration_test.go's/internal/operatingmarket's own
// migration_0076_integration_test.go's scratch-database pattern exactly -
// this file reuses migration_0075_integration_test.go's own scratch-DB
// helpers (migration0075ScratchDatabase/migration0075ScratchPool/
// migration0075MigrationsDir/migration0075AppliedVersions), unexported and
// package-private, since both files live in package jurisdiction.
package jurisdiction

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type rlsPosture struct {
	enabled bool
	forced  bool
}

func readRLSPostureFor(t *testing.T, pool *db.Pool, table string) rlsPosture {
	t.Helper()
	var p rlsPosture
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&p.enabled, &p.forced)
	})
	if err != nil {
		t.Fatalf("read RLS posture for %s: %v", table, err)
	}
	return p
}

func countPoliciesFor(t *testing.T, pool *db.Pool, table string) int {
	t.Helper()
	var count int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1`, table).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count policies for %s: %v", table, err)
	}
	return count
}

// migration0099Version (PRH-REF, PROVIDER-REF-BOUND-1: provider reference
// CHECK constraints) is the chain's tip for this file's full-chain
// scenario. migration_0075_integration_test.go stages its own directory
// ending at 0098 and never sees it. Its down only drops CHECK
// constraints, so it is unconditionally reversible.
const migration0099Version = int64(99)

// TestMigration0077_EnablesAndForcesRLSOnTenantsLicencesAndJurisdictions
// proves the schema-level baseline directly against pg_class - all three
// tables must carry ENABLE + FORCE ROW LEVEL SECURITY after migrating up.
func TestMigration0077_EnablesAndForcesRLSOnTenantsLicencesAndJurisdictions(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	if !migration0075AppliedVersions(t, pool)[migration0077Version] {
		t.Fatal("expected migration 0077 to be applied")
	}

	for _, table := range []string{"tenants", "licences", "jurisdictions"} {
		p := readRLSPostureFor(t, pool, table)
		if !p.enabled {
			t.Fatalf("expected %s to have ROW LEVEL SECURITY enabled after migration 0077, got enabled=%v", table, p.enabled)
		}
		if !p.forced {
			t.Fatalf("expected %s to FORCE ROW LEVEL SECURITY after migration 0077, got forced=%v", table, p.forced)
		}
	}
}

// TestMigration0077_PerCommandPoliciesNoForAllAndNoLicenceDelete asserts
// the exact RLS posture on all three tables directly against pg_policies:
// per-command policies only (no FOR ALL anywhere), `tenants` gets exactly
// one DELETE policy (the one legitimate DELETE, restricted to
// platform-admin scope), and NEITHER `licences` NOR `jurisdictions` gets
// any DELETE policy at all.
func TestMigration0077_PerCommandPoliciesNoForAllAndNoLicenceDelete(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}

	type policyRow struct {
		table string
		name  string
		cmd   string
	}

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tablename, policyname, cmd FROM pg_policies
			 WHERE tablename IN ('tenants', 'licences', 'jurisdictions')
			 ORDER BY tablename, policyname`)
		if err != nil {
			return err
		}
		defer rows.Close()

		var got []policyRow
		for rows.Next() {
			var p policyRow
			if err := rows.Scan(&p.table, &p.name, &p.cmd); err != nil {
				return err
			}
			got = append(got, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		byTable := map[string][]policyRow{}
		deleteCount := map[string]int{}
		for _, p := range got {
			byTable[p.table] = append(byTable[p.table], p)
			if p.cmd == "ALL" {
				t.Fatalf("table %s has a FOR ALL policy (%s) - forbidden by migration 0077's own design", p.table, p.name)
			}
			if p.cmd == "DELETE" {
				deleteCount[p.table]++
			}
		}

		for _, table := range []string{"tenants", "licences", "jurisdictions"} {
			if len(byTable[table]) == 0 {
				t.Fatalf("expected at least one RLS policy on %s, found none", table)
			}
		}

		if deleteCount["tenants"] != 1 {
			t.Fatalf("expected exactly 1 DELETE policy on tenants (tenants_platform_admin_delete), found %d", deleteCount["tenants"])
		}
		if deleteCount["licences"] != 0 {
			t.Fatalf("expected NO DELETE policy on licences, found %d", deleteCount["licences"])
		}
		if deleteCount["jurisdictions"] != 0 {
			t.Fatalf("expected NO DELETE policy on jurisdictions, found %d", deleteCount["jurisdictions"])
		}

		// Fix 11 (Stage 4I Phase E-SECURITY fix round, DB/RLS P4-3): assert
		// the COMPLETE, EXACT policy set - not just individual properties -
		// so a future migration silently ADDING an extra permissive policy
		// (which, under Postgres RLS semantics, can only ever WIDEN access)
		// fails this test instead of passing silently alongside the checks
		// above.
		wantPolicies := map[policyRow]bool{
			{table: "tenants", name: "tenants_read", cmd: "SELECT"}:                              true,
			{table: "tenants", name: "tenants_platform_admin_insert", cmd: "INSERT"}:             true,
			{table: "tenants", name: "tenants_platform_admin_update", cmd: "UPDATE"}:             true,
			{table: "tenants", name: "tenants_platform_admin_delete", cmd: "DELETE"}:             true,
			{table: "licences", name: "licences_read", cmd: "SELECT"}:                            true,
			{table: "licences", name: "licences_platform_admin_insert", cmd: "INSERT"}:           true,
			{table: "licences", name: "licences_platform_admin_update", cmd: "UPDATE"}:           true,
			{table: "jurisdictions", name: "jurisdictions_read", cmd: "SELECT"}:                  true,
			{table: "jurisdictions", name: "jurisdictions_platform_admin_insert", cmd: "INSERT"}: true,
			{table: "jurisdictions", name: "jurisdictions_platform_admin_update", cmd: "UPDATE"}: true,
		}
		if len(wantPolicies) != 10 {
			t.Fatalf("test bug: whitelist must name exactly 10 tuples, named %d", len(wantPolicies))
		}
		if len(got) != len(wantPolicies) {
			t.Fatalf("expected exactly %d (tablename, policyname, cmd) tuples across tenants/licences/jurisdictions, found %d: %+v", len(wantPolicies), len(got), got)
		}
		for _, p := range got {
			if !wantPolicies[p] {
				t.Fatalf("unexpected policy not in the exact whitelist: %+v (a future migration must not silently widen access here)", p)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("RLS posture check: %v", err)
	}
}

// TestMigration0077_RestoresPreMigrationRLSPostureOnDownThenFailsOnDirtyDatabase
// covers two properties in sequence: (a) the down migration restores the
// EXACT pre-migration RLS posture on all three tables (relrowsecurity=
// false, relforcerowsecurity=false, zero policies) - migration 0077's own
// down.sql does not carry a "refuse if rows exist" guard (tenants/
// licences/jurisdictions are core tables that will always hold rows, so
// that guard shape does not apply here, unlike migration 0075/0076's own
// Phase-E-introduced tables); and (b) RE-APPLYING migration 0077 on top
// of data that VIOLATES its own new invariant (uq_tenants_exclusive_own_
// licence - two tenants sharing one BYOL licence) FAILS with a unique
// violation, leaving the migration NOT recorded as applied and the schema
// in its pre-0077 state - proving the new index is a REAL constraint, not
// merely present in the file, and that a migration failure never leaves a
// partially-applied schema behind (MigrateUp runs each migration in its
// own transaction).
func TestMigration0077_RestoresPreMigrationRLSPostureOnDownThenFailsOnDirtyDatabase(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}

	// (a) Roll back 0099 (PRH-REF's provider reference CHECK constraints,
	// whose down only drops constraints) then 0098 (Stage 10.3 W3a's casino_statement mismatch kind,
	// whose down refuses only once a casino_statement mismatch exists,
	// which this scenario never creates) then 0097 (Stage 10.3 W2b's casino rejection record and
	// reconciliation kinds, whose down refuses only once casino
	// reconciliation evidence exists, which this scenario never creates)
	// then 0095 (Stage 10.3's KYC verification reason bound,
	// whose down only drops the constraint) then 0094 (Stage 10.3's casino
	// capability settlement-completeness CHECK, whose down only drops the
	// constraint) then 0093 (Stage 10.1's settlement causation trigger-body
	// fix, whose down only restores the prior body) then 0092 (Stage
	// 10.1's deposit-reversal partial unique index, whose down only drops
	// the index) then 0091 (Stage 10 W1's sportsbook settlement lifecycle,
	// whose down migration refuses only once sportsbook settlement
	// evidence exists, which this scenario never creates) then 0090
	// (Stage 9.2 fix round's sb_jurisdiction_restrictions
	// staff-principal-resolution trigger) then 0089 (Stage 9.2 fix round's
	// casino SEC-S92-1 principal-eligibility hardening) then 0088 (Stage
	// 9.2's sportsbook cross-player book-exposure limits) then 0087
	// (Stage 9.2's sportsbook jurisdiction/market gating) then 0086
	// (Stage 9.2's casino-catalogue-dual-control) then 0085 (Stage 9.1's
	// casino_games platform-principal trigger) then 0084 (Stage 9.1's
	// catalogue-write-authorization RLS) then 0083 (Stage 9.1's
	// schema_migrations checksum column) then 0082 (Stage 9's bundled
	// DB-hardening migration) then 0081 (Stage 8's sportsbook_bets
	// provider reference columns) then 0080 (Stage 8's
	// casino_provider_rounds) then 0079 (Stage 7's casino_launch_sessions
	// brand-pinning fix) then 0078 (Stage 6's sportsbook foundation), all
	// now sitting on the chain's tip and reversible in this scenario, then 0077, and confirm 0077's own EXACT pre-migration
	// posture is restored.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 23)
	if err != nil {
		t.Fatalf("down migrations 0099/0098/0097/0096/0095/0094/0093/0092/0091/0090/0089/0088/0087/0086/0085/0084/0083/0082/0081/0080/0079/0078/0077 on a clean database: %v", err)
	}
	wantDown := []int64{
		migration0099Version, migration0098Version, migration0097Version, migration0096Version, migration0095Version, migration0094Version, migration0093Version, migration0092Version, migration0091Version, migration0090Version, migration0089Version, migration0088Version, migration0087Version, migration0086Version, migration0085Version, migration0084Version, migration0083Version,
		migration0082Version, migration0081Version, migration0080Version, migration0079Version, migration0078Version, migration0077Version,
	}
	if len(rolledBack) != len(wantDown) {
		t.Fatalf("expected exactly migrations %v to be rolled back, got %v", wantDown, rolledBack)
	}
	for i, v := range wantDown {
		if rolledBack[i] != v {
			t.Fatalf("expected exactly migrations %v to be rolled back in that order, got %v", wantDown, rolledBack)
		}
	}
	for _, table := range []string{"tenants", "licences", "jurisdictions"} {
		p := readRLSPostureFor(t, pool, table)
		if p.enabled {
			t.Fatalf("expected %s to have relrowsecurity=false after rolling back migration 0077, got %v", table, p.enabled)
		}
		if p.forced {
			t.Fatalf("expected %s to have relforcerowsecurity=false after rolling back migration 0077 (NO FORCE must precede DISABLE), got %v", table, p.forced)
		}
		if n := countPoliciesFor(t, pool, table); n != 0 {
			t.Fatalf("expected 0 policies on %s after rolling back migration 0077, found %d", table, n)
		}
	}

	// (b) Seed a genuine BYOL-exclusivity violation: two tenants, both
	// licensing_model='own_licence', bound to the SAME licensee='tenant'
	// licence. This is legal in the pre-0077 schema (no exclusivity index
	// exists yet) - exactly the live-verified gap the new unique index
	// closes.
	var jurisdictionID, licenceID, tenantAID, tenantBID uuid.UUID
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		jurisdictionID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Dirty Migration Test')`,
			jurisdictionID, "DIRTY-"+jurisdictionID.String()[:8]); err != nil {
			return err
		}
		licenceID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'tenant', $3)`,
			licenceID, jurisdictionID, "DIRTY-LIC-"+licenceID.String()[:8]); err != nil {
			return err
		}
		tenantAID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model, licence_id) VALUES ($1, $2, 'Dirty Tenant A', 'own_licence', $3)`,
			tenantAID, "dirty-a-"+tenantAID.String()[:8], licenceID); err != nil {
			return err
		}
		tenantBID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model, licence_id) VALUES ($1, $2, 'Dirty Tenant B', 'own_licence', $3)`,
			tenantBID, "dirty-b-"+tenantBID.String()[:8], licenceID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a BYOL-exclusivity-violating pair of tenants: %v", err)
	}

	// Re-applying migration 0077 must now FAIL when it attempts to create
	// uq_tenants_exclusive_own_licence, because the seeded data already
	// violates it.
	_, err = pool.MigrateUp(context.Background(), dir)
	if err == nil {
		t.Fatal("expected re-applying migration 0077 to fail against data that violates uq_tenants_exclusive_own_licence")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %T: %v", err, err)
	}
	if pgErr.Code != "23505" {
		t.Fatalf("expected SQLSTATE 23505 (unique_violation), got %s: %v", pgErr.Code, err)
	}
	if !strings.Contains(err.Error(), "uq_tenants_exclusive_own_licence") {
		t.Fatalf("expected the error to name uq_tenants_exclusive_own_licence, got: %v", err)
	}

	// The failed migration must not be recorded as applied, and the
	// posture must remain exactly what it was before the failed attempt -
	// MigrateUp runs each migration in its own transaction, so the failed
	// CREATE UNIQUE INDEX statement rolls back everything else migration
	// 0077 does in the same file too (the RLS enablement/policies).
	if migration0075AppliedVersions(t, pool)[migration0077Version] {
		t.Fatal("migration 0077 must not be recorded as applied after a failed re-application")
	}
	for _, table := range []string{"tenants", "licences", "jurisdictions"} {
		p := readRLSPostureFor(t, pool, table)
		if p.enabled || p.forced {
			t.Fatalf("expected %s to remain WITHOUT row-level security after the failed migration 0077 re-application, got enabled=%v forced=%v", table, p.enabled, p.forced)
		}
	}

	// And both dirty rows survived untouched (the failure did not corrupt
	// anything).
	var stillThere int
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id IN ($1, $2)`, tenantAID, tenantBID).Scan(&stillThere)
	})
	if err != nil {
		t.Fatalf("verify dirty tenants survived: %v", err)
	}
	if stillThere != 2 {
		t.Fatalf("expected both dirty tenant rows to survive the failed migration attempt, got %d", stillThere)
	}
}

// TestMigration0077_SchemaMatchesTheCurrentMigrationFile guards against
// the same staleness hazard migration 0076's own
// TestMigration0076_SchemaMatchesTheCurrentMigrationFile fences
// (MKT-MIG76-1): asserts directly against pg_policies/pg_indexes/
// information_schema that the exact set of migration 0077 constructs
// exist, rather than trusting that "the file says so".
func TestMigration0077_SchemaMatchesTheCurrentMigrationFile(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		// The exclusivity index exists, is UNIQUE, and is a partial index
		// keyed on the GENERATED expected_licensee column.
		var indexDef string
		if err := tx.QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes WHERE indexname = 'uq_tenants_exclusive_own_licence'`,
		).Scan(&indexDef); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("expected index uq_tenants_exclusive_own_licence to exist - this database's migration 0077 is STALE: rebuild via migrate-down-then-up before trusting any test results")
			}
			return err
		}
		if !strings.Contains(indexDef, "UNIQUE") {
			t.Fatalf("expected uq_tenants_exclusive_own_licence to be a UNIQUE index, got: %s", indexDef)
		}
		if !strings.Contains(indexDef, "expected_licensee") {
			t.Fatalf("expected uq_tenants_exclusive_own_licence to key on expected_licensee, got: %s", indexDef)
		}

		// The licences_read policy's predicate references `tenants` (the
		// composite-ownership EXISTS join, reused verbatim from migration
		// 0076's licence_country_ceilings_read).
		var licencesReadQual string
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(qual, '') FROM pg_policies WHERE tablename = 'licences' AND policyname = 'licences_read'`,
		).Scan(&licencesReadQual); err != nil {
			return err
		}
		if licencesReadQual == "" {
			t.Fatal("expected policy licences_read to exist with a non-empty USING predicate")
		}
		if !strings.Contains(licencesReadQual, "tenants") {
			t.Fatalf("expected licences_read's predicate to reference tenants (composite-ownership EXISTS), got: %s", licencesReadQual)
		}
		if !strings.Contains(licencesReadQual, "platform_admin_principal_id") {
			t.Fatalf("expected licences_read's predicate to reference platform_admin_principal_id, got: %s", licencesReadQual)
		}

		// Every write policy across the three tables references
		// platform_admin_principal_id (the uniform write posture).
		rows, err := tx.Query(ctx, `
			SELECT tablename, policyname, COALESCE(qual, ''), COALESCE(with_check, '')
			  FROM pg_policies
			 WHERE tablename IN ('tenants', 'licences', 'jurisdictions') AND cmd IN ('INSERT', 'UPDATE', 'DELETE')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var writePolicyCount int
		for rows.Next() {
			var table, name, qual, withCheck string
			if err := rows.Scan(&table, &name, &qual, &withCheck); err != nil {
				return err
			}
			writePolicyCount++
			combined := qual + withCheck
			if !strings.Contains(combined, "platform_admin_principal_id") {
				t.Fatalf("expected write policy %s on %s to reference platform_admin_principal_id, got qual=%q with_check=%q", name, table, qual, withCheck)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if writePolicyCount == 0 {
			t.Fatal("expected at least one write policy across tenants/licences/jurisdictions - this database's migration 0077 is STALE")
		}

		// Fix 5 (this fix round): tenants_read's predicate must exclude
		// player scope, mirroring every sibling read policy in this family.
		var tenantsReadQual string
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(qual, '') FROM pg_policies WHERE tablename = 'tenants' AND policyname = 'tenants_read'`,
		).Scan(&tenantsReadQual); err != nil {
			return err
		}
		if !strings.Contains(tenantsReadQual, "player_account_id") {
			t.Fatalf("expected tenants_read's predicate to exclude player scope (player_account_id conjunct) - this database's migration 0077 is STALE, got: %s", tenantsReadQual)
		}

		// Fix 4 (this fix round): all three tables carry a dedicated
		// BEFORE TRUNCATE FOR EACH STATEMENT deny trigger - RLS does not
		// govern TRUNCATE at all, so this is the only mechanism that
		// reaches it.
		for _, table := range []string{"tenants", "licences", "jurisdictions"} {
			var triggerCount int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM pg_trigger WHERE tgrelid = $1::regclass AND tgname = $2 AND NOT tgisinternal`,
				table, table+"_deny_truncate",
			).Scan(&triggerCount); err != nil {
				return err
			}
			if triggerCount != 1 {
				t.Fatalf("expected exactly 1 deny-truncate trigger (%s_deny_truncate) on %s - this database's migration 0077 is STALE, found %d", table, table, triggerCount)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("schema verification: %v", err)
	}
}
