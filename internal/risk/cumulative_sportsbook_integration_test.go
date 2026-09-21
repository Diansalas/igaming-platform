//go:build integration

// Stage 9.2 Workstream B Wave 1 (ADR 0083 §6.1.1, §6.1.4, §12.2 items
// 31/33): the DB-backed coverage for operationCumulativeSpecs' new
// OperationSportsbookBet entry.
//
// These tests post the REAL cash-funded placement shape
// (transaction_type=sportsbook_bet, Dr player_cash / Cr
// player_locked_cash - ADR 0038 §3, and exactly what
// internal/sportsbook's PlaceBet builds in its single betInput) through
// ledger.Post unmodified, and then read it back through the SHIPPED spec,
// the way cumulative_bonus_conversion_integration_test.go proves the
// bonus_conversion entry. They deliberately do NOT import
// internal/sportsbook: internal/risk owns the measurement, the posting
// shape is the only fact it depends on, and a parallel Stage 9.2
// workstream is editing that package concurrently.
package risk

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// postSportsbookBetShape posts the two-player-owned-leg placement shape.
// Both legs are wallet-scoped, so ledger_entries.player_account_id is
// non-NULL on BOTH - the exact condition that makes an account-type-blind
// sum net to zero.
func postSportsbookBetShape(t *testing.T, pool *db.Pool, f fixture, walletID uuid.UUID, assetCode string, stake int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, ledger.AccountPlayerCash, assetCode)
		if err != nil {
			return err
		}
		locked, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, ledger.AccountPlayerLockedCash, assetCode)
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxSportsbookBet,
			IdempotencyKey: "sportsbook_bet:" + uuid.New().String(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cash, Direction: ledger.Debit, Amount: stake},
				{LedgerAccountID: locked, Direction: ledger.Credit, Amount: stake},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("post sportsbook_bet placement shape: %v", err)
	}
}

// overrideSpecForTest temporarily REPLACES a production cumulativeSpec
// and restores it afterwards. Distinct from injectTestOnlySpec, which
// refuses to shadow a production entry on purpose: these tests need to
// deform sportsbook_bet's own shipped spec in order to prove the
// fail-closed branches, and the original must be restored so no later
// test in the package observes the deformed one.
func overrideSpecForTest(t *testing.T, op Operation, spec cumulativeSpec) {
	t.Helper()
	original, existed := operationCumulativeSpecs[op]
	if !existed {
		t.Fatalf("%s has no production cumulative spec to override - use injectTestOnlySpec instead", op)
	}
	operationCumulativeSpecs[op] = spec
	t.Cleanup(func() { operationCumulativeSpecs[op] = original })
}

// TestSportsbookCumulative_TwoPlayerOwnedLegsAreNotNettedToZero is ADR
// 0083 §12.2 item 33: the sportsbook-specific instance of ADR 0031
// §32(a)'s fail-OPEN.
func TestSportsbookCumulative_TwoPlayerOwnedLegsAreNotNettedToZero(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	walletID := seedWalletFor(t, pool, f, "EUR", 1_000_000)

	const stake int64 = 2500
	postSportsbookBetShape(t, pool, f, walletID, "EUR", stake)

	spec, ok := operationCumulativeSpecs[OperationSportsbookBet]
	if !ok {
		t.Fatal("expected a PRODUCTION cumulativeSpec for OperationSportsbookBet (ADR 0083 §6.1.1)")
	}
	windowStart := time.Now().UTC().Add(-time.Hour)

	usage := func(t *testing.T, s cumulativeSpec) (int64, error) {
		t.Helper()
		var total int64
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			got, err := cumulativeUsage(ctx, tx, s, RiskRequest{
				TenantID: f.tenantID, PlayerAccountID: f.playerID, AssetCode: "EUR", Operation: OperationSportsbookBet,
			}, windowStart)
			if err != nil {
				return err
			}
			total = got.Int64()
			return nil
		})
		return total, err
	}

	t.Run("shipped spec measures the full stake", func(t *testing.T) {
		got, err := usage(t, spec)
		if err != nil {
			t.Fatalf("cumulative usage against a real sportsbook_bet posting: %v", err)
		}
		if got != stake {
			t.Fatalf("expected the full stake %d to be measured (the player_cash debit), got %d", stake, got)
		}
	})

	// Negative control, part 1 - the defect's own signature. An
	// account-type-blind sum over the SAME rows nets the two player-owned
	// legs to exactly zero: +stake on player_cash's debit, -stake on
	// player_locked_cash's credit. This is what a cumulative sportsbook
	// cap would have measured without the ledger_accounts join, i.e. no
	// cap would ever bind however much was staked.
	t.Run("an account-type-blind sum nets the same rows to zero", func(t *testing.T) {
		if blind := preFixBlindUsage(t, pool, f, "EUR", spec.allTransactionTypes(), windowStart); blind != 0 {
			t.Fatalf("expected the account-type-blind query to net the two player-owned legs to 0 (the ADR 0031 §32(a) defect), got %d", blind)
		}
	})

	// Negative control, part 2 - what emptying IgnoredAccountTypes does
	// TODAY. It cannot reproduce the zero above, and that is the point:
	// since the leg-aware fix, an undeclared player-side leg is refused
	// outright rather than dropped. So the current cost of forgetting
	// player_locked_cash is a loud fail-CLOSED, never a silent
	// under-count. Both halves are asserted so a future refactor that
	// reintroduced "ignore what you don't recognise" would fail here
	// rather than quietly restore the fail-open.
	t.Run("emptying IgnoredAccountTypes fails closed instead of under-counting", func(t *testing.T) {
		deformed := spec
		deformed.IgnoredAccountTypes = nil
		got, err := usage(t, deformed)
		if err == nil {
			t.Fatalf("expected an undeclared player_locked_cash leg to fail closed, got usage %d", got)
		}
		assertErrorIs(t, err, ErrUnrecognizedCumulativeLeg)
	})

	// End to end through Evaluate with a real risk_rules row - the whole
	// point of the map entry. A tenant's cumulative_amount rule for
	// operation=sportsbook_bet is now evaluable, and DENIES once the
	// rolling-window staked total would exceed it.
	t.Run("a cumulative_amount rule for sportsbook_bet now denies", func(t *testing.T) {
		createTestRule(t, pool, &f.tenantID, CreateRuleParams{
			Operation: OperationSportsbookBet, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
			AssetCode: "EUR", Threshold: stake, RuleKind: RuleHardLimit,
		})
		decision, err := evaluateWithTenant(t, pool, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationSportsbookBet, AssetCode: "EUR", Amount: 1,
			LicensingMode: "under_platform_licence",
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if decision.Outcome != OutcomeDeny {
			t.Fatalf("expected DENY once already-staked %d + 1 exceeds threshold %d, got %+v", stake, stake, decision)
		}
	})
}

// TestSportsbookCumulative_FailsClosedOnEveryUnmeasurableConfiguration is
// internal/risk's half of ADR 0083 §12.2 item 31: every unmeasurable
// configuration of a sportsbook_bet cumulative rule produces a non-nil
// ERROR - never an ALLOW, and never a DENY dressed up as a decline (ADR
// 0083 §6.1.4). The bet-abort half of item 31 belongs to
// internal/sportsbook's own suite, since it is PlaceBet that propagates
// these errors; what is proven here is that they are raised at all.
func TestSportsbookCumulative_FailsClosedOnEveryUnmeasurableConfiguration(t *testing.T) {
	newFixtureWithRule := func(t *testing.T) (*db.Pool, fixture) {
		t.Helper()
		pool := testPool(t)
		f := seedFixture(t, pool)
		createTestRule(t, pool, &f.tenantID, CreateRuleParams{
			Operation: OperationSportsbookBet, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
			AssetCode: "EUR", Threshold: 1_000_000, RuleKind: RuleHardLimit,
		})
		return pool, f
	}
	request := func(f fixture) RiskRequest {
		return RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationSportsbookBet, AssetCode: "EUR", Amount: 1,
			LicensingMode: "under_platform_licence",
		}
	}

	t.Run("ErrInvalidCumulativeSpec when the spec is incomplete", func(t *testing.T) {
		pool, f := newFixtureWithRule(t)
		overrideSpecForTest(t, OperationSportsbookBet, cumulativeSpec{
			TransactionTypes: []string{"sportsbook_bet"},
			// No MeasuredAccountTypes, no ConsumingDirection.
		})
		decision, err := evaluateWithTenant(t, pool, request(f))
		if err == nil {
			t.Fatalf("expected an incomplete spec to fail closed, got %+v", decision)
		}
		assertErrorIs(t, err, ErrInvalidCumulativeSpec)
	})

	t.Run("ErrUnrecognizedCumulativeLeg when the posting shape grows an undeclared leg", func(t *testing.T) {
		pool, f := newFixtureWithRule(t)
		walletID := seedWalletFor(t, pool, f, "EUR", 1_000_000)
		postSportsbookBetShape(t, pool, f, walletID, "EUR", 500)
		spec := operationCumulativeSpecs[OperationSportsbookBet]
		spec.IgnoredAccountTypes = nil
		overrideSpecForTest(t, OperationSportsbookBet, spec)

		decision, err := evaluateWithTenant(t, pool, request(f))
		if err == nil {
			t.Fatalf("expected an undeclared player-side leg to fail closed, got %+v", decision)
		}
		assertErrorIs(t, err, ErrUnrecognizedCumulativeLeg)
	})

	t.Run("ErrConflictingRules when two equally specific rules match", func(t *testing.T) {
		pool := testPool(t)
		f := seedFixture(t, pool)
		for _, threshold := range []int64{100, 200} {
			createTestRule(t, pool, &f.tenantID, CreateRuleParams{
				PlayerAccountID: &f.playerID, Operation: OperationSportsbookBet, LimitKind: LimitCumulativeAmount,
				TimeWindow: WindowRollingHour, AssetCode: "EUR", Threshold: threshold, RuleKind: RuleConfigurableLimit,
			})
		}
		decision, err := evaluateWithTenant(t, pool, request(f))
		if err == nil {
			t.Fatalf("expected two equally-specific configurable rules to fail closed, got %+v", decision)
		}
		assertErrorIs(t, err, ErrConflictingRules)
	})

	t.Run("ErrPlayerScopedConnection when the transaction is not tenant-scoped", func(t *testing.T) {
		pool, f := newFixtureWithRule(t)
		err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := Evaluate(ctx, tx, request(f))
			return err
		})
		if err == nil {
			t.Fatal("expected a player-scoped connection to fail closed")
		}
		assertErrorIs(t, err, ErrPlayerScopedConnection)
	})

	// The one error sportsbook_bet must NO LONGER produce. Before this
	// entry existed, every cumulative_amount rule authored for it errored
	// on every bet with ErrUnsupportedCumulativeOperation (ADR 0083
	// §6.1.4's closed gap). The sentinel itself must remain live for
	// operations that genuinely have no spec - deposit is asserted here so
	// this sub-test cannot pass by the sentinel having been deleted.
	t.Run("ErrUnsupportedCumulativeOperation no longer applies to sportsbook_bet but still guards unwired operations", func(t *testing.T) {
		pool, f := newFixtureWithRule(t)
		if _, err := evaluateWithTenant(t, pool, request(f)); err != nil {
			t.Fatalf("a well-formed cumulative sportsbook_bet rule must now be evaluable, got: %v", err)
		}
		if _, ok := operationCumulativeSpecs[OperationDeposit]; ok {
			t.Skip("deposit gained a cumulative spec; pick another unwired operation for this control")
		}
		createTestRule(t, pool, &f.tenantID, CreateRuleParams{
			Operation: OperationDeposit, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
			AssetCode: "EUR", Threshold: 100, RuleKind: RuleHardLimit,
		})
		depositReq := request(f)
		depositReq.Operation = OperationDeposit
		decision, err := evaluateWithTenant(t, pool, depositReq)
		if err == nil {
			t.Fatalf("expected an unwired operation's cumulative rule to still fail closed, got %+v", decision)
		}
		assertErrorIs(t, err, ErrUnsupportedCumulativeOperation)
	})
}
