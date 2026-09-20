//go:build integration

// Tests for ADR 0045 §3.5-A AMENDMENT-3 (finding SEC-E-REV-2, "the bare
// close"): closing an in-force active+disabled brand- or operation-scope
// row (setting effective_to with NO successor insert in the same
// transaction) is functionally identical to an inherit-rung withdrawal -
// the same widening effect AMENDMENT-2 gated - and is now refused, AT
// COMMIT, by the new DEFERRED constraint trigger
// ocp_inherit_rung_close_requires_successor (migration 0076, amended in
// place a third time). Test names are mandatory, taken verbatim from the
// architect's ruling.
package operatingmarket

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// bareClosePolicyRow issues the raw SQL that every test in this file uses
// to reproduce "the bare close": an UPDATE that sets effective_to on the
// currently open row for a key, with NO successor insert in the same
// statement or transaction. This is deliberately NOT
// CreateOperatingCountryPolicyVersion - that sanctioned writer ALWAYS
// closes and inserts together (policy_admin.go's own AMENDMENT-3
// comment), so a bare close can only be reproduced via raw SQL, exactly
// as an attacker or a careless raw-SQL admin script would.
func bareClosePolicyRow(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, scopeKind, countryCode string, brandID *uuid.UUID, operationCode, productCode *string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE operating_country_policies SET effective_to = now()
		 WHERE tenant_id = $1 AND scope_kind = $2 AND country_code = $3
		   AND brand_id IS NOT DISTINCT FROM $4
		   AND operation_code IS NOT DISTINCT FROM $5
		   AND product_code IS NOT DISTINCT FROM $6
		   AND effective_to IS NULL`,
		tenantID, scopeKind, countryCode, brandID, operationCode, productCode)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// 1. Bare close of a brand-rung disable is rejected at commit.
func TestOperatingCountryPolicy_BareCloseOfBrandRungDisableIsRejectedAtCommit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "JM"
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

	newQuery := func() Query {
		return Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()}
	}
	if res := resolveNow(t, pool, f.tenantID, newQuery()); res.Outcome() != OutcomeDisabledByBrand {
		t.Fatalf("sanity: expected disabled_by_brand before the bare close, got %s", res.Outcome())
	}

	commitErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "brand", cc, &f.brandID, nil, nil)
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("expected exactly 1 row closed, got %d", affected)
		}
		return nil
	})
	if commitErr == nil {
		t.Fatal("expected the bare close to be rejected at commit")
	}
	if !errors.Is(ClassifyCommitError(commitErr), ErrPolicyCloseRequiresSuccessor) {
		t.Fatalf("expected ErrPolicyCloseRequiresSuccessor, got %v", commitErr)
	}

	// The whole transaction rolled back: the block persists, undisturbed.
	if res := resolveNow(t, pool, f.tenantID, newQuery()); res.Outcome() != OutcomeDisabledByBrand {
		t.Fatalf("expected disabled_by_brand to remain after the rejected bare close, got %s", res.Outcome())
	}
}

// 2. Bare close of an operation-rung disable is rejected at commit -
// covers both brand_id IS NULL/NOT NULL and both product_code IS
// NULL/non-NULL shapes (all four combinations).
func TestOperatingCountryPolicy_BareCloseOfOperationRungDisableIsRejectedAtCommit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	casino := "casino"

	cases := []struct {
		name        string
		cc          string
		brandID     *uuid.UUID
		productCode *string
	}{
		{"tenantwide-everyproduct", "GD", nil, nil},
		{"tenantwide-product", "LC", nil, &casino},
		{"brand-everyproduct", "VC", &f.brandID, nil},
		{"brand-product", "AG", &f.brandID, &casino},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// resolve()'s STEP 1/STEP 2 gate on the ceiling and tenant rung
			// UNCONDITIONALLY, before the operation rung is ever considered -
			// required so the sanity check below actually observes
			// disabled_by_operation rather than not_permitted_by_licence.
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

			newQuery := func() Query {
				return Query{TenantID: f.tenantID, BrandID: tc.brandID, CountryCode: tc.cc, OperationCode: opWagering, ProductCode: tc.productCode, AsOf: time.Now().UTC()}
			}
			if res := resolveNow(t, pool, f.tenantID, newQuery()); res.Outcome() != OutcomeDisabledByOperation {
				t.Fatalf("sanity: expected disabled_by_operation before the bare close (%s), got %s", tc.name, res.Outcome())
			}

			opCode := opWagering
			commitErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "operation", tc.cc, tc.brandID, &opCode, tc.productCode)
				if err != nil {
					return err
				}
				if affected != 1 {
					return fmt.Errorf("expected exactly 1 row closed (%s), got %d", tc.name, affected)
				}
				return nil
			})
			if commitErr == nil {
				t.Fatalf("expected the bare close to be rejected at commit (%s)", tc.name)
			}
			if !errors.Is(ClassifyCommitError(commitErr), ErrPolicyCloseRequiresSuccessor) {
				t.Fatalf("expected ErrPolicyCloseRequiresSuccessor (%s), got %v", tc.name, commitErr)
			}

			if res := resolveNow(t, pool, f.tenantID, newQuery()); res.Outcome() != OutcomeDisabledByOperation {
				t.Fatalf("expected disabled_by_operation to remain after the rejected bare close (%s), got %s", tc.name, res.Outcome())
			}
		})
	}
}

// 3. The close STATEMENT itself succeeds; only Commit() fails - this is
// the proof that DEFERRABLE INITIALLY DEFERRED is load-bearing, not
// decoration.
func TestOperatingCountryPolicy_CloseSuccessorCheckIsDeferredToCommitNotStatement(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "BB"
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

	ctx := context.Background()
	tx, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatalf("begin manually-managed tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
		t.Fatalf("set tenant context: %v", err)
	}

	affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "brand", cc, &f.brandID, nil, nil)
	if err != nil {
		t.Fatalf("the bare close STATEMENT itself must succeed (the check is DEFERRED to commit, not statement time): %v", err)
	}
	if affected != 1 {
		t.Fatalf("expected exactly 1 row closed, got %d", affected)
	}

	commitErr := tx.Commit(ctx)
	if commitErr == nil {
		t.Fatal("expected Commit() to fail: the deferred constraint trigger must fire at commit")
	}
	if !errors.Is(ClassifyCommitError(commitErr), ErrPolicyCloseRequiresSuccessor) {
		t.Fatalf("expected ErrPolicyCloseRequiresSuccessor from Commit(), got %v", commitErr)
	}
}

// 4. CRITICAL, run early: the sanctioned writer (close-then-insert in one
// transaction) still succeeds at every rung - tenant, brand, and all four
// operation shapes - including two SUCCESSIVE version writes for the same
// key (close R -> insert S1, THEN close S1 -> insert S2). Every version in
// this test stays state=disabled/status=active (the kill-switch shape,
// needing no authorization and no ceiling/tenant baseline) so the test
// isolates AMENDMENT-3's own effect.
//
// IMPLEMENTATION NOTE, DISCLOSED (found while implementing this exact
// test, not anticipated by the ruling): the second write-cycle (close S1
// -> insert S2) is issued in its OWN transaction, not packed into the
// SAME transaction as the first (close R -> insert S1), because doing so
// is impossible by construction - independently of AMENDMENT-3.
// operating_country_policies_stamp_times/enforce_append_only both stamp
// effective_from/effective_to with bare `now()`, which PostgreSQL freezes
// at TRANSACTION START (transaction_timestamp() semantics, not
// clock_timestamp()); two writes to the same key inside one transaction
// therefore always produce effective_from(S1) == effective_to(S1),
// tripping ocp_effective_to_after_from and surfacing as
// ErrConcurrentPolicyWrite even with zero concurrent writers - exactly
// CreateOperatingCountryPolicyVersion's own documented, PRE-EXISTING
// "call at most once per transaction per key" limitation (PHASE-D-CR-P3-5,
// shared verbatim with CreateLicenceCountryCeilingVersion), reproduced
// directly against this test before this adaptation was made. What this
// test actually proves - successive version-chain writes for the same key
// keep succeeding after AMENDMENT-3, and each individual close-then-insert
// cycle (the only shape AMENDMENT-3's trigger ever evaluates) still
// commits cleanly - is unaffected by which transaction boundary carries
// the second cycle.
func TestOperatingCountryPolicy_SanctionedCloseThenInsertStillSucceedsAtEveryRung(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	casino := "casino"

	shapes := []struct {
		name    string
		scope   Scope
		brandID *uuid.UUID
		opCode  string
		product *string
		cc      string
	}{
		{"tenant", ScopeTenant, nil, "", nil, "AI"},
		{"brand", ScopeBrand, &f.brandID, "", nil, "BS"},
		{"operation-tenantwide-everyproduct", ScopeOperation, nil, opWagering, nil, "DM"},
		{"operation-brand-everyproduct", ScopeOperation, &f.brandID, opWagering, nil, "GD"},
		{"operation-tenantwide-product", ScopeOperation, nil, opWagering, &casino, "KN"},
		{"operation-brand-product", ScopeOperation, &f.brandID, opWagering, &casino, "LC"},
	}

	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			// Create R (the very first version - nothing to close yet).
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
					Scope: s.scope, TenantID: f.tenantID, BrandID: s.brandID, OperationCode: s.opCode,
					ProductCode: s.product, CountryCode: s.cc, State: StateDisabled, Status: StatusActive,
					Actor: testActor(f.staffActorID, "create-r-"+s.name),
				})
				return err
			})
			if err != nil {
				t.Fatalf("create R (%s): %v", s.name, err)
			}

			// Cycle 1, its OWN transaction: close R -> insert S1.
			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
					Scope: s.scope, TenantID: f.tenantID, BrandID: s.brandID, OperationCode: s.opCode,
					ProductCode: s.product, CountryCode: s.cc, State: StateDisabled, Status: StatusActive,
					Actor: testActor(f.staffActorID, "create-s1-"+s.name),
				})
				return err
			})
			if err != nil {
				t.Fatalf("close R / insert S1 failed to commit (%s): %v", s.name, err)
			}

			// Cycle 2, a SEPARATE transaction (see the disclosed
			// implementation note above): close S1 -> insert S2.
			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
					Scope: s.scope, TenantID: f.tenantID, BrandID: s.brandID, OperationCode: s.opCode,
					ProductCode: s.product, CountryCode: s.cc, State: StateDisabled, Status: StatusActive,
					Actor: testActor(f.staffActorID, "create-s2-"+s.name),
				})
				return err
			})
			if err != nil {
				t.Fatalf("close S1 / insert S2 failed to commit (%s): %v", s.name, err)
			}

			var opArg any
			if s.opCode != "" {
				opArg = s.opCode
			}
			var openCount, totalCount int
			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if err := tx.QueryRow(ctx, `
					SELECT count(*) FROM operating_country_policies
					 WHERE tenant_id = $1 AND scope_kind = $2 AND country_code = $3
					   AND brand_id IS NOT DISTINCT FROM $4
					   AND operation_code IS NOT DISTINCT FROM $5
					   AND product_code IS NOT DISTINCT FROM $6
					   AND effective_to IS NULL`,
					f.tenantID, string(s.scope), s.cc, s.brandID, opArg, s.product).Scan(&openCount); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `
					SELECT count(*) FROM operating_country_policies
					 WHERE tenant_id = $1 AND scope_kind = $2 AND country_code = $3
					   AND brand_id IS NOT DISTINCT FROM $4
					   AND operation_code IS NOT DISTINCT FROM $5
					   AND product_code IS NOT DISTINCT FROM $6`,
					f.tenantID, string(s.scope), s.cc, s.brandID, opArg, s.product).Scan(&totalCount)
			})
			if err != nil {
				t.Fatalf("verify final state (%s): %v", s.name, err)
			}
			if openCount != 1 {
				t.Fatalf("expected exactly 1 open row for %s, got %d", s.name, openCount)
			}
			if totalCount != 3 {
				t.Fatalf("expected exactly 3 total rows (R, S1, S2) for %s, got %d", s.name, totalCount)
			}
		})
	}
}

// 5. One transaction: an authorized brand-rung withdrawal PLUS an
// unauthorized operation-rung bare close (a DIFFERENT key). The WHOLE
// transaction must roll back - including the authorized withdrawal - and
// audit_log must gain ZERO new rows from it.
func TestOperatingCountryPolicy_PiggybackAuthorizedWithdrawalPlusBareCloseRollsBackWholeTransaction(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	ccBrand := "DM"
	ccOp := "GD"

	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccBrand)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccBrand)
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccOp)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccOp)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-brand"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: ccOp,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable operation: %v", err)
	}

	var auditCountBefore int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created'`, f.tenantID).Scan(&auditCountBefore)
	})
	if err != nil {
		t.Fatalf("count audit rows before: %v", err)
	}

	opCode := opWagering
	commitErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The authorized, fully legal brand-rung withdrawal.
		if _, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "piggyback-withdraw-ref",
			Actor: testActor(f.staffActorID, "piggyback-withdraw-brand"),
		}); err != nil {
			return fmt.Errorf("authorized withdrawal: %w", err)
		}
		// The unauthorized operation-rung bare close, piggybacked in the
		// SAME transaction against a DIFFERENT key.
		affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "operation", ccOp, nil, &opCode, nil)
		if err != nil {
			return fmt.Errorf("bare close: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf("expected exactly 1 row closed, got %d", affected)
		}
		return nil
	})
	if commitErr == nil {
		t.Fatal("expected the whole transaction to be rejected at commit")
	}
	if !errors.Is(ClassifyCommitError(commitErr), ErrPolicyCloseRequiresSuccessor) {
		t.Fatalf("expected ErrPolicyCloseRequiresSuccessor, got %v", commitErr)
	}

	// The authorized withdrawal must ALSO have rolled back.
	resBrand := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if resBrand.Outcome() != OutcomeDisabledByBrand {
		t.Fatalf("expected disabled_by_brand to remain (the piggybacked withdrawal must roll back too), got %s", resBrand.Outcome())
	}

	// The operation-rung row must still be open, undisturbed.
	resOp := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccOp, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if resOp.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation to remain (the bare close must roll back), got %s", resOp.Outcome())
	}

	var auditCountAfter int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created'`, f.tenantID).Scan(&auditCountAfter)
	})
	if err != nil {
		t.Fatalf("count audit rows after: %v", err)
	}
	if auditCountAfter != auditCountBefore {
		t.Fatalf("expected ZERO new audit_log rows for the rolled-back transaction (before=%d, after=%d)", auditCountBefore, auditCountAfter)
	}
}

// 6. Bare-closing an ENABLED row, or an already-WITHDRAWN row, at an
// inherit rung must succeed - no over-blocking.
func TestOperatingCountryPolicy_BareCloseOfEnabledOrWithdrawnRowAtInheritRungIsPermitted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	t.Run("enabled row", func(t *testing.T) {
		cc := "SR"
		enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
		enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
				State: StateEnabled, Status: StatusActive, AuthorizationReference: "enable-brand-ref",
				Actor: testActor(f.staffActorID, "enable-brand"),
			})
			return err
		})
		if err != nil {
			t.Fatalf("enable brand: %v", err)
		}

		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "brand", cc, &f.brandID, nil, nil)
			if err != nil {
				return err
			}
			if affected != 1 {
				return fmt.Errorf("expected exactly 1 row closed, got %d", affected)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bare close of an ENABLED row must succeed (removes nothing that was blocking): %v", err)
		}
	})

	t.Run("already withdrawn row", func(t *testing.T) {
		cc := "SV"
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
		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
				State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-ref",
				Actor: testActor(f.staffActorID, "withdraw-brand"),
			})
			return err
		})
		if err != nil {
			t.Fatalf("withdraw brand disable: %v", err)
		}

		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "brand", cc, &f.brandID, nil, nil)
			if err != nil {
				return err
			}
			if affected != 1 {
				return fmt.Errorf("expected exactly 1 row closed, got %d", affected)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bare close of an ALREADY-WITHDRAWN row must succeed (removes nothing that was blocking): %v", err)
		}
	})
}

// 7. A TENANT-rung bare close succeeds (out of AMENDMENT-3's scope: absence
// there is terminal, never widening) - resolve() afterward must be
// not_configured/policy_expired, never permitted.
func TestOperatingCountryPolicy_TenantRungBareCloseNarrowsSoIsPermitted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "HT"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: f.tenantID, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-tenant"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable tenant: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "tenant", cc, nil, nil, nil)
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("expected exactly 1 row closed, got %d", affected)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a bare close at the TENANT rung must succeed (absence there is terminal, never widening): %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if res.Outcome() != OutcomeNotConfigured && res.Outcome() != OutcomePolicyExpired {
		t.Fatalf("expected not_configured or policy_expired after the tenant-rung bare close, got %s", res.Outcome())
	}
	if res.Permitted() {
		t.Fatal("must never resolve permitted after a tenant-rung bare close")
	}
}

// 8. A ceiling bare close succeeds (out of AMENDMENT-3's scope entirely -
// ceiling absence is fail-closed by its own resolve() logic) - resolve()
// afterward must be not_permitted_by_licence, never permitted.
func TestLicenceCountryCeiling_BareCloseIsFailClosedSoIsPermitted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "BS"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE licence_country_ceilings SET effective_to = now()
			 WHERE licence_id = $1 AND country_code = $2 AND effective_to IS NULL`,
			f.licenceID, cc)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected exactly 1 row closed, got %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a bare close on licence_country_ceilings must succeed (fail-closed, never widening, and out of AMENDMENT-3's scope): %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()})
	if res.Outcome() != OutcomeNotPermittedByLicence {
		t.Fatalf("expected not_permitted_by_licence after the ceiling bare close, got %s", res.Outcome())
	}
	if res.Permitted() {
		t.Fatal("must never resolve permitted after a ceiling bare close")
	}
}

// 9. Close + insert an UNAUTHORIZED withdrawn successor is still caught by
// AMENDMENT-2's own CHECK (23514, ocp_inherit_rung_withdrawal_requires_authorization)
// - the two amendments compose; AMENDMENT-3 does not weaken AMENDMENT-2.
func TestOperatingCountryPolicy_CloseWithUnauthorizedWithdrawnSuccessorIsStillRejectedByAmendment2(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "KN"
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

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "brand", cc, &f.brandID, nil, nil)
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("expected exactly 1 row closed, got %d", affected)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, brand_id, country_code, state, status, authorization_reference,
				reason_code, policy_version, created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'brand', $2, $3, 'disabled', 'withdrawn', NULL, 'compose-test', $4, 'staff', $5)`,
			f.tenantID, f.brandID, cc, PolicyVersion, f.staffActorID)
		return err
	})
	if err == nil {
		t.Fatal("expected the unauthorized withdrawn successor insert to be rejected by AMENDMENT-2's CHECK")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != pgCheckViolation {
		t.Fatalf("expected SQLSTATE 23514, got %s: %v", pgErr.Code, err)
	}
	if pgErr.ConstraintName != "ocp_inherit_rung_withdrawal_requires_authorization" {
		t.Fatalf("expected constraint ocp_inherit_rung_withdrawal_requires_authorization, got %q", pgErr.ConstraintName)
	}
}

// 10. Two transactions: the closer fails at commit; a concurrent peer
// inserting an open successor at the SAME key blocks on the partial
// unique index (because the closer's UPDATE is uncommitted) and can only
// proceed once the closer's transaction ends - at which point R is
// confirmed still open (the closer's failure was real, not silently
// committed), so the peer's insert is a genuine duplicate. Deterministic
// technique: uncommitted competing row + pg_stat_activity poll, NO sleep,
// NO WaitGroup-only synchronization.
func TestOperatingCountryPolicy_ConcurrentCloseCannotBeRescuedByAnotherTransactionsSuccessor(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "VC"

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "create-r"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("create R: %v", err)
	}

	ctx := context.Background()
	closerTx, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatalf("begin closer tx: %v", err)
	}
	defer func() { _ = closerTx.Rollback(ctx) }()
	if _, err := closerTx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
		t.Fatalf("set tenant context: %v", err)
	}
	affected, err := bareClosePolicyRow(ctx, closerTx, f.tenantID, "brand", cc, &f.brandID, nil, nil)
	if err != nil {
		t.Fatalf("closer's bare close statement: %v", err)
	}
	if affected != 1 {
		t.Fatalf("expected exactly 1 row closed, got %d", affected)
	}

	peerErrCh := make(chan error, 1)
	go func() {
		peerErrCh <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO operating_country_policies (
					tenant_id, scope_kind, brand_id, country_code, state, status, authorization_reference,
					reason_code, policy_version, created_by_actor_type, created_by_actor_id
				) VALUES ($1, 'brand', $2, $3, 'disabled', 'active', NULL, 'peer-insert', $4, 'staff', $5)`,
				f.tenantID, f.brandID, cc, PolicyVersion, f.staffActorID)
			return err
		})
	}()

	blocked := waitForBlockedStatement(t, pool, "INSERT INTO operating_country_policies")
	if !blocked {
		t.Fatal("timed out waiting for the peer's INSERT to block on the partial unique index against the closer's uncommitted update")
	}

	commitErr := closerTx.Commit(ctx)
	if commitErr == nil {
		t.Fatal("expected the closer's Commit to fail (no successor was inserted in its own transaction)")
	}
	if !errors.Is(ClassifyCommitError(commitErr), ErrPolicyCloseRequiresSuccessor) {
		t.Fatalf("expected ErrPolicyCloseRequiresSuccessor from the closer's failed commit, got %v", commitErr)
	}

	peerErr := <-peerErrCh
	if peerErr == nil {
		t.Fatal("expected the peer's insert to fail once unblocked: R is still open after the closer's failed commit rolled it back, so the peer's insert is a genuine duplicate")
	}
	var pgErr *pgconn.PgError
	if !errors.As(peerErr, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError from the peer insert, got %T: %v", peerErr, peerErr)
	}
	if pgErr.Code != pgUniqueViolation {
		t.Fatalf("expected SQLSTATE 23505 (unique violation) from the peer insert once R was confirmed still open, got %s: %v", pgErr.Code, peerErr)
	}

	var openCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id=$1 AND scope_kind='brand' AND brand_id=$2 AND country_code=$3 AND effective_to IS NULL`, f.tenantID, f.brandID, cc).Scan(&openCount)
	})
	if err != nil {
		t.Fatalf("count open rows: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("expected exactly 1 open row (R, undisturbed) after both the failed close and the failed peer insert, got %d", openCount)
	}
}

// 11. ON DELETE CASCADE on tenants still works after AMENDMENT-3 - the one
// thing this fix could plausibly break, since the new trigger is AFTER
// UPDATE, not DELETE, so a cascade DELETE never fires it, but this is
// confirmed directly rather than assumed. The fixture includes a
// brand-rung active+disabled OPEN row - the EXACT row shape
// (scope_kind IN ('brand','operation'), status='active', state='disabled',
// effective_to IS NULL) AMENDMENT-3's WHEN clause targets - not merely a
// tenant-rung row, so the test's name ("still cascades after AMENDMENT-3")
// actually exercises the row shape that motivates the amendment, even
// though a DELETE can never fire an AFTER UPDATE trigger regardless of
// row shape (a coverage/naming-accuracy fix, not a real risk).
func TestOperatingCountryPolicy_TenantDeletionStillCascadesAfterAmendment3(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "AW"
	ccBrand := "AZ"

	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	disableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccBrand)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccBrand)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-brand-for-cascade-fixture"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand (%s), the AMENDMENT-3-shaped fixture row: %v", ccBrand, err)
	}

	var countBefore int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1`, f.tenantID).Scan(&countBefore)
	})
	if err != nil {
		t.Fatalf("count before: %v", err)
	}
	if countBefore < 4 {
		t.Fatalf("sanity: expected at least 4 rows (tenant-rung: one closed, one open; brand-rung: one open, active+disabled) before deletion, got %d", countBefore)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, f.tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	// RLS on operating_country_policies has NO platform-wide read policy
	// at all (ADR 0045 §7.3), so a scopeless connection sees zero rows
	// regardless of whether the cascade actually ran - the down
	// migration's own established technique (temporarily disabling RLS
	// inside one transaction) is required for a genuine verification, not
	// a shortcut.
	var countAfter int
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE operating_country_policies DISABLE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1`, f.tenantID).Scan(&countAfter); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE operating_country_policies ENABLE ROW LEVEL SECURITY`)
		return err
	})
	if err != nil {
		t.Fatalf("count after (RLS-bypassed for verification only): %v", err)
	}
	if countAfter != 0 {
		t.Fatalf("expected ON DELETE CASCADE to remove every row for the deleted tenant, got %d remaining", countAfter)
	}
}

// 12. ClassifyCommitError maps the REAL commit error from a bare close to
// ErrPolicyCloseRequiresSuccessor.
func TestOperatingCountryPolicy_CommitErrorIsClassifiedAsPolicyCloseRequiresSuccessor(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "TT"
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

	if err := ClassifyCommitError(nil); err != nil {
		t.Fatalf("ClassifyCommitError(nil) must return nil, got %v", err)
	}

	commitErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		affected, err := bareClosePolicyRow(ctx, tx, f.tenantID, "brand", cc, &f.brandID, nil, nil)
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("expected exactly 1 row closed, got %d", affected)
		}
		return nil
	})
	if commitErr == nil {
		t.Fatal("expected the bare close to fail at commit")
	}
	classified := ClassifyCommitError(commitErr)
	if !errors.Is(classified, ErrPolicyCloseRequiresSuccessor) {
		t.Fatalf("expected ClassifyCommitError to map the real commit error to ErrPolicyCloseRequiresSuccessor, got %v (from %v)", classified, commitErr)
	}
}

// 13. Two DIFFERENT keys, each legally closed and given an authorized
// successor (an inherit-rung withdrawal - the sanctioned close-then-insert
// shape), inside ONE transaction. This proves the DEFERRED constraint
// trigger correctly handles TWO queued events at commit, not merely the
// single-event case every other test in this file exercises - the trigger
// fires once per row via `FOR EACH ROW`, and both must independently find
// their own successor at commit for the whole transaction to succeed.
func TestOperatingCountryPolicy_MultipleKeysCloseAndInsertInOneTransactionAllCommit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	ccBrand := "PY"
	ccOp := "PE"

	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccBrand)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccBrand)
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccOp)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccOp)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-brand"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand (%s): %v", ccBrand, err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: ccOp,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable operation (%s): %v", ccOp, err)
	}

	// Sanity: both keys are actually blocking before the withdrawals.
	if res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand, OperationCode: opWagering, AsOf: time.Now().UTC()}); res.Outcome() != OutcomeDisabledByBrand {
		t.Fatalf("sanity: expected disabled_by_brand before withdrawal, got %s", res.Outcome())
	}
	if res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccOp, OperationCode: opWagering, AsOf: time.Now().UTC()}); res.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("sanity: expected disabled_by_operation before withdrawal, got %s", res.Outcome())
	}

	// ONE transaction: two authorized inherit-rung withdrawals (close ->
	// insert an authorized withdrawn successor) at TWO DIFFERENT keys. Each
	// close queues its own deferred ocp_inherit_rung_close_requires_successor
	// event; both must find their own successor at commit.
	commitErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeBrand, TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "multi-key-withdraw-brand",
			Actor: testActor(f.staffActorID, "withdraw-brand"),
		}); err != nil {
			return fmt.Errorf("withdraw brand (%s): %w", ccBrand, err)
		}
		if _, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: ccOp,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "multi-key-withdraw-operation",
			Actor: testActor(f.staffActorID, "withdraw-operation"),
		}); err != nil {
			return fmt.Errorf("withdraw operation (%s): %w", ccOp, err)
		}
		return nil
	})
	if commitErr != nil {
		t.Fatalf("expected both legal close-then-insert cycles to commit cleanly, got: %v", ClassifyCommitError(commitErr))
	}

	// Both changes must be durable: the block at each key is gone.
	if res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: ccBrand, OperationCode: opWagering, AsOf: time.Now().UTC()}); res.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted at the brand key (%s) after the committed withdrawal, got %s", ccBrand, res.Outcome())
	}
	if res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: ccOp, OperationCode: opWagering, AsOf: time.Now().UTC()}); res.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted at the operation key (%s) after the committed withdrawal, got %s", ccOp, res.Outcome())
	}
}
