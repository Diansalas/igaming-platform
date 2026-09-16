//go:build integration

// QA adversarial review (Stage 4G-FINAL): the shipped regression
// (TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession)
// only proves the POSITIVE case (a jurisdiction-scoped rule denies a bet
// in ITS OWN jurisdiction). This file proves the two negative cases that
// were NOT covered: a jurisdiction-scoped rule must never deny a bet from
// a DIFFERENT jurisdiction, and a session minted with NO resolved
// jurisdiction (empty string) must never accidentally match a
// jurisdiction-scoped rule.
//
// These tests were briefly removed by the adversarial reviewer after
// they failed against a shared dev database polluted by leftover
// platform-wide risk_rules from earlier ad hoc test runs (risk_rules is
// append-only - a "throwaway" rule created directly rather than through
// a test with its own t.Cleanup can never be deleted, only disabled).
// Re-added after confirming (a) the underlying Rule.matches() logic was
// always correct, (b) the pollution was cleared by disabling every
// leftover platform-wide row, and (c) the two Stage 4G tests that had
// been creating platform-wide rows without disabling them
// (TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode,
// TestRiskRules_TenantSeesOwnAndPlatformWideRules) now clean up via
// t.Cleanup, so this class of pollution should not recur.
package casino

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/risk"
)

func TestReceiveCallback_JurisdictionScopedRiskRuleDoesNotDenyADifferentJurisdiction(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	ruleJurisdiction := seedJurisdiction(t, pool)
	sessionJurisdiction := seedJurisdiction(t, pool)
	if ruleJurisdiction == sessionJurisdiction {
		t.Fatalf("expected two distinct jurisdiction codes")
	}
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSessionWithJurisdiction(t, pool, f, "mock-casino", "EUR", sessionJurisdiction)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		JurisdictionCode: ruleJurisdiction, Operation: risk.OperationCasinoBet,
		LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 50, RuleKind: risk.RuleHardLimit,
	})

	payload := provider.CallbackPayload(CallbackEventBet, "bet-diff-jurisdiction", "", "round-1", "game-1", 75, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected a rule scoped to a DIFFERENT jurisdiction to never deny this bet, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 100000-75 {
		t.Fatalf("expected the bet to post normally, got balance %d", balance)
	}
}

func TestReceiveCallback_SessionWithNoJurisdictionDoesNotMatchJurisdictionScopedRule(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	ruleJurisdiction := seedJurisdiction(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	// mintSession (not mintSessionWithJurisdiction) leaves
	// casino_launch_sessions.jurisdiction_code NULL -> "" server-side,
	// exactly the "LaunchGame itself had no resolved jurisdiction" case
	// the orchestrator.go postBet comment names.
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		JurisdictionCode: ruleJurisdiction, Operation: risk.OperationCasinoBet,
		LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 50, RuleKind: risk.RuleHardLimit,
	})

	payload := provider.CallbackPayload(CallbackEventBet, "bet-no-jurisdiction", "", "round-1", "game-1", 75, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected an empty session jurisdiction to never match a jurisdiction-scoped rule, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 100000-75 {
		t.Fatalf("expected the bet to post normally, got balance %d", balance)
	}
}
