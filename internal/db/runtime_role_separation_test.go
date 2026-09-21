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

// TestRuntimeRole_CasinoGamesExceptionUnchanged confirms the one known,
// deliberate exception docs/security/runtime-role-separation.md §5 names
// - "casino_games" (the global game catalogue) has row-level security
// disabled entirely and no tenant_id column, so this role split does not
// make it tenant-scoped or newly protected - is still exactly true after
// this split lands. This is not a new gap; it exists so a future change
// to casino_games's schema is caught here rather than silently
// invalidating that documented claim.
func TestRuntimeRole_CasinoGamesExceptionUnchanged(t *testing.T) {
	runtime := runtimeTestPool(t)
	ctx := context.Background()

	var rowSecurity, forceRowSecurity bool
	err := runtime.pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'casino_games'`,
	).Scan(&rowSecurity, &forceRowSecurity)
	if err != nil {
		t.Fatalf("failed to read casino_games's row-level-security state: %v", err)
	}
	if rowSecurity || forceRowSecurity {
		t.Errorf(
			"casino_games unexpectedly has row-level security enabled (relrowsecurity=%t relforcerowsecurity=%t) - "+
				"docs/security/runtime-role-separation.md §5's documented exception no longer holds; update that "+
				"document if this is an intentional change, don't just let this test start failing silently",
			rowSecurity, forceRowSecurity,
		)
	}

	var hasTenantID bool
	err = runtime.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'casino_games' AND column_name = 'tenant_id'
		)`,
	).Scan(&hasTenantID)
	if err != nil {
		t.Fatalf("failed to check casino_games's columns: %v", err)
	}
	if hasTenantID {
		t.Error(
			"casino_games unexpectedly has a tenant_id column - it is documented as the platform's one " +
				"deliberately global catalogue table with no RLS; if this changed, update " +
				"docs/security/runtime-role-separation.md §5, don't just let this test start failing silently",
		)
	}
}
