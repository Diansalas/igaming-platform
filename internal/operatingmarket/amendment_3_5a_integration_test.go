//go:build integration

// Tests for ADR 0045 §3.5-A AMENDMENT-1 (Stage 4I Phase E fix round): the
// write-time narrowing-enforcement step (migration 0076's new trigger
// step (4)) and the resolver's full-candidate-set, first-disabled-wins
// rewrite of STEP 4 (resolve.go). Test names are mandatory, taken
// verbatim from the architect's ruling.
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

// stringPtrEqual compares two *string by value (nil-safe) - a small local
// helper mirroring types.go's own unexported uuidPtrEqual, used only by
// this file's ChainStep.ProductCode assertions.
func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// operationPolicyRowCount/ operationPolicyAuditCount are small local
// helpers for the "zero rows/audit written on refusal" assertions below.
func operationPolicyRowCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, cc string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND country_code = $2 AND scope_kind = 'operation'`, tenantID, cc).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count operation policy rows: %v", err)
	}
	return count
}

func operationPolicyAuditCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, cc string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND metadata->>'country_code' = $2`,
			tenantID, cc).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return count
}

// toggleCeilingTrigger enables/disables
// operating_country_policies_enforce_ceiling directly - used ONLY to
// construct a raw-SQL write that bypasses the trigger entirely, proving
// the resolver alone (not the trigger) is what makes resolution correct.
func toggleCeilingTrigger(t *testing.T, pool *db.Pool, enable bool) {
	t.Helper()
	verb := "DISABLE"
	if enable {
		verb = "ENABLE"
	}
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE operating_country_policies %s TRIGGER operating_country_policies_ceiling`, verb))
		return err
	})
	if err != nil {
		t.Fatalf("%s ceiling trigger: %v", verb, err)
	}
}

// TestOperatingCountryPolicy_ProductSpecificDisableNarrowsEveryProductEnable
// replaces the deleted TestOperatingCountryPolicy_ProductSpecificRowBeatsEveryProductRow
// (which asserted the WRONG widening direction as correct - a
// product-specific ENABLE was allowed to beat a broader DISABLE). This
// version asserts the only legal direction: a product-specific DISABLE
// narrows a broader every-product ENABLE.
func TestOperatingCountryPolicy_ProductSpecificDisableNarrowsEveryProductEnable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "AR"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"

	// The every-product (product_code IS NULL) operation row: ENABLED.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "wagering-on-everywhere",
			Actor: testActor(f.staffActorID, "enable-every-product-wagering"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable every-product wagering: %v", err)
	}

	// The product-specific (casino) operation row: DISABLED. Narrowing -
	// always legal, regardless of the amendment.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-casino-wagering"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable casino-specific wagering: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC(),
	})
	if res.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation for the casino-specific query (a more-specific disable narrows the broader enable), got %s", res.Outcome())
	}

	sportsbook := "sportsbook"
	res2 := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &sportsbook, AsOf: time.Now().UTC(),
	})
	if res2.Outcome() != OutcomePermitted {
		t.Fatalf("expected permitted for the sportsbook query (no product-specific row applies; the broader row is enabled), got %s", res2.Outcome())
	}
}

// TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused
// is the write-time trigger step (4) test, via the public API only: three
// sub-cases where a more-specific ENABLE is refused under a broader
// in-force DISABLE, plus the two shapes that remain legal.
func TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	casino := "casino"

	writeDisabled := func(cc string, brandID *uuid.UUID, productCode *string, reason string) {
		t.Helper()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeOperation, TenantID: f.tenantID, BrandID: brandID, OperationCode: opWagering,
				ProductCode: productCode, CountryCode: cc, State: StateDisabled, Status: StatusActive,
				Actor: testActor(f.staffActorID, reason),
			})
			return err
		})
		if err != nil {
			t.Fatalf("write disabled row (%s): %v", reason, err)
		}
	}

	attemptEnable := func(cc string, brandID *uuid.UUID, productCode *string, reason string) error {
		t.Helper()
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeOperation, TenantID: f.tenantID, BrandID: brandID, OperationCode: opWagering,
				ProductCode: productCode, CountryCode: cc, State: StateEnabled, Status: StatusActive,
				AuthorizationReference: "attempt", Actor: testActor(f.staffActorID, reason),
			})
			return err
		})
	}

	t.Run("product-specific under every-product disable", func(t *testing.T) {
		cc := "TT"
		enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
		enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
		writeDisabled(cc, nil, nil, "disable-every-product")

		beforeRows, beforeAudits := operationPolicyRowCount(t, pool, f.tenantID, cc), operationPolicyAuditCount(t, pool, f.tenantID, cc)
		err := attemptEnable(cc, nil, &casino, "attempt-product-specific-enable")
		if !errors.Is(err, ErrCeilingExceeded) {
			t.Fatalf("expected ErrCeilingExceeded, got %v", err)
		}
		afterRows, afterAudits := operationPolicyRowCount(t, pool, f.tenantID, cc), operationPolicyAuditCount(t, pool, f.tenantID, cc)
		if afterRows != beforeRows || afterAudits != beforeAudits {
			t.Fatalf("expected zero rows/audit written on refusal, rows %d->%d audits %d->%d", beforeRows, afterRows, beforeAudits, afterAudits)
		}
	})

	t.Run("brand-specific under tenant-wide disable", func(t *testing.T) {
		cc := "JM"
		enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
		enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
		writeDisabled(cc, nil, nil, "disable-tenant-wide")

		beforeRows, beforeAudits := operationPolicyRowCount(t, pool, f.tenantID, cc), operationPolicyAuditCount(t, pool, f.tenantID, cc)
		err := attemptEnable(cc, &f.brandID, nil, "attempt-brand-specific-enable")
		if !errors.Is(err, ErrCeilingExceeded) {
			t.Fatalf("expected ErrCeilingExceeded, got %v", err)
		}
		afterRows, afterAudits := operationPolicyRowCount(t, pool, f.tenantID, cc), operationPolicyAuditCount(t, pool, f.tenantID, cc)
		if afterRows != beforeRows || afterAudits != beforeAudits {
			t.Fatalf("expected zero rows/audit written on refusal, rows %d->%d audits %d->%d", beforeRows, afterRows, beforeAudits, afterAudits)
		}
	})

	t.Run("brand+product under tenant-wide+every-product disable", func(t *testing.T) {
		cc := "BB"
		enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
		enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
		writeDisabled(cc, nil, nil, "disable-tenant-wide-every-product")

		beforeRows, beforeAudits := operationPolicyRowCount(t, pool, f.tenantID, cc), operationPolicyAuditCount(t, pool, f.tenantID, cc)
		err := attemptEnable(cc, &f.brandID, &casino, "attempt-brand-product-enable")
		if !errors.Is(err, ErrCeilingExceeded) {
			t.Fatalf("expected ErrCeilingExceeded, got %v", err)
		}
		afterRows, afterAudits := operationPolicyRowCount(t, pool, f.tenantID, cc), operationPolicyAuditCount(t, pool, f.tenantID, cc)
		if afterRows != beforeRows || afterAudits != beforeAudits {
			t.Fatalf("expected zero rows/audit written on refusal, rows %d->%d audits %d->%d", beforeRows, afterRows, beforeAudits, afterAudits)
		}
	})

	t.Run("still legal: broader enable while narrower disable stands", func(t *testing.T) {
		cc := "SR"
		enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
		enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
		writeDisabled(cc, nil, &casino, "disable-casino-first")

		if err := attemptEnable(cc, nil, nil, "enable-every-product-after-narrower-disable"); err != nil {
			t.Fatalf("expected the broader enable to succeed while a narrower disable stands, got %v", err)
		}

		sportsbook := "sportsbook"
		if res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &sportsbook, AsOf: time.Now().UTC()}); res.Outcome() != OutcomePermitted {
			t.Fatalf("expected permitted for sportsbook, got %s", res.Outcome())
		}
		if res := resolveNow(t, pool, f.tenantID, Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}); res.Outcome() != OutcomeDisabledByOperation {
			t.Fatalf("expected disabled_by_operation for casino (the narrower disable still governs it), got %s", res.Outcome())
		}
	})

	t.Run("still legal: same-key replace of a closed disable", func(t *testing.T) {
		cc := "GY"
		enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
		enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
		writeDisabled(cc, nil, nil, "disable-tenant-wide-initial")

		if err := attemptEnable(cc, nil, nil, "re-enable-same-key"); err != nil {
			t.Fatalf("expected an ordinary same-key enable-after-disable to succeed, got %v", err)
		}
	})
}

// TestOperatingCountryPolicy_BroaderDisableStillBlocksAfterNarrowerRowWithdrawn
// is THE regression test reproducing the exact reviewer sequence via the
// public API only: a broader disable must keep blocking after its
// narrower sibling (which was legally written to further narrow it) is
// withdrawn - the defect this whole fix round exists to close.
func TestOperatingCountryPolicy_BroaderDisableStillBlocksAfterNarrowerRowWithdrawn(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "SX"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"

	// Broad, every-product disable.
	broadRec := PolicyRecord{}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		broadRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-every-product-wagering"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable every-product wagering: %v", err)
	}

	// Legal narrowing: a product-specific (casino) disable beneath it.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-casino-wagering-narrower"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable casino-specific wagering (narrowing): %v", err)
	}

	// Withdraw the narrower casino-specific disable. NOT unconditionally
	// legal: the operation rung is an INHERIT rung (ADR 0045 §3.5-A
	// AMENDMENT-2), so this withdrawal removes this rung's own opinion and
	// requires a non-blank AuthorizationReference even though it withdraws
	// a DISABLE.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-casino-specific-disable-ref",
			Actor: testActor(f.staffActorID, "withdraw-casino-specific-disable"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw the narrower casino-specific disable: %v", err)
	}

	q := Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
	res := resolveNow(t, pool, f.tenantID, q)
	if res.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("THE REGRESSION: expected disabled_by_operation (the broader every-product disable must still block after its narrower sibling was withdrawn), got %s - the old algorithm wrongly resolved this permitted", res.Outcome())
	}
	if res.Permitted() {
		t.Fatal("must never be Permitted() here")
	}
	if res.Outcome() == OutcomeNotConfigured {
		t.Fatal("must never be not_configured here")
	}

	var exp Explanation
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, q)
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy: %v", err)
	}
	if exp.BlockingVersionID == nil || *exp.BlockingVersionID != broadRec.ID {
		t.Fatalf("expected BlockingVersionID to name the every-product row %s, got %v", broadRec.ID, exp.BlockingVersionID)
	}
}

// TestOperatingCountryPolicy_RawSQLWidenedRowCannotProducePermitted proves
// the resolver ALONE is correct even with the write-time trigger bypassed
// entirely (raw SQL) - the belt half of "belt and braces".
func TestOperatingCountryPolicy_RawSQLWidenedRowCannotProducePermitted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "AG"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"

	// Broad, every-product disable via the sanctioned path.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-every-product-wagering"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable every-product wagering: %v", err)
	}

	// Bypass the trigger entirely and insert a WIDENING row directly: a
	// casino-specific ENABLED, ACTIVE row, which the sanctioned write path
	// would refuse (§A.2's new trigger step 4).
	toggleCeilingTrigger(t, pool, false)
	t.Cleanup(func() { toggleCeilingTrigger(t, pool, true) })
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, operation_code, product_code, country_code,
				state, status, authorization_reference, reason_code, policy_version,
				created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'operation', $2, $3, $4, 'enabled', 'active', 'raw-sql-widen-bypass', 'raw-sql-widen-bypass-test', $5, 'staff', $6)`,
			f.tenantID, opWagering, casino, cc, PolicyVersion, f.staffActorID)
		return err
	})
	toggleCeilingTrigger(t, pool, true)
	if err != nil {
		t.Fatalf("raw-SQL insert of a widening row (trigger bypassed): %v", err)
	}

	q := Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
	res := resolveNow(t, pool, f.tenantID, q)
	if res.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation even with a raw-SQL-inserted widening row bypassing the trigger, got %s", res.Outcome())
	}

	// Now withdraw that raw row via the SANCTIONED path (withdrawal is
	// always legal - the trigger's early-return direction) and confirm the
	// outcome is STILL disabled_by_operation.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateDisabled, Status: StatusWithdrawn, AuthorizationReference: "withdraw-raw-widened-row-ref",
			Actor: testActor(f.staffActorID, "withdraw-raw-widened-row"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("withdraw the raw-SQL-inserted row via the sanctioned path: %v", err)
	}

	res2 := resolveNow(t, pool, f.tenantID, q)
	if res2.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation to remain after withdrawing the raw-SQL-inserted row, got %s", res2.Outcome())
	}
}

// TestOperatingCountryPolicy_BroadDisableAfterNarrowEnableBlocksRegardlessOfWriteOrder
// documents that the write-time trigger's deliberate incompleteness (it
// only checks UPWARD at write time) is safe: writing a narrow enable
// first (legal - no broader disable exists yet), then a broad disable
// (always legal), still blocks the narrow query - the resolver, not the
// trigger, is what makes this safe.
func TestOperatingCountryPolicy_BroadDisableAfterNarrowEnableBlocksRegardlessOfWriteOrder(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "DM"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"

	// Narrow enable FIRST - legal, since no broader disable exists yet.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateEnabled, Status: StatusActive, AuthorizationReference: "casino-ok",
			Actor: testActor(f.staffActorID, "enable-casino-first"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable casino-specific wagering first: %v", err)
	}

	// Broad disable SECOND - always legal (fail-closed direction).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-every-product-second"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable every-product wagering second: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC(),
	})
	if res.Outcome() != OutcomeDisabledByOperation {
		t.Fatalf("expected disabled_by_operation regardless of write order, got %s", res.Outcome())
	}
}

// TestResolveOperatingCountryPolicy_BlockingRowIsTheBroadestLiveDisable
// asserts that when two live candidates are both disabled, the BLOCKING
// row named by Explain() is the LEAST specific (broadest) one - not the
// most specific, and not simply the first one found.
func TestResolveOperatingCountryPolicy_BlockingRowIsTheBroadestLiveDisable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "BS"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"

	var broadRec, narrowRec PolicyRecord
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		broadRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-broad"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable broad (every-product): %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		narrowRec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: opWagering, ProductCode: &casino, CountryCode: cc,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-narrow"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable narrow (casino-specific): %v", err)
	}

	q := Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
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
	if *exp.BlockingVersionID != broadRec.ID {
		t.Fatalf("expected the BROADEST live-disabled row (%s) to be named as blocking, got %s (the narrower row %s must NOT be named)", broadRec.ID, *exp.BlockingVersionID, narrowRec.ID)
	}
}

// TestResolveOperatingCountryPolicy_DuplicateRankDetectedBeneathAHigherRankedCandidate
// fails against the OLD LIMIT-2 query (which only ever compares the top
// two candidates), proving the full-candidate-set fix is load-bearing: a
// legitimate higher-ranked candidate sits above a duplicate-rank PAIR at
// a lower rank, and the duplicate must still be caught.
func TestResolveOperatingCountryPolicy_DuplicateRankDetectedBeneathAHigherRankedCandidate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "GD"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	topProduct := "sportsbook"
	asOf := time.Now().UTC()

	toggleStampTimesTrigger(t, pool, false)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The legitimate, HIGHEST-ranked candidate: brand+product specific.
		if _, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
				state, status, reason_code, policy_version, effective_from, effective_to,
				created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'operation', $2, $3, $4, $5, 'disabled', 'active', 'dup-rank-top', $6, $7, NULL, 'staff', $8)`,
			f.tenantID, f.brandID, opWagering, topProduct, cc, PolicyVersion, asOf.Add(-3*time.Hour), f.staffActorID); err != nil {
			return err
		}
		// A duplicate-rank PAIR at a LOWER rank (brand-specific, every
		// product): row A, in force via a future effective_to (not
		// colliding with the partial unique index).
		if _, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
				state, status, reason_code, policy_version, effective_from, effective_to,
				created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'operation', $2, $3, NULL, $4, 'disabled', 'active', 'dup-rank-a', $5, $6, $7, 'staff', $8)`,
			f.tenantID, f.brandID, opWagering, cc, PolicyVersion, asOf.Add(-2*time.Hour), asOf.Add(2*time.Hour), f.staffActorID); err != nil {
			return err
		}
		// Row B, still open.
		_, err := tx.Exec(ctx, `
			INSERT INTO operating_country_policies (
				tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
				state, status, reason_code, policy_version, effective_from, effective_to,
				created_by_actor_type, created_by_actor_id
			) VALUES ($1, 'operation', $2, $3, NULL, $4, 'disabled', 'active', 'dup-rank-b', $5, $6, NULL, 'staff', $7)`,
			f.tenantID, f.brandID, opWagering, cc, PolicyVersion, asOf.Add(-1*time.Hour), f.staffActorID)
		return err
	})
	toggleStampTimesTrigger(t, pool, true)
	if err != nil {
		t.Fatalf("raw insert top-rank + duplicate-rank rows: %v", err)
	}

	res := resolveNow(t, pool, f.tenantID, Query{
		TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, ProductCode: &topProduct, AsOf: asOf,
	})
	if res.Outcome() != OutcomeConfigurationConflict {
		t.Fatalf("expected configuration_conflict (a duplicate-rank pair beneath a legitimate higher-ranked candidate must still be caught by a full-candidate-set scan), got %s", res.Outcome())
	}
}

// TestExplain_EmitsEveryApplicableOperationCandidate asserts the chain has
// one step per applicable operation-rung candidate (up to 4),
// specificity-descending, with BrandID/ProductCode populated and
// Inherited:true on the withdrawn one.
func TestExplain_EmitsEveryApplicableOperationCandidate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "LC"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	casino := "casino"
	create := func(brandID *uuid.UUID, productCode *string, state State, status Status, authRef, reason string) PolicyRecord {
		t.Helper()
		var rec PolicyRecord
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
				Scope: ScopeOperation, TenantID: f.tenantID, BrandID: brandID, OperationCode: opWagering,
				ProductCode: productCode, CountryCode: cc, State: state, Status: status,
				AuthorizationReference: authRef, Actor: testActor(f.staffActorID, reason),
			})
			return err
		})
		if err != nil {
			t.Fatalf("create operation policy (%s): %v", reason, err)
		}
		return rec
	}

	neitherRec := create(nil, nil, StateEnabled, StatusActive, "neither-ok", "enable-neither")
	productOnlyRec := create(nil, &casino, StateEnabled, StatusActive, "product-only-ok", "enable-product-only")
	create(&f.brandID, nil, StateEnabled, StatusActive, "brand-only-ok", "enable-brand-only") // superseded below
	brandProductRec := create(&f.brandID, &casino, StateEnabled, StatusActive, "brand-product-ok", "enable-brand-product")
	brandOnlyWithdrawnRec := create(&f.brandID, nil, StateDisabled, StatusWithdrawn, "withdraw-brand-only-ref", "withdraw-brand-only")

	q := Query{TenantID: f.tenantID, BrandID: &f.brandID, CountryCode: cc, OperationCode: opWagering, ProductCode: &casino, AsOf: time.Now().UTC()}
	var exp Explanation
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exp, err = ExplainOperatingCountryPolicy(ctx, tx, q)
		return err
	})
	if err != nil {
		t.Fatalf("ExplainOperatingCountryPolicy: %v", err)
	}
	if exp.Outcome != OutcomePermitted {
		t.Fatalf("expected permitted (the most specific live row is enabled), got %s", exp.Outcome)
	}

	var opSteps []ChainStep
	for _, step := range exp.Chain {
		if step.Scope == ScopeOperation {
			opSteps = append(opSteps, step)
		}
	}
	if len(opSteps) != 4 {
		t.Fatalf("expected exactly 4 operation-rung chain steps (one per applicable candidate), got %d", len(opSteps))
	}

	wantOrder := []struct {
		versionID uuid.UUID
		brandID   *uuid.UUID
		product   *string
		inherited bool
	}{
		{brandProductRec.ID, &f.brandID, &casino, false},
		{brandOnlyWithdrawnRec.ID, &f.brandID, nil, true},
		{productOnlyRec.ID, nil, &casino, false},
		{neitherRec.ID, nil, nil, false},
	}

	for i, want := range wantOrder {
		got := opSteps[i]
		if !got.Present {
			t.Fatalf("step %d: expected Present=true", i)
		}
		if got.VersionID == nil || *got.VersionID != want.versionID {
			t.Fatalf("step %d: expected version %s, got %v", i, want.versionID, got.VersionID)
		}
		if !uuidPtrEqual(got.BrandID, want.brandID) {
			t.Fatalf("step %d: expected BrandID %v, got %v", i, want.brandID, got.BrandID)
		}
		if !stringPtrEqual(got.ProductCode, want.product) {
			t.Fatalf("step %d: expected ProductCode %v, got %v", i, want.product, got.ProductCode)
		}
		if got.Inherited != want.inherited {
			t.Fatalf("step %d: expected Inherited=%v, got %v", i, want.inherited, got.Inherited)
		}
	}
}
