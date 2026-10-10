//go:build integration

package operatingmarket

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestCreateOperatingCountryPolicyVersion_CrossTenantWriteTargetDenied(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "NI"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableCeiling(t, pool, f.platformAdmin, f.otherLicenceID, cc)

	// A connection scoped to f.tenantID attempts to write a policy row
	// NAMING f.otherTenantID as the target.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.otherTenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "attacker-ref", Actor: testActor(f.staffActorID, "cross-tenant-attempt"),
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a cross-tenant write target, got %v", err)
	}

	// The target tenant's rows are untouched, and no audit row was written
	// under either tenant for this attempt.
	var rowCount, auditCount int
	verifyErr := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND country_code = $2`, f.otherTenantID, cc).Scan(&rowCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND metadata->>'country_code' = $2`, f.otherTenantID, cc).Scan(&auditCount)
	})
	if verifyErr != nil {
		t.Fatalf("verify target tenant untouched: %v", verifyErr)
	}
	if rowCount != 0 {
		t.Fatalf("expected zero rows for the target tenant, got %d", rowCount)
	}
	if auditCount != 0 {
		t.Fatalf("expected zero audit rows for the target tenant, got %d", auditCount)
	}
}

func TestCreateOperatingCountryPolicyVersion_ForeignBrandRejectedByCompositeFK(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "SV"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// Seed a brand under the OTHER tenant.
	var foreignBrandID uuid.UUID
	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		foreignBrandID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Foreign Brand', 'pending_launch')`,
			foreignBrandID, f.otherTenantID, "fb-"+foreignBrandID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed foreign brand: %v", err)
	}

	// Attempt to write a brand-scope row under f.tenantID naming the
	// FOREIGN tenant's brand - the composite FK (brand_id, tenant_id)
	// REFERENCES brands (id, tenant_id) makes this row unstorable.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &foreignBrandID, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "should-fail",
			Actor: testActor(f.staffActorID, "foreign-brand-attempt"),
		})
		return err
	})
	if err == nil {
		t.Fatal("expected the foreign-brand write to fail (composite FK)")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput (23503 mapped), got %v", err)
	}
}

func TestOperatingCountryPolicies_AppendOnlyUpdateDeleteTruncateRefused(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "BO"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	rec := enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// UPDATE of any column other than effective_to is refused.
	t.Run("update non-effective_to column refused", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE operating_country_policies SET reason_code = 'tampered' WHERE id = $1`, rec.ID)
			return err
		})
		assertPgCode(t, err, pgRaisedError)
	})

	// DELETE is refused (no DELETE policy at all - RLS silently returns 0
	// rows affected rather than raising, per ADR 0045 §7.3's asymmetry).
	t.Run("delete refused, row survives", func(t *testing.T) {
		var rowsAffected int64
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM operating_country_policies WHERE id = $1`, rec.ID)
			if err != nil {
				return err
			}
			rowsAffected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			t.Fatalf("delete attempt: %v", err)
		}
		if rowsAffected != 0 {
			t.Fatalf("expected 0 rows affected by the DELETE, got %d", rowsAffected)
		}
		var stillThere int
		verifyErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE id = $1`, rec.ID).Scan(&stillThere)
		})
		if verifyErr != nil {
			t.Fatalf("verify survival: %v", verifyErr)
		}
		if stillThere != 1 {
			t.Fatalf("expected the row to survive the DELETE attempt, got count %d", stillThere)
		}
	})

	// TRUNCATE is refused outright by a statement-level trigger.
	t.Run("truncate refused", func(t *testing.T) {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE operating_country_policies`)
			return err
		})
		assertPgCode(t, err, pgRaisedError)
	})

	// Same three assertions on licence_country_ceilings (which additionally
	// gets a DENY-DELETE trigger, not just RLS absence).
	ceiling := enableCeiling(t, pool, f.platformAdmin, f.licenceID, "HT")
	t.Run("ceiling update refused", func(t *testing.T) {
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE licence_country_ceilings SET reason_code = 'tampered' WHERE id = $1`, ceiling.ID)
			return err
		})
		assertPgCode(t, err, pgRaisedError)
	})
	t.Run("ceiling delete refused - by RLS absence-of-policy or by the deny-DELETE trigger", func(t *testing.T) {
		// This table has NO DELETE policy AND a full deny-DELETE trigger
		// (ADR 0045 §2.3/§7.2's asymmetry vs. operating_country_policies).
		// Under the ordinary, non-superuser, NOBYPASSRLS application role
		// this test connects as, RLS filters the row out before the
		// trigger ever fires (0 rows affected, no error) - the trigger is
		// the defense-in-depth layer for a role that could bypass RLS
		// (BYPASSRLS/superuser), which this test does not exercise. Either
		// outcome is accepted; row survival is what is actually asserted -
		// mirrors jurisdiction_precedence_configs' own identical
		// TestJurisdictionPrecedenceConfigs_Immutability/delete_rejected
		// precedent.
		var rowsAffected int64
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM licence_country_ceilings WHERE id = $1`, ceiling.ID)
			if err != nil {
				return err
			}
			rowsAffected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			assertPgCode(t, err, pgRaisedError)
		} else if rowsAffected != 0 {
			t.Fatalf("expected the DELETE to affect 0 rows (no DELETE policy exists), affected %d", rowsAffected)
		}
		var stillThere int
		verifyErr := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings WHERE id = $1`, ceiling.ID).Scan(&stillThere)
		})
		if verifyErr != nil {
			t.Fatalf("verify survival: %v", verifyErr)
		}
		if stillThere != 1 {
			t.Fatalf("expected the ceiling row to survive the DELETE attempt, got count %d", stillThere)
		}
	})
	t.Run("ceiling truncate refused", func(t *testing.T) {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE licence_country_ceilings`)
			return err
		})
		assertPgCode(t, err, pgRaisedError)
	})
}
