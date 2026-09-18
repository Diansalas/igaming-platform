//go:build integration

// Stage 4H-B1 Wave 2 Phase 4: proves operationCumulativeSpecs'
// PRODUCTION OperationBonusConversion entry (cumulative.go) is correct
// against a REAL bonus_conversion posting - including the Rule B2
// (extended) mirror generator's own automatically-appended
// promo_liability/bonus_expense legs (internal/ledger/bonus_mirror.go) -
// not merely a hand-rolled, test-injected spec the way
// cumulative_leg_integration_test.go proves the underlying primitive.
// Unlike that file's injectTestOnlySpec helper, this test reads
// operationCumulativeSpecs[OperationBonusConversion] exactly as shipped.
package risk

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// postBonusConversion funds a fresh player_bonus balance via a real
// bonus_grant posting, then converts part of it to cash via a real
// bonus_conversion posting - both going through ledger.Post unmodified,
// so Rule B2's mirror generator adds the promo_liability/bonus_expense
// legs exactly as it would for internal/bonus's own ConvertGrant.
func postBonusConversion(t *testing.T, pool *db.Pool, f fixture, assetCode string, grantAmount, convertAmount int64) uuid.UUID {
	t.Helper()
	operatorCost := &ledger.BonusCostAttribution{Funding: ledger.FundingOperator}
	var playerAccountID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerID, assetCode)
		if err != nil {
			return err
		}
		playerBonus, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerBonus, assetCode)
		if err != nil {
			return err
		}
		playerCash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerCash, assetCode)
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxBonusGrant,
			IdempotencyKey: "bonus_grant:" + uuid.New().String(), CorrelationID: uuid.New(),
			Entries:   []ledger.EntryInput{{LedgerAccountID: playerBonus, Direction: ledger.Credit, Amount: grantAmount}},
			BonusCost: operatorCost,
		}); err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxBonusConversion,
			IdempotencyKey: "bonus_conversion:" + uuid.New().String(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: playerBonus, Direction: ledger.Debit, Amount: convertAmount},
				{LedgerAccountID: playerCash, Direction: ledger.Credit, Amount: convertAmount},
			},
			BonusCost: operatorCost,
		}); err != nil {
			return err
		}
		playerAccountID = f.playerID
		return nil
	})
	if err != nil {
		t.Fatalf("post bonus grant + conversion: %v", err)
	}
	return playerAccountID
}

// TestOperationBonusConversionCumulativeSpec_MeasuresRealPosting proves
// the shipped OperationBonusConversion cumulativeSpec is complete and
// correct against a REAL four-leg bonus_conversion posting: it measures
// exactly the player_cash credit, is not tripped into
// ErrUnrecognizedCumulativeLeg by the player_bonus counterparty leg it
// declares Ignored, and never even sees the wallet-less promo_liability/
// bonus_expense mirror legs (no player_account_id on either).
func TestOperationBonusConversionCumulativeSpec_MeasuresRealPosting(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	postBonusConversion(t, pool, f, "EUR", 1000, 300)

	spec, ok := operationCumulativeSpecs[OperationBonusConversion]
	if !ok {
		t.Fatal("expected a PRODUCTION cumulativeSpec for OperationBonusConversion (Stage 4H-B1 Wave 2 Phase 4)")
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("OperationBonusConversion spec is incomplete: %v", err)
	}

	windowStart := time.Now().UTC().Add(-time.Hour)
	var usage int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := cumulativeUsage(ctx, tx, spec, RiskRequest{
			TenantID: f.tenantID, PlayerAccountID: f.playerID, AssetCode: "EUR", Operation: OperationBonusConversion,
		}, windowStart)
		if err != nil {
			return err
		}
		usage = got.Int64()
		return nil
	})
	if err != nil {
		t.Fatalf("cumulative usage against a real bonus_conversion posting: %v", err)
	}
	if usage != 300 {
		t.Fatalf("expected measured usage 300 (the released amount, not the 1000 granted), got %d", usage)
	}
}

// TestEvaluate_BonusConversionCumulativeAmountDenies proves the spec is
// wired end to end through Evaluate: a real HARD_LIMIT cumulative_amount
// rule on bonus_conversion actually DENYs once the rolling-window
// released total would exceed it.
func TestEvaluate_BonusConversionCumulativeAmountDenies(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	postBonusConversion(t, pool, f, "EUR", 1000, 300)

	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationBonusConversion, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
		AssetCode: "EUR", Threshold: 300, RuleKind: RuleHardLimit,
	})

	var decision RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decision, err = Evaluate(ctx, tx, RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: OperationBonusConversion, AssetCode: "EUR", Amount: 1,
			LicensingMode: "under_platform_licence",
		})
		return err
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if decision.Outcome != OutcomeDeny {
		t.Fatalf("expected DENY once already-released 300 + 1 exceeds threshold 300, got %+v", decision)
	}
}
