//go:build integration

// Stage 4H-B1 Wave 3 Phase 3 (item D) test suite for the Grant-expiry
// sweep (expiry_sweep.go). Proves it uses the EXISTING TerminateGrant
// machinery unchanged (both the simple case and N1.4's own AOE-non-empty
// deferral to pending_settlement), never a parallel termination path.
package bonus

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func TestRunExpirySweepForTenant_TerminatesExpiredGrant(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "expiry-1")
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{
			Amount: big.NewInt(1000), ActorType: ActorSystem,
		})
		grantID = result.ID
		// expires_at is set by ActivateGrant only when WageringTimeLimit
		// is supplied (nil here) - write it directly for this test, since
		// SetGrantExpiryOnce is exactly the function ActivateGrant itself
		// would have called had a wagering_time_limit been configured.
		_, err := SetGrantExpiryOnce(ctx, tx, f.tenantID, grantID, time.Now().UTC().Add(10*time.Millisecond))
		return err
	})
	if err != nil {
		t.Fatalf("seed expired grant: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	var outcome ExpirySweepOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunExpirySweepForTenant(ctx, tx, f.tenantID, uuid.Nil, time.Now().UTC())
		return runErr
	})
	if err != nil {
		t.Fatalf("RunExpirySweepForTenant: %v", err)
	}
	if outcome.GrantsExamined != 1 || outcome.GrantsTerminated != 1 {
		t.Fatalf("expected exactly one grant examined and terminated, got %+v", outcome)
	}

	status := readGrantStatus(t, pool, f.tenantID, grantID)
	if status != GrantExpired {
		t.Fatalf("expected the grant to be expired, got %s", status)
	}

	entries, err := readGrantProgressReasonCodes(t, pool, f.tenantID, grantID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rc := range entries {
		if rc == ExpirySweepReasonCode {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a Progress entry carrying reason_code %q (doc 10 §10's own completeness mandate), got %v", ExpirySweepReasonCode, entries)
	}
}

func TestRunExpirySweepForTenant_DoesNotTouchNotYetExpiredGrants(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "expiry-not-yet")
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{
			Amount: big.NewInt(1000), ActorType: ActorSystem,
		})
		grantID = result.ID
		_, err := SetGrantExpiryOnce(ctx, tx, f.tenantID, grantID, time.Now().UTC().Add(24*time.Hour))
		return err
	})
	if err != nil {
		t.Fatalf("seed not-yet-expired grant: %v", err)
	}

	var outcome ExpirySweepOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunExpirySweepForTenant(ctx, tx, f.tenantID, uuid.Nil, time.Now().UTC())
		return runErr
	})
	if err != nil {
		t.Fatalf("RunExpirySweepForTenant: %v", err)
	}
	if outcome.GrantsExamined != 0 {
		t.Fatalf("expected zero grants examined (not yet expired), got %d", outcome.GrantsExamined)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != GrantActivated {
		t.Fatalf("expected the grant to remain activated, got %s", status)
	}
}

// TestRunExpirySweepForTenant_OpenExposureDefersToPendingSettlement
// proves the expiry sweep inherits N1.4's own pending_settlement
// deferral for free, via TerminateGrant, rather than the sweep needing
// its own AOE-awareness.
func TestRunExpirySweepForTenant_OpenExposureDefersToPendingSettlement(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	correlationID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "expiry-open-exposure")
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{
			Amount: big.NewInt(1000), ActorType: ActorSystem,
		})
		grantID = result.ID

		lockTxID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1, $2, 'casino_bet', $3, $4)`,
			lockTxID, f.tenantID, "bet-"+lockTxID.String(), correlationID,
		); err != nil {
			return err
		}
		if err := RecordWageringContribution(ctx, tx, f.tenantID, grantID, RecordWageringContributionParams{
			OfferVersionID: g.OfferVersionID, LockLedgerTransactionID: lockTxID, CorrelationID: correlationID,
			AssetCode: f.assetCode, StakedBonusAmount: big.NewInt(500), ContributionWeightBP: 10000, QualifyingScaled: big.NewInt(500),
		}); err != nil {
			return err
		}
		_, err := SetGrantExpiryOnce(ctx, tx, f.tenantID, grantID, time.Now().UTC().Add(10*time.Millisecond))
		return err
	})
	if err != nil {
		t.Fatalf("seed grant with open exposure: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	var outcome ExpirySweepOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunExpirySweepForTenant(ctx, tx, f.tenantID, uuid.Nil, time.Now().UTC())
		return runErr
	})
	if err != nil {
		t.Fatalf("RunExpirySweepForTenant: %v", err)
	}
	if outcome.GrantsDeferred != 1 || outcome.GrantsTerminated != 0 {
		t.Fatalf("expected the grant to be DEFERRED to pending_settlement (open in-flight exposure), got %+v", outcome)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != GrantPendingSettlement {
		t.Fatalf("expected pending_settlement, got %s", status)
	}
}

func readGrantStatus(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) GrantStatus {
	t.Helper()
	var status GrantStatus
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := GetGrantByID(ctx, tx, grantID)
		if err != nil {
			return err
		}
		status = g.Status
		return nil
	})
	if err != nil {
		t.Fatalf("read grant status: %v", err)
	}
	return status
}

func readGrantProgressReasonCodes(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) ([]string, error) {
	t.Helper()
	var codes []string
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		entries, err := ListGrantProgress(ctx, tx, tenantID, grantID)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.ReasonCode != nil {
				codes = append(codes, *e.ReasonCode)
			}
		}
		return nil
	})
	return codes, err
}
