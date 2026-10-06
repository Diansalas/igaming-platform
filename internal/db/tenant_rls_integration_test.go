//go:build integration

// Integration tests require a real PostgreSQL database with migrations
// applied (TEST_DATABASE_URL) and are gated behind the "integration"
// build tag so `go test ./...` (unit tests only) stays fast and
// dependency-free; CI runs `go test -tags=integration ./...` against a
// real Postgres service container. See docs/testing/testing-strategy.md.
package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
)

// pgRLSViolationCode is the Postgres SQLSTATE for "new row violates
// row-level security policy" (insufficient_privilege). Asserting this
// specific code - rather than just "an error occurred" - matters because
// a different bug (e.g. a NULL-handling defect in the policy expression
// itself) could also produce *some* error on insert; only this code
// proves the database actually evaluated and enforced the RLS policy.
const pgRLSViolationCode = "42501"

func assertRLSViolation(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a row-level-security violation, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != pgRLSViolationCode {
		t.Fatalf("expected SQLSTATE %s (row-level security violation), got %s: %v", pgRLSViolationCode, pgErr.Code, err)
	}
}

func testPool(t *testing.T) *Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	prooftest.Install(t, url) // SIGNED-ACTOR-PROOF (ADR 0110): per-process random key, owner-provisioned
	return pool
}

// createTestTenant inserts a row into the tenants table so a
// tenant_jurisdiction_configs row can legally reference it via the
// foreign key. Stage 4I Phase E-SECURITY (migration 0077) gave `tenants`
// RLS with a read-open policy but NO tenant-scoped/scopeless write policy
// of any kind, so both the INSERT and the DELETE cleanup below require a
// genuinely platform-admin-scoped transaction (WithPlatformAdmin), not
// WithoutTenant.
func createTestTenant(t *testing.T, pool *Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'under_platform_licence')`,
			id, "Test Tenant "+id.String(), "test-"+id.String(),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to create test tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

// createTestJurisdiction inserts a row into the platform-wide
// jurisdictions lookup table so tenant_jurisdiction_configs rows can
// legally reference it via the foreign key. Stage 4I Phase E-SECURITY
// (migration 0077) gave `jurisdictions` RLS with a read-open policy but
// writes restricted to a platform-admin-scoped transaction, so the INSERT
// uses WithPlatformAdmin, not WithoutTenant.
//
// NO CLEANUP: migration 0077 deliberately created no DELETE policy on
// `jurisdictions` (by design - a jurisdiction registry row is not meant to
// be deletable in normal operation, same posture as `licences`). A prior
// version of this helper ran a `DELETE FROM jurisdictions` cleanup under
// WithPlatformAdmin; that statement was always a silent zero-row no-op
// under FORCE ROW LEVEL SECURITY once migration 0077 landed (RLS's USING
// clause on DELETE simply filters every row out - no DELETE policy means
// none matches), confirmed to have leaked 649 test rows across one
// whole-repo test run. Test jurisdiction rows created by this helper are
// therefore intentionally permanent, exactly like every other
// non-deletable append-only-ish registry in this codebase.
func createTestJurisdiction(t *testing.T, pool *Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, $3)`,
			id, "TEST-"+id.String()[:8], "Test Jurisdiction "+id.String(),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to create test jurisdiction: %v", err)
	}
	return id
}

// TestTenantIsolation_RLSBlocksCrossTenantAccess is the load-bearing
// proof for docs/decisions/0002-multi-tenancy-isolation-strategy.md: a
// transaction scoped to tenant A must never be able to read tenant B's
// row in a table protected by row-level security, even when the query
// itself does not filter by tenant_id. This is what makes RLS the actual
// enforcement mechanism rather than application-level discipline.
func TestTenantIsolation_RLSBlocksCrossTenantAccess(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)
	jurisdiction := createTestJurisdiction(t, pool)

	// Seed one tenant_jurisdiction_configs row per tenant, each inserted
	// while scoped to its own tenant (as a real provisioning flow would
	// do - see docs/architecture/03-database-architecture.md).
	seed := func(tenantID uuid.UUID, rulesetID string) {
		err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, $3)`,
				tenantID, jurisdiction, rulesetID,
			)
			return err
		})
		if err != nil {
			t.Fatalf("failed to seed tenant_jurisdiction_configs for %s: %v", tenantID, err)
		}
	}
	seed(tenantA, "tenant-a-ruleset")
	seed(tenantB, "tenant-b-ruleset")

	// Tenant A's own row is readable within its own tenant context.
	var ownRuleset string
	err := pool.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT kyc_ruleset_id FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tenantA).Scan(&ownRuleset)
	})
	if err != nil {
		t.Fatalf("tenant A failed to read its own config: %v", err)
	}
	if ownRuleset != "tenant-a-ruleset" {
		t.Errorf("expected tenant A's own ruleset, got %q", ownRuleset)
	}

	// The core assertion: scoped to tenant A, an EXPLICIT query for
	// tenant B's row by id must return no rows - RLS filters it out
	// regardless of the WHERE clause, proving the isolation does not
	// depend on application code remembering to filter correctly.
	err = pool.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		var rulesetID string
		return tx.QueryRow(ctx, `SELECT kyc_ruleset_id FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tenantB).Scan(&rulesetID)
	})
	if err == nil {
		t.Fatal("expected no rows when tenant A queries tenant B's config, but a row was returned")
	}
	if err != pgx.ErrNoRows {
		t.Fatalf("expected pgx.ErrNoRows, got: %v", err)
	}

	// Also prove an unfiltered SELECT scoped to tenant A never returns
	// tenant B's row mixed in.
	err = pool.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
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
				t.Fatal("tenant A's unfiltered query leaked tenant B's row")
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("unexpected error scanning tenant A's rows: %v", err)
	}
}

// TestTenantIsolation_InsertRequiresMatchingTenantContext proves the
// WITH CHECK half of the policy: a transaction scoped to tenant A cannot
// insert a row claiming to belong to tenant B.
func TestTenantIsolation_InsertRequiresMatchingTenantContext(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)
	jurisdiction := createTestJurisdiction(t, pool)

	err := pool.WithTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, $3)`,
			tenantB, jurisdiction, "Forged row",
		)
		return err
	})
	assertRLSViolation(t, err)
}

// TestWithoutTenant_DeniesRLSProtectedTable proves WithoutTenant cannot
// be used to bypass RLS on tenant_jurisdiction_configs - it exists only
// for genuinely platform-level reads (e.g. `tenants`/`jurisdictions`,
// both read-open since migration 0077) and for platform-level tables that
// still carry no RLS at all (`persons`). It is NOT a general-purpose
// scopeless write path - since migration 0077, `tenants`/`jurisdictions`
// writes additionally require WithPlatformAdmin, not merely WithoutTenant.
func TestWithoutTenant_DeniesRLSProtectedTable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantA := createTestTenant(t, pool)
	jurisdiction := createTestJurisdiction(t, pool)

	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, $3)`,
			tenantA, jurisdiction, "Should not be allowed",
		)
		return err
	})
	assertRLSViolation(t, err)
}

// TestConnection_IsNotPrivileged is a standing regression test for the
// Stage 1 finding that both CI and the dev docker-compose stack
// originally connected as the Postgres bootstrap superuser - which
// silently bypasses row-level security regardless of FORCE ROW LEVEL
// SECURITY, making every other test in this file pass for the wrong
// reason. db.Connect now refuses to establish a connection as such a
// role at all (see verifyNotPrivileged in db.go); this test additionally
// asserts the invariant directly against whatever role
// TEST_DATABASE_URL actually points at, so a future misconfiguration is
// caught here even if some other, more permissive code path is ever
// added.
func TestConnection_IsNotPrivileged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var isSuperuser, bypassesRLS bool
	err := pool.pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&isSuperuser, &bypassesRLS)
	if err != nil {
		t.Fatalf("failed to check role privileges: %v", err)
	}
	if isSuperuser {
		t.Error("TEST_DATABASE_URL connects as a superuser - row-level security is entirely bypassed regardless of policy correctness")
	}
	if bypassesRLS {
		t.Error("TEST_DATABASE_URL connects as a role with BYPASSRLS - row-level security is entirely bypassed regardless of policy correctness")
	}
}
