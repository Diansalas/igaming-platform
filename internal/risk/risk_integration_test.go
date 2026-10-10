//go:build integration

// Real-PostgreSQL tests for risk_rules' RLS, immutability, and
// Evaluate's precedence/conflict/fail-closed behavior - migration 0041.
// Follows internal/kyc/kyc_integration_test.go's own fixture conventions.
package risk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fixture struct {
	tenantID uuid.UUID
	brandID  uuid.UUID
	playerID uuid.UUID
}

func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	var f fixture
	f.tenantID = uuid.New()
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence', 'active')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.brandID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Test Brand', 'active')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		f.playerID = uuid.New()
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerID, f.tenantID, f.brandID, personID, f.playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed brand/player: %v", err)
	}
	return f
}

const pgRLSViolation = "42501"
const pgForeignKeyViolation = "23503"

func assertPgError(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("expected a Postgres error %s, got: %v", code, err)
	}
}

// --- RLS: a tenant sees its own rules plus every platform-wide rule, and
// cannot forge a row for a different tenant ---

func TestRiskRules_TenantSeesOwnAndPlatformWideRules(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)

	// Tenant A's own rule.
	var tenantARuleID uuid.UUID
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := CreateRule(ctx, tx, CreateRuleParams{
			TenantID: &fA.tenantID, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
			Threshold: 1000, ThresholdExponent: ThresholdExponentOf(2), CreatedByActorType: "staff", CreatedByActorID: uuid.New(),
		})
		tenantARuleID = r.ID
		return err
	})
	if err != nil {
		t.Fatalf("create tenant A rule: %v", err)
	}

	// Platform-wide rule. Disabled via t.Cleanup (Stage 4G-FINAL hardening
	// fix - see internal/risk/evaluator_test.go's sibling comment): left
	// active, a platform-wide HARD_LIMIT accumulates forever across every
	// run of this suite (risk_rules is append-only, never deletable) and
	// can eventually deny a legitimate future test's own casino_bet once
	// enough of these threshold=5000 rows exist for evaluator.go's
	// aggregation to reconsider - confirmed live: this exact row had
	// silently accumulated many times over before this fix.
	var platformRuleID uuid.UUID
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		r, err := CreateRule(ctx, tx, CreateRuleParams{
			Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
			Threshold: 5000, ThresholdExponent: ThresholdExponentOf(2), RuleKind: RuleHardLimit, CreatedByActorType: "staff", CreatedByActorID: uuid.New(),
		})
		platformRuleID = r.ID
		return err
	})
	if err != nil {
		t.Fatalf("create platform-wide rule: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := DisableRule(ctx, tx, DisableRuleParams{RuleID: platformRuleID, ActorType: "staff", ActorID: uuid.New()})
			return err
		})
	})

	// Tenant B sees the platform-wide rule, but NEVER tenant A's own -
	// existence checks by ID rather than an exact count, since risk_rules
	// is a shared, persistent table across this whole test run and other
	// tests' own platform-wide rows remain visible too (a genuine,
	// documented property of a platform-wide policy, not test pollution
	// to hide behind an exact count).
	containsRuleID := func(rules []Rule, id uuid.UUID) bool {
		for _, r := range rules {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rules, err := ListRulesForOperation(ctx, tx, OperationCasinoBet)
		if err != nil {
			return err
		}
		if !containsRuleID(rules, platformRuleID) {
			t.Fatal("expected tenant B to see the platform-wide rule")
		}
		if containsRuleID(rules, tenantARuleID) {
			t.Fatal("expected tenant B to NEVER see tenant A's own tenant-scoped rule")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}

	// Tenant A sees BOTH its own rule and the platform-wide one.
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rules, err := ListRulesForOperation(ctx, tx, OperationCasinoBet)
		if err != nil {
			return err
		}
		if !containsRuleID(rules, platformRuleID) || !containsRuleID(rules, tenantARuleID) {
			t.Fatal("expected tenant A to see both its own rule and the platform-wide one")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("list as tenant A: %v", err)
	}
}

func TestRiskRules_CrossTenantForgedInsertDenied(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, operation, limit_kind, time_window, threshold, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, 'casino_bet', 'max_amount', 'transaction', 100, 'staff', gen_random_uuid())`,
			fA.tenantID,
		)
		return err
	})
	assertPgError(t, err, pgRLSViolation)

	// A tenant-scoped connection cannot smuggle in a platform-wide row
	// either (tenant_id NULL) - migration 0041's tenant_and_platform_write
	// policy requires app.tenant_id ITSELF be unset for that branch.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, operation, limit_kind, time_window, threshold, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), NULL, 'casino_bet', 'max_amount', 'transaction', 100, 'staff', gen_random_uuid())`,
		)
		return err
	})
	assertPgError(t, err, pgRLSViolation)
}

// TestRiskRules_PlayerScopeConnectionCannotReadOrWrite is a regression
// test for a live-database-confirmed P1 finding from PostgreSQL/RLS
// specialist review: migration 0041's original policies omitted the
// "app.player_account_id IS NULL" guard every sibling dual-scope table
// (player_restrictions, sessions) carries, so a db.WithPlayerScope
// connection (which sets BOTH app.tenant_id and app.player_account_id)
// satisfied the plain tenant-match predicate and could read, insert, and
// disable OTHER players' risk_rules rows - a real violation of this
// codebase's own documented WithPlayerScope isolation contract
// (internal/db/tenant_rls.go), even though no player-facing HTTP handler
// exercises this path today.
func TestRiskRules_PlayerScopeConnectionCannotReadOrWrite(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var otherPlayerRuleID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := CreateRule(ctx, tx, CreateRuleParams{
			TenantID: &f.tenantID, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
			Threshold: 100, ThresholdExponent: ThresholdExponentOf(2), CreatedByActorType: "staff", CreatedByActorID: uuid.New(),
		})
		otherPlayerRuleID = r.ID
		return err
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// SELECT: a player-scoped connection must see NOTHING in risk_rules.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM risk_rules WHERE tenant_id = $1`, f.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected a player-scoped connection to see 0 risk_rules rows, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scope select: %v", err)
	}

	// INSERT: a player-scoped connection must never be able to create a
	// risk_rules row, even one naming its own tenant/player.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, player_account_id, operation, limit_kind, time_window, threshold, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, $2, 'casino_bet', 'max_amount', 'transaction', 999999999, 'staff', gen_random_uuid())`,
			f.tenantID, f.playerID,
		)
		return err
	})
	assertPgError(t, err, pgRLSViolation)

	// UPDATE (disable): a player-scoped connection must never be able to
	// disable another player's (or its own tenant's) risk_rules row.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE risk_rules SET status = 'disabled' WHERE id = $1`, otherPlayerRuleID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatal("expected a player-scoped connection to affect 0 rows attempting to disable a risk rule")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scope update: %v", err)
	}
}

func TestRiskRules_CompositeFKRejectsCrossTenantPlayerAccount(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, player_account_id, operation, limit_kind, time_window, threshold, threshold_exponent, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, $2, 'casino_bet', 'max_amount', 'transaction', 100, 2, 'staff', gen_random_uuid())`,
			fB.tenantID, fA.playerID,
		)
		return err
	})
	assertPgError(t, err, pgForeignKeyViolation)
}

// --- Immutability: only status/description/effective_until may change;
// DELETE/TRUNCATE are refused outright ---

func TestRiskRules_CoreFieldsAreImmutableAndAppendOnly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var ruleID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := CreateRule(ctx, tx, CreateRuleParams{
			TenantID: &f.tenantID, LicensingMode: "under_platform_licence", Operation: OperationCasinoBet,
			LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
			Threshold: 1000, ThresholdExponent: ThresholdExponentOf(2), CreatedByActorType: "staff", CreatedByActorID: uuid.New(),
		})
		ruleID = r.ID
		return err
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE risk_rules SET threshold = 9999 WHERE id = $1`, ruleID)
		return err
	})
	if err == nil {
		t.Fatal("expected mutating threshold to be refused")
	}

	// Stage 4G-FINAL regression (risk specialist review): licensing_mode
	// joined the immutability trigger's core-fields check in migration
	// 0042 - confirm a future trigger rewrite can't silently drop it from
	// that comparison without a test catching it.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE risk_rules SET licensing_mode = 'own_licence' WHERE id = $1`, ruleID)
		return err
	})
	if err == nil {
		t.Fatal("expected mutating licensing_mode to be refused")
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DisableRule(ctx, tx, DisableRuleParams{RuleID: ruleID, ActorType: "staff", ActorID: uuid.New()})
		return err
	})
	if err != nil {
		t.Fatalf("expected disabling the rule (the one legitimate mutation) to succeed, got: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM risk_rules WHERE id = $1`, ruleID)
		return err
	})
	if err == nil {
		t.Fatal("expected DELETE to be refused")
	}
}

// --- Evaluate: hard limit beats configurable, most-specific configurable
// wins, conflicting rules fail closed, unsupported operations fail closed ---

func createTestRule(t *testing.T, pool *db.Pool, tenantID *uuid.UUID, params CreateRuleParams) Rule {
	t.Helper()
	params.TenantID = tenantID
	if params.CreatedByActorID == uuid.Nil {
		params.CreatedByActorID = uuid.New()
	}
	if params.CreatedByActorType == "" {
		params.CreatedByActorType = "staff"
	}
	// Stage 4H-B0-R6 (ADR 0031 §35): an asset-agnostic amount rule must
	// declare which asset exponent its threshold's minor units are
	// expressed in. Every case in this file that does not care about
	// denomination evaluates against EUR (exponent 2), so defaulting the
	// declaration here preserves each test's original meaning exactly;
	// the denomination-specific cases in exponent_integration_test.go set
	// it explicitly instead.
	if params.LimitKind.isAmountShaped() && params.AssetCode == "" && params.ThresholdExponent == nil {
		params.ThresholdExponent = ThresholdExponentOf(2)
	}
	var r Rule
	var err error
	withTx := func(ctx context.Context, tx pgx.Tx) error {
		r, err = CreateRule(ctx, tx, params)
		return err
	}
	if tenantID == nil {
		err = pool.WithoutTenant(context.Background(), withTx)
	} else {
		err = pool.WithTenant(context.Background(), *tenantID, withTx)
	}
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return r
}

func TestEvaluate_HardLimitDeniesEvenWithoutAnyConfigurableRule(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 100, RuleKind: RuleHardLimit,
	})

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 200,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeDeny || decision.Code != CodeHardLimitBreach {
		t.Fatalf("expected a hard-limit deny, got %+v", decision)
	}
}

// TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode
// is Stage 4G-FINAL Part D's own regression: a platform-wide HARD_LIMIT
// expressing the PLATFORM's own licence's legal ceiling (LicensingMode
// "under_platform_licence") must NEVER also bind a tenant operating
// under a different licensing arrangement - proven here without a real
// BYOL tenant existing yet by scoping the rule to "own_licence" (the
// OTHER value) and confirming seedFixture's own "under_platform_licence"
// tenant is correctly unaffected by it.
func TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// Platform-wide rule scoped to "own_licence" tenants only. Disabled
	// via t.Cleanup - a platform-wide rule is visible to EVERY tenant
	// fixture in this shared test database and, unlike a tenant-scoped
	// rule (naturally isolated by each test's own fresh tenant), would
	// otherwise remain ACTIVE and pollute every other test's Evaluate
	// call for casino_bet for the rest of the test run (risk_rules is
	// append-only - it can never be deleted, only disabled). Confirmed
	// necessary: an earlier version of this test without cleanup left 13
	// such rows active in the shared dev database mid-session, which
	// broke unrelated casino/risk tests until manually disabled.
	rule := createTestRule(t, pool, nil, CreateRuleParams{
		LicensingMode: "own_licence", Operation: OperationCasinoBet,
		LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 100, RuleKind: RuleHardLimit,
	})
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := DisableRule(ctx, tx, DisableRuleParams{RuleID: rule.ID, ActorType: "staff", ActorID: uuid.New()})
			return err
		})
	})

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 200,
			LicensingMode: "under_platform_licence",
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("expected an own_licence-scoped hard limit to never bind an under_platform_licence request, got %+v", decision)
	}
}

func TestEvaluate_MostSpecificConfigurableRuleWins(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// Brand-wide default: max 200.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		BrandID: &f.brandID, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 200,
	})
	// Player-specific override: max 50 - narrower, must win over the brand default.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		PlayerAccountID: &f.playerID, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 50,
	})

	// 75 is under the brand default (200) but over the player override
	// (50) - if the brand rule incorrectly won, this would ALLOW.
	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 75,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeDeny {
		t.Fatalf("expected the player-specific override to win and deny 75 (> its own 50 threshold), got %+v", decision)
	}

	// 40 is under BOTH thresholds - must allow.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 40,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("expected 40 to be allowed under both thresholds, got %+v", decision)
	}
}

func TestEvaluate_ConflictingRulesAtSameSpecificityFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// Two rules with the IDENTICAL set of scope dimensions (same player,
	// same asset) for the same (limit_kind, time_window) - a genuine
	// configuration conflict, not merely a coincidental specificity tie.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		PlayerAccountID: &f.playerID, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 50,
		AssetCode: "EUR",
	})
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		PlayerAccountID: &f.playerID, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 30,
		AssetCode: "EUR",
	})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 10,
		})
		return err
	})
	if !errors.Is(err, ErrConflictingRules) {
		t.Fatalf("expected ErrConflictingRules, got: %v", err)
	}
}

// TestEvaluate_AdditionalScopeDimensionIsMoreSpecificNotATie is a
// regression test for a real P1 finding from specialist review: the
// original specificity() scored only the single highest-ranked dimension
// present, so a tenant-only rule and a tenant+asset rule (a strictly
// NARROWER, more specific match) were scored as an artificial TIE,
// forcing ErrConflictingRules - and therefore a fail-closed outage of the
// ENTIRE operation for that tenant - for two rules that were never
// actually in conflict. The fixed bitmask scoring must treat "tenant +
// asset" as strictly more specific than "tenant alone".
func TestEvaluate_AdditionalScopeDimensionIsMoreSpecificNotATie(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// Tenant-wide default: max 200, no asset scoping.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 200,
	})
	// Same tenant, additionally scoped to EUR - strictly more specific,
	// must win for an EUR request without being treated as a tie.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		AssetCode: "EUR", Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 50,
	})

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 75,
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected the asset-scoped rule to resolve without a spurious conflict, got: %v", err)
	}
	if decision.Outcome != OutcomeDeny {
		t.Fatalf("expected the more-specific EUR rule (50) to win over the tenant-wide default (200) and deny 75, got %+v", decision)
	}
}

// seedJurisdiction creates a real jurisdictions row (risk_rules.
// jurisdiction_code FKs into it) - mirrors internal/identity/
// identity_integration_test.go's identical seeding pattern.
func seedJurisdiction(t *testing.T, pool *db.Pool) string {
	t.Helper()
	code := "TEST-" + uuid.NewString()[:8]
	// Stage 4I Phase E-SECURITY (migration 0077): `jurisdictions` writes
	// now require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'Test Jurisdiction')`, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}
	return code
}

// TestEvaluate_LicensingModePlusJurisdictionIsMoreSpecificThanJurisdictionAlone
// is the LicensingMode analogue of
// TestEvaluate_AdditionalScopeDimensionIsMoreSpecificNotATie (risk
// specialist review finding: the original regression only covered
// AssetCode as the "additional dimension" - this proves the same
// property holds for LicensingMode, the newest and lowest-ranked bit in
// specificity()).
func TestEvaluate_LicensingModePlusJurisdictionIsMoreSpecificThanJurisdictionAlone(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdictionCode := seedJurisdiction(t, pool)
	// Jurisdiction-wide default: max 200.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		JurisdictionCode: jurisdictionCode, Operation: OperationCasinoBet,
		LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 200,
	})
	// Same jurisdiction, additionally scoped by LicensingMode - strictly
	// more specific, must win without being treated as a tie.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		JurisdictionCode: jurisdictionCode, LicensingMode: "under_platform_licence", Operation: OperationCasinoBet,
		LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 50,
	})

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 75,
			JurisdictionCode: jurisdictionCode, LicensingMode: "under_platform_licence",
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected the licensing-mode-scoped rule to resolve without a spurious conflict, got: %v", err)
	}
	if decision.Outcome != OutcomeDeny {
		t.Fatalf("expected the more-specific licensing-mode-scoped rule (50) to win over the jurisdiction-wide default (200) and deny 75, got %+v", decision)
	}
}

func TestEvaluate_UnsupportedCumulativeOperationFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// deposit has no ledger-transaction-type mapping this stage.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationDeposit, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingDay, Threshold: 1000,
	})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationDeposit, AssetCode: "EUR", Amount: 100,
		})
		return err
	})
	if !errors.Is(err, ErrUnsupportedCumulativeOperation) {
		t.Fatalf("expected ErrUnsupportedCumulativeOperation, got: %v", err)
	}
}

func TestEvaluate_MissingAmountFailsClosedRatherThanSilentlyAllowing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 100,
	})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", // Amount deliberately omitted.
		})
		return err
	})
	if !errors.Is(err, ErrMissingAmount) {
		t.Fatalf("expected ErrMissingAmount, got: %v", err)
	}
}

// TestEvaluate_DenyBeatsReviewAcrossConfigurableGroupsRegardlessOfOrder is
// a regression test for a real P1 finding from specialist review: the
// aggregation across independent (limit_kind, time_window) configurable
// groups iterates a Go map (randomized order) and previously overwrote
// the aggregate action unconditionally on every group, so whichever
// group happened to be visited LAST decided deny-vs-review - a genuinely
// non-deterministic final Outcome for the identical rule set, run to
// run. Two breaching groups here (one action=review, one action=deny)
// must always resolve to DENY, regardless of iteration order - repeated
// to make a lucky pass on one random order improbable.
func TestEvaluate_DenyBeatsReviewAcrossConfigurableGroupsRegardlessOfOrder(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 10, Action: ActionReview,
	})
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMinAmount, TimeWindow: WindowTransaction,
		Threshold: 1000, Action: ActionDeny,
	})

	for i := 0; i < 20; i++ {
		var decision RiskDecision
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			decision, err = Evaluate(ctx, tx, RiskRequest{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
				Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 50,
			})
			return err
		})
		if err != nil {
			t.Fatalf("iteration %d: evaluate: %v", i, err)
		}
		if decision.Outcome != OutcomeDeny {
			t.Fatalf("iteration %d: expected DENY to always beat REVIEW regardless of map iteration order, got %+v", i, decision)
		}
	}
}

// TestEvaluate_HardLimitReviewAction proves a HARD_LIMIT rule configured
// with action=review actually produces REVIEW (not silently ignored, and
// not escalated to DENY) when nothing else denies - a real code path
// (evaluator.go's hardBreached-without-deny branch) with zero prior test
// coverage per adversarial specialist review.
func TestEvaluate_HardLimitReviewAction(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 100, RuleKind: RuleHardLimit, Action: ActionReview,
	})

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 200,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeReview {
		t.Fatalf("expected a hard-limit review-action breach to produce REVIEW, got %+v", decision)
	}
}

// TestEvaluate_RiskSignalContributesReview proves a RuleRiskSignal that
// breaches actually produces REVIEW - a real code path with zero prior
// test coverage per adversarial specialist review.
func TestEvaluate_RiskSignalContributesReview(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 100, RuleKind: RuleRiskSignal,
	})

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 200,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeReview || decision.Code != CodeRiskSignalFlag {
		t.Fatalf("expected a breaching risk signal to produce REVIEW/%s, got %+v", CodeRiskSignalFlag, decision)
	}

	// Below threshold: the signal does not breach, so nothing flags it.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 50,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("expected a non-breaching risk signal to allow, got %+v", decision)
	}
}

// TestEvaluate_EffectiveWindowIsEnforcedAtEvaluateLevel proves Evaluate
// ITSELF (not just the unit-level isEffective() helper) skips a
// not-yet-effective or already-expired rule against real database rows -
// per adversarial specialist review, only a unit test of isEffective()
// existed previously, never an integration test of Evaluate's own
// filtering.
func TestEvaluate_EffectiveWindowIsEnforcedAtEvaluateLevel(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// Both effective_from and effective_until are immutable once set
	// (migration 0041's trigger only permits status/description changes
	// in place) - a rule outside its effective window must be forged via
	// direct INSERT with both timestamps already set correctly, never via
	// a later UPDATE (which would also violate the "effective_until must
	// be after effective_from" CHECK if effective_from defaults to
	// creation time).
	notYetEffectiveID := uuid.New()
	alreadyExpiredID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, operation, limit_kind, time_window, threshold, threshold_exponent, rule_kind, action, effective_from, created_by_actor_type, created_by_actor_id)
			 VALUES ($1, $2, 'casino_bet', 'max_amount', 'transaction', 10, 2, 'hard_limit', 'deny', now() + interval '1 day', 'staff', gen_random_uuid())`,
			notYetEffectiveID, f.tenantID,
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO risk_rules (id, tenant_id, operation, limit_kind, time_window, threshold, threshold_exponent, rule_kind, action, effective_from, effective_until, created_by_actor_type, created_by_actor_id)
			 VALUES ($1, $2, 'casino_bet', 'min_amount', 'transaction', 1000000, 2, 'hard_limit', 'deny', now() - interval '2 days', now() - interval '1 day', 'staff', gen_random_uuid())`,
			alreadyExpiredID, f.tenantID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("force effective window via direct SQL: %v", err)
	}

	var decision RiskDecision
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 200,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("expected both a not-yet-effective and an already-expired hard limit to be skipped by Evaluate itself, got %+v", decision)
	}
}

func TestEvaluate_DisabledRuleNeverApplies(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	r := createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction, Threshold: 10, RuleKind: RuleHardLimit,
	})
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DisableRule(ctx, tx, DisableRuleParams{RuleID: r.ID, ActorType: "staff", ActorID: uuid.New()})
		return err
	})
	if err != nil {
		t.Fatalf("disable rule: %v", err)
	}

	var decision RiskDecision
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 500,
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("expected a disabled hard limit to never apply, got %+v", decision)
	}
}
