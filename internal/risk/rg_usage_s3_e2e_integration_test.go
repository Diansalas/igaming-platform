//go:build integration

// ADR 0115 S3 (ledger-finance review F-g): the RG usage measurement is checked against ledgers produced by the REAL
// sportsbook place / settle / void / rollback code, not hand-built rows. External test package for the same
// import-cycle reason as cumulative_sportsbook_settlement_integration_test.go.
package risk_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/rg"
)

func TestRGUsageS3_RealSportsbookPostings(t *testing.T) {
	pool := sbSharedPool(t)
	f := seedSBRiskFixture(t, pool, sbProbeThreshold)

	// B1: 100 lost.                       wager 100  loss +100
	b1 := mustPlaceSBBet(t, pool, f, 100)
	applySBStep(t, pool, f, b1, settleStep(1, "lost"))
	// B2: 100 won (payout 200 at odds 2.0). wager 100  loss 100 - 200 = -100
	b2 := mustPlaceSBBet(t, pool, f, 100)
	applySBStep(t, pool, f, b2, settleStep(1, "won"))
	// B3: 60 voided while open.            wager 60 - 60 = 0  loss 0
	b3 := mustPlaceSBBet(t, pool, f, 60)
	applySBStep(t, pool, f, b3, voidStep())
	// B4: 40 won (payout 80), settlement rolled back. A settlement rollback is NOT a stake reversal.
	//                                      wager 40  loss 40 - 80 + 80 = 40
	b4 := mustPlaceSBBet(t, pool, f, 40)
	applySBStep(t, pool, f, b4, settleStep(1, "won"))
	applySBStep(t, pool, f, b4, rollbackStep(1))
	// B5: 30 won (payout 60), then voided after settlement (rollback of the settlement + void of the stake).
	//                                      wager 30 - 30 = 0  loss 30 - 60 + 60 - 30 = 0
	b5 := mustPlaceSBBet(t, pool, f, 30)
	applySBStep(t, pool, f, b5, settleStep(1, "won"))
	applySBStep(t, pool, f, b5, voidStep())

	var u rg.StakeUsage
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = rg.MeasureStakeUsage(ctx, tx, rg.UsageRequest{TenantID: f.tenantID, PlayerAccountID: f.playerID, Scope: rg.LimitScopeTenantPerson,
			AssetCode: sbAsset, Windows: []rg.WindowSpec{{Period: rg.PeriodRolling24h}, {Period: rg.PeriodCalendarDay, CalendarTZ: "UTC"}}})
		return err
	}); err != nil {
		t.Fatalf("measure: %v", err)
	}
	w := u.Windows[0]
	if w.Wager.Account.String() != "240" || w.Loss.Account.String() != "40" || w.Wager.Person.String() != "240" {
		t.Fatalf("real sportsbook ledger: want wager 240 / loss 40, got wager %s / loss %s (person wager %s)", w.Wager.Account, w.Loss.Account, w.Wager.Person)
	}
}
