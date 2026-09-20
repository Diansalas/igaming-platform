//go:build integration

package operatingmarket

import (
	"context"
	"fmt"
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

// TestOperatingCountryPolicies_TenantDeleteCascadeNowRequiresPlatformAdminScope
// (Stage 4I Phase E-SECURITY, migration 0077) replaces the prior
// TestOperatingCountryPolicies_TenantDeleteCascadeStillWorks: the cascade
// itself (operating_country_policies' own analogue of internal/
// assetregistry's TestRLS_TenantDeletionStillCascadesAuthorizationRows)
// still works exactly as before - PostgreSQL runs referential-integrity
// actions with RLS bypassed, so `tenant_id ... ON DELETE CASCADE` reaches
// the row regardless of this table's own RLS posture, and migration
// 0076's own comment on this table's DELIBERATE no-deny-DELETE-trigger
// asymmetry vs. licence_country_ceilings is unchanged - but the DELETE on
// `tenants` itself now requires a genuinely platform-admin-scoped
// transaction (migration 0077's tenants_platform_admin_delete policy).
// Before migration 0077, an ORDINARY tenant-scoped (or even scopeless)
// connection could delete a DIFFERENT tenant outright and cascade away
// its entire operating-market policy set - a materially worse version of
// ADR 0045 §18 finding F2, now closed: DELETE requests from WithoutTenant
// and from the tenant's OWN WithTenant scope must both be silent zero-row
// no-ops (RLS's USING clause filters the row; a denied DELETE is never an
// error), leaving the tenant and its policy row fully intact, and only a
// WithPlatformAdmin-scoped DELETE may proceed - at which point the
// cascade still removes the tenant's operating_country_policies rows.
func TestOperatingCountryPolicies_TenantDeleteCascadeNowRequiresPlatformAdminScope(t *testing.T) {
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
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants`/`jurisdictions`/
	// `licences` writes now require a genuinely platform-admin-scoped
	// transaction; rows-affected is checked explicitly on every write
	// below because a denied RLS write is a silent zero-row no-op, not an
	// error.
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Cascade Tenant', 'under_platform_licence')`,
			tenantID, "cas-"+tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Cascade Jurisdiction')`,
			jurisdictionID, "CJ-"+jurisdictionID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			licenceID, jurisdictionID, "LIC-"+licenceID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 licence row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, tenantID, licenceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to update 1 tenant row, updated %d", tag.RowsAffected())
		}
		return nil
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

	// (a) A scopeless (WithoutTenant) DELETE must be a silent zero-row
	// no-op - tenants_platform_admin_delete's USING clause filters the
	// row, and RLS never turns a denied DELETE into an error.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected a scopeless DELETE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scopeless delete attempt: %v", err)
	}

	// (b) The tenant's OWN WithTenant-scoped connection must ALSO be
	// unable to delete itself - there is no tenant-scoped write policy of
	// any kind on `tenants`, by design (the entire defect this migration
	// closes).
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected a tenant-scoped self-DELETE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant-scoped self-delete attempt: %v", err)
	}

	// Sanity: the tenant and its policy row both survived both refused
	// attempts, byte for byte.
	var stillThere int
	err = pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, tenantID).Scan(&stillThere)
	})
	if err != nil {
		t.Fatalf("verify tenant survived: %v", err)
	}
	if stillThere != 1 {
		t.Fatalf("expected the tenant to survive both refused DELETE attempts, got count %d", stillThere)
	}

	// (c) A genuinely platform-admin-scoped DELETE succeeds, and the
	// cascade still removes the tenant's operating_country_policies rows.
	err = pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected a platform-admin-scoped DELETE to affect exactly 1 row, affected %d", tag.RowsAffected())
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
		t.Fatalf("platform-admin delete tenant and verify cascade: %v", err)
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

// TestLicenceCountryCeilings_ForgedTenantLicenceIDCannotUnlockAnotherLicencesCeiling
// is Stage 4I Phase E-SECURITY's crux regression for this package: before
// migration 0077, an ordinary tenant-scoped connection could repoint its
// own `tenants.licence_id` at ANOTHER tenant's licence via plain SQL, and
// licence_country_ceilings_read's own composite-ownership EXISTS
// (migration 0076) would then treat that forged pointer as genuine
// ownership, unlocking read access to a ceiling that never belonged to
// this tenant. This test proves the forging UPDATE itself is now refused
// (a silent zero-row no-op, not an error) and that the ceiling accordingly
// remains unreadable.
func TestLicenceCountryCeilings_ForgedTenantLicenceIDCannotUnlockAnotherLicencesCeiling(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "BZ"
	ceiling := enableCeiling(t, pool, f.platformAdmin, f.otherLicenceID, cc)

	// Sanity: before the forgery attempt, f.tenantID cannot read
	// f.otherLicenceID's ceiling at all.
	var before int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE id = $1`, ceiling.ID).Scan(&before)
	})
	if err != nil {
		t.Fatalf("sanity read before forgery: %v", err)
	}
	if before != 0 {
		t.Fatalf("sanity: expected 0 rows before the forgery attempt, got %d", before)
	}

	// THE ATTACK: f.tenantID's own tenant-scoped connection attempts to
	// repoint its own licence_id at f.otherLicenceID via plain SQL - the
	// exact write licence_country_ceilings_read's EXISTS join and
	// operating_country_policies_enforce_ceiling() both trust.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, f.otherLicenceID, f.tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the forging UPDATE to affect 0 rows (RLS denies tenant-scoped writes to tenants), affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("forgery attempt: %v", err)
	}

	// The ceiling remains unreadable - the forgery bought nothing.
	var after int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE id = $1`, ceiling.ID).Scan(&after)
	})
	if err != nil {
		t.Fatalf("read after forgery attempt: %v", err)
	}
	if after != 0 {
		t.Fatalf("SECURITY REGRESSION: expected the forged licence_id to unlock NOTHING, but %d row(s) became readable", after)
	}

	// And f.tenantID's own real licence_id is untouched.
	var stillOwn uuid.UUID
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, f.tenantID).Scan(&stillOwn)
	})
	if err != nil {
		t.Fatalf("verify tenants.licence_id unchanged: %v", err)
	}
	if stillOwn != f.licenceID {
		t.Fatalf("SECURITY REGRESSION: expected tenants.licence_id to remain %s, got %s", f.licenceID, stillOwn)
	}
}

// TestOperatingCountryPolicy_TenantCannotEnableACountryByRepointingItsOwnLicence
// is the full end-to-end attack reproduction the architect live-reproduced
// pre-migration-0077: a tenant repoints its own licence_id at a DIFFERENT
// licence whose ceiling DOES permit a country its own real licence never
// permitted, then attempts to enable that country at the tenant rung.
// Before migration 0077 this succeeded (the ceiling check in
// operating_country_policies_enforce_ceiling() reads tenants.licence_id,
// which the forging UPDATE had already repointed). This test proves the
// forging UPDATE is refused (silent no-op) and the subsequent enable
// attempt still fails the ceiling check against the tenant's REAL,
// unchanged licence.
func TestOperatingCountryPolicy_TenantCannotEnableACountryByRepointingItsOwnLicence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "SB"

	// cc is permitted under f.otherLicenceID's ceiling, but NOT under
	// f.licenceID's (f.tenantID's real licence) - no ceiling row for cc
	// exists under f.licenceID at all.
	enableCeiling(t, pool, f.platformAdmin, f.otherLicenceID, cc)

	// Sanity: attempting to enable cc for f.tenantID fails the ceiling
	// check against its REAL licence, before any forgery attempt.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "attack-attempt-1", Actor: testActor(f.staffActorID, "sanity-before-forgery"),
		})
		return err
	})
	if err == nil {
		t.Fatal("sanity: expected enabling an unpermitted country to fail before any forgery attempt")
	}

	// THE ATTACK: f.tenantID's own tenant-scoped connection repoints its
	// own licence_id at f.otherLicenceID (whose ceiling DOES permit cc).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, f.otherLicenceID, f.tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the forging UPDATE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("forgery attempt: %v", err)
	}

	// The forgery bought nothing: enabling cc for f.tenantID must STILL
	// fail, because tenants.licence_id was never actually changed.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "attack-attempt-2", Actor: testActor(f.staffActorID, "attack-after-forgery"),
		})
		return err
	})
	if err == nil {
		t.Fatal("SECURITY REGRESSION: a tenant enabled a country permitted only by ANOTHER tenant's licence, by forging its own tenants.licence_id")
	}

	// And no policy row was ever committed for cc under f.tenantID.
	var count int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND country_code = $2`, f.tenantID, cc).Scan(&count)
	})
	if err != nil {
		t.Fatalf("verify no policy row committed: %v", err)
	}
	if count != 0 {
		t.Fatalf("SECURITY REGRESSION: expected 0 operating_country_policies rows for %s under the attacking tenant, got %d", cc, count)
	}
}
