//go:build integration

// ADR 0115 S3 (ledger-finance review F-g): the RG usage measurement checked against ledgers produced by the REAL casino
// postBet / postWin / postRollback code (not hand-built rows).
package casino

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/rg"
)

func TestRGUsageS3_RealCasinoPostings(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()

	deliver := func(ev CallbackEventType, txID, orig, round string, amount int64, session uuid.UUID) {
		t.Helper()
		outcome := OutcomeSucceeded
		if ev == CallbackEventRollback {
			outcome = ""
		}
		payload := provider.CallbackPayload(f.tenantID, ev, txID, orig, round, "game-1", amount, "EUR", outcome, "", f.playerRef, session)
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
			return err
		}); err != nil {
			t.Fatalf("%s %s: %v", ev, txID, err)
		}
	}
	// R1: bet 1000, win 2500, then the WIN is rolled back (a payout correction).  wager 1000  loss 1000 - 2500 + 2500 = 1000
	deliver(CallbackEventBet, "b1", "", "r1", 1000, sessionID)
	deliver(CallbackEventWin, "w1", "", "r1", 2500, uuid.Nil)
	deliver(CallbackEventRollback, "rw1", "w1", "r1", 0, uuid.Nil)
	// R2: bet 700 rolled back (stake return).                                     wager 700 - 700 = 0  loss 0
	deliver(CallbackEventBet, "b2", "", "r2", 700, sessionID)
	deliver(CallbackEventRollback, "rb2", "b2", "r2", 0, uuid.Nil)
	// R3: bet 400, no win (a loss).                                                wager 400  loss 400
	deliver(CallbackEventBet, "b3", "", "r3", 400, sessionID)
	// R4: bet 300, win 100.                                                        wager 300  loss 200
	deliver(CallbackEventBet, "b4", "", "r4", 300, sessionID)
	deliver(CallbackEventWin, "w4", "", "r4", 100, uuid.Nil)

	var u rg.StakeUsage
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = rg.MeasureStakeUsage(ctx, tx, rg.UsageRequest{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID, Scope: rg.LimitScopeTenantPerson,
			AssetCode: "EUR", Windows: []rg.WindowSpec{{Period: rg.PeriodRolling24h}}})
		return err
	}); err != nil {
		t.Fatalf("measure: %v", err)
	}
	w := u.Windows[0]
	// wager 1000 + 0 + 400 + 300 = 1700; loss 1000 + 0 + 400 + 200 = 1600
	if w.Wager.Account.String() != "1700" || w.Loss.Account.String() != "1600" {
		t.Fatalf("real casino ledger: want wager 1700 / loss 1600, got wager %s / loss %s", w.Wager.Account, w.Loss.Account)
	}
	// The funding is a manual adjustment, not a deposit.
	var d rg.DepositUsage
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = rg.MeasureDepositUsage(ctx, tx, rg.UsageRequest{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID, Scope: rg.LimitScopeAccount,
			AssetCode: "EUR", Windows: []rg.WindowSpec{{Period: rg.PeriodRolling24h}}})
		return err
	}); err != nil {
		t.Fatalf("deposit usage: %v", err)
	}
	if d.Windows[0].Total.Account.Sign() != 0 {
		t.Fatalf("a manual-adjustment funding must not count as deposit usage, got %s", d.Windows[0].Total.Account)
	}
}
