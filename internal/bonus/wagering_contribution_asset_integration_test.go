//go:build integration

// Stage 4H-B1 Wave 3 Phase 4 (risk) regression suite for
// DR-4HB1W3-RISK-01: a wagering contribution whose own asset differs from
// the Grant's asset must be refused outright, never silently summed into
// that Grant's P_firm.
//
// Why this matters, stated once: DeriveWageringProgress (wagering.go) sums
// every contribution row's QualifyingScaled as one bare minor-unit total,
// and CheckAndCompleteGrant/ConvertGrant compare that total against a
// target derived from the Grant's own granted_amount in the GRANT's asset
// (wagering_contribution_entry.go WageringTargetScaled). A contribution
// recorded in a different asset therefore corrupts a value-authorizing
// comparison across decimal exponents (ADR 0007's multi-wallet model gives
// a single player wallets at exponent 2, 8 and 18 simultaneously) - which
// is precisely the cross-denomination comparison ADR 0031 §34/§35 and
// internal/risk/denomination.go forbid at every other value-authorizing
// boundary on this platform.
package bonus

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestRecordWageringContribution_CrossAssetContributionIsRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	// A SECOND, independently-registered asset - the ordinary multi-wallet
	// case (ADR 0007), not a contrived one. It needs only to EXIST here;
	// the guard under test fires before any authorization/gate work.
	otherAsset := newBonusTestAssetCode()
	createAndActivateAsset(t, pool, otherAsset, seedPlatformAdminForBonus(t, pool), seedPlatformAdminForBonus(t, pool))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "cross-asset-1")
		result, outcome, err := IssueAndActivateGenericWageringBonus(ctx, tx, GenericWageringBonusParams{
			Grant: g, Amount: big.NewInt(1000), ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode,
		})
		if err != nil || !outcome.Allowed {
			t.Fatalf("issue/activate: %v / %+v", err, outcome)
		}

		lockTxID := uuid.New()
		correlationID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1, $2, 'casino_bet', $3, $4)`,
			lockTxID, f.tenantID, "bet-"+lockTxID.String(), correlationID,
		); err != nil {
			return err
		}

		// The contribution names a DIFFERENT asset than the Grant's own.
		err = RecordWageringContribution(ctx, tx, f.tenantID, result.ID, RecordWageringContributionParams{
			OfferVersionID: g.OfferVersionID, LockLedgerTransactionID: lockTxID, CorrelationID: correlationID,
			AssetCode: otherAsset, StakedBonusAmount: big.NewInt(500), ContributionWeightBP: 10000, QualifyingScaled: big.NewInt(500),
		})
		if !errors.Is(err, ErrWageringContributionAssetMismatch) {
			t.Fatalf("expected ErrWageringContributionAssetMismatch for a contribution denominated in %s against a %s Grant, got %v", otherAsset, f.assetCode, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing was recorded: fail-closed means no row, not a partial one.
	// (Read in a fresh transaction - the one above deliberately left the
	// mismatched write refused mid-transaction.)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM bonus_wagering_progress w
			   JOIN bonus_grants g ON g.id = w.grant_id
			  WHERE w.tenant_id = $1 AND w.asset_code <> g.asset_code`, f.tenantID,
		).Scan(&count)
	})
	if err != nil {
		t.Fatal(err)
	}
}
