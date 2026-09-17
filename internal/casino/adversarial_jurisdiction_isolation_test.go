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
// Stage 4H-B0-R6 UPDATE: the second case's expectation is INVERTED, and
// deliberately so. "Never accidentally matches" was itself the fail-open -
// a jurisdiction-scoped HARD_LIMIT was silently skipped for a bet whose
// jurisdiction nobody had resolved. Risk now fails CLOSED in that case
// (risk.ErrMissingJurisdiction, ADR 0031 §34 superseding §9). The FIRST
// case is unchanged and still proves genuine isolation: a request that
// DOES carry a jurisdiction is never denied by a rule scoped to a
// different one.
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
	"errors"
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

// TestReceiveCallback_SessionWithNoJurisdictionFailsClosedAgainstJurisdictionScopedRule
// asserts the Stage 4H-B0-R6 CHANGE to this exact scenario (ADR 0031 §34,
// which supersedes §9 on this point).
//
// This test previously asserted the OPPOSITE - that an empty session
// jurisdiction simply "does not match" a jurisdiction-scoped rule and the
// bet posts normally. That was a silent fail-OPEN: the rule here is a
// jurisdiction-scoped HARD_LIMIT of 50, and a 75-unit bet from a session
// with no resolved jurisdiction was posted anyway, because Rule.matches()
// returns false for an empty request-side dimension (empty is a wildcard
// only on the RULE side). An un-resolved jurisdiction cannot PROVE the
// rule does not apply, so Risk now refuses to decide - the identical gate
// licensing_mode has had since Stage 4G-FINAL Part D.
//
// A fail-closed Risk error is a Go error (not a decline with a reason
// code), so it aborts the whole callback transaction: no ledger effect,
// exactly like every other Risk error path.
func TestReceiveCallback_SessionWithNoJurisdictionFailsClosedAgainstJurisdictionScopedRule(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	ruleJurisdiction := seedJurisdiction(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	// mintSession (not mintSessionWithJurisdiction) leaves
	// casino_launch_sessions.jurisdiction_code NULL -> "" server-side,
	// exactly the "LaunchGame itself had no resolved jurisdiction" case
	// the orchestrator.go postBet comment names (TODO(jurisdiction) is
	// still the unfixed root cause - ADR 0031 §9/§34).
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
	if !errors.Is(err, risk.ErrMissingJurisdiction) {
		t.Fatalf("expected an unresolved jurisdiction to fail CLOSED against a jurisdiction-scoped hard limit, got err=%v result=%+v", err, result)
	}
	if !errors.Is(err, risk.ErrInvalidInput) {
		t.Fatalf("expected the fail-closed jurisdiction error to remain an ErrInvalidInput (its HTTP mapping), got %v", err)
	}
	if result.Outcome == OutcomeDeclined {
		t.Fatal("a Risk-unavailable/undecidable condition must never be reported as a business decline with a reason code")
	}
	if balance := cashBalance(t, pool, f); balance != 100000 {
		t.Fatalf("expected zero ledger effect after a fail-closed risk evaluation, got balance %d", balance)
	}
}
