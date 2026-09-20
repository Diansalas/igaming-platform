//go:build integration

// Stage 4I Phase E-SECURITY (migration 0077): direct RLS-level proofs for
// `tenants`/`licences`/`jurisdictions`, mirroring internal/db/tenant_rls_
// integration_test.go's own "prove the DATABASE denies access, not that
// application code happens to filter correctly" discipline. Follows
// jurisdiction_integration_test.go's fixture conventions exactly -
// seedFixture gives f.tenantID (bound to f.licenceID, licensee
// 'platform'), f.otherTenantID (unbound), f.jurisdictionID/f.jurisdiction2,
// f.brandID/f.playerID under f.tenantID, and f.platformAdmin.
package jurisdiction

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// --- tenants ---

// TestTenantsRLS_TenantScopedConnectionCannotUpdateOwnLicenceID is the
// crux regression: before migration 0077, a tenant-scoped connection
// could repoint its OWN tenants.licence_id at ANY licence (including
// another tenant's), which is exactly the write migration 0076's ceiling
// trigger and licence_country_ceilings_read's composite-ownership EXISTS
// both implicitly trusted. This must now be refused: a silent zero-row
// no-op, never an error (RLS's USING clause on UPDATE simply filters the
// row out before WITH CHECK is ever evaluated).
func TestTenantsRLS_TenantScopedConnectionCannotUpdateOwnLicenceID(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	foreignLicence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, foreignLicence.ID, f.tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the tenant-scoped UPDATE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant-scoped self-repoint attempt: %v", err)
	}

	if got := tenantLicenceID(t, pool, f.tenantID); got == nil || *got != f.licenceID {
		t.Fatalf("expected tenants.licence_id to remain %s (unchanged), got %v", f.licenceID, got)
	}
}

// TestTenantsRLS_TenantScopedConnectionCannotUpdateAnotherTenantsRow
// proves a tenant-scoped connection cannot write ANY column of a
// DIFFERENT tenant's row - not merely licence_id.
func TestTenantsRLS_TenantScopedConnectionCannotUpdateAnotherTenantsRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET name = 'Forged Name' WHERE id = $1`, f.otherTenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the cross-tenant UPDATE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant update attempt: %v", err)
	}
}

// TestTenantsRLS_TenantScopedConnectionCannotDeleteAnotherTenant proves
// the pre-migration escalation of ADR 0045 §18 finding F2 (an ordinary
// tenant-scoped connection deleting a DIFFERENT tenant, cascading away its
// entire operating-market policy set) is now closed at the `tenants`
// layer itself.
func TestTenantsRLS_TenantScopedConnectionCannotDeleteAnotherTenant(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, f.otherTenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the cross-tenant DELETE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant delete attempt: %v", err)
	}

	var stillThere int
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, f.otherTenantID).Scan(&stillThere)
	})
	if err != nil {
		t.Fatalf("verify survival: %v", err)
	}
	if stillThere != 1 {
		t.Fatalf("expected f.otherTenantID to survive the refused DELETE, got count %d", stillThere)
	}
}

// TestTenantsRLS_TenantScopedConnectionCannotDefeatCompositeFKByChangingLicensingModel
// is the exact attack-4b shape the architect live-reproduced: changing
// BOTH licensing_model and licence_id in the SAME UPDATE statement, which
// defeats the composite FK (tenants_licence_matches_model) because the
// FK's own `expected_licensee` GENERATED column is recomputed from the
// NEW licensing_model in the same statement - the FK never sees a
// mismatch. Migration 0077 does not need to special-case this shape at
// all: ANY tenant-scoped write to `tenants` is refused, unconditionally,
// regardless of which columns it touches.
func TestTenantsRLS_TenantScopedConnectionCannotDefeatCompositeFKByChangingLicensingModel(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// A licensee='tenant' licence - f.tenantID's real licensing_model
	// ('under_platform_licence') could never legally bind to this without
	// ALSO changing licensing_model in the same statement.
	tenantLicensedLicence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "tenant")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE tenants SET licensing_model = 'own_licence', licence_id = $1 WHERE id = $2`,
			tenantLicensedLicence.ID, f.tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the combined licensing_model+licence_id UPDATE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("composite-FK-defeating attack attempt: %v", err)
	}

	var licensingModel string
	got := tenantLicenceID(t, pool, f.tenantID)
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT licensing_model FROM tenants WHERE id = $1`, f.tenantID).Scan(&licensingModel)
	})
	if err != nil {
		t.Fatalf("read licensing_model: %v", err)
	}
	if licensingModel != "under_platform_licence" {
		t.Fatalf("expected licensing_model to remain 'under_platform_licence', got %q", licensingModel)
	}
	if got == nil || *got != f.licenceID {
		t.Fatalf("expected tenants.licence_id to remain %s, got %v", f.licenceID, got)
	}
}

// TestTenantsRLS_ScopelessConnectionCannotInsertOrUpdate proves
// WithoutTenant (no GUC set at all) cannot write `tenants` either -
// WithoutTenant is a READ-only escape hatch for this table, not a
// bypass.
func TestTenantsRLS_ScopelessConnectionCannotInsertOrUpdate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		id := uuid.New()
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Scopeless Attempt', 'under_platform_licence')`,
			id, "scopeless-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the scopeless INSERT to affect 0 rows or raise an RLS error, affected %d", tag.RowsAffected())
		}
		return nil
	})
	// A scopeless INSERT can surface EITHER as an RLS violation (42501,
	// since WITH CHECK evaluates to false for every candidate row) or,
	// depending on planner behavior, as 0 rows affected with no error -
	// both are acceptable refusals; only a genuinely committed row would
	// be a regression.
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("expected either a nil error (0 rows) or SQLSTATE 42501, got: %v", err)
		}
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET name = 'Scopeless Update' WHERE id = $1`, f.tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the scopeless UPDATE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scopeless update attempt: %v", err)
	}
}

// TestTenantsRLS_PlatformAdminCanProvisionAssignAndDelete is the positive
// control: every write this file proves refused above must succeed from a
// genuinely platform-admin-scoped connection.
func TestTenantsRLS_PlatformAdminCanProvisionAssignAndDelete(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	newTenantID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Platform Admin Provisioned', 'under_platform_licence')`,
			newTenantID, "pa-prov-"+newTenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("platform-admin provision: %v", err)
	}

	licence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: newTenantID, LicenceID: &licence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "provision-assign"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("platform-admin assign: %v", err)
	}
	if got := tenantLicenceID(t, pool, newTenantID); got == nil || *got != licence.ID {
		t.Fatalf("expected tenants.licence_id %s, got %v", licence.ID, got)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, newTenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to delete 1 tenant row, deleted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("platform-admin delete: %v", err)
	}
}

// TestTenantsRLS_SlugLookupAndActiveTenantSweepStillReadFromScopelessConnection
// fences the deliberately read-open (`USING (true)`) posture chosen for
// `tenants`: identity.GetTenantBySlug (staff-login tenant resolution, no
// tenant context by construction) and the three WithoutTenant
// active-tenant sweeps (internal/rg/enumeration_sweep.go,
// internal/reconciliation/scheduler.go, internal/bonus/schedulers.go, all
// `SELECT id FROM tenants WHERE status = 'active'`) must all still read
// tenants correctly from a scopeless connection after migration 0077.
func TestTenantsRLS_SlugLookupAndActiveTenantSweepStillReadFromScopelessConnection(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var slug string
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT slug FROM tenants WHERE id = $1`, f.tenantID).Scan(&slug)
	})
	if err != nil {
		t.Fatalf("read tenant slug: %v", err)
	}

	// Mirrors identity.GetTenantBySlug's exact query and scope.
	var lookedUpID uuid.UUID
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id FROM tenants WHERE slug = $1`, slug,
		).Scan(&lookedUpID)
	})
	if err != nil {
		t.Fatalf("slug lookup from a scopeless connection: %v", err)
	}
	if lookedUpID != f.tenantID {
		t.Fatalf("expected slug lookup to resolve %s, got %s", f.tenantID, lookedUpID)
	}

	// Mirrors the three active-tenant sweeps' exact query and scope.
	var found bool
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM tenants WHERE status = 'active'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			if id == f.tenantID {
				found = true
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("active-tenant sweep read from a scopeless connection: %v", err)
	}
	if !found {
		t.Fatal("expected the active-tenant sweep query to see f.tenantID from a scopeless connection")
	}
}

// --- licences ---

// TestLicencesRLS_TenantScopedConnectionCannotSuspendOrExtendAnyLicence
// proves a tenant-scoped connection cannot write `licences` at all - not
// even its OWN bound licence.
func TestLicencesRLS_TenantScopedConnectionCannotSuspendOrExtendAnyLicence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE licences SET status = 'suspended' WHERE id = $1`, f.licenceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the tenant-scoped UPDATE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant-scoped suspend-own-licence attempt: %v", err)
	}

	var status string
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM licences WHERE id = $1`, f.licenceID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read licence status: %v", err)
	}
	if status != "active" {
		t.Fatalf("expected licence status to remain 'active', got %q", status)
	}
}

// TestLicencesRLS_TenantScopedConnectionCannotInsertALicence proves a
// tenant cannot mint its own licence via raw INSERT.
func TestLicencesRLS_TenantScopedConnectionCannotInsertALicence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		id := uuid.New()
		tag, err := tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			id, f.jurisdictionID, "FORGED-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the tenant-scoped INSERT to affect 0 rows or raise an RLS error, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("expected either a nil error (0 rows) or SQLSTATE 42501, got: %v", err)
		}
	}
}

// TestLicencesRLS_TenantReadsOnlyItsOwnBoundLicence proves the composite-
// ownership read arm: f.tenantID can read f.licenceID (its own), but not
// a licence bound to a different tenant.
func TestLicencesRLS_TenantReadsOnlyItsOwnBoundLicence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	foreign := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &foreign.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "foreign-bind"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("bind foreign licence to f.otherTenantID: %v", err)
	}

	var ownCount, foreignCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, f.licenceID).Scan(&ownCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, foreign.ID).Scan(&foreignCount)
	})
	if err != nil {
		t.Fatalf("tenant-scoped licence reads: %v", err)
	}
	if ownCount != 1 {
		t.Fatalf("expected f.tenantID to read its own bound licence, got count %d", ownCount)
	}
	if foreignCount != 0 {
		t.Fatalf("expected f.tenantID to read ZERO rows of a licence bound to a different tenant, got %d", foreignCount)
	}
}

// TestLicencesRLS_PlayerScopedConnectionReadsZeroLicences proves the
// leading player_account_id-IS-NULL conjunct: a player-scoped connection
// (both app.tenant_id and app.player_account_id set) must read ZERO
// licences, even its own tenant's bound one.
func TestLicencesRLS_PlayerScopedConnectionReadsZeroLicences(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var count int
	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, f.licenceID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("player-scoped licence read: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a player-scoped connection to read ZERO licences, got %d", count)
	}
}

// TestLicencesRLS_PlatformAdminReadsEveryLicence is the positive control:
// platform-admin scope sees every licence, regardless of tenant binding.
func TestLicencesRLS_PlatformAdminReadsEveryLicence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	foreign := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")

	var ownCount, foreignCount int
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, f.licenceID).Scan(&ownCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, foreign.ID).Scan(&foreignCount)
	})
	if err != nil {
		t.Fatalf("platform-admin licence reads: %v", err)
	}
	if ownCount != 1 || foreignCount != 1 {
		t.Fatalf("expected platform-admin to read every licence, got own=%d foreign=%d", ownCount, foreignCount)
	}
}

// --- jurisdictions ---

// TestJurisdictionsRLS_TenantScopedConnectionCannotInsertOrUpdate proves
// `jurisdictions` writes require platform-admin scope even though reads
// are open.
func TestJurisdictionsRLS_TenantScopedConnectionCannotInsertOrUpdate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		id := uuid.New()
		tag, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Forged Jurisdiction')`,
			id, "FORGED-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the tenant-scoped INSERT to affect 0 rows or raise an RLS error, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("expected either a nil error (0 rows) or SQLSTATE 42501, got: %v", err)
		}
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE jurisdictions SET name = 'Forged Name' WHERE id = $1`, f.jurisdictionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the tenant-scoped UPDATE to affect 0 rows, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant-scoped jurisdiction update attempt: %v", err)
	}
}

// TestJurisdictionsRLS_ReadOpenFromEveryScopeSoResolverJoinIsUnchanged
// fences the HDR-J-5 player-jurisdiction path (resolver.go's licence ->
// jurisdiction JOIN): `jurisdictions_read` is deliberately USING (true),
// so reads must succeed identically from scopeless, tenant, platform-admin
// and player scopes.
func TestJurisdictionsRLS_ReadOpenFromEveryScopeSoResolverJoinIsUnchanged(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	assertReads := func(name string, run func(fn db.TxFunc) error) {
		t.Helper()
		var count int
		err := run(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM jurisdictions WHERE id = $1`, f.jurisdictionID).Scan(&count)
		})
		if err != nil {
			t.Fatalf("%s: read jurisdiction: %v", name, err)
		}
		if count != 1 {
			t.Fatalf("%s: expected to read exactly 1 jurisdiction row, got %d", name, count)
		}
	}

	assertReads("scopeless", func(fn db.TxFunc) error {
		return pool.WithoutTenant(context.Background(), fn)
	})
	assertReads("tenant-scoped (owning)", func(fn db.TxFunc) error {
		return pool.WithTenant(context.Background(), f.tenantID, fn)
	})
	assertReads("tenant-scoped (unrelated)", func(fn db.TxFunc) error {
		return pool.WithTenant(context.Background(), f.otherTenantID, fn)
	})
	assertReads("platform-admin", func(fn db.TxFunc) error {
		return pool.WithPlatformAdmin(context.Background(), f.platformAdmin, fn)
	})
	assertReads("player-scoped", func(fn db.TxFunc) error {
		return pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, fn)
	})
}

// --- cross-cutting scope-forgery proofs ---

// TestRegistryRLS_ForgedPlatformAdminGUCAlongsideTenantGUCGrantsNothing
// proves a connection that sets BOTH app.platform_admin_principal_id AND
// app.tenant_id (a scope shape no sanctioned db.Pool helper ever produces,
// but not something SQL itself prevents) still cannot write `tenants`/
// `licences`/`jurisdictions` - every write policy explicitly requires
// app.tenant_id to be UNSET, not merely platform_admin_principal_id to be
// set.
func TestRegistryRLS_ForgedPlatformAdminGUCAlongsideTenantGUCGrantsNothing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, f.platformAdmin.String()); err != nil {
			return err
		}
		id := uuid.New()
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Forged Dual Scope', 'under_platform_licence')`,
			id, "forged-dual-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the forged dual-scope INSERT to affect 0 rows or raise an RLS error, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("expected either a nil error (0 rows) or SQLSTATE 42501, got: %v", err)
		}
	}
}

// TestRegistryRLS_MalformedTenantGUCFailsClosedLoudly proves a malformed
// (non-UUID) app.tenant_id value fails LOUDLY (a database error) rather
// than being silently coerced or ignored - `licences_read`'s predicate
// casts the GUC to ::uuid, so a malformed value raises 22P02
// (invalid_text_representation) the moment the policy is evaluated,
// exactly the "must error, not silently misinterpret" posture this
// codebase requires of every scope assertion.
func TestRegistryRLS_MalformedTenantGUCFailsClosedLoudly(t *testing.T) {
	pool := testPool(t)
	seedFixture(t, pool)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'not-a-uuid', true)`); err != nil {
			return err
		}
		var count int
		return tx.QueryRow(ctx, `SELECT count(*) FROM licences`).Scan(&count)
	})
	if err == nil {
		t.Fatal("expected a malformed app.tenant_id to fail loudly when licences_read evaluates it, got nil error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "22P02" {
		t.Fatalf("expected SQLSTATE 22P02 (invalid_text_representation), got %s: %v", pgErr.Code, err)
	}
}

// --- BYOL exclusivity (uq_tenants_exclusive_own_licence) ---

// TestTenantsRLS_ExclusiveOwnLicenceCannotBeBoundToTwoTenants is Task
// 3(B)/(C)'s live-verified gap, now closed: two distinct tenants, both
// licensing_model='own_licence', cannot bind the SAME licensee='tenant'
// licence - the second AssignTenantLicence call must fail. The underlying
// unique violation on uq_tenants_exclusive_own_licence is now mapped by
// AssignTenantLicence's error-handling switch (Fix 1, this fix round) to
// ErrInvalidInput with a diagnosable message, rather than escaping as a
// raw, unmapped *pgconn.PgError - so this test asserts the MAPPED error,
// not the underlying SQLSTATE, and the second tenant's licence_id must
// remain unset.
func TestTenantsRLS_ExclusiveOwnLicenceCannotBeBoundToTwoTenants(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	byolLicence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "tenant")

	tenantX := uuid.New()
	tenantY := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		for id, name := range map[uuid.UUID]string{tenantX: "BYOL Exclusive X", tenantY: "BYOL Exclusive Y"} {
			tag, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, $3, 'own_licence')`,
				id, "byol-ex-"+id.String()[:8], name)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("expected to insert 1 tenant row")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed two BYOL tenants: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: tenantX, LicenceID: &byolLicence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "byol-first-bind"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("first BYOL assignment must succeed: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: tenantY, LicenceID: &byolLicence.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "byol-second-bind-attempt"},
		})
		return err
	})
	if err == nil {
		t.Fatal("expected the second tenant's assignment of the SAME BYOL licence to fail")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput (the mapped uq_tenants_exclusive_own_licence violation), got %T: %v", err, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("expected the raw *pgconn.PgError to be mapped away, but it still escaped: %v", err)
	}

	if got := tenantLicenceID(t, pool, tenantY); got != nil {
		t.Fatalf("expected tenantY's licence_id to remain NULL after the refused second bind, got %v", got)
	}
	if got := tenantLicenceID(t, pool, tenantX); got == nil || *got != byolLicence.ID {
		t.Fatalf("expected tenantX's licence_id to remain %s, got %v", byolLicence.ID, got)
	}
}

// TestTenantsRLS_PlatformLicenceMayStillBeSharedAcrossManyTenants proves
// uq_tenants_exclusive_own_licence is scoped ONLY to
// expected_licensee='tenant' - a licensee='platform' licence remains
// legitimately shareable across every under_platform_licence tenant, per
// ADR 0006's own hybrid-licensing model.
func TestTenantsRLS_PlatformLicenceMayStillBeSharedAcrossManyTenants(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// f.tenantID already holds f.licenceID (licensee='platform'). Bind the
	// SAME licence to f.otherTenantID too.
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.otherTenantID, LicenceID: &f.licenceID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "shared-platform-licence"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected sharing a platform-licensee licence across two tenants to succeed, got: %v", err)
	}

	if got := tenantLicenceID(t, pool, f.tenantID); got == nil || *got != f.licenceID {
		t.Fatalf("expected f.tenantID's licence_id to remain %s, got %v", f.licenceID, got)
	}
	if got := tenantLicenceID(t, pool, f.otherTenantID); got == nil || *got != f.licenceID {
		t.Fatalf("expected f.otherTenantID's licence_id to be %s, got %v", f.licenceID, got)
	}
}

// TestTenantsRLS_PlayerScopedConnectionReadsZeroTenants proves Fix 5 (Stage
// 4I Phase E-SECURITY fix round): tenants_read's new leading
// NULLIF(app.player_account_id) IS NULL conjunct, mirroring
// TestLicencesRLS_PlayerScopedConnectionReadsZeroLicences's own pattern -
// a player-scoped connection (both app.tenant_id and app.player_account_id
// set) must read ZERO tenants, even its own.
func TestTenantsRLS_PlayerScopedConnectionReadsZeroTenants(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var count int
	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, f.tenantID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("player-scoped tenants read: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a player-scoped connection to read ZERO tenants (even its own), got %d", count)
	}
}

// TestTenantsRLS_NonPlayerScopesStillReadTenants is the positive control
// for Fix 5: a tenant-scoped connection (no player scope) must still be
// able to read tenants rows, including another tenant's - tenants_read
// remains USING (true) for every non-player scope; only player scope was
// narrowed.
func TestTenantsRLS_NonPlayerScopesStillReadTenants(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var ownCount, otherCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, f.tenantID).Scan(&ownCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, f.otherTenantID).Scan(&otherCount)
	})
	if err != nil {
		t.Fatalf("tenant-scoped tenants read: %v", err)
	}
	if ownCount != 1 {
		t.Fatalf("expected a tenant-scoped connection to read its own tenants row, got count %d", ownCount)
	}
	if otherCount != 1 {
		t.Fatalf("expected tenants_read to remain USING (true) for non-player scopes (cross-tenant read still allowed), got count %d", otherCount)
	}
}

// --- Fix 4: deny-TRUNCATE triggers ---
//
// These three tests deliberately run against a dedicated SCRATCH database
// (migration0075ScratchDatabase/migration0075ScratchPool, the same
// mechanism migration_0075_integration_test.go and
// migration_0077_integration_test.go already use), NOT the shared
// testPool() database every other test in this package (and every
// concurrently-running package in a whole-repo `go test ./...` invocation)
// writes against. `TRUNCATE tenants/licences/jurisdictions CASCADE`
// requires PostgreSQL to acquire ACCESS EXCLUSIVE locks on every table
// reachable via a foreign key from the target - for `tenants` in
// particular, that is a very large fraction of the entire schema - BEFORE
// the deny-truncate trigger ever gets a chance to fire and raise. Running
// that against the shared database while other packages hold open
// transactions on the same tables is a genuine, reproduced deadlock
// hazard (confirmed live: this exact test intermittently failed with
// SQLSTATE 40P01 "deadlock detected" against the shared database under a
// full whole-repo `go test -tags=integration ./...` run), not a flaky
// test - an isolated scratch database with no concurrent writers removes
// the hazard entirely while still proving the same property.

// TestTenantsRLS_TruncateIsDeniedEvenFromTenantScope proves Fix 4 (Stage
// 4I Phase E-SECURITY fix round): a TRUNCATE against `tenants` is refused
// by the new tenants_deny_truncate statement-level trigger itself (not by
// an unrelated table's accident partway through a cascade), even from a
// tenant-scoped connection - RLS does not govern TRUNCATE at all, so the
// trigger is the only real control.
func TestTenantsRLS_TruncateIsDeniedEvenFromTenantScope(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE tenants CASCADE`)
		return err
	})
	assertPgCode(t, err, pgRaisedError)
}

// TestLicencesRLS_TruncateIsDeniedEvenFromPlatformScope proves the same
// property for `licences` - even a platform-admin-scoped connection (which
// holds full write access to the table under ordinary DML) cannot bypass
// the deny-truncate trigger, since a trigger is not bypassed by role or
// policy the way RLS is.
func TestLicencesRLS_TruncateIsDeniedEvenFromPlatformScope(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	f := seedFixture(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE licences CASCADE`)
		return err
	})
	assertPgCode(t, err, pgRaisedError)
}

// TestJurisdictionsRLS_TruncateIsDeniedEvenFromScopelessConnection proves
// the same property for `jurisdictions` from a scopeless
// (WithoutTenant) connection - the trigger fires regardless of scope,
// unlike RLS which TRUNCATE bypasses entirely.
func TestJurisdictionsRLS_TruncateIsDeniedEvenFromScopelessConnection(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	dir := migration0075MigrationsDir(t)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the full chain: %v", err)
	}
	_ = seedFixture(t, pool)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE jurisdictions CASCADE`)
		return err
	})
	assertPgCode(t, err, pgRaisedError)
}

// --- Fix 6: jurisdiction.assertTenantScope's missing player-scope conjunct ---

// TestAssertTenantScope_PlayerScopedTransactionIsScopeMismatch is a
// white-box test (package jurisdiction has direct access to the
// unexported assertTenantScope) proving Fix 6 (Stage 4I Phase E-SECURITY
// fix round): a player-scoped transaction now fails with ErrScopeMismatch,
// mirroring internal/operatingmarket's own assertTenantScope exact
// pattern, rather than silently returning inScope=true/false based on
// app.tenant_id alone. This path is unreachable through Resolve() today
// (both production callers always pass a non-nil PlayerAccountID, which
// short-circuits before resolveTenantLicence), so this test calls
// assertTenantScope directly, as the only way to exercise the latent gap
// this fix closes.
func TestAssertTenantScope_PlayerScopedTransactionIsScopeMismatch(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assertTenantScope(ctx, tx, f.tenantID)
		return err
	})
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("expected ErrScopeMismatch for a player-scoped transaction, got %v", err)
	}
}

// TestAssertTenantScope_TenantScopedTransactionStillWorks is the positive
// control for Fix 6: an ordinary tenant-scoped (non-player) transaction is
// unaffected by the new conjunct.
func TestAssertTenantScope_TenantScopedTransactionStillWorks(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var inScope bool
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inScope, err = assertTenantScope(ctx, tx, f.tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("tenant-scoped assertTenantScope: %v", err)
	}
	if !inScope {
		t.Fatal("expected a tenant-scoped transaction to be in scope for its own tenant id")
	}
}
