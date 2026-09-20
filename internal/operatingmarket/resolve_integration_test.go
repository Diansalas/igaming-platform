//go:build integration

package operatingmarket

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const opWagering = "wagering"

func TestPlatformOperations_SeededActiveAndExtensibleWithoutMigration(t *testing.T) {
	pool := testPool(t)
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT code, active FROM platform_operations ORDER BY code`)
		if err != nil {
			return err
		}
		defer rows.Close()
		got := map[string]bool{}
		for rows.Next() {
			var code string
			var active bool
			if err := rows.Scan(&code, &active); err != nil {
				return err
			}
			got[code] = active
		}
		want := map[string]bool{"registration": true, "deposit": true, "withdrawal": true, "wagering": true}
		for code, active := range want {
			if got[code] != active {
				t.Fatalf("expected %s active=%v, got %v (present=%v)", code, active, got[code], func() bool { _, ok := got[code]; return ok }())
			}
		}
		// Extensible without a migration: a new row (inactive by the
		// fail-closed default) can be added by a plain INSERT.
		newCode := "test_new_operation_" + uuid.New().String()[:8]
		if _, err := tx.Exec(ctx, `INSERT INTO platform_operations (code, display_name) VALUES ($1, 'Test New Operation')`, newCode); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT active FROM platform_operations WHERE code = $1`, newCode).Scan(&active); err != nil {
			return err
		}
		if active {
			t.Fatal("expected a newly inserted operation to default to active=false (fail-closed)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("platform_operations check: %v", err)
	}
}

func TestOperatingCountryPolicy_LicenceEnabledAllowsTenantEnable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "BR"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if res.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted, got %s", res.Outcome())
	}
	if !res.Permitted() {
		t.Fatal("expected Permitted() true")
	}
}

func TestOperatingCountryPolicy_LicenceDisabledRefusesTenantEnable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "CO"
	// No ceiling row at all for this country - the tenant enable must be
	// refused by the write-time trigger.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "should-not-matter", Actor: testActor(f.staffActorID, "attempt-enable"),
		})
		return err
	})
	if err == nil {
		t.Fatal("expected the enable to be refused - no ceiling row exists for this country")
	}
	if !errors.Is(err, ErrCeilingExceeded) {
		t.Fatalf("expected ErrCeilingExceeded, got %v", err)
	}

	// Nothing was written, and no audit row exists for this attempt.
	var rowCount, auditCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND country_code = $2`, f.tenantID, cc).Scan(&rowCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND metadata->>'country_code' = $2`, f.tenantID, cc).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("verify nothing written: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("expected zero operating_country_policies rows, got %d", rowCount)
	}
	if auditCount != 0 {
		t.Fatalf("expected zero audit rows for the refused attempt, got %d", auditCount)
	}

	// Now enable the ceiling explicitly DISABLED, and confirm the same refusal.
	disableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "should-not-matter", Actor: testActor(f.staffActorID, "attempt-enable-2"),
		})
		return err
	})
	if !errors.Is(err, ErrCeilingExceeded) {
		t.Fatalf("expected ErrCeilingExceeded with an explicitly disabled ceiling, got %v", err)
	}
}

func TestOperatingCountryPolicy_LicenceContractionImmediatelyUnavailable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "MX"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	before := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if before.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted before contraction, got %s", before.Outcome())
	}

	disableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	after := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if after.Outcome() != OutcomeNotPermittedByLicence {
		t.Fatalf("expected not_permitted_by_licence immediately after contraction, got %s", after.Outcome())
	}

	// The tenant row itself is untouched - confirmed by re-enabling the
	// ceiling and observing permitted again with NO tenant-row rewrite.
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	reenabled := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if reenabled.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted again after re-expansion (tenant row was never touched), got %s", reenabled.Outcome())
	}
}

func TestOperatingCountryPolicy_LicenceReenableDoesNotAutoEnableLowerScopes(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// Country DO: tenant policy stays ENABLED through an off/on licence cycle.
	ccEnabled := "DO"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccEnabled)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccEnabled)
	disableCeiling(t, pool, f.platformAdmin, f.licenceID, ccEnabled)
	blocked := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccEnabled, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if blocked.Outcome() != OutcomeNotPermittedByLicence {
		t.Fatalf("expected not_permitted_by_licence while ceiling is off, got %s", blocked.Outcome())
	}
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccEnabled)
	restored := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccEnabled, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if restored.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted again - the previously-enabled tenant row must not need re-authoring, got %s", restored.Outcome())
	}

	// Country EE: tenant policy stays DISABLED through an off/on licence cycle.
	ccDisabled := "EE"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccDisabled)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccDisabled)
	disableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccDisabled)
	beforeCycle := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccDisabled, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if beforeCycle.Outcome() != OutcomeDisabledByTenant {
		t.Fatalf("expected disabled_by_tenant before the licence cycle, got %s", beforeCycle.Outcome())
	}
	disableCeiling(t, pool, f.platformAdmin, f.licenceID, ccDisabled)
	duringCycle := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccDisabled, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if duringCycle.Outcome() != OutcomeNotPermittedByLicence {
		t.Fatalf("expected not_permitted_by_licence (licence is the blocker, evaluated first) during the cycle, got %s", duringCycle.Outcome())
	}
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccDisabled)
	afterCycle := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccDisabled, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if afterCycle.Outcome() != OutcomeDisabledByTenant {
		t.Fatalf("expected disabled_by_tenant again after re-expansion (the tenant's own disable must NOT auto-enable), got %s", afterCycle.Outcome())
	}
}

func TestOperatingCountryPolicy_TenantAbsenceIsNotConfiguredNeverPermitted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "PE"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	// No tenant-scope row written at all.

	res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if res.Outcome() != OutcomeNotConfigured {
		t.Fatalf("expected not_configured (a licence GRANTS, it does not INSTRUCT), got %s", res.Outcome())
	}
	if res.Permitted() {
		t.Fatal("not_configured must never be Permitted()")
	}
}

func TestOperatingCountryPolicy_BrandAndOperationAbsenceInherit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "CL"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	// No brand-scope row, no operation-scope row.

	res := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC(),
	})
	if res.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted (brand and operation absence both inherit the enabled tenant rung), got %s", res.Outcome())
	}
}

func TestOperatingCountryPolicy_ExplicitDisableOverridesInheritedEnable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "UY"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// A brand row is validly enabled WHILE the tenant is still enabled.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "brand-ok",
			Actor: testActor(f.staffActorID, "enable-brand"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable brand policy: %v", err)
	}

	// The tenant is SUBSEQUENTLY disabled - the write-time trigger only
	// checks upward at write time, so the brand row above is now orphaned
	// (still 'enabled' in storage).
	disableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	res := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC(),
	})
	if res.Outcome() != OutcomeDisabledByTenant {
		t.Fatalf("expected disabled_by_tenant (top-down first-disabled-wins names the HIGHEST disabled rung, not most-specific-wins), got %s", res.Outcome())
	}
}

func TestOperatingCountryPolicy_NoPathDefaultsToEnabled(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// (a) Completely fresh tenant with no licence at all.
	freshTenant := uuid.New()
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'No Licence Tenant', 'under_platform_licence')`,
			freshTenant, "nolic-"+freshTenant.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant with no licence: %v", err)
	}
	r1 := resolveNow(t, pool, freshTenant, Query{TenantID: freshTenant, CountryCode: "ZW", OperationCode: opWagering, AsOf: time.Now().UTC()})
	if r1.Permitted() {
		t.Fatalf("expected never-permitted for a tenant with no licence, got %s", r1.Outcome())
	}

	// (b) No ceiling row at all for a country under the fixture's own licence.
	r2 := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: "ZM", OperationCode: opWagering, AsOf: time.Now().UTC()})
	if r2.Permitted() {
		t.Fatalf("expected never-permitted with no ceiling row, got %s", r2.Outcome())
	}

	// (c) Ceiling enabled, but no tenant policy row at all.
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, "GH")
	r3 := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: "GH", OperationCode: opWagering, AsOf: time.Now().UTC()})
	if r3.Permitted() {
		t.Fatalf("expected never-permitted with no tenant policy row, got %s", r3.Outcome())
	}

	// (d) Ceiling explicitly disabled.
	disableCeiling(t, pool, f.platformAdmin, f.licenceID, "KE")
	r4 := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: "KE", OperationCode: opWagering, AsOf: time.Now().UTC()})
	if r4.Permitted() {
		t.Fatalf("expected never-permitted with an explicitly disabled ceiling, got %s", r4.Outcome())
	}
}

func TestResolveOperatingCountryPolicy_AsOfDeterministicAndNotYetEffective(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "PA"
	// The ceiling must already be in force well before the future-dated
	// tenant row below, so ONLY the tenant rung is "not yet effective" -
	// not the ceiling itself.
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	// A tenant-scope row explicitly dated in the FUTURE, constructed via
	// raw SQL (bypassing the stamp_times trigger) rather than relying on
	// real wall-clock scheduling - deterministic, no timing assumption.
	futureFrom := time.Now().UTC().Add(24 * time.Hour)
	toggleStampTimesTrigger(t, pool, false)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, country_code, state, status, authorization_reference,
				reason_code, policy_version, effective_from, effective_to, created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'tenant', $2, 'enabled', 'active', 'future-test-ref', 'future-test', $3, $4, NULL, 'staff', $5)`,
			f.tenantID, cc, PolicyVersion, futureFrom, f.staffActorID)
		return err
	})
	toggleStampTimesTrigger(t, pool, true)
	if err != nil {
		t.Fatalf("raw insert future-dated row: %v", err)
	}

	beforeFuture := futureFrom.Add(-time.Hour)
	past := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: beforeFuture})
	if past.Outcome() != OutcomePolicyNotYetEffective {
		t.Fatalf("expected policy_not_yet_effective when AsOf precedes the only version's effective_from, got %s", past.Outcome())
	}

	// Two calls with the SAME AsOf against the same row set return the
	// same Outcome, always (INV-M-3).
	r1 := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: beforeFuture})
	r2 := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: beforeFuture})
	if r1.Outcome() != r2.Outcome() {
		t.Fatalf("two resolutions with the SAME AsOf against the same row set must return the same Outcome (INV-M-3): got %s then %s", r1.Outcome(), r2.Outcome())
	}

	// At (or after) the future effective_from, the row governs and
	// resolves permitted.
	atFuture := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: futureFrom.Add(time.Minute)})
	if atFuture.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted once AsOf reaches the future-dated row's effective_from, got %s", atFuture.Outcome())
	}
}

func TestResolveOperatingCountryPolicy_GappedChainYieldsPolicyExpired(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "GT"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	asOf := time.Now().UTC()
	from := asOf.Add(-2 * time.Hour)
	to := asOf.Add(-1 * time.Hour)

	toggleStampTimesTrigger(t, pool, false)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, country_code, state, status, authorization_reference,
				reason_code, policy_version, effective_from, effective_to, created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'tenant', $2, 'enabled', 'active', 'gap-test-ref', 'gap-test', $3, $4, $5, 'staff', $6)`,
			f.tenantID, cc, PolicyVersion, from, to, f.staffActorID)
		return err
	})
	toggleStampTimesTrigger(t, pool, true)
	if err != nil {
		t.Fatalf("raw insert gapped row: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: asOf})
	if res.Outcome() != OutcomePolicyExpired {
		t.Fatalf("expected policy_expired (a gap: the latest version closed before AsOf with no successor), got %s", res.Outcome())
	}
}

func TestResolveOperatingCountryPolicy_DuplicateInForceYieldsConfigurationConflict(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "HN"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	asOf := time.Now().UTC()

	toggleStampTimesTrigger(t, pool, false)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Row A: in force at asOf via a future effective_to (not NULL, so
		// it does not collide with the partial unique index).
		if _, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, country_code, state, status, authorization_reference,
				reason_code, policy_version, effective_from, effective_to, created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'tenant', $2, 'enabled', 'active', 'dup-test-ref-a', 'dup-test', $3, $4, $5, 'staff', $6)`,
			f.tenantID, cc, PolicyVersion, asOf.Add(-2*time.Hour), asOf.Add(2*time.Hour), f.staffActorID); err != nil {
			return err
		}
		// Row B: also in force at asOf, still open (effective_to NULL).
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, country_code, state, status, authorization_reference,
				reason_code, policy_version, effective_from, effective_to, created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'tenant', $2, 'enabled', 'active', 'dup-test-ref-b', 'dup-test', $3, $4, NULL, 'staff', $5)`,
			f.tenantID, cc, PolicyVersion, asOf.Add(-1*time.Hour), f.staffActorID)
		return err
	})
	toggleStampTimesTrigger(t, pool, true)
	if err != nil {
		t.Fatalf("raw insert overlapping rows: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: asOf})
	if res.Outcome() != OutcomeConfigurationConflict {
		t.Fatalf("expected configuration_conflict (two simultaneously in-force tenant rows), got %s", res.Outcome())
	}
}

func TestResolveOperatingCountryPolicy_UnparsableStoredValueYieldsInvalidConfiguration(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "SV"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	toggleStateCheckConstraint(t, pool, false)
	t.Cleanup(func() { toggleStateCheckConstraint(t, pool, true) })
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, country_code, state, status, authorization_reference,
				reason_code, policy_version, created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'tenant', $2, 'bogus_state', 'active', 'unparsable-test', 'unparsable-test', $3, 'staff', $4)`,
			f.tenantID, cc, PolicyVersion, f.staffActorID)
		return err
	})
	if err != nil {
		t.Fatalf("raw insert unparsable row: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if res.Outcome() != OutcomeInvalidConfiguration {
		t.Fatalf("expected invalid_configuration for an unparsable stored state value, got %s", res.Outcome())
	}
}

func toggleStampTimesTrigger(t *testing.T, pool *db.Pool, enable bool) {
	t.Helper()
	verb := "DISABLE"
	if enable {
		verb = "ENABLE"
	}
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE operating_country_policies %s TRIGGER operating_country_policies_stamp_times`, verb))
		return err
	})
	if err != nil {
		t.Fatalf("%s stamp_times trigger: %v", verb, err)
	}
}

func toggleStateCheckConstraint(t *testing.T, pool *db.Pool, add bool) {
	t.Helper()
	var stmt string
	if add {
		// NOT VALID: re-adds the constraint for every FUTURE write without
		// re-validating this test's own already-corrupted row (which the
		// table's own RLS/append-only posture leaves no sanctioned way to
		// delete from this role - no DELETE policy exists on this table by
		// design, ADR 0045 §7.3). The constraint still fully applies to
		// every subsequent INSERT/UPDATE.
		stmt = `ALTER TABLE operating_country_policies ADD CONSTRAINT operating_country_policies_state_check CHECK (state = ANY (ARRAY['enabled'::text, 'disabled'::text])) NOT VALID`
	} else {
		// IF EXISTS: tolerant of a prior interrupted test run (e.g. a
		// process kill between DROP and the re-add) already having
		// removed it - this test always restores it via the `add` branch
		// before returning, successful or not, via t.Cleanup.
		stmt = `ALTER TABLE operating_country_policies DROP CONSTRAINT IF EXISTS operating_country_policies_state_check`
	}
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, stmt)
		return err
	})
	if err != nil {
		t.Fatalf("toggle state CHECK constraint (add=%v): %v", add, err)
	}
}

// TestResolveOperatingCountryPolicy_NotYetIssuedLicenceYieldsNotPermittedByLicence
// is ADR 0045 §18's finding F4 at the resolve() level: a licence whose
// issued_at is in the future must resolve not_permitted_by_licence, never
// permitted, for an otherwise fully-permitted row set (this is the same
// "identical stored row set, different resolve() answer" property that
// justifies bumping PolicyVersion to v3 for this finding alone).
func TestResolveOperatingCountryPolicy_NotYetIssuedLicenceYieldsNotPermittedByLicence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "MS"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	before := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if before.Outcome() != OutcomePermitted {
		t.Fatalf("sanity: expected permitted before issued_at is set in the future, got %s", before.Outcome())
	}

	futureIssue := time.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
	// Stage 4I Phase E-SECURITY (migration 0077): `licences` writes now
	// require a genuinely platform-admin-scoped transaction - a
	// WithoutTenant UPDATE here would be a SILENT ZERO-ROW NO-OP (RLS
	// denies the write, no error is raised), which would make this test
	// pass for the wrong reason (falling through to the "before" sanity
	// assertion's own permitted outcome) rather than genuinely proving
	// the not-yet-issued-licence behavior. Rows-affected is checked
	// explicitly for exactly that reason.
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE licences SET issued_at = $2 WHERE id = $1`, f.licenceID, futureIssue)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to update 1 licence row, updated %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set future issued_at: %v", err)
	}

	after := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if after.Outcome() != OutcomeNotPermittedByLicence {
		t.Fatalf("expected not_permitted_by_licence for a not-yet-issued licence, got %s", after.Outcome())
	}
	if after.Permitted() {
		t.Fatal("must never resolve permitted for a not-yet-issued licence")
	}

	var exp Explanation
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy: %v", err)
	}
	if exp.CeilingReason != CeilingReasonLicenceNotYetIssued {
		t.Fatalf("expected CeilingReasonLicenceNotYetIssued, got %s", exp.CeilingReason)
	}

	// A licence issued in the past (or on AsOf's own date) is unaffected.
	// Stage 4I Phase E-SECURITY (migration 0077): same rows-affected
	// discipline as above - a WithoutTenant UPDATE here would silently
	// affect zero rows rather than error.
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE licences SET issued_at = $2 WHERE id = $1`, f.licenceID, time.Now().UTC().Truncate(24*time.Hour))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to update 1 licence row, updated %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set issued_at to today: %v", err)
	}
	restored := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if restored.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted again once issued_at is on/before AsOf's date, got %s", restored.Outcome())
	}
}
