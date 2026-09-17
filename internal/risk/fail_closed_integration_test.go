//go:build integration

// Stage 4H-B0-R6 Workstream D, Part 2: the fail-closed audit of every
// Risk enforcement path, as tests (ADR 0031 §34's table, case by case).
//
// The property under test is always the same one: a condition in which
// Risk CANNOT decide must never resolve to ALLOW, and must never be
// reported as a business decline either - it is an error, which aborts
// the transaction the guarded operation would have posted in.
package risk

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func assertErrorIs(t *testing.T, err error, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("expected error %v, got %v", target, err)
	}
}

// evaluateWithTenant is the normal, correctly-scoped call shape.
func evaluateWithTenant(t *testing.T, pool *db.Pool, req RiskRequest) (RiskDecision, error) {
	t.Helper()
	var decision RiskDecision
	err := pool.WithTenant(context.Background(), req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, req)
		return err
	})
	return decision, err
}

func baseRequest(f fixture) RiskRequest {
	return RiskRequest{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
		Operation: OperationCasinoBet, AssetCode: "EUR", Amount: 100,
		LicensingMode: "under_platform_licence",
	}
}

// --- Risk unavailable / timed out / cancelled ---

// TestEvaluate_DatabaseErrorFailsClosed poisons the transaction before
// Evaluate runs (a division by zero aborts the pg transaction, so every
// subsequent statement in it errors) - the closest faithful simulation of
// "the database is unavailable mid-evaluation" without tearing down the
// shared dev cluster.
func TestEvaluate_DatabaseErrorFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
			t.Fatal("expected the poisoning statement itself to fail")
		}
		var evalErr error
		decision, evalErr = Evaluate(ctx, tx, baseRequest(f))
		return evalErr
	})
	if err == nil {
		t.Fatal("expected a database error during evaluation to fail closed, got a decision")
	}
	if decision.Outcome == OutcomeAllow {
		t.Fatalf("a failed evaluation must never yield ALLOW, got %+v", decision)
	}
}

func TestEvaluate_TimeoutFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		timedOut, cancel := context.WithTimeout(ctx, 1*time.Millisecond)
		defer cancel()
		<-timedOut.Done()
		_, err := Evaluate(timedOut, tx, baseRequest(f))
		if err == nil {
			t.Fatal("expected an expired context to fail closed, got a decision")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected the deadline to surface, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
}

func TestEvaluate_CancelledContextFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := Evaluate(cancelled, tx, baseRequest(f))
		if err == nil {
			t.Fatal("expected a cancelled context to fail closed, got a decision")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
}

// --- Wrongly-scoped transactions: the two RLS-shaped fail-opens ---

// TestEvaluate_UnscopedTransactionFailsClosed proves the fail-open ADR
// 0031 §34 records: on a db.WithoutTenant transaction, risk_rules' RLS
// read policy returns ONLY platform-wide rules, so a tenant's own
// HARD_LIMIT is invisible and the request would resolve to ALLOW.
func TestEvaluate_UnscopedTransactionFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 10, RuleKind: RuleHardLimit,
	})

	var decision RiskDecision
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, baseRequest(f))
		return err
	})
	if err == nil {
		t.Fatalf("expected an unscoped transaction to fail closed, got %+v", decision)
	}
	assertErrorIs(t, err, ErrTenantScopeMismatch)
}

func TestEvaluate_WrongTenantScopeFailsClosed(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)

	// Transaction scoped to tenant B, request claiming tenant A.
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Evaluate(ctx, tx, baseRequest(fA))
		return err
	})
	if err == nil {
		t.Fatal("expected a request for a different tenant than the transaction's own scope to fail closed")
	}
	assertErrorIs(t, err, ErrTenantScopeMismatch)
}

// TestEvaluate_PlayerScopedTransactionFailsClosed proves the second
// RLS-shaped fail-open: every risk_rules policy requires
// app.player_account_id to be NULL, so a player-scoped connection reads
// ZERO rules - which before this stage meant ALLOW for every request.
func TestEvaluate_PlayerScopedTransactionFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 10, RuleKind: RuleHardLimit,
	})

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Evaluate(ctx, tx, baseRequest(f))
		return err
	})
	if err == nil {
		t.Fatal("expected a player-scoped transaction to be refused outright")
	}
	assertErrorIs(t, err, ErrPlayerScopedConnection)
}

// --- Missing request context ---

func TestEvaluate_UnknownOperationFailsClosedRatherThanMatchingNoRules(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// A real hard limit exists under the CORRECT spelling.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f)
	req.Operation = Operation("casino_bett") // one typo
	decision, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatalf("expected an unrecognized operation to fail closed, got %+v", decision)
	}
	assertErrorIs(t, err, ErrUnknownOperation)
	assertErrorIs(t, err, ErrInvalidInput)
}

func TestEvaluate_MissingPlayerFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// A player-specific rule - the most specific shape there is - which
	// matches() would silently skip for a uuid.Nil player.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		PlayerAccountID: &f.playerID, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount,
		TimeWindow: WindowTransaction, Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f)
	req.PlayerAccountID = uuid.Nil
	_, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatal("expected a missing player_account_id to fail closed")
	}
	assertErrorIs(t, err, ErrMissingPlayer)
}

func TestEvaluate_MissingJurisdictionFailsClosedWhenAJurisdictionScopedRuleExists(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdiction := seedJurisdiction(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		JurisdictionCode: jurisdiction, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount,
		TimeWindow: WindowTransaction, Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f) // no JurisdictionCode
	_, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatal("expected an unresolved jurisdiction to fail closed while a jurisdiction-scoped rule is effective")
	}
	assertErrorIs(t, err, ErrMissingJurisdiction)
	assertErrorIs(t, err, ErrMissingScopeContext)

	// Supplying the jurisdiction resolves the gate: the same rule then
	// applies normally (and denies, threshold 10 < amount 100).
	req.JurisdictionCode = jurisdiction
	decision, err := evaluateWithTenant(t, pool, req)
	if err != nil {
		t.Fatalf("evaluate with a resolved jurisdiction: %v", err)
	}
	if decision.Outcome != OutcomeDeny {
		t.Fatalf("expected the jurisdiction-scoped hard limit to deny once jurisdiction is supplied, got %+v", decision)
	}
}

func TestEvaluate_MissingLicensingModeFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		LicensingMode: "under_platform_licence", Operation: OperationCasinoBet, LimitKind: LimitMaxAmount,
		TimeWindow: WindowTransaction, Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f)
	req.LicensingMode = ""
	_, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatal("expected a missing licensing_mode to fail closed")
	}
	assertErrorIs(t, err, ErrMissingLicensingMode)
}

func TestEvaluate_MissingAssetFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	// Asset-agnostic amount rule: before this stage its threshold was
	// compared against req.Amount with no asset context at all.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f)
	req.AssetCode = ""
	_, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatal("expected a missing asset_code to fail closed for an amount-shaped rule")
	}
	assertErrorIs(t, err, ErrMissingAsset)
}

func TestEvaluate_AssetScopedRuleWithNoRequestAssetFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		AssetCode: "BTC", Operation: OperationCasinoBet, LimitKind: LimitMaxAmount,
		TimeWindow: WindowTransaction, Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f)
	req.AssetCode = ""
	_, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatal("expected an asset-scoped rule plus an assetless request to fail closed")
	}
	assertErrorIs(t, err, ErrMissingAsset)
}

func TestEvaluate_UnknownAssetFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f)
	req.AssetCode = "NOT-A-REAL-ASSET"
	_, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatal("expected an unregistered asset to fail closed")
	}
	assertErrorIs(t, err, ErrUnknownAsset)
}

func TestEvaluate_MissingProviderScopeContextFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		ProviderID: "mock-casino", Operation: OperationCasinoBet, LimitKind: LimitMaxAmount,
		TimeWindow: WindowTransaction, Threshold: 10, RuleKind: RuleHardLimit,
	})

	req := baseRequest(f) // no ProviderID
	_, err := evaluateWithTenant(t, pool, req)
	if err == nil {
		t.Fatal("expected a provider-scoped rule plus a providerless request to fail closed")
	}
	assertErrorIs(t, err, ErrMissingScopeContext)
}

// TestEvaluate_DisabledScopedRuleDoesNotTriggerTheMissingScopeGate keeps
// the gate honest: it is scoped to rules that are actually LIVE policy,
// so a disabled or not-yet-effective rule must not make an unrelated
// dimension mandatory for every request.
func TestEvaluate_DisabledScopedRuleDoesNotTriggerTheMissingScopeGate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	jurisdiction := seedJurisdiction(t, pool)
	rule := createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		JurisdictionCode: jurisdiction, Operation: OperationCasinoBet, LimitKind: LimitMaxAmount,
		TimeWindow: WindowTransaction, Threshold: 10, RuleKind: RuleHardLimit,
	})
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DisableRule(ctx, tx, DisableRuleParams{RuleID: rule.ID, ActorType: "staff", ActorID: uuid.New()})
		return err
	})
	if err != nil {
		t.Fatalf("disable rule: %v", err)
	}

	decision, err := evaluateWithTenant(t, pool, baseRequest(f))
	if err != nil {
		t.Fatalf("a disabled jurisdiction-scoped rule must not make jurisdiction mandatory: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("expected ALLOW with only a disabled rule present, got %+v", decision)
	}
}

// --- Outcome classification: four distinct outcomes ---

func TestOutcome_IsKnownClassifiesTheFourthOutcomeSeparately(t *testing.T) {
	for _, known := range []Outcome{OutcomeAllow, OutcomeDeny, OutcomeReview} {
		if !known.IsKnown() {
			t.Fatalf("%q must be a known outcome", known)
		}
	}
	for _, unknown := range []Outcome{Outcome(""), Outcome("allowed"), Outcome("ALLOW"), Outcome("pending")} {
		if unknown.IsKnown() {
			t.Fatalf("%q must NOT be a known outcome - an enforcement point has to be able to tell a real decision from an unclassifiable one", unknown)
		}
	}
}

// TestEvaluate_AlwaysReturnsAKnownOutcome is the self-check's own test:
// every decision Evaluate hands back must be classifiable by a caller.
func TestEvaluate_AlwaysReturnsAKnownOutcome(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 10, RuleKind: RuleHardLimit, Action: ActionReview,
	})
	decision, err := evaluateWithTenant(t, pool, baseRequest(f))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !decision.Outcome.IsKnown() {
		t.Fatalf("Evaluate returned an unclassifiable outcome %q", decision.Outcome)
	}
	if decision.Outcome != OutcomeReview {
		t.Fatalf("expected REVIEW from a review-action hard limit (kept distinct from DENY), got %+v", decision)
	}
	if decision.Code != CodeHardLimitBreach {
		t.Fatalf("expected a REVIEW to carry its own explainable code, got %q", decision.Code)
	}
}

// --- Concurrency ---

// TestEvaluate_ConcurrentEvaluationsAreConsistent runs many independent,
// correctly-scoped evaluations of the same rule set at once (each in its
// own transaction, as every real caller does). Run under -race, this also
// covers the shared read paths: the rule list, the per-call asset
// exponent resolver, and the cumulative advisory lock.
func TestEvaluate_ConcurrentEvaluationsAreConsistent(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 50, RuleKind: RuleHardLimit,
	})
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
		Threshold: 1_000_000, RuleKind: RuleConfigurableLimit,
	})

	const workers = 8
	outcomes := make([]Outcome, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			decision, err := evaluateWithTenant(t, pool, baseRequest(f))
			outcomes[i], errs[i] = decision.Outcome, err
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d: unexpected error: %v", i, errs[i])
		}
		// amount 100 > max 50 -> deterministic DENY for every worker.
		if outcomes[i] != OutcomeDeny {
			t.Fatalf("worker %d: expected a deterministic DENY, got %q", i, outcomes[i])
		}
	}
}
