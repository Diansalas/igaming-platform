//go:build integration

package operatingmarket

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestOperatingMarketTables_RLSPerCommandAndNoForAllNoDelete asserts the
// exact RLS posture on all three new tables directly against
// pg_policies/pg_class - per-command policies only, no FOR ALL, no
// DELETE policy anywhere, FORCE ROW LEVEL SECURITY on all three, and NO
// platform-admin read policy on operating_country_policies (ADR 0045 §7.3).
func TestOperatingMarketTables_RLSPerCommandAndNoForAllNoDelete(t *testing.T) {
	pool := testPool(t)

	type policyRow struct {
		table string
		name  string
		cmd   string
	}

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tablename, policyname, cmd FROM pg_policies
			 WHERE tablename IN ('platform_operations', 'licence_country_ceilings', 'operating_country_policies')
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
		for _, p := range got {
			byTable[p.table] = append(byTable[p.table], p)
			if p.cmd == "ALL" {
				t.Fatalf("table %s has a FOR ALL policy (%s) - forbidden by ADR 0045 §7", p.table, p.name)
			}
			if p.cmd == "DELETE" {
				t.Fatalf("table %s has a DELETE policy (%s) - forbidden, no table may have one", p.table, p.name)
			}
		}

		for _, table := range []string{"platform_operations", "licence_country_ceilings", "operating_country_policies"} {
			if len(byTable[table]) == 0 {
				t.Fatalf("expected at least one RLS policy on %s, found none", table)
			}
		}

		for _, p := range byTable["operating_country_policies"] {
			if p.name == "operating_country_policies_platform_read" || p.name == "operating_country_policies_platform_admin_read" {
				t.Fatalf("operating_country_policies must have NO platform-admin read policy (ADR 0045 §7.3), found %s", p.name)
			}
		}

		var enabled, forced bool
		for _, table := range []string{"platform_operations", "licence_country_ceilings", "operating_country_policies"} {
			if err := tx.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced); err != nil {
				return err
			}
			if !enabled || !forced {
				t.Fatalf("expected %s to have ENABLE+FORCE ROW LEVEL SECURITY, got enabled=%v forced=%v", table, enabled, forced)
			}
		}

		// Strengthen beyond names/commands (Phase E fix round P3 item):
		// assert the actual qual/with_check predicate text of the two
		// security-load-bearing read policies contains the conjuncts this
		// package's own doc comments claim are load-bearing - not a
		// byte-for-byte text match (PostgreSQL's own deparsing of the
		// predicate is not guaranteed stable prose across versions), but a
		// substring check that would fail if the leading player-exclusion
		// conjunct, or the tenant/ownership join, were ever silently
		// dropped from the stored policy.
		predicateChecks := []struct {
			table, policy, wantSubstring string
		}{
			{"operating_country_policies", "operating_country_policies_tenant_read", "player_account_id"},
			{"licence_country_ceilings", "licence_country_ceilings_read", "player_account_id"},
			{"licence_country_ceilings", "licence_country_ceilings_read", "tenants"},
		}
		for _, pc := range predicateChecks {
			var qual string
			if err := tx.QueryRow(ctx, `SELECT COALESCE(qual, '') FROM pg_policies WHERE tablename = $1 AND policyname = $2`, pc.table, pc.policy).Scan(&qual); err != nil {
				return err
			}
			if qual == "" {
				t.Fatalf("expected policy %s on %s to exist with a non-empty USING predicate", pc.policy, pc.table)
			}
			if !strings.Contains(qual, pc.wantSubstring) {
				t.Fatalf("expected policy %s on %s's predicate to reference %q, got: %s", pc.policy, pc.table, pc.wantSubstring, qual)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RLS posture check: %v", err)
	}
}

// TestOperatingMarketTables_PlayerScopedConnectionReadsZeroRows confirms
// the leading player_account_id-IS-NULL conjunct: a player-scoped
// connection (which sets BOTH app.tenant_id and app.player_account_id)
// must read ZERO rows from operating_country_policies, even for its own
// tenant's rows.
func TestOperatingMarketTables_PlayerScopedConnectionReadsZeroRows(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "CR"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// Seed a genuine player account under this tenant/brand so
	// WithPlayerScope has a real row to bind to.
	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		playerID = uuid.New()
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, f.tenantID, f.brandID, personID, playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed player: %v", err)
	}

	var count int
	err = pool.WithPlayerScope(context.Background(), f.tenantID, playerID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND country_code = $2`, f.tenantID, cc).Scan(&count)
	})
	if err != nil {
		t.Fatalf("player-scoped read: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a player-scoped connection to read ZERO rows, got %d", count)
	}
}

// TestLicenceCountryCeilings_PlayerScopedConnectionReadsZeroRows is
// licence_country_ceilings' own analogue of
// TestOperatingMarketTables_PlayerScopedConnectionReadsZeroRows (Phase E
// fix round P3 item: only operating_country_policies had this functional
// negative-probe before this test was added) - a player-scoped connection
// (both app.tenant_id and app.player_account_id set) must read ZERO rows
// from licence_country_ceilings, even for its own tenant's bound licence.
func TestLicenceCountryCeilings_PlayerScopedConnectionReadsZeroRows(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "SR"
	ceiling := enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		playerID = uuid.New()
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, f.tenantID, f.brandID, personID, playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed player: %v", err)
	}

	var count int
	err = pool.WithPlayerScope(context.Background(), f.tenantID, playerID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE id = $1`, ceiling.ID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("player-scoped read: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a player-scoped connection to read ZERO rows of licence_country_ceilings (even for its own tenant's bound licence), got %d", count)
	}
}

// TestOperatingCountryPolicies_TenantDeleteCascadeStillWorks (Phase E fix
// round P3 item) is operating_country_policies' own analogue of
// internal/assetregistry's TestRLS_TenantDeletionStillCascadesAuthorizationRows.
// Migration 0076's own comment on this table discloses a DELIBERATE
// asymmetry vs. licence_country_ceilings: operating_country_policies gets
// NO deny-DELETE trigger arm (only an append-only UPDATE trigger),
// specifically so `tenant_id ... ON DELETE CASCADE` keeps working when a
// tenant row is deleted - PostgreSQL runs referential-integrity actions
// with RLS bypassed, so the cascade reaches the row regardless of this
// table's own RLS posture. The migration's comment warns a "well-meaning
// symmetry fix" (adding a full deny-DELETE trigger here to match
// licence_country_ceilings) would silently break tenant deletion - this
// is the property that would have caught it, and it did not previously
// exist.
func TestOperatingCountryPolicies_TenantDeleteCascadeStillWorks(t *testing.T) {
	pool := testPool(t)

	// A deliberately bare tenant with its own jurisdiction/licence - no
	// brand, no player account (assetregistry's own reasoning: an
	// unrelated non-cascading FK elsewhere would fail the DELETE for a
	// reason that has nothing to do with the property under test here).
	tenantID := uuid.New()
	jurisdictionID := uuid.New()
	licenceID := uuid.New()
	platformAdmin := uuid.New()
	staffActorID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Cascade Tenant', 'under_platform_licence')`,
			tenantID, "cas-"+tenantID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Cascade Jurisdiction')`,
			jurisdictionID, "CJ-"+jurisdictionID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			licenceID, jurisdictionID, "LIC-"+licenceID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, tenantID, licenceID)
		return err
	})
	if err != nil {
		t.Fatalf("seed bare tenant/jurisdiction/licence: %v", err)
	}

	cc := "AW"
	enableCeiling(t, pool, platformAdmin, licenceID, cc)
	rec := enableTenantPolicy(t, pool, tenantID, staffActorID, cc)
	if rec.ID == uuid.Nil {
		t.Fatal("sanity: tenant policy record id must be set")
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected the cascade to remove the deleted tenant's operating_country_policies rows, %d remain", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("delete tenant and verify cascade: %v", err)
	}
}

func TestLicenceCountryCeilings_ForeignTenantCannotReadAnotherLicencesCeiling(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "PY"
	ceiling := enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	// f.otherTenantID is bound to f.otherLicenceID - a DIFFERENT licence -
	// so a connection scoped to it must read ZERO rows for f.licenceID's ceiling.
	var count int
	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE id = $1`, ceiling.ID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("foreign tenant read: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a foreign tenant's connection to read ZERO rows of another licence's ceiling, got %d", count)
	}

	// The OWNING tenant (bound to f.licenceID) CAN read it.
	var ownCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE id = $1`, ceiling.ID).Scan(&ownCount)
	})
	if err != nil {
		t.Fatalf("owning tenant read: %v", err)
	}
	if ownCount != 1 {
		t.Fatalf("expected the owning tenant's connection to read exactly 1 row, got %d", ownCount)
	}
}
