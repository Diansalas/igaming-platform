//go:build integration

// QA re-verification for the Stage 4I Phase E fix round (independent of
// the fix round's own authors). Two purposes:
//
//  1. TestQAReverify_Step4WithdrawnTopCandidateSourcesFromLiveLowerRankedRow
//     resolves the exact ambiguity flagged in this round's gating review:
//     qa_adversarial_integration_test.go's own
//     TestQAAdversarial_Step4WithdrawnTopCandidateInheritsWithNoFallbackToLiveRow
//     asserts only Outcome() == Permitted, which is true whether the
//     algorithm (a) genuinely inherits from the brand rung, ignoring the
//     operation rung entirely once its top candidate is withdrawn, or (b)
//     falls through to STEP 4.5 and sources from the lower-ranked LIVE
//     operation-rung row - both produce Permitted here, so the existing
//     test cannot tell them apart. This file uses
//     ExplainOperatingCountryPolicy's SourceScope/SourceVersionID (which
//     Result deliberately withholds) to name the actual source directly,
//     and adds a second, outcome-distinguishing variant (the lower-ranked
//     row DISABLED instead of ENABLED) that would fail if the
//     implementation actually inherited from the brand rung rather than
//     sourcing from the operation rung's live row.
//
//  2. Two additional adversarial rank/state combinations this round's own
//     test files do not exercise: (a) a withdrawn top-rank tombstone
//     coexisting with two DIFFERENT live-disabled candidates at the two
//     "middle" specificity families (brand-only, product-only), which
//     never tie in rank with each other but are both less specific than
//     top and more specific than "neither" - confirming the code's
//     documented tie-break (brand-present ranks above product-only) is
//     genuinely what decides the blocking row, not an accident of query
//     order; and (b) every operation-rung candidate present but ALL
//     withdrawn, confirming a full-tombstone candidate set behaves
//     identically to zero candidates (pure inherit).
package operatingmarket

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestQAReverify_Step4WithdrawnTopCandidateSourcesFromLiveLowerRankedRow
// directly names the source of the QA adversarial test's scenario via
// Explain(), then adds a distinguishing variant.
func TestQAReverify_Step4WithdrawnTopCandidateSourcesFromLiveLowerRankedRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "LC"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	brandRec, err := writeBrandPolicy(t, pool, f, cc, StateEnabled)
	if err != nil {
		t.Fatalf("enable brand: %v", err)
	}

	// Lower-ranked candidate: tenant-wide (brand_id IS NULL) operation
	// row, ENABLED, left ACTIVE (live).
	var tenantWideOpRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tenantWideOpRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "tenant-wide-op-ok",
			Actor: testActor(f.staffActorID, "enable-tenant-wide-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable tenant-wide operation row: %v", err)
	}

	// Higher-ranked candidate: brand-specific operation row, created
	// ENABLED then immediately WITHDRAWN.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "brand-specific-op-ok",
			Actor: testActor(f.staffActorID, "enable-brand-specific-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable brand-specific operation row: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-brand-specific-operation-ref",
			Actor: testActor(f.staffActorID, "withdraw-brand-specific-operation"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw brand-specific operation row: %v", err)
	}

	q := Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()}

	var exp Explanation
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, q)
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy: %v", err)
	}
	if exp.Outcome != OutcomePermitted {
		t.Fatalf("expected permitted, got %s", exp.Outcome)
	}

	// THE DIRECT CHECK: name the actual source. If the algorithm truly
	// "inherits with no fallback" (ignoring the operation rung entirely
	// once its top candidate is withdrawn), SourceScope must be
	// ScopeBrand and SourceVersionID must be brandRec.ID. If it instead
	// falls through to STEP 4.5 and sources from the live lower-ranked
	// operation row, SourceScope will be ScopeOperation and
	// SourceVersionID will be tenantWideOpRec.ID.
	t.Logf("QA-REVERIFY TRACE: exp.SourceScope=%s exp.SourceVersionID=%v brandRec.ID=%s tenantWideOpRec.ID=%s",
		exp.SourceScope, exp.SourceVersionID, brandRec.ID, tenantWideOpRec.ID)
	if exp.SourceScope != ScopeOperation {
		t.Fatalf("EXPECTED FINDING NOT REPRODUCED: expected SourceScope=operation (the algorithm sources from the live lower-ranked operation row via STEP 4.5, it does NOT purely inherit from the brand rung), got %s", exp.SourceScope)
	}
	if exp.SourceVersionID == nil || *exp.SourceVersionID != tenantWideOpRec.ID {
		t.Fatalf("expected SourceVersionID to name the live tenant-wide operation row %s, got %v", tenantWideOpRec.ID, exp.SourceVersionID)
	}

	// DISTINGUISHING VARIANT: same shape, but the lower-ranked candidate
	// is DISABLED instead of ENABLED. If the algorithm genuinely sources
	// from it (as just proven above via Explain), this must now block
	// (disabled_by_operation). If it were a true "inherit from brand,
	// ignore the operation rung" implementation, this would still resolve
	// permitted (wrongly) since the brand rung alone is enabled.
	cc2 := "GD"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc2)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc2)
	if _, err := writeBrandPolicy(t, pool, f, cc2, StateEnabled); err != nil {
		t.Fatalf("enable brand (variant): %v", err)
	}
	// Write order matters here: the write-time trigger (step (4), ADR
	// 0045 §3.5-A AMENDMENT-1) refuses any ENABLE that would widen an
	// already-in-force broader DISABLE. So the brand-specific ENABLE must
	// be written FIRST (legal - no broader disable exists yet, mirroring
	// TestOperatingCountryPolicy_BroadDisableAfterNarrowEnableBlocksRegardlessOfWriteOrder's
	// own required order), then the tenant-wide DISABLE (always legal -
	// the narrowing/disable direction), then the brand-specific row is
	// withdrawn (withdrawal is always legal regardless of direction).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering, CountryCode: cc2,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "brand-specific-op-ok-variant",
			Actor: testActor(f.staffActorID, "enable-brand-specific-operation-variant"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable brand-specific operation row (variant): %v", err)
	}
	var tenantWideDisabledRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tenantWideDisabledRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc2,
			State: StateDisabled, Status: StatusActive,
			Actor: testActor(f.staffActorID, "disable-tenant-wide-operation-variant"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable tenant-wide operation row (variant): %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering, CountryCode: cc2,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-brand-specific-operation-variant-ref",
			Actor: testActor(f.staffActorID, "withdraw-brand-specific-operation-variant"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw brand-specific operation row (variant): %v", err)
	}

	q2 := Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc2, OperationCode: opWagering, AsOf: time.Now().UTC()}
	var exp2 Explanation
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp2, err = ExplainOperatingCountryPolicy(ctx, tx, q2)
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy (variant): %v", err)
	}
	if exp2.Outcome != OutcomeDisabledByOperation {
		t.Fatalf("DISTINGUISHING VARIANT FAILED: expected disabled_by_operation (proving the algorithm genuinely sources/blocks from the live lower-ranked operation row rather than inheriting from the enabled brand rung), got %s", exp2.Outcome)
	}
	if exp2.BlockingVersionID == nil || *exp2.BlockingVersionID != tenantWideDisabledRec.ID {
		t.Fatalf("expected the blocking row to be the live tenant-wide disabled operation row %s, got %v", tenantWideDisabledRec.ID, exp2.BlockingVersionID)
	}
}

// TestQAReverify_TwoDistinctMiddleRankFamiliesBothDisabledBlockingIsDeterministic
// covers a rank/state combination none of this round's own test files
// exercise: brand-only and product-only are DIFFERENT specificity
// families (neither's rank ties with the other per sameRank's own
// definition), yet both sit strictly between "neither" (least specific)
// and "brand+product" (most specific). With BOTH middle rows disabled and
// live, and the top row enabled+live, the least-specific live-disabled
// row must be named as blocking - proving the documented tie-break
// (brand-present ranks above product-only in the query's own ORDER BY,
// so product-only is treated as LESS specific and is found first when
// scanning from the least-specific end) is what actually decides it, not
// an accident of iteration order over an otherwise-tied set.
func TestQAReverify_TwoDistinctMiddleRankFamiliesBothDisabledBlockingIsDeterministic(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "AI"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	if _, err := writeBrandPolicy(t, pool, f, cc, StateEnabled); err != nil {
		t.Fatalf("enable brand: %v", err)
	}

	casino := "casino"

	// Top rank (brand+product): ENABLED, live.
	var topRec PolicyRecord
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		topRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering,
			ProductCode: &casino, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "top-rank-ok", Actor: testActor(f.staffActorID, "enable-top-rank"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable top-rank (brand+product) row: %v", err)
	}

	// Middle rank A (brand-only, every product): DISABLED, live.
	var brandOnlyRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		brandOnlyRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, BrandID: &f.brandID, OperationCode: opWagering,
			CountryCode: cc, State: StateDisabled, Status: StatusActive,
			Actor: testActor(f.staffActorID, "disable-brand-only"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable brand-only row: %v", err)
	}

	// Middle rank B (product-only, every brand): DISABLED, live.
	var productOnlyRec PolicyRecord
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		productOnlyRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino,
			CountryCode: cc, State: StateDisabled, Status: StatusActive,
			Actor: testActor(f.staffActorID, "disable-product-only"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable product-only row: %v", err)
	}

	q := Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
	var exp Explanation
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, q)
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy: %v", err)
	}
	if exp.Outcome != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation, got %s", exp.Outcome)
	}
	if exp.BlockingVersionID == nil {
		t.Fatal("expected a non-nil BlockingVersionID")
	}
	// The code's documented tie-break: product-only is treated as the
	// LESS specific of the two middle families (ORDER BY brand-present
	// DESC ranks it below brand-only), so it must be named as blocking -
	// never the brand-only row, and never the enabled top-rank row.
	if *exp.BlockingVersionID != productOnlyRec.ID {
		t.Fatalf("expected the product-only row (%s, treated as less specific than brand-only per the query's own ORDER BY) to be named as blocking, got %s (brand-only=%s, top-rank=%s)",
			productOnlyRec.ID, *exp.BlockingVersionID, brandOnlyRec.ID, topRec.ID)
	}
	if exp.BlockingScope != ScopeOperation {
		t.Fatalf("expected BlockingScope=operation, got %s", exp.BlockingScope)
	}
}

// TestQAReverify_AllFourOperationRungCandidatesWithdrawnBehavesAsPureInherit
// covers the other uncovered combination: every one of the four possible
// operation-rung specificity slots has a row, but ALL FOUR are withdrawn
// tombstones. The full-candidate-set fix must treat this identically to
// zero candidates - pure inherit from the brand rung - never
// configuration_conflict, never any blocked/disabled outcome sourced from
// a tombstone.
func TestQAReverify_AllFourOperationRungCandidatesWithdrawnBehavesAsPureInherit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "MS"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	brandRec, err := writeBrandPolicy(t, pool, f, cc, StateEnabled)
	if err != nil {
		t.Fatalf("enable brand: %v", err)
	}

	casino := "casino"

	createThenWithdraw := func(brandID *uuid.UUID, productCode *string, reason string) {
		t.Helper()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeOperation, TenantID: f.tenantID, BrandID: brandID, OperationCode: opWagering,
				ProductCode: productCode, CountryCode: cc, State: StateEnabled, Status: StatusActive,
				AuthorizationReference: reason + "-ok", Actor: testActor(f.staffActorID, reason),
			})
			return err
		})
		if err != nil {
			t.Fatalf("create %s: %v", reason, err)
		}
		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeOperation, TenantID: f.tenantID, BrandID: brandID, OperationCode: opWagering,
				ProductCode: productCode, CountryCode: cc, State: StateDisabled, Status: StatusWithdrawn,
				AuthorizationReference: "withdraw-" + reason + "-ref",
				Actor:                  testActor(f.staffActorID, "withdraw-"+reason),
			})
			return err
		})
		if err != nil {
			t.Fatalf("withdraw %s: %v", reason, err)
		}
	}

	createThenWithdraw(&f.brandID, &casino, "brand-product")
	createThenWithdraw(&f.brandID, nil, "brand-only")
	createThenWithdraw(nil, &casino, "product-only")
	createThenWithdraw(nil, nil, "neither")

	q := Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
	var exp Explanation
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, q)
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy: %v", err)
	}
	if exp.Outcome != OutcomePermitted {
		t.Fatalf("expected permitted (all four operation-rung candidates are tombstones; must behave as pure inherit from the enabled brand rung), got %s", exp.Outcome)
	}
	if exp.SourceScope != ScopeBrand {
		t.Fatalf("expected SourceScope=brand (pure inherit, no operation-rung candidate is live), got %s", exp.SourceScope)
	}
	if exp.SourceVersionID == nil || *exp.SourceVersionID != brandRec.ID {
		t.Fatalf("expected SourceVersionID to name the brand rung row %s, got %v", brandRec.ID, exp.SourceVersionID)
	}

	var opSteps int
	for _, step := range exp.Chain {
		if step.Scope == ScopeOperation {
			opSteps++
			if !step.Inherited {
				t.Fatalf("expected every operation-rung chain step to be Inherited=true (all are withdrawn tombstones), got one false")
			}
		}
	}
	if opSteps != 4 {
		t.Fatalf("expected exactly 4 operation-rung chain steps (one per withdrawn candidate), got %d", opSteps)
	}
}
