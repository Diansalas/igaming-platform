//go:build integration

// Stage 4H-B0-R6 Workstream D, Part 3: decimal-exponent awareness, the
// P1-2 interaction gap ADR 0031 §32(d) found and §35 closes.
//
// Validated against assets at exponents 0, 2, 6, 8 and 18, with the
// `assets` table as the ONLY source of exponent truth - internal/risk
// hard-codes no decimal count anywhere, and these tests would fail if it
// did (an exponent-2 assumption cannot satisfy the exponent-0 or
// exponent-18 cases).
package risk

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// pgCheckViolation is PostgreSQL's CHECK-constraint violation SQLSTATE,
// used to prove migration 0046's denomination constraint is enforced by
// the DATABASE and not only by CreateRule's own validation.
const pgCheckViolation = "23514"

// The registry ships EUR/USD/GBP/BRL/MXN at exponent 2, USDT at 6 and BTC
// at 8 (migration 0003) - nothing at 0 or 18, so those two are created as
// synthetic test assets with fixed codes (created once, reused by every
// later run: `assets` is append-only and dual-controlled, so a test must
// not mint a fresh code per run).
const (
	assetExp0  = "RSKEXP0"
	assetExp2  = "EUR"
	assetExp6  = "USDT"
	assetExp8  = "BTC"
	assetExp18 = "RSKEXP18"
)

// ensureAssetAtExponent guarantees an `assets` row for code at exponent.
//
// Creating an asset became a dual-controlled platform operation in
// migration 0044 (ADR 0037 §C.5.3, owned by the asset-registry
// specialist), so this helper walks that four-eyes path in raw SQL:
// requester principal -> pending change request -> approval by a DIFFERENT
// platform principal -> insert. It deliberately does NOT import
// internal/assetregistry (another specialist owns that package and is
// building it in parallel this stage) - if that path changes, this helper
// fails loudly rather than silently diverging. Risk itself reads only
// assets.decimal_exponent and never creates an asset in production code.
func ensureAssetAtExponent(t *testing.T, pool *db.Pool, code, assetType string, exponent int16) {
	// migration 0044 requires a network for a crypto asset; the approved
	// payload must name the same one the insert uses.
	network := ""
	if assetType == "crypto" {
		network = "risk-test-network"
	}
	t.Helper()
	var existing *int16
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT decimal_exponent FROM assets WHERE code = $1`, code).Scan(&existing)
	})
	if err == nil && existing != nil {
		if *existing != exponent {
			t.Fatalf("test asset %s exists at exponent %d, expected %d", code, *existing, exponent)
		}
		return
	}

	requester, approver := uuid.New(), uuid.New()
	requestID := uuid.New()
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []uuid.UUID{requester, approver} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO staff_users (id, tenant_id, email, password_hash, role)
				 VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
				id, id.String()+"@platform.example.com"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed platform principals: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, requester.String()); err != nil {
			return err
		}
		// A previous, abandoned run may have left a pending request for
		// this code (assets and their change requests are append-only);
		// the apply-time consume picks the OLDEST pending request, so a
		// stale one would be matched against this insert and rejected.
		if _, err := tx.Exec(ctx,
			`UPDATE asset_change_requests SET state = 'cancelled' WHERE asset_code = $1 AND state = 'pending'`, code); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO asset_change_requests (id, operation, asset_code, payload, reason_code, requested_by_principal_id)
			 VALUES ($1, 'create', $2, $3::jsonb, 'risk exponent regression test', $4)`,
			requestID, code, fmt.Sprintf(`{"asset_type":%q,"decimal_exponent":%d,"network":%q}`, assetType, exponent, network), requester)
		return err
	})
	if err != nil {
		t.Fatalf("file asset change request for %s: %v", code, err)
	}
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, approver.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO asset_change_approvals (request_id, approver_principal_id, decision) VALUES ($1, $2, 'approve')`,
			requestID, approver)
		return err
	})
	if err != nil {
		t.Fatalf("approve asset change request for %s: %v", code, err)
	}
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, requester.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO assets (code, asset_type, decimal_exponent, display_name, network) VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
			code, assetType, exponent, "Risk exponent-"+fmt.Sprint(exponent)+" test asset", network)
		return err
	})
	if err != nil {
		t.Fatalf("create test asset %s: %v", code, err)
	}
}

// allTestExponents is the full matrix this workstream is required to
// validate: 0, 2, 6, 8, 18.
func allTestExponents(t *testing.T, pool *db.Pool) []struct {
	asset    string
	exponent int16
} {
	t.Helper()
	ensureAssetAtExponent(t, pool, assetExp0, "fiat", 0)
	ensureAssetAtExponent(t, pool, assetExp18, "crypto", 18)
	return []struct {
		asset    string
		exponent int16
	}{
		{assetExp0, 0},
		{assetExp2, 2},
		{assetExp6, 6},
		{assetExp8, 8},
		{assetExp18, 18},
	}
}

// TestEvaluate_WildcardAssetRuleAppliesAtItsOwnExponent proves an
// asset-agnostic ("applies regardless of asset") amount rule is compared
// in minor units of exactly the exponent it declares - at every one of
// the five exponents, with no decimal count assumed anywhere.
func TestEvaluate_WildcardAssetRuleAppliesAtItsOwnExponent(t *testing.T) {
	pool := testPool(t)
	for _, c := range allTestExponents(t, pool) {
		t.Run(fmt.Sprintf("exponent_%d_%s", c.exponent, c.asset), func(t *testing.T) {
			f := seedFixture(t, pool)
			createTestRule(t, pool, &f.tenantID, CreateRuleParams{
				Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
				Threshold: 100, ThresholdExponent: ThresholdExponentOf(c.exponent), RuleKind: RuleHardLimit,
			})

			req := baseRequest(f)
			req.AssetCode = c.asset

			req.Amount = 101
			decision, err := evaluateWithTenant(t, pool, req)
			if err != nil {
				t.Fatalf("evaluate over-threshold amount: %v", err)
			}
			if decision.Outcome != OutcomeDeny {
				t.Fatalf("expected 101 > 100 minor units at exponent %d to DENY, got %+v", c.exponent, decision)
			}

			req.Amount = 100
			decision, err = evaluateWithTenant(t, pool, req)
			if err != nil {
				t.Fatalf("evaluate at-threshold amount: %v", err)
			}
			if decision.Outcome != OutcomeAllow {
				t.Fatalf("expected exactly 100 minor units at exponent %d to be allowed, got %+v", c.exponent, decision)
			}
		})
	}
}

// TestEvaluate_WildcardAssetRuleFailsClosedAtADifferentExponent is the
// direct regression for ADR 0031 §32(d): before this stage a threshold
// authored for one exponent was silently re-interpreted in another
// asset's minor units - effectively unlimited in one direction, always
// denying in the other. It is now refused, and specifically NOT rescaled
// (rescaling would assert major-unit equivalence across assets, the
// design ledger-finance already rejected for withdrawal policies).
func TestEvaluate_WildcardAssetRuleFailsClosedAtADifferentExponent(t *testing.T) {
	pool := testPool(t)
	matrix := allTestExponents(t, pool)
	for _, ruleAt := range matrix {
		for _, requestIn := range matrix {
			if ruleAt.exponent == requestIn.exponent {
				continue
			}
			t.Run(fmt.Sprintf("rule_exp_%d_request_%s", ruleAt.exponent, requestIn.asset), func(t *testing.T) {
				f := seedFixture(t, pool)
				createTestRule(t, pool, &f.tenantID, CreateRuleParams{
					Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
					Threshold: 100, ThresholdExponent: ThresholdExponentOf(ruleAt.exponent), RuleKind: RuleHardLimit,
				})
				req := baseRequest(f)
				req.AssetCode = requestIn.asset
				req.Amount = 10_000_000
				decision, err := evaluateWithTenant(t, pool, req)
				if err == nil {
					t.Fatalf("expected a threshold denominated at exponent %d to fail closed against a %s (exponent %d) request, got %+v",
						ruleAt.exponent, requestIn.asset, requestIn.exponent, decision)
				}
				assertErrorIs(t, err, ErrThresholdDenominationMismatch)
				if decision.Outcome == OutcomeAllow {
					t.Fatal("a denomination mismatch must never resolve to ALLOW")
				}
			})
		}
	}
}

// TestEvaluate_AssetScopedRuleResolvesItsExponentFromTheRegistry proves
// the other legitimate denomination shape: an asset-scoped rule declares
// no exponent at all, and the comparison is in that asset's own minor
// units, read from `assets` (never copied onto the rule row, never
// assumed).
func TestEvaluate_AssetScopedRuleResolvesItsExponentFromTheRegistry(t *testing.T) {
	pool := testPool(t)
	for _, c := range allTestExponents(t, pool) {
		t.Run(fmt.Sprintf("exponent_%d_%s", c.exponent, c.asset), func(t *testing.T) {
			f := seedFixture(t, pool)
			createTestRule(t, pool, &f.tenantID, CreateRuleParams{
				AssetCode: c.asset, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount,
				TimeWindow: WindowTransaction, Threshold: 100, RuleKind: RuleHardLimit,
			})

			req := baseRequest(f)
			req.AssetCode = c.asset
			req.Amount = 101
			decision, err := evaluateWithTenant(t, pool, req)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if decision.Outcome != OutcomeDeny {
				t.Fatalf("expected an asset-scoped rule to deny 101 > 100 in %s, got %+v", c.asset, decision)
			}
			if len(decision.MatchedRules) != 1 {
				t.Fatalf("expected exactly one matched rule for explainability, got %+v", decision.MatchedRules)
			}

			// A different asset simply does not match this rule (a
			// non-match, NOT an error) - the asset scope is doing its
			// ordinary matching job, and there is no other rule to apply.
			other := assetExp2
			if c.asset == assetExp2 {
				other = assetExp8
			}
			req.AssetCode = other
			req.Amount = 10_000_000
			decision, err = evaluateWithTenant(t, pool, req)
			if err != nil {
				t.Fatalf("evaluate a different asset: %v", err)
			}
			if decision.Outcome != OutcomeAllow {
				t.Fatalf("expected an asset-scoped rule to not apply to %s at all, got %+v", other, decision)
			}
		})
	}
}

// TestRule_ThresholdExponentResolution covers the pure denomination
// resolver directly, including the ONE case the database can no longer
// produce: a legacy (pre-migration-0046) asset-agnostic amount rule with
// no declared denomination. Migration 0046's CHECK is NOT VALID
// precisely so such rows are grandfathered as data rather than guessed
// at, and the evaluator refuses them.
func TestRule_ThresholdExponentResolution(t *testing.T) {
	legacy := Rule{ID: uuid.New(), LimitKind: LimitMaxAmount}
	if _, err := legacy.thresholdExponent(2); err == nil {
		t.Fatal("expected a legacy undeclared-denomination rule to fail closed")
	} else {
		assertErrorIs(t, err, ErrMissingThresholdDenomination)
	}

	declared := Rule{ID: uuid.New(), LimitKind: LimitMaxAmount, ThresholdExponent: ThresholdExponentOf(8)}
	if exp, err := declared.thresholdExponent(8); err != nil || exp != 8 {
		t.Fatalf("expected a matching declared exponent to resolve to 8, got %d, %v", exp, err)
	}
	if _, err := declared.thresholdExponent(18); err == nil {
		t.Fatal("expected a declared exponent of 8 to fail closed against an exponent-18 request")
	} else {
		assertErrorIs(t, err, ErrThresholdDenominationMismatch)
	}

	scoped := Rule{ID: uuid.New(), LimitKind: LimitMaxAmount, AssetCode: "BTC"}
	if exp, err := scoped.thresholdExponent(8); err != nil || exp != 8 {
		t.Fatalf("expected an asset-scoped rule to adopt the request asset's exponent, got %d, %v", exp, err)
	}

	// Defense in depth: a row that (only via direct SQL, pre-0046) has
	// BOTH an asset scope and a conflicting declared exponent is refused
	// rather than one side being silently preferred.
	inconsistent := Rule{ID: uuid.New(), LimitKind: LimitMaxAmount, AssetCode: "BTC", ThresholdExponent: ThresholdExponentOf(2)}
	if _, err := inconsistent.thresholdExponent(8); err == nil {
		t.Fatal("expected an asset-scoped rule with a conflicting declared exponent to fail closed")
	} else {
		assertErrorIs(t, err, ErrThresholdDenominationMismatch)
	}
}

// TestCreateRule_DenominationIsRequiredExactlyOnce is the authoring-side
// half: an ambiguous amount rule can no longer be created at all (the
// stronger form of fail-closed - a rule the engine cannot interpret must
// never be storable, ADR 0031 §4's own standing principle).
func TestCreateRule_DenominationIsRequiredExactlyOnce(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	cases := []struct {
		name   string
		params CreateRuleParams
	}{
		{"neither asset_code nor threshold_exponent", CreateRuleParams{
			Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 100,
		}},
		{"both asset_code and threshold_exponent", CreateRuleParams{
			Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 100,
			AssetCode: "EUR", ThresholdExponent: ThresholdExponentOf(2),
		}},
		{"exponent out of range", CreateRuleParams{
			Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 100,
			ThresholdExponent: ThresholdExponentOf(19),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			params := c.params
			params.TenantID = &f.tenantID
			params.CreatedByActorType = "staff"
			params.CreatedByActorID = uuid.New()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateRule(ctx, tx, params)
				return err
			})
			if err == nil {
				t.Fatal("expected an ambiguous threshold denomination to be refused at creation")
			}
			assertErrorIs(t, err, ErrInvalidInput)
		})
	}

	// And the database refuses it independently of the Go validation
	// (CLAUDE.md: enforced by the database, not by discipline in
	// application code).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, operation, limit_kind, time_window, threshold, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, 'casino_bet', 'max_amount', 'transaction', 100, 'staff', gen_random_uuid())`,
			f.tenantID)
		return err
	})
	assertPgError(t, err, pgCheckViolation)
}

// TestEvaluate_ThresholdBeyondInt64FailsClosed pins the disclosed
// int64 limitation (ADR 0031 §35): risk_rules.threshold is NUMERIC(38,0),
// Go compares in int64 because every amount it is compared against is
// int64. A threshold beyond int64 - only writable by direct SQL, never
// through CreateRule or the HTTP API - makes the row unscannable, and
// that must fail CLOSED rather than wrap, truncate, or be skipped.
func TestEvaluate_ThresholdBeyondInt64FailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	ensureAssetAtExponent(t, pool, assetExp18, "crypto", 18)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, operation, limit_kind, time_window, threshold, threshold_exponent,
			     rule_kind, action, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, 'casino_bet', 'max_amount', 'transaction', 99999999999999999999, 18,
			     'hard_limit', 'deny', 'staff', gen_random_uuid())`,
			f.tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("insert an over-int64 threshold via direct SQL: %v", err)
	}

	req := baseRequest(f)
	req.AssetCode = assetExp18
	decision, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatalf("expected an over-int64 threshold to fail closed, got %+v", decision)
	}
	if decision.Outcome == OutcomeAllow {
		t.Fatal("an unscannable threshold must never resolve to ALLOW")
	}
}
