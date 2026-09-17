//go:build integration

// Stage 4H-B0-R6 Workstream D, Part 1: the regression tests for ADR 0031
// §32(a)'s latent fail-OPEN, now fixed per §33.
//
// The bug: Rule.breach()'s cumulative-usage query summed
// (debit - credit) over ledger_entries filtered by tenant/player/asset/
// transaction_type WITHOUT joining ledger_accounts, so it was blind to
// account_type. That is correct only for a posting shape whose single
// player-owned leg is the measured one (casino_bet, whose house_gaming
// counterparty is wallet-less). For a transaction posting TWO
// player-owned legs, player_account_id is denormalized identically onto
// both and they cancel - usage computes as ZERO no matter how much was
// staked.
//
// These tests therefore do NOT re-test casino_bet's shape (which passed
// before the fix, by accident). They post a REAL two-player-owned-leg
// ledger transaction - withdrawal_requested, Dr player_cash /
// Cr player_withdrawal_hold, Flow 3 step A - and prove:
//
//  1. the fixed, leg-aware query computes the full non-zero usage, AND
//     the pre-fix account-type-blind query computes exactly 0 against the
//     very same rows (the bug's own signature, asserted in the same test
//     so the regression cannot silently return);
//  2. Evaluate therefore DENIES a request that the pre-fix evaluator
//     would have allowed;
//  3. an undeclared player-side leg fails closed rather than
//     under-counting.
//
// The operation mapping is injected TEST-ONLY and removed by t.Cleanup:
// production's operationCumulativeSpecs still contains exactly one entry
// (casino_bet). This stage wires no new operation (no internal/sportsbook
// or withdrawal Risk call site exists) - it fixes the primitive so that
// whenever one is wired, it is correct on the first try.
package risk

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// seedWalletFor gives fixture f a wallet in assetCode and funds its
// player_cash account, so a two-player-leg transaction can be posted
// against a real, sufficiently funded account. The funding transaction is
// a manual_adjustment, which no cumulativeSpec in these tests measures -
// it cannot contaminate the usage under test.
func seedWalletFor(t *testing.T, pool *db.Pool, f fixture, assetCode string, fund int64) uuid.UUID {
	t.Helper()
	var walletID uuid.UUID
	reason := "test fixture funding"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerID, assetCode)
		if err != nil {
			return err
		}
		walletID = w.ID
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerCash, assetCode)
		if err != nil {
			return err
		}
		adj, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, assetCode)
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fund-" + uuid.New().String(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adj, Direction: ledger.Debit, Amount: fund},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: fund},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed wallet: %v", err)
	}
	return walletID
}

// postTwoPlayerOwnedLegTransaction posts the real Flow 3 step A shape:
// Dr player_cash / Cr player_withdrawal_hold, BOTH of which are
// wallet-scoped (so ledger_entries.player_account_id is non-NULL on both
// legs - the exact condition that made the pre-fix query net to zero).
func postTwoPlayerOwnedLegTransaction(t *testing.T, pool *db.Pool, f fixture, walletID uuid.UUID, assetCode string, amount int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, ledger.AccountPlayerCash, assetCode)
		if err != nil {
			return err
		}
		hold, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, ledger.AccountPlayerWithdrawalHold, assetCode)
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalRequested,
			IdempotencyKey: "hold-" + uuid.New().String(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cash, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: hold, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("post two-player-owned-leg transaction: %v", err)
	}
}

// preFixBlindUsage reproduces the EXACT pre-fix query (no ledger_accounts
// join, no account_type awareness) so each test can assert the bug's own
// signature - a zero total - against the same rows the fixed query reads.
func preFixBlindUsage(t *testing.T, pool *db.Pool, f fixture, assetCode string, txTypes []string, windowStart time.Time) int64 {
	t.Helper()
	var n pgtype.Numeric
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(CASE WHEN le.direction = 'debit' THEN le.amount ELSE -le.amount END), 0)
			 FROM ledger_entries le
			 JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id
			 WHERE le.tenant_id = $1
			   AND le.player_account_id = $2
			   AND le.asset_code = $3
			   AND lt.transaction_type = ANY($4)
			   AND le.created_at >= $5`,
			f.tenantID, f.playerID, assetCode, txTypes, windowStart,
		).Scan(&n)
	})
	if err != nil {
		t.Fatalf("pre-fix blind usage query: %v", err)
	}
	got, err := numericToBigInt(n)
	if err != nil {
		t.Fatalf("convert pre-fix usage: %v", err)
	}
	return got.Int64()
}

// injectTestOnlySpec adds a cumulativeSpec for op for the duration of one
// test and removes it afterwards, so production's own map (exactly one
// entry: casino_bet) is never widened by a test. OperationWithdrawal is
// used because migration 0041's `operation` CHECK must accept the value
// for the rule row to exist at all, and withdrawal has no production spec
// and no Risk call site - so no production behavior depends on it.
func injectTestOnlySpec(t *testing.T, op Operation, spec cumulativeSpec) {
	t.Helper()
	if _, exists := operationCumulativeSpecs[op]; exists {
		t.Fatalf("refusing to shadow a PRODUCTION cumulative spec for %s in a test", op)
	}
	operationCumulativeSpecs[op] = spec
	t.Cleanup(func() { delete(operationCumulativeSpecs, op) })
}

func TestEvaluate_CumulativeUsageIsLegAwareForATwoPlayerOwnedLegOperation(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	walletID := seedWalletFor(t, pool, f, "EUR", 1_000_000)

	const held int64 = 1000
	postTwoPlayerOwnedLegTransaction(t, pool, f, walletID, "EUR", held)

	spec := cumulativeSpec{
		TransactionTypes:     []string{string(ledger.TxWithdrawalRequested)},
		MeasuredAccountTypes: []string{"player_cash"},
		IgnoredAccountTypes:  []string{"player_withdrawal_hold"},
		ConsumingDirection:   directionDebit,
	}
	injectTestOnlySpec(t, OperationWithdrawal, spec)

	windowStart := time.Now().UTC().Add(-time.Hour)

	// (1) The bug's own signature: the pre-fix, account-type-blind query
	// nets the two player-owned legs to exactly zero.
	if blind := preFixBlindUsage(t, pool, f, "EUR", spec.allTransactionTypes(), windowStart); blind != 0 {
		t.Fatalf("expected the pre-fix account-type-blind query to net two player-owned legs to 0 (the bug), got %d", blind)
	}

	// (2) The fixed, leg-aware query sees the full usage.
	var usage int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := cumulativeUsage(ctx, tx, spec, RiskRequest{
			TenantID: f.tenantID, PlayerAccountID: f.playerID, AssetCode: "EUR", Operation: OperationWithdrawal,
		}, windowStart)
		if err != nil {
			return err
		}
		usage = got.Int64()
		return nil
	})
	if err != nil {
		t.Fatalf("leg-aware cumulative usage: %v", err)
	}
	if usage != held {
		t.Fatalf("expected leg-aware cumulative usage %d, got %d", held, usage)
	}

	// (3) End to end through Evaluate: a cumulative cap of `held` is
	// breached by one more unit. The pre-fix evaluator would have
	// computed 0 + 1 <= 1000 and ALLOWED - the fail-open.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationWithdrawal, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
		AssetCode: "EUR", Threshold: held, RuleKind: RuleHardLimit,
	})
	var decision RiskDecision
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationWithdrawal, AssetCode: "EUR", Amount: 1,
			LicensingMode: "under_platform_licence",
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeDeny {
		t.Fatalf("expected DENY once the two-player-leg usage is measured correctly (usage %d + 1 > threshold %d), got %+v", held, held, decision)
	}
}

// TestEvaluate_UnrecognizedCumulativeLegFailsClosed proves the
// self-defending half of ADR 0031 §33: if the posting shape contains a
// player-side leg the spec declares neither as measured nor as ignored
// (which is what would happen if the ledger's shape changed under Risk -
// a bonus-funded stake leg, a new hold account, a widened
// transaction-type set), the evaluation STOPS instead of silently
// under-counting usage.
func TestEvaluate_UnrecognizedCumulativeLegFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	walletID := seedWalletFor(t, pool, f, "EUR", 1_000_000)
	postTwoPlayerOwnedLegTransaction(t, pool, f, walletID, "EUR", 500)

	// Identical to the previous test's spec, minus the declaration of the
	// player_withdrawal_hold counterparty.
	injectTestOnlySpec(t, OperationWithdrawal, cumulativeSpec{
		TransactionTypes:     []string{string(ledger.TxWithdrawalRequested)},
		MeasuredAccountTypes: []string{"player_cash"},
		ConsumingDirection:   directionDebit,
	})
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationWithdrawal, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
		AssetCode: "EUR", Threshold: 1_000_000, RuleKind: RuleHardLimit,
	})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationWithdrawal, AssetCode: "EUR", Amount: 1,
			LicensingMode: "under_platform_licence",
		})
		return err
	})
	if err == nil {
		t.Fatal("expected an undeclared player-side ledger leg to fail closed, got a decision")
	}
	assertErrorIs(t, err, ErrUnrecognizedCumulativeLeg)
}

// TestEvaluate_IncompleteCumulativeSpecFailsClosed proves a spec that
// omits any of the three facts §33 requires is refused rather than
// defaulted - the default is exactly what produced the original fail-open.
func TestEvaluate_IncompleteCumulativeSpecFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	injectTestOnlySpec(t, OperationWithdrawal, cumulativeSpec{
		TransactionTypes: []string{string(ledger.TxWithdrawalRequested)},
		// No MeasuredAccountTypes, no ConsumingDirection.
	})
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationWithdrawal, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
		AssetCode: "EUR", Threshold: 100, RuleKind: RuleHardLimit,
	})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationWithdrawal, AssetCode: "EUR", Amount: 1,
			LicensingMode: "under_platform_licence",
		})
		return err
	})
	if err == nil {
		t.Fatal("expected an incomplete cumulative spec to fail closed")
	}
	assertErrorIs(t, err, ErrInvalidCumulativeSpec)
}

// TestCasinoBetCumulativeSpecIsUnchangedAndComplete pins the ONE
// production spec: casino_bet, measuring the player_cash debit and
// netting casino_rollback. A future author who widens
// operationCumulativeSpecs must not quietly change casino's own
// measurement, and every spec in the map must be complete.
func TestCasinoBetCumulativeSpecIsUnchangedAndComplete(t *testing.T) {
	spec, ok := operationCumulativeSpecs[OperationCasinoBet]
	if !ok {
		t.Fatal("casino_bet must have a cumulative spec - it is the only wired operation")
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("casino_bet spec is incomplete: %v", err)
	}
	if len(spec.TransactionTypes) != 1 || spec.TransactionTypes[0] != "casino_bet" {
		t.Fatalf("unexpected casino_bet transaction types: %v", spec.TransactionTypes)
	}
	if len(spec.ReversalTypes) != 1 || spec.ReversalTypes[0] != "casino_rollback" {
		t.Fatalf("casino_bet must net casino_rollback and nothing else, got %v", spec.ReversalTypes)
	}
	if len(spec.MeasuredAccountTypes) != 1 || spec.MeasuredAccountTypes[0] != "player_cash" {
		t.Fatalf("casino_bet must measure player_cash only (a bonus-funded stake leg must be added deliberately), got %v", spec.MeasuredAccountTypes)
	}
	if spec.ConsumingDirection != directionDebit {
		t.Fatalf("a stake consumes capacity on debit, got %q", spec.ConsumingDirection)
	}
	for op, s := range operationCumulativeSpecs {
		if err := s.validate(); err != nil {
			t.Fatalf("cumulative spec for %s is incomplete: %v", op, err)
		}
	}
}
