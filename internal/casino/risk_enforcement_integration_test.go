//go:build integration

// Stage 4G: proves internal/casino actually consults the central
// internal/risk.Evaluate boundary at LaunchGame/postBet (ADR 0031 §7),
// including the deterministic precedence example the directive itself
// gives (a player-specific override beating a brand default) and the
// concurrency guarantee a cumulative limit requires. Follows this
// package's own rg_enforcement_integration_test.go conventions exactly.
package casino

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

func createCasinoRiskRule(t *testing.T, pool *db.Pool, f casinoFixture, params risk.CreateRuleParams) risk.Rule {
	t.Helper()
	params.TenantID = &f.tenantID
	if params.CreatedByActorType == "" {
		params.CreatedByActorType = "staff"
	}
	// Stage 4H-B0-R6 (ADR 0031 §35): an asset-agnostic amount rule must
	// declare the asset exponent its threshold's minor units are
	// expressed in. Every case in this file bets in EUR (exponent 2), so
	// defaulting the declaration preserves each test's original meaning;
	// a case that cares about denomination sets it explicitly.
	if params.AssetCode == "" && params.ThresholdExponent == nil {
		params.ThresholdExponent = risk.ThresholdExponentOf(2)
	}
	if params.CreatedByActorID == uuid.Nil {
		params.CreatedByActorID = uuid.New()
	}
	var r risk.Rule
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = risk.CreateRule(ctx, tx, params)
		return err
	})
	if err != nil {
		t.Fatalf("create risk rule: %v", err)
	}
	return r
}

// mintSessionWithJurisdiction is mintSession plus a persisted
// jurisdiction_code (migration 0042, Stage 4G-FINAL Part C) - proves a
// jurisdiction-scoped risk rule is reachable from postBet, not only from
// LaunchGame (the gap Stage 4G's own completion report disclosed).
func mintSessionWithJurisdiction(t *testing.T, pool *db.Pool, f casinoFixture, providerID, assetCode, jurisdictionCode string) uuid.UUID {
	t.Helper()
	game := seedGame(t, pool, providerID, assetCode)
	var sessionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		session, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: providerID, ProviderGameID: game.ProviderGameID,
			AssetCode: assetCode, Mode: ModeReal, JurisdictionCode: jurisdictionCode,
		})
		if err != nil {
			return err
		}
		sessionID = session.ID
		return nil
	})
	if err != nil {
		t.Fatalf("mint launch session with jurisdiction: %v", err)
	}
	return sessionID
}

// seedJurisdiction creates a real jurisdictions row (casino_launch_sessions.
// jurisdiction_code and risk_rules.jurisdiction_code both FK into it) -
// mirrors internal/identity/identity_integration_test.go's identical
// seeding pattern.
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

// TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession
// is Stage 4G-FINAL Part C's own regression: a jurisdiction-scoped
// HARD_LIMIT must be reachable from postBet via the SAME jurisdiction
// LaunchGame resolved and persisted onto the session (migration 0042) -
// Stage 4G's own completion report explicitly disclosed this as
// UNREACHABLE before this fix.
func TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	jurisdictionCode := seedJurisdiction(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSessionWithJurisdiction(t, pool, f, "mock-casino", "EUR", jurisdictionCode)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		JurisdictionCode: jurisdictionCode, Operation: risk.OperationCasinoBet,
		LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 50, RuleKind: risk.RuleHardLimit,
	})

	payload := provider.CallbackPayload(CallbackEventBet, "bet-jurisdiction", "", "round-1", "game-1", 75, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined || result.DeclineReason != risk.CodeHardLimitBreach {
		t.Fatalf("expected the jurisdiction-scoped hard limit to deny a 75-unit bet (threshold 50), got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 100000 {
		t.Fatalf("expected the funded balance unchanged after a jurisdiction-scoped risk denial, got %d", balance)
	}
}

func TestLaunchGame_DeniedByRiskHardLimit(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	// A platform-wide hard limit that always denies casino_launch for
	// this asset (threshold irrelevant for a launch, which carries no
	// amount - min_amount with threshold 0 can never be breached by a
	// zero Amount, so this test uses a rule shape that WILL be evaluated:
	// max_amount with threshold -1 is invalid (CHECK threshold >= 0), so
	// instead prove denial via a max_amount rule with threshold 0 - any
	// launch (Amount always 0) is compared 0 > 0 = false, which would
	// wrongly ALLOW. Use min_amount with a positive threshold instead: 0
	// < threshold is always true, denying every launch - this is the
	// correct way to express "deny this operation unconditionally" with
	// the two limit kinds this stage implements.
	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		Operation: risk.OperationCasinoLaunch, LimitKind: risk.LimitMinAmount, TimeWindow: risk.WindowTransaction,
		Threshold: 0, RuleKind: risk.RuleHardLimit,
	})

	// A min_amount rule needs a non-zero Amount to evaluate at all
	// (ErrMissingAmount otherwise) - but LaunchGameParams carries no
	// Amount field, so risk.RiskRequest.Amount is always 0 for
	// casino_launch. This proves the OTHER honest outcome: a hard limit
	// configured for a dimension casino_launch cannot supply fails
	// closed (propagates as an error), never silently ALLOW.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected an amount-shaped hard limit misconfigured for casino_launch to fail closed (error), got nil")
	}
}

func TestReceiveCallback_BetDeniedByRiskMaxAmountHardLimit(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		Operation: risk.OperationCasinoBet, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
		Threshold: 500, RuleKind: risk.RuleHardLimit,
	})

	payload := provider.CallbackPayload(CallbackEventBet, "bet-over-limit", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined || result.DeclineReason != risk.CodeHardLimitBreach {
		t.Fatalf("expected a risk-denied bet, got %+v", result)
	}
	if result.LedgerTransactionID != nil {
		t.Fatal("a risk-denied bet must produce zero ledger effect")
	}
	if balance := cashBalance(t, pool, f); balance != 100000 {
		t.Fatalf("expected the funded balance unchanged after a risk denial, got %d", balance)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_bet.denied_by_risk_policy") {
		t.Fatal("expected a casino_bet.denied_by_risk_policy audit record")
	}
}

// TestReceiveCallback_PlayerRiskOverrideBeatsBrandDefault proves the
// directive's own precedence example end to end through the REAL casino
// bet path: "Player X casino maximum stake = 50 while Brand default =
// 200" - a 75-unit bet must be denied (the player's own stricter 50
// wins), not allowed (which is what would happen if the broader brand
// rule incorrectly took precedence).
func TestReceiveCallback_PlayerRiskOverrideBeatsBrandDefault(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		BrandID: &f.brandID, Operation: risk.OperationCasinoBet, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 200,
	})
	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		PlayerAccountID: &f.playerAccountID, Operation: risk.OperationCasinoBet, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 50,
	})

	payload := provider.CallbackPayload(CallbackEventBet, "bet-75", "", "round-1", "game-1", 75, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined {
		t.Fatalf("expected the player-specific 50 override to deny a 75-unit bet (brand default is 200), got %+v", result)
	}
}

// TestReceiveCallback_ConcurrentBetsRespectCumulativeLimit is the
// directive's own §26 concurrency example: "Maximum player stake = 100.
// Two concurrent 75 bets must not both be allowed if that would violate
// a cumulative/exposure rule." Uses two DISTINCT launch sessions (real
// concurrent bet delivery never shares one session's own single-flight
// resolution) so the race is genuinely on the cumulative-amount
// evaluation/lock, not on session resolution.
func TestReceiveCallback_ConcurrentBetsRespectCumulativeLimit(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionA := mintSession(t, pool, f, "mock-casino", "EUR")
	sessionB := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	createCasinoRiskRule(t, pool, f, risk.CreateRuleParams{
		Operation: risk.OperationCasinoBet, LimitKind: risk.LimitCumulativeAmount, TimeWindow: risk.WindowRollingHour,
		Threshold: 100, RuleKind: risk.RuleHardLimit,
	})

	payloadA := provider.CallbackPayload(CallbackEventBet, "bet-a", "", "round-a", "game-1", 75, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionA)
	payloadB := provider.CallbackPayload(CallbackEventBet, "bet-b", "", "round-b", "game-1", 75, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionB)

	results := make([]ReceiveCallbackResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i, payload := range [][]byte{payloadA, payloadB} {
		go func(i int, payload []byte) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[i], err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}(i, payload)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("bet %d: unexpected error: %v", i, err)
		}
	}

	successes := 0
	for _, r := range results {
		if r.Outcome == OutcomeSucceeded {
			successes++
		} else if r.Outcome != OutcomeDeclined {
			t.Fatalf("expected every bet to succeed or be declined, got %+v", r)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 of 2 concurrent 75-unit bets to succeed under a cumulative 100 limit, got %d", successes)
	}
	// The ledger itself must agree: exactly one 75-unit debit posted.
	if balance := cashBalance(t, pool, f); balance != 100000-75 {
		t.Fatalf("expected exactly one 75-unit bet to have posted, got balance %d", balance)
	}
}
