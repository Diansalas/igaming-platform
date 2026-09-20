//go:build integration

// Tests for ADR 0045 §3.5-A AMENDMENT-2 (finding SEC-E-REV-1): withdrawing
// an in-force active+disabled row at the BRAND or OPERATION rung is
// functionally a widening act (absence at those rungs INHERITS from the
// rung above) and now requires a non-blank AuthorizationReference, both at
// the Go validation layer (policy_admin.go) and, independently, via the
// new CHECK constraint ocp_inherit_rung_withdrawal_requires_authorization
// (migration 0076, amended in place). Test names are mandatory, taken
// verbatim from the architect's ruling.
package operatingmarket

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestOperatingCountryPolicy_WithdrawingABrandRungDisableRequiresAuthorization
// is the direct brand-rung case: tenant enabled, brand disabled/active ->
// disabled_by_brand; withdrawing that disable with a blank
// AuthorizationReference is refused and changes nothing; withdrawing it
// with a non-blank reference succeeds and flips the resolution to
// permitted.
func TestOperatingCountryPolicy_WithdrawingABrandRungDisableRequiresAuthorization(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "AI"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-brand"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand: %v", err)
	}

	// NOTE: AsOf is captured FRESH before each resolve - these writes are
	// effective-dated, so reusing one fixed AsOf across writes separated in
	// time would keep "seeing" the state as of that earlier moment.
	newQuery := func() Query {
		return Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()}
	}
	res := resolveNow(t, pool, f.tenantID, newQuery())
	if res.Outcome() != OutcomeDisabledByBrand {
		t.Fatalf("expected disabled_by_brand, got %s", res.Outcome())
	}

	// Blank AuthorizationReference -> refused.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, Actor: testActor(f.staffActorID, "withdraw-brand-disable-no-ref"),
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a blank-reference brand-rung withdrawal, got %v", err)
	}

	res = resolveNow(t, pool, f.tenantID, newQuery())
	if res.Outcome() != OutcomeDisabledByBrand {
		t.Fatalf("expected disabled_by_brand to remain after the refused withdrawal, got %s", res.Outcome())
	}

	// Non-blank AuthorizationReference -> succeeds, flips to permitted.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-brand-disable-ref",
			Actor: testActor(f.staffActorID, "withdraw-brand-disable-with-ref"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw brand disable with a reference: %v", err)
	}

	res = resolveNow(t, pool, f.tenantID, newQuery())
	if res.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted after the authorized withdrawal, got %s", res.Outcome())
	}
}

// TestOperatingCountryPolicy_WithdrawingAnOperationRungDisableRequiresAuthorization
// covers all three operation-rung shapes: tenant-wide/every-product,
// brand-named, and product-named.
func TestOperatingCountryPolicy_WithdrawingAnOperationRungDisableRequiresAuthorization(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	casino := "casino"

	cases := []struct {
		name        string
		cc          string
		brandID     *uuid.UUID
		productCode *string
	}{
		{"tenant-wide-every-product", "PY", nil, nil},
		{"brand-named", "UY", &f.brandID, nil},
		{"product-named", "BO", nil, &casino},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enableCeiling(t, pool, f.platformAdmin, f.licenceID, tc.cc)
			enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, tc.cc)

			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
					Scope: ScopeOperation, TenantID: f.tenantID, BrandID: tc.brandID, OperationCode: opWagering,
					ProductCode: tc.productCode, CountryCode: tc.cc, State: StateDisabled, Status: StatusActive,
					Actor: testActor(f.staffActorID, "disable-operation-"+tc.name),
				})
				return err
			})
			if err != nil {
				t.Fatalf("disable operation (%s): %v", tc.name, err)
			}

			// NOTE: AsOf is captured FRESH before each resolve - see the
			// identical note in the brand-rung test above.
			newQuery := func() Query {
				return Query{TenantID: f.tenantID, BrandID: tc.brandID, CountryCode: tc.cc, OperationCode: opWagering, ProductCode: tc.productCode, AsOf: time.Now().UTC()}
			}
			res := resolveNow(t, pool, f.tenantID, newQuery())
			if res.Outcome() != OutcomeDisabledByOperation {
				t.Fatalf("expected disabled_by_operation, got %s", res.Outcome())
			}

			// Blank AuthorizationReference -> refused.
			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
					Scope: ScopeOperation, TenantID: f.tenantID, BrandID: tc.brandID, OperationCode: opWagering,
					ProductCode: tc.productCode, CountryCode: tc.cc, State: StateDisabled, Status: StatusWithdrawn,
					Actor: testActor(f.staffActorID, "withdraw-operation-no-ref-"+tc.name),
				})
				return err
			})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput for a blank-reference operation-rung withdrawal (%s), got %v", tc.name, err)
			}

			res = resolveNow(t, pool, f.tenantID, newQuery())
			if res.Outcome() != OutcomeDisabledByOperation {
				t.Fatalf("expected disabled_by_operation to remain after the refused withdrawal (%s), got %s", tc.name, res.Outcome())
			}

			// Non-blank AuthorizationReference -> succeeds.
			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
					Scope: ScopeOperation, TenantID: f.tenantID, BrandID: tc.brandID, OperationCode: opWagering,
					ProductCode: tc.productCode, CountryCode: tc.cc, State: StateDisabled, Status: StatusWithdrawn,
					AuthorizationReference: "withdraw-operation-ref-" + tc.name,
					Actor:                  testActor(f.staffActorID, "withdraw-operation-with-ref-"+tc.name),
				})
				return err
			})
			if err != nil {
				t.Fatalf("withdraw operation disable with a reference (%s): %v", tc.name, err)
			}

			res = resolveNow(t, pool, f.tenantID, newQuery())
			if res.Outcome() != OutcomePermitted {
				t.Fatalf("expected permitted after the authorized withdrawal (%s), got %s", tc.name, res.Outcome())
			}
		})
	}
}

// TestOperatingCountryPolicy_WithdrawingTheBroadDisableAboveALiveCarveOutRequiresAuthorization
// reproduces the legal reverse-order carve-out (narrow enable first, then
// broad disable - AMENDMENT-1 territory) and confirms withdrawing the
// broad disable now requires an AuthorizationReference too.
func TestOperatingCountryPolicy_WithdrawingTheBroadDisableAboveALiveCarveOutRequiresAuthorization(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "NI"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"

	// Narrow enable FIRST - legal, no broader disable exists yet.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "carve-out-ok",
			Actor: testActor(f.staffActorID, "enable-casino-carve-out"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable casino carve-out: %v", err)
	}

	// Broad disable SECOND - always legal (the fail-closed/narrowing
	// direction needs no reference).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-broad-second"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable broad every-product: %v", err)
	}

	q := Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
	res := resolveNow(t, pool, f.tenantID, q)
	if res.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation (AMENDMENT-1: the live broad disable still blocks the live narrow carve-out), got %s", res.Outcome())
	}

	// Withdraw the broad disable with a blank reference: refused.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, Actor: testActor(f.staffActorID, "withdraw-broad-no-ref"),
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a blank-reference broad-disable withdrawal, got %v", err)
	}

	res = resolveNow(t, pool, f.tenantID, q)
	if res.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation to remain unchanged after the refused withdrawal, got %s", res.Outcome())
	}
}

// TestOperatingCountryPolicy_WithdrawingABlockingDisableDoesNotSilentlyLegalizeAPreviouslyRefusedEnable
// pins the designed behavior explicitly: a broad disable's authorized
// withdrawal legitimately makes a previously-refused narrower enable
// writable again.
func TestOperatingCountryPolicy_WithdrawingABlockingDisableDoesNotSilentlyLegalizeAPreviouslyRefusedEnable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "HN"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"

	// Broad disable in force FIRST.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-broad-first"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable broad every-product: %v", err)
	}

	// The carve-out enable is refused by the write-time trigger (AMENDMENT-1).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "attempt-carve-out",
			Actor: testActor(f.staffActorID, "attempt-carve-out-enable"),
		})
		return err
	})
	if !errors.Is(err, ErrCeilingExceeded) {
		t.Fatalf("expected ErrCeilingExceeded for the carve-out enable under the broad disable, got %v", err)
	}

	// Withdraw the broad disable with a blank reference: refused.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, Actor: testActor(f.staffActorID, "withdraw-broad-no-ref"),
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for the blank-reference withdrawal, got %v", err)
	}

	// WITH a reference, the withdrawal succeeds.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-broad-with-ref",
			Actor: testActor(f.staffActorID, "withdraw-broad-with-ref-actor"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw broad disable with a reference: %v", err)
	}

	// DESIGNED BEHAVIOR, PINNED EXPLICITLY: the previously-refused carve-out
	// enable is now writable (no broader in-force disable remains).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "carve-out-now-ok",
			Actor: testActor(f.staffActorID, "carve-out-enable-after-withdrawal"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected the previously-refused carve-out enable to now succeed after the authorized withdrawal, got %v", err)
	}

	q := Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
	res := resolveNow(t, pool, f.tenantID, q)
	if res.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted after the carve-out enable, got %s", res.Outcome())
	}
}

// TestOperatingCountryPolicy_TenantRungWithdrawalIsTerminalSoNeedsNoAuthorization
// is the fence proving the constraint's scope is correctly narrow: absence
// at the TENANT rung is terminal, never inherited, so a tenant-rung
// withdrawal can only ever narrow and needs no authorization.
func TestOperatingCountryPolicy_TenantRungWithdrawalIsTerminalSoNeedsNoAuthorization(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "SV"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// NOTE: AsOf is captured FRESH before each resolve - see the identical
	// note in the brand-rung test above.
	newQuery := func() Query {
		return Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()}
	}
	if res := resolveNow(t, pool, f.tenantID, newQuery()); res.Outcome() != OutcomePermitted {
		t.Fatalf("sanity: expected permitted before withdrawal, got %s", res.Outcome())
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, Actor: testActor(f.staffActorID, "withdraw-tenant-no-ref"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("a blank-reference TENANT-rung withdrawal must succeed (absence there is terminal, never widening): %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, newQuery())
	if res.Outcome() != OutcomeNotConfigured {
		t.Fatalf("expected not_configured after the tenant-rung withdrawal, got %s", res.Outcome())
	}
	if res.Permitted() {
		t.Fatal("must never resolve permitted after a tenant-rung withdrawal")
	}
}

// TestLicenceCountryCeiling_WithdrawalIsFailClosedAndNeedsNoAuthorization is
// the same fence for the ceiling rung: a ceiling withdrawal is fail-closed
// (not_permitted_by_licence, never permitted), so it needs no
// authorization either.
func TestLicenceCountryCeiling_WithdrawalIsFailClosedAndNeedsNoAuthorization(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "GT"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// NOTE: AsOf is captured FRESH before each resolve - see the identical
	// note in the brand-rung test above.
	newQuery := func() Query {
		return Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()}
	}
	if res := resolveNow(t, pool, f.tenantID, newQuery()); res.Outcome() != OutcomePermitted {
		t.Fatalf("sanity: expected permitted before ceiling withdrawal, got %s", res.Outcome())
	}

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
			LicenceID: f.licenceID, CountryCode: cc, State: StateDisabled, Status: StatusWithdrawn,
			Actor: testActor(f.platformAdmin, "withdraw-ceiling-no-ref"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("a blank-reference ceiling withdrawal must succeed (fail-closed, never widening): %v", err)
	}

	var exp Explanation
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, newQuery())
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy: %v", err)
	}
	if exp.Outcome != OutcomeNotPermittedByLicence {
		t.Fatalf("expected not_permitted_by_licence after the ceiling withdrawal, got %s", exp.Outcome)
	}
	if exp.CeilingReason != CeilingReasonCeilingWithdrawn {
		t.Fatalf("expected CeilingReasonCeilingWithdrawn, got %s", exp.CeilingReason)
	}
}

// TestOperatingCountryPolicy_DisableWriteStillNeedsNoAuthorizationAtEveryScope
// is the non-regression guard: writing state='disabled', status='active'
// (the emergency kill-switch) with a blank reference still succeeds at
// every scope/table AMENDMENT-2 touches.
func TestOperatingCountryPolicy_DisableWriteStillNeedsNoAuthorizationAtEveryScope(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// licence_country_ceilings.
	ccCeiling := "PA"
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
			LicenceID: f.licenceID, CountryCode: ccCeiling, State: StateDisabled, Status: StatusActive,
			Actor: testActor(f.platformAdmin, "disable-ceiling-no-ref"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable ceiling with no reference: %v", err)
	}

	// tenant scope.
	ccTenant := "CR"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccTenant)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: ccTenant,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-tenant-no-ref"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable tenant with no reference: %v", err)
	}

	// brand scope.
	ccBrand := "BZ"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccBrand)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccBrand)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-brand-no-ref"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand with no reference: %v", err)
	}

	// operation scope.
	ccOp := "MX"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccOp)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccOp)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: ccOp,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-operation-no-ref"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable operation with no reference: %v", err)
	}
}

// TestOperatingCountryPolicy_InheritRungWithdrawalCheckIsAConstraintNotATrigger
// proves the mechanism is a CHECK constraint, not a trigger step: with
// every user trigger on operating_country_policies disabled, a direct raw
// INSERT of a brand-rung withdrawn row with authorization_reference = NULL
// is still rejected, with SQLSTATE 23514 naming the exact constraint.
func TestOperatingCountryPolicy_InheritRungWithdrawalCheckIsAConstraintNotATrigger(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "DO"

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE operating_country_policies DISABLE TRIGGER USER`)
		return err
	})
	if err != nil {
		t.Fatalf("disable user triggers: %v", err)
	}
	t.Cleanup(func() {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `ALTER TABLE operating_country_policies ENABLE TRIGGER USER`)
			return err
		})
		if err != nil {
			t.Fatalf("re-enable user triggers: %v", err)
		}
	})

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, brand_id, country_code,
				state, status, authorization_reference, reason_code, policy_version,
				created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'brand', $2, $3, 'disabled', 'withdrawn', NULL, 'raw-sql-constraint-test', $4, 'staff', $5)`,
			f.tenantID, f.brandID, cc, PolicyVersion, f.staffActorID)
		return err
	})
	if err == nil {
		t.Fatal("expected the raw INSERT to be rejected even with every user trigger disabled")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "23514" {
		t.Fatalf("expected SQLSTATE 23514, got %s: %v", pgErr.Code, err)
	}
	if pgErr.ConstraintName != "ocp_inherit_rung_withdrawal_requires_authorization" {
		t.Fatalf("expected constraint ocp_inherit_rung_withdrawal_requires_authorization, got %q", pgErr.ConstraintName)
	}
}

// TestOperatingMarketAudit_WideningWithdrawalIsDistinguishableFromAHarmlessOne
// unmarshals real JSON audit metadata for four writes, confirming
// rung_block_transition/widening_capable distinguish a widening
// inherit-rung withdrawal from a harmless one, a plain disable, and a
// tenant-rung withdrawal (which removes a block but can never widen).
func TestOperatingMarketAudit_WideningWithdrawalIsDistinguishableFromAHarmlessOne(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	readMeta := func(targetID string) map[string]any {
		t.Helper()
		var raw []byte
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND target_id = $2`,
				f.tenantID, targetID).Scan(&raw)
		})
		if err != nil {
			t.Fatalf("read audit metadata for %s: %v", targetID, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal audit metadata: %v", err)
		}
		return m
	}

	// (a) brand-rung withdrawal of an active disable -> removes_block,
	// widening_capable=true.
	ccA := "PE"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccA)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccA)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccA,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-brand-a"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand (a): %v", err)
	}
	var withdrawARec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		withdrawARec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccA,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "widen-a-ref",
			Actor: testActor(f.staffActorID, "withdraw-brand-disable-a"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw brand disable (a): %v", err)
	}
	metaA := readMeta(withdrawARec.ID.String())
	if metaA["rung_block_transition"] != "removes_block" {
		t.Fatalf("(a) expected rung_block_transition=removes_block, got %v", metaA["rung_block_transition"])
	}
	if metaA["widening_capable"] != true {
		t.Fatalf("(a) expected widening_capable=true, got %v", metaA["widening_capable"])
	}

	// (b) brand-rung withdrawal of an active ENABLE -> no_block_change,
	// widening_capable=false.
	ccB := "BO"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccB)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccB)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccB,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "enable-brand-b",
			Actor: testActor(f.staffActorID, "enable-brand-b"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable brand (b): %v", err)
	}
	var withdrawBRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		withdrawBRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccB,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "widen-b-ref",
			Actor: testActor(f.staffActorID, "withdraw-brand-enable-b"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw brand enable (b): %v", err)
	}
	metaB := readMeta(withdrawBRec.ID.String())
	if metaB["rung_block_transition"] != "no_block_change" {
		t.Fatalf("(b) expected rung_block_transition=no_block_change, got %v", metaB["rung_block_transition"])
	}
	if metaB["widening_capable"] != false {
		t.Fatalf("(b) expected widening_capable=false, got %v", metaB["widening_capable"])
	}

	// (c) plain disable write -> adds_block, widening_capable=false.
	ccC := "EC"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccC)
	var tenantDisableRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tenantDisableRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: ccC,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-tenant-c"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable tenant (c): %v", err)
	}
	metaC := readMeta(tenantDisableRec.ID.String())
	if metaC["rung_block_transition"] != "adds_block" {
		t.Fatalf("(c) expected rung_block_transition=adds_block, got %v", metaC["rung_block_transition"])
	}
	if metaC["widening_capable"] != false {
		t.Fatalf("(c) expected widening_capable=false, got %v", metaC["widening_capable"])
	}

	// (d) tenant-rung withdrawal of an active disable -> removes_block,
	// widening_capable=false: proves widening_capable is NOT a synonym for
	// removes_block. The tenant rung is terminal, not inherited, so
	// removing a block there can never widen.
	var withdrawDRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		withdrawDRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: ccC,
			State: StateDisabled, Status: StatusWithdrawn, Actor: testActor(f.staffActorID, "withdraw-tenant-disable-d"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw tenant disable (d): %v", err)
	}
	metaD := readMeta(withdrawDRec.ID.String())
	if metaD["rung_block_transition"] != "removes_block" {
		t.Fatalf("(d) expected rung_block_transition=removes_block, got %v", metaD["rung_block_transition"])
	}
	if metaD["widening_capable"] != false {
		t.Fatalf("(d) expected widening_capable=false (tenant rung is terminal, not inherit), got %v", metaD["widening_capable"])
	}
}

// TestOperatingMarketAudit_NoWideningEventIsFindableOnlyByStateEnabled is
// the binding auditor-rule test (audit.go's own doc comment): after one
// enable and one inherit-rung widening withdrawal, filtering on
// metadata->>'state' = 'enabled' finds only the enable, while filtering on
// metadata->>'widening_capable' = 'true' finds both.
func TestOperatingMarketAudit_NoWideningEventIsFindableOnlyByStateEnabled(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "GY"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	// The ONE enable.
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	// A supporting disable at the brand rung (state=disabled,
	// widening_capable=false - not counted in either target bucket below).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-brand"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand: %v", err)
	}

	// The ONE inherit-rung widening withdrawal: removes the brand-rung
	// block, letting the enabled tenant rung's permit through - the shape
	// that re-permits without ever writing the word "enabled".
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "widen-withdraw-ref",
			Actor: testActor(f.staffActorID, "withdraw-brand-disable"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw brand disable: %v", err)
	}

	var stateEnabledCount, wideningCapableCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND metadata->>'state' = 'enabled'`,
			f.tenantID).Scan(&stateEnabledCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND metadata->>'widening_capable' = 'true'`,
			f.tenantID).Scan(&wideningCapableCount)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if stateEnabledCount != 1 {
		t.Fatalf("THE WRONG FILTER: expected exactly 1 row for metadata->>'state' = 'enabled' (misses the inherit-rung widening withdrawal), got %d", stateEnabledCount)
	}
	if wideningCapableCount != 2 {
		t.Fatalf("THE RIGHT FILTER: expected exactly 2 rows for metadata->>'widening_capable' = 'true' (the enable AND the widening withdrawal), got %d", wideningCapableCount)
	}
}
