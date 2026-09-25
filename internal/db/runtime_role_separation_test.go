//go:build integration

// This file is the permanent CI regression test for PLAT-ROLESPLIT-1
// (docs/security/runtime-role-separation.md) - the "operational
// consequence flagged by the independent security review, not yet
// addressed by any CI change" that document's §5 named explicitly: the
// six-probe (later twenty-probe) verification that a non-owning
// "igaming_runtime" role cannot perform owner-only/privileged operations
// had, until this file, only ever been run once, by hand, against a
// manually created role. Every assertion below reproduces that same
// verification as an ordinary Go test, so a future migration or handler
// that accidentally comes to depend on an owner-only operation is caught
// here instead of only in production.
//
// Requires TEST_RUNTIME_DATABASE_URL, pointing at the SAME database
// TEST_DATABASE_URL uses but connecting as the runtime role
// ("igaming_runtime" in this repo's dev/CI convention - see
// deploy/init-app-role.sql, the Makefile's dev-db-init-roles target, and
// .github/workflows/ci.yml). Skips cleanly (t.Skip, not a failure) if
// unset, exactly like every other integration test in this repository
// skips on a missing TEST_DATABASE_URL - no environment that hasn't
// provisioned this role yet is broken by this file's existence.
package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgInsufficientPrivilegeCode is the Postgres SQLSTATE returned by every
// probe below (confirmed empirically against a real local Postgres 16 -
// see this file's accompanying task report). Asserting this exact code,
// AND a message substring naming the specific reason, is what
// distinguishes "Postgres genuinely refused this operation for the
// reason we intend" from "some unrelated error occurred" (e.g. a typo in
// the probe SQL would also produce *an* error, just not this one).
const pgInsufficientPrivilegeCode = "42501"

func runtimeTestPool(t *testing.T) *Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping PLAT-ROLESPLIT-1 runtime-role regression test")
	}
	pool, err := Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("failed to connect to test database as the runtime role: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// assertDenied asserts err is a Postgres error with SQLSTATE 42501
// (insufficient_privilege) whose message contains wantSubstr.
func assertDenied(t *testing.T, probe string, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected the runtime role to be denied, but the statement succeeded", probe)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a *pgconn.PgError, got %T: %v", probe, err, err)
	}
	if pgErr.Code != pgInsufficientPrivilegeCode {
		t.Fatalf("%s: expected SQLSTATE %s (insufficient_privilege), got %s: %v", probe, pgInsufficientPrivilegeCode, pgErr.Code, err)
	}
	if !strings.Contains(pgErr.Message, wantSubstr) {
		t.Fatalf("%s: expected error message to contain %q, got %q", probe, wantSubstr, pgErr.Message)
	}
}

// TestRuntimeRole_AdversarialProbesAllDenied runs the full adversarial
// matrix docs/security/runtime-role-separation.md §5's six-probe
// verification and the independent security review's twenty-probe
// extension covered, against a real database, as the actual
// TEST_RUNTIME_DATABASE_URL role. Every one of these is exactly the kind
// of operation an owning role (like "igaming") could perform trivially,
// and which this split exists to make structurally impossible for the
// role the running application actually connects as.
func TestRuntimeRole_AdversarialProbesAllDenied(t *testing.T) {
	pool := runtimeTestPool(t)
	ctx := context.Background()

	probes := []struct {
		name       string
		stmt       string
		wantSubstr string
	}{
		{
			name:       "session_replication_role (classic trigger-bypass)",
			stmt:       `SET session_replication_role = 'replica'`,
			wantSubstr: `permission denied to set parameter "session_replication_role"`,
		},
		{
			name:       "SET ROLE igaming",
			stmt:       `SET ROLE igaming`,
			wantSubstr: `permission denied to set role "igaming"`,
		},
		{
			name:       "ALTER TABLE (add column)",
			stmt:       `ALTER TABLE tenants ADD COLUMN plat_rolesplit_1_probe boolean`,
			wantSubstr: `must be owner of table tenants`,
		},
		{
			name:       "DROP TABLE",
			stmt:       `DROP TABLE tenants`,
			wantSubstr: `must be owner of table tenants`,
		},
		{
			name:       "TRUNCATE",
			stmt:       `TRUNCATE tenants`,
			wantSubstr: `permission denied for table tenants`,
		},
		{
			name:       "ALTER POLICY",
			stmt:       `ALTER POLICY tenants_read ON tenants USING (true)`,
			wantSubstr: `must be owner of table tenants`,
		},
		{
			name:       "DROP POLICY",
			stmt:       `DROP POLICY tenants_read ON tenants`,
			wantSubstr: `must be owner of relation tenants`,
		},
		{
			name:       "DISABLE ROW LEVEL SECURITY",
			stmt:       `ALTER TABLE tenants DISABLE ROW LEVEL SECURITY`,
			wantSubstr: `must be owner of table tenants`,
		},
		{
			name:       "ALTER TABLE ... OWNER TO",
			stmt:       `ALTER TABLE tenants OWNER TO igaming_runtime`,
			wantSubstr: `must be owner of table tenants`,
		},
		{
			name:       "CREATE ROLE",
			stmt:       `CREATE ROLE plat_rolesplit_1_probe_role LOGIN`,
			wantSubstr: `permission denied to create role`,
		},
		{
			name:       "ALTER ROLE ... BYPASSRLS",
			stmt:       `ALTER ROLE igaming_runtime BYPASSRLS`,
			wantSubstr: `permission denied to alter role`,
		},
		{
			name:       "CREATE FUNCTION ... SECURITY DEFINER",
			stmt:       `CREATE FUNCTION plat_rolesplit_1_probe_fn() RETURNS void AS $$ BEGIN END; $$ LANGUAGE plpgsql SECURITY DEFINER`,
			wantSubstr: `permission denied for schema public`,
		},
	}

	for _, p := range probes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			_, err := pool.Raw().Exec(ctx, p.stmt)
			assertDenied(t, p.name, err, p.wantSubstr)
		})
	}
}

// TestRuntimeRole_OrdinaryDMLIsGenuinelyRLSScoped proves the flip side of
// the probes above: the runtime role can still perform ordinary
// application DML, and - unlike the current "igaming" credential, which
// is exempt as the table owner - row-level security is genuinely
// enforced for it. Seeds two tenants' data using the runtime role
// itself (not merely the migration-owner role), then confirms a
// cross-tenant read scoped to tenant A returns zero rows for tenant B's
// data.
func TestRuntimeRole_OrdinaryDMLIsGenuinelyRLSScoped(t *testing.T) {
	primary := testPool(t) // "igaming" - used only to seed the platform-wide tenant/jurisdiction registry rows.
	runtime := runtimeTestPool(t)
	ctx := context.Background()

	tenantA := createTestTenant(t, primary)
	tenantB := createTestTenant(t, primary)
	jurisdiction := createTestJurisdiction(t, primary)

	seed := func(tenantID uuid.UUID, rulesetID string) {
		err := runtime.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, $3)`,
				tenantID, jurisdiction, rulesetID,
			)
			return err
		})
		if err != nil {
			t.Fatalf("runtime role failed to seed tenant_jurisdiction_configs for %s: %v", tenantID, err)
		}
	}
	// Seeded via the RUNTIME role itself, proving it can perform ordinary
	// application writes, not merely that some other, more privileged
	// role can.
	seed(tenantA, "runtime-role-tenant-a")
	seed(tenantB, "runtime-role-tenant-b")

	var ownRuleset string
	err := runtime.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT kyc_ruleset_id FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tenantA).Scan(&ownRuleset)
	})
	if err != nil {
		t.Fatalf("runtime role, scoped to tenant A, failed to read its own config: %v", err)
	}
	if ownRuleset != "runtime-role-tenant-a" {
		t.Errorf("expected tenant A's own ruleset, got %q", ownRuleset)
	}

	// The core assertion: scoped to tenant A via the RUNTIME role, an
	// explicit query for tenant B's row by id returns zero rows - RLS is
	// enforced for this specific role, not merely "the connection is
	// denied outright" (the adversarial probes above prove that the
	// role cannot escalate; this proves ordinary reads are also
	// correctly narrowed).
	err = runtime.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		var rulesetID string
		return tx.QueryRow(ctx, `SELECT kyc_ruleset_id FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tenantB).Scan(&rulesetID)
	})
	if err == nil {
		t.Fatal("expected no rows when the runtime role, scoped to tenant A, queries tenant B's config, but a row was returned")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows, got: %v", err)
	}

	// And an unfiltered SELECT scoped to tenant A via the runtime role
	// never returns tenant B's row mixed in.
	err = runtime.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id FROM tenant_jurisdiction_configs`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			if id == tenantB {
				t.Fatal("runtime role's unfiltered query, scoped to tenant A, leaked tenant B's row")
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("unexpected error scanning tenant A's rows via the runtime role: %v", err)
	}
}

// TestRuntimeRole_SportsbookBetSettlementsPrivileges is the devops-owned
// probe ADR 0088 §3.5 calls for: it proves the guarded REVOKE in
// migration 0091 (and its mirrors in deploy/init-app-role.sql and
// .github/workflows/ci.yml) actually took effect against the real runtime
// role, while confirming the role still holds the SELECT/INSERT
// privileges it needs for ordinary application traffic against this
// append-only history table. This is deliberately a *privilege-level*
// probe, not a re-run of the deny-trigger suite: ADR 0088 §3.5 is explicit
// that the deny triggers (BEFORE UPDATE OR DELETE / BEFORE TRUNCATE,
// exercised for every role including the owner by
// TestSportsbookBetSettlements_DenyTriggers-style tests owned by
// code-reviewer/qa, §14) are the BINDING control; this REVOKE is defence
// in depth only, so it is verified here as a privilege check
// (has_table_privilege / a real denied statement), not as a re-statement
// of trigger behaviour.
func TestRuntimeRole_SportsbookBetSettlementsPrivileges(t *testing.T) {
	runtime := runtimeTestPool(t)
	ctx := context.Background()

	var exists bool
	err := runtime.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'sportsbook_bet_settlements'
		)`,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("failed to check sportsbook_bet_settlements existence: %v", err)
	}
	if !exists {
		t.Skip("sportsbook_bet_settlements does not exist yet (migration 0091 not applied); skipping ADR 0088 §3.5 probe")
	}

	// Privileges that must remain GRANTED (ordinary application traffic:
	// reading history and inserting new settlement/rollback/void/tombstone
	// rows through the append-only table).
	for _, priv := range []string{"SELECT", "INSERT"} {
		var granted bool
		err := runtime.pool.QueryRow(ctx,
			`SELECT has_table_privilege('igaming_runtime', 'sportsbook_bet_settlements', $1)`, priv,
		).Scan(&granted)
		if err != nil {
			t.Fatalf("failed to check %s privilege on sportsbook_bet_settlements: %v", priv, err)
		}
		if !granted {
			t.Errorf("expected igaming_runtime to hold %s on sportsbook_bet_settlements (ordinary application traffic), it does not", priv)
		}
	}

	// Privileges the ADR 0088 §3.5 REVOKE must have stripped.
	for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
		var granted bool
		err := runtime.pool.QueryRow(ctx,
			`SELECT has_table_privilege('igaming_runtime', 'sportsbook_bet_settlements', $1)`, priv,
		).Scan(&granted)
		if err != nil {
			t.Fatalf("failed to check %s privilege on sportsbook_bet_settlements: %v", priv, err)
		}
		if granted {
			t.Errorf("expected igaming_runtime to NOT hold %s on sportsbook_bet_settlements (ADR 0088 §3.5 REVOKE), but it does", priv)
		}
	}

	// Belt-and-braces: attempt real UPDATE/DELETE/TRUNCATE statements and
	// confirm they fail with insufficient_privilege (42501) - not merely
	// that has_table_privilege reports them absent, but that the
	// privilege-level denial is what Postgres actually enforces for this
	// role at the SQL level (independent of, and layered under, the deny
	// triggers themselves).
	probes := []struct {
		name string
		stmt string
	}{
		{"UPDATE", `UPDATE sportsbook_bet_settlements SET request_id = 'plat-rolesplit-1-probe' WHERE false`},
		{"DELETE", `DELETE FROM sportsbook_bet_settlements WHERE false`},
		{"TRUNCATE", `TRUNCATE sportsbook_bet_settlements`},
	}
	for _, p := range probes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			_, err := runtime.Raw().Exec(ctx, p.stmt)
			assertDenied(t, p.name, err, "permission denied for table sportsbook_bet_settlements")
		})
	}
}

// platformCatalogueTables is the six platform-wide catalogue tables
// migration 0084 (ADR 0081, ARCH-DB-2) hardened, and their expected
// policy names (ADR 0081 §3.3) - casino_games gets a platform_admin-scoped
// write policy set, the five sb_* tables get a catalogue_sync-scoped one.
var platformCatalogueTables = []struct {
	table          string
	policySuffixes []string
}{
	{"casino_games", []string{"read", "platform_admin_insert", "platform_admin_update", "platform_admin_delete_visibility"}},
	{"sb_sports", []string{"read", "catalogue_sync_insert", "catalogue_sync_update", "catalogue_sync_delete_visibility"}},
	{"sb_competitions", []string{"read", "catalogue_sync_insert", "catalogue_sync_update", "catalogue_sync_delete_visibility"}},
	{"sb_events", []string{"read", "catalogue_sync_insert", "catalogue_sync_update", "catalogue_sync_delete_visibility"}},
	{"sb_markets", []string{"read", "catalogue_sync_insert", "catalogue_sync_update", "catalogue_sync_delete_visibility"}},
	{"sb_selections", []string{"read", "catalogue_sync_insert", "catalogue_sync_update", "catalogue_sync_delete_visibility"}},
}

// TestRuntimeRole_PlatformCatalogueTablesForceRLSAndHaveNoTenantColumn
// replaces TestRuntimeRole_CasinoGamesExceptionUnchanged, which asserted
// the PRE-migration-0084 state (casino_games had RLS disabled entirely) -
// that assertion is now false by construction. This test asserts the NEW
// invariant docs/security/runtime-role-separation.md §5's "Stage 9.1
// update" section documents for all six platform-wide catalogue tables
// (ADR 0081 §7.5):
//
//   - relrowsecurity AND relforcerowsecurity are both true (RLS is
//     genuinely enabled AND enforced against the table-owning role, not
//     merely nominally present);
//   - no tenant_id/brand_id column exists - this half of the OLD
//     exception is permanent and intentional (ADR 0081 §2.2: these are
//     single-canonical-row platform catalogue data, never tenant-owned),
//     and stays asserted as a positive invariant rather than silently
//     dropped;
//   - the expected policy names (ADR 0081 §3.3) are present.
func TestRuntimeRole_PlatformCatalogueTablesForceRLSAndHaveNoTenantColumn(t *testing.T) {
	runtime := runtimeTestPool(t)
	ctx := context.Background()

	for _, tc := range platformCatalogueTables {
		tc := tc
		t.Run(tc.table, func(t *testing.T) {
			var rowSecurity, forceRowSecurity bool
			err := runtime.pool.QueryRow(ctx,
				`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, tc.table,
			).Scan(&rowSecurity, &forceRowSecurity)
			if err != nil {
				t.Fatalf("failed to read %s's row-level-security state: %v", tc.table, err)
			}
			if !rowSecurity || !forceRowSecurity {
				t.Errorf(
					"%s: expected relrowsecurity AND relforcerowsecurity both true after migration 0084 "+
						"(ADR 0081), got relrowsecurity=%t relforcerowsecurity=%t",
					tc.table, rowSecurity, forceRowSecurity,
				)
			}

			for _, col := range []string{"tenant_id", "brand_id"} {
				var hasCol bool
				err := runtime.pool.QueryRow(ctx,
					`SELECT EXISTS (
						SELECT 1 FROM information_schema.columns
						WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
					)`, tc.table, col,
				).Scan(&hasCol)
				if err != nil {
					t.Fatalf("failed to check %s's columns: %v", tc.table, err)
				}
				if hasCol {
					t.Errorf(
						"%s unexpectedly has a %s column - ADR 0081 §2.2 rules this platform-wide catalogue "+
							"table must never carry one; if this changed, update "+
							"docs/security/runtime-role-separation.md §5 and ADR 0081, don't just let this "+
							"test start failing silently",
						tc.table, col,
					)
				}
			}

			for _, suffix := range tc.policySuffixes {
				policyName := tc.table + "_" + suffix
				var exists bool
				err := runtime.pool.QueryRow(ctx,
					`SELECT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = $1 AND policyname = $2)`,
					tc.table, policyName,
				).Scan(&exists)
				if err != nil {
					t.Fatalf("failed to check policy %s on %s: %v", policyName, tc.table, err)
				}
				if !exists {
					t.Errorf("%s: expected policy %q (ADR 0081 §3.3) to exist, it does not", tc.table, policyName)
				}
			}
		})
	}
}
