//go:build integration

// Independent QA adversarial regression coverage for Stage 4I Phase
// E-SECURITY (migration 0077), written by the qa specialist as part of
// an independent review of ADR 0046. These scenarios are deliberately
// NOT named in the ADR itself and exercise combinations the review
// wanted separately verified: (1) a tenant's read access to a licence it
// was PREVIOUSLY, but is no longer, bound to; (2) a stray/leftover
// tenant GUC alongside a genuine platform-admin GUC on an otherwise
// correctly-scoped WithPlatformAdmin transaction; (3)/(4) that
// uq_tenants_exclusive_own_licence is correctly scoped to
// expected_licensee='tenant' only, verified via raw SQL rather than
// AssignTenantLicence, and that it still fires for two DISTINCT
// own_licence tenants attempting the SAME licence even when seeded via
// raw SQL.
package jurisdiction

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestQAAdversarial_TenantCannotReadLicenceItWasPreviouslyBoundToAfterReassignment
// proves licences_read's composite-ownership EXISTS clause is evaluated
// against CURRENT tenants.licence_id, not some cached/historical binding:
// a tenant that legitimately read its own licence before a reassignment
// must lose read access the moment AssignTenantLicence repoints it
// elsewhere (or unassigns it), even though the licence row itself never
// changes.
func TestQAAdversarial_TenantCannotReadLicenceItWasPreviouslyBoundToAfterReassignment(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// f.tenantID already holds f.licenceID (seedFixture). Sanity: it can
	// read its own bound licence right now.
	var before int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, f.licenceID).Scan(&before)
	})
	if err != nil {
		t.Fatalf("sanity read before reassignment: %v", err)
	}
	if before != 1 {
		t.Fatalf("sanity: expected f.tenantID to read its own bound licence before reassignment, got %d", before)
	}

	// Reassign f.tenantID to a DIFFERENT licence via the sanctioned admin
	// path (platform-admin-scoped, per migration 0077's contract).
	replacement := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AssignTenantLicence(ctx, tx, AssignTenantLicenceParams{
			TenantID: f.tenantID, LicenceID: &replacement.ID,
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "qa-adversarial-reassign"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("reassign f.tenantID to replacement licence: %v", err)
	}
	if got := tenantLicenceID(t, pool, f.tenantID); got == nil || *got != replacement.ID {
		t.Fatalf("expected f.tenantID's licence_id to be the replacement %s, got %v", replacement.ID, got)
	}

	// THE ASSERTION: f.tenantID's own connection must now read ZERO rows
	// for the licence it used to hold - the licence row itself was never
	// deleted or modified, only the tenant's OWN pointer moved away from
	// it.
	var after int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, f.licenceID).Scan(&after)
	})
	if err != nil {
		t.Fatalf("read former licence after reassignment: %v", err)
	}
	if after != 0 {
		t.Fatalf("SECURITY REGRESSION: expected f.tenantID to read ZERO rows for a licence it is no longer bound to, got %d", after)
	}

	// And it CAN read its new, current licence.
	var current int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licences WHERE id = $1`, replacement.ID).Scan(&current)
	})
	if err != nil {
		t.Fatalf("read current licence after reassignment: %v", err)
	}
	if current != 1 {
		t.Fatalf("expected f.tenantID to read its current bound licence, got %d", current)
	}
}

// TestQAAdversarial_StrayTenantGUCAlongsideGenuinePlatformAdminScopeBreaksWrite
// is the mirror image of registry_rls_integration_test.go's own
// TestRegistryRLS_ForgedPlatformAdminGUCAlongsideTenantGUCGrantsNothing
// (which starts from WithTenant and forges a platform-admin GUC on top).
// This test starts from a GENUINE db.Pool.WithPlatformAdmin transaction
// (the sanctioned, correctly-scoped call every real admin handler makes)
// and then sets a stray/leftover app.tenant_id on the SAME transaction -
// a shape that should never happen through any sanctioned db.Pool helper,
// but could arise from a connection-pooling bug or a copy-paste error
// mixing scope helpers. Every write policy in migration 0077 requires
// app.tenant_id to be UNSET, not merely platform_admin_principal_id to be
// set - so this stray GUC must BREAK the write, not be silently ignored
// in favor of the (also-present) valid platform-admin credential.
func TestQAAdversarial_StrayTenantGUCAlongsideGenuinePlatformAdminScopeBreaksWrite(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		// Simulate a stray leftover app.tenant_id on this otherwise
		// genuinely platform-admin-scoped transaction.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			return err
		}
		id := uuid.New()
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Stray Tenant GUC Attempt', 'under_platform_licence')`,
			id, "stray-guc-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("SECURITY REGRESSION: expected the write to affect 0 rows once a stray app.tenant_id is present alongside a genuine platform-admin GUC, affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("expected either a nil error (0 rows) or SQLSTATE 42501, got: %v", err)
		}
	}

	// Same proof against an UPDATE of the platform admin's own row is not
	// meaningful here (tenants has no self-row for a staff principal), so
	// additionally prove the same stray-GUC shape also breaks a write to
	// `licences` (INSERT), which shares the identical
	// platform_admin_principal_id-set-AND-tenant_id-unset predicate.
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			return err
		}
		id := uuid.New()
		tag, err := tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			id, f.jurisdictionID, "STRAY-"+id.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("SECURITY REGRESSION: expected the licences INSERT to affect 0 rows once a stray app.tenant_id is present alongside a genuine platform-admin GUC, affected %d", tag.RowsAffected())
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

// TestQAAdversarial_UniqueOwnLicenceIndexNeverBlocksPlatformLicenseeSharingViaRawSQL
// independently re-verifies (via raw SQL against `tenants` directly,
// bypassing AssignTenantLicence entirely) that
// uq_tenants_exclusive_own_licence's partial WHERE clause
// (expected_licensee = 'tenant') truly never fires for
// licensee='platform' rows, even when THREE distinct
// under_platform_licence tenants are pointed at the very same licence in
// the same transaction.
func TestQAAdversarial_UniqueOwnLicenceIndexNeverBlocksPlatformLicenseeSharingViaRawSQL(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	platformLicence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "platform")

	tenantA, tenantB, tenantC := uuid.New(), uuid.New(), uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		for id, name := range map[uuid.UUID]string{
			tenantA: "QA Shared Platform Licence A",
			tenantB: "QA Shared Platform Licence B",
			tenantC: "QA Shared Platform Licence C",
		} {
			tag, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, slug, name, licensing_model, licence_id) VALUES ($1, $2, $3, 'under_platform_licence', $4)`,
				id, "qa-shared-"+id.String()[:8], name, platformLicence.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("expected to insert 1 tenant row for %s, inserted %d", name, tag.RowsAffected())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SECURITY/CORRECTNESS REGRESSION: expected three under_platform_licence tenants to share one licensee='platform' licence without tripping uq_tenants_exclusive_own_licence, got: %v", err)
	}

	for _, id := range []uuid.UUID{tenantA, tenantB, tenantC} {
		if got := tenantLicenceID(t, pool, id); got == nil || *got != platformLicence.ID {
			t.Fatalf("expected tenant %s's licence_id to be %s, got %v", id, platformLicence.ID, got)
		}
	}
}

// TestQAAdversarial_UniqueOwnLicenceIndexStillFiresForRawSQLSeededOwnLicenceTenants
// is the negative control for the above: seeding two own_licence tenants
// bound to the SAME licensee='tenant' licence via raw SQL (not
// AssignTenantLicence) in the SAME multi-row INSERT statement must still
// violate uq_tenants_exclusive_own_licence - proving the constraint is a
// genuine database-level index, not something only AssignTenantLicence's
// own Go code happens to prevent.
func TestQAAdversarial_UniqueOwnLicenceIndexStillFiresForRawSQLSeededOwnLicenceTenants(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	byolLicence := mustCreatePlatformLicence(t, pool, f.platformAdmin, f.jurisdiction2, "tenant")

	tenantX, tenantY := uuid.New(), uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model, licence_id) VALUES
			   ($1, $2, 'QA Raw SQL BYOL X', 'own_licence', $3),
			   ($4, $5, 'QA Raw SQL BYOL Y', 'own_licence', $3)`,
			tenantX, "qa-raw-byol-x-"+tenantX.String()[:8], byolLicence.ID,
			tenantY, "qa-raw-byol-y-"+tenantY.String()[:8])
		return err
	})
	if err == nil {
		t.Fatal("SECURITY REGRESSION: expected a raw-SQL multi-row INSERT binding two own_licence tenants to the same licence to violate uq_tenants_exclusive_own_licence")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "23505" {
		t.Fatalf("expected SQLSTATE 23505 (unique_violation), got %s: %v", pgErr.Code, err)
	}
	if pgErr.ConstraintName != "uq_tenants_exclusive_own_licence" {
		t.Fatalf("expected constraint uq_tenants_exclusive_own_licence, got %q", pgErr.ConstraintName)
	}

	// Neither row should have committed - the whole statement (and its
	// transaction) failed.
	var count int
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id IN ($1, $2)`, tenantX, tenantY).Scan(&count)
	})
	if err != nil {
		t.Fatalf("verify neither row committed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected neither tenant row to have committed after the unique violation, found %d", count)
	}
}
