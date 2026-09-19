//go:build integration

// Stage 4H-B1 Wave 3 Phase 3 (item C) test suite for the cashback
// scheduling job (cashback_scheduler.go). Seeds real casino_bet/
// casino_win ledger transactions directly (via internal/ledger.Post) -
// this package cannot import internal/casino at all (internal/casino
// already imports internal/bonus for the G-2 settlement seams, so the
// reverse import would be a cycle), mirroring
// internal/casino/bonus_settlement_integration_test.go's own documented
// posture of simulating a real posting shape directly rather than
// depending on the producing package's own orchestration.
package bonus

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func seedCasinoBetForCashback(t *testing.T, pool *db.Pool, f lifecycleFixture, amount int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, f.assetCode)
		if err != nil {
			return err
		}
		house, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountHouseGaming, f.assetCode)
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxCasinoBet, IdempotencyKey: "mock-casino:" + uuid.NewString(),
			CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cash, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: house, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed casino bet for cashback: %v", err)
	}
}

func seedCashbackOffer(t *testing.T, pool *db.Pool, f lifecycleFixture, rateBP int32, windowSeconds int64) campaignOffer {
	t.Helper()
	var co campaignOffer
	completionMechanic := "C2"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, Status: CampaignActive, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		if err != nil {
			return err
		}
		co.campaignID = c.ID
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		if err != nil {
			return err
		}
		co.campaignVersionID = v.ID
		if _, err := SetCampaignCurrentVersion(ctx, tx, f.tenantID, c.ID, v.ID); err != nil {
			return err
		}
		o, err := CreateOffer(ctx, tx, Offer{
			TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID,
			GrantPolicy: GrantPolicyAutoIssue, Status: OfferActive, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID,
		})
		if err != nil {
			return err
		}
		co.offerID = o.ID
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardPercentageWithCap, RewardAssetCode: f.assetCode,
			RewardCalculation:      []byte(`{"rate_bp":` + strconv.Itoa(int(rateBP)) + `,"window_seconds":` + strconv.FormatInt(windowSeconds, 10) + `}`),
			CompletionMechanic:     &completionMechanic,
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID,
		})
		if err != nil {
			return err
		}
		co.offerVersionID = ov.ID
		if _, err := SetOfferCurrentVersion(ctx, tx, f.tenantID, o.ID, ov.ID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed cashback offer: %v", err)
	}
	return co
}

func TestRunCashbackSchedulerForTenant_IssuesCashbackForNetLoss(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedCashbackOffer(t, pool, f, 1000, 86400) // 10% cashback, 1-day window
	seedCasinoBetForCashback(t, pool, f, 1000) // a 1000-unit net loss (no matching win)

	// Simulate the window having elapsed WITHOUT waiting a real day -
	// asOf is this function's own caller-supplied instant (RunCashback
	// SchedulerForTenant's own doc comment: "kept a pure input here for
	// testability").
	asOf := time.Now().UTC().Add(48 * time.Hour)

	var outcome CashbackSchedulerOutcome
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunCashbackSchedulerForTenant(ctx, tx, f.tenantID, uuid.Nil, asOf)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunCashbackSchedulerForTenant: %v", err)
	}
	if outcome.WindowsEvaluated < 1 {
		t.Fatalf("expected at least one elapsed window evaluated, got %d", outcome.WindowsEvaluated)
	}
	if outcome.GrantsDenied != 1 || outcome.GrantsIssued != 0 {
		// Same pre-existing, disclosed jurisdiction gap as the deposit
		// sweep (JurisdictionCode="") - the issuance ATTEMPT is what this
		// test proves happened, not that the gate allows it.
		t.Fatalf("expected exactly one denied (jurisdiction gap) cashback issuance attempt, got issued=%d denied=%d", outcome.GrantsIssued, outcome.GrantsDenied)
	}

	var grantCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, f.playerID).Scan(&grantCount)
	})
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grantCount != 1 {
		t.Fatalf("expected exactly one cashback grant attempt recorded for the player, got %d", grantCount)
	}

	// A second tick at the SAME asOf must not re-process the same window
	// (the watermark advanced) - proving §7.18.4 item 5's own idempotency
	// claim holds even though the watermark itself is "non-load-bearing
	// for correctness" (bonus_grants' own unique trigger_reference
	// constraint is the REAL backstop; this assertion proves the
	// performance-optimization watermark path also behaves correctly).
	var second CashbackSchedulerOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		second, runErr = RunCashbackSchedulerForTenant(ctx, tx, f.tenantID, uuid.Nil, asOf)
		return runErr
	})
	if err != nil {
		t.Fatalf("second RunCashbackSchedulerForTenant: %v", err)
	}
	if second.GrantsIssued != 0 && second.GrantsDenied != 0 {
		t.Fatalf("expected the second tick to attempt zero NEW issuances (watermark advanced past the window), got issued=%d denied=%d", second.GrantsIssued, second.GrantsDenied)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, f.playerID).Scan(&grantCount)
	})
	if err != nil {
		t.Fatalf("count grants after second tick: %v", err)
	}
	if grantCount != 1 {
		t.Fatalf("expected still exactly one grant after a second identical tick (no duplicate), got %d", grantCount)
	}
}

func TestRunCashbackSchedulerForTenant_NoLossNoGrant(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedCashbackOffer(t, pool, f, 1000, 86400)
	// No casino_bet activity at all in the window.

	asOf := time.Now().UTC().Add(48 * time.Hour)
	var outcome CashbackSchedulerOutcome
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunCashbackSchedulerForTenant(ctx, tx, f.tenantID, uuid.Nil, asOf)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunCashbackSchedulerForTenant: %v", err)
	}
	if outcome.GrantsIssued != 0 || outcome.GrantsDenied != 0 {
		t.Fatalf("expected zero issuance attempts with no qualifying play, got issued=%d denied=%d", outcome.GrantsIssued, outcome.GrantsDenied)
	}
}
