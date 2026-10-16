//go:build integration

// A5 / owner decision 13 (2026-10-15): negative-balance policy "block until cleared". A wallet whose
// player_cash (or player_locked_cash) balance is negative is refused for bets (casino and sportsbook),
// launches, bonus (deposit-less) play and withdrawals, with the closed code wallet_negative_balance; a
// deposit is never blocked; the block lifts by itself once the balance is no longer negative; another
// wallet of the same player is unaffected.
package rg

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

func a5Decisions(t *testing.T, a account, walletID uuid.UUID) map[string]Decision {
	t.Helper()
	pool := testPool(t)
	out := map[string]Decision{}
	elig := EligibilityParams{TenantID: a.tenantID, BrandID: a.brandID, PlayerAccountID: a.accountID, WalletID: walletID}
	if err := pool.WithTenant(context.Background(), a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := EvaluateEligibility(ctx, tx, elig)
		if err != nil {
			return err
		}
		out["eligibility (launch, bonus)"] = d
		if d, err = EvaluateWithdrawalEligibility(ctx, tx, elig); err != nil {
			return err
		}
		out["withdrawal"] = d
		for name, act := range map[string]Action{"casino wager": ActionCasinoWager, "sportsbook wager": ActionSportsbookWager, "deposit": ActionDeposit, "casino launch": ActionCasinoLaunch} {
			var amount int64 = 100
			if act == ActionCasinoLaunch {
				amount = 0
			}
			d, err := EvaluateAction(ctx, tx, ActionParams{TenantID: a.tenantID, BrandID: a.brandID, PlayerAccountID: a.accountID,
				WalletID: walletID, Action: act, AssetCode: "EUR", Amount: amount})
			if err != nil {
				return err
			}
			out[name] = d
		}
		return nil
	}); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return out
}

func TestA5_NegativeBalance_BlocksPlayAndWithdrawal_NeverDeposit_LiftsWhenCleared(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)
	a := seedAccount(t, pool, tenantID, uuid.Nil)
	var usdWallet uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := negbal.Deposit(ctx, tx, tenantID, a.walletID, "EUR", 100); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, tenantID, a.brandID, a.accountID, "USD")
		usdWallet = w.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, d := range a5Decisions(t, a, a.walletID) {
		if !d.Allowed {
			t.Fatalf("a positive wallet: %s refused (%s)", name, d.Code)
		}
	}
	// A chargeback of 250 against 100 already spent... the wallet is at -150.
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := negbal.Chargeback(ctx, tx, tenantID, a.walletID, "EUR", 250)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, d := range a5Decisions(t, a, a.walletID) {
		if name == "deposit" {
			if !d.Allowed {
				t.Fatalf("a deposit is never blocked by a negative balance, got %s", d.Code)
			}
			continue
		}
		if d.Allowed || d.Code != CodeWalletNegativeBalance {
			t.Fatalf("%s on a negative wallet: allowed=%v code=%q, want %q", name, d.Allowed, d.Code, CodeWalletNegativeBalance)
		}
	}
	// The player's other wallet (another asset) is not blocked.
	for name, d := range a5Decisions(t, a, usdWallet) {
		if name == "casino wager" || name == "sportsbook wager" || name == "deposit" || name == "casino launch" {
			continue // EUR-denominated actions; the USD wallet's eligibility is what matters here
		}
		if !d.Allowed {
			t.Fatalf("the other wallet must stay eligible: %s refused (%s)", name, d.Code)
		}
	}
	// Partially cured (-50): still blocked. Fully cured (0): the block lifts without any flag to clear.
	for _, c := range []struct {
		deposit int64
		blocked bool
	}{{100, true}, {50, false}} {
		if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return negbal.Deposit(ctx, tx, tenantID, a.walletID, "EUR", c.deposit)
		}); err != nil {
			t.Fatal(err)
		}
		d := a5Decisions(t, a, a.walletID)["casino wager"]
		if blocked := !d.Allowed && d.Code == CodeWalletNegativeBalance; blocked != c.blocked {
			t.Fatalf("after +%d: blocked=%v want %v (code %q)", c.deposit, blocked, c.blocked, d.Code)
		}
	}
}

// A negative player_locked_cash (the OB-1 / lock-release residual) blocks the same way.
func TestA5_NegativeLockedCash_Blocks(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)
	a := seedAccount(t, pool, tenantID, uuid.Nil)
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
			ledger.AccountSpec{WalletID: &a.walletID, AccountType: ledger.AccountPlayerLockedCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		// The projection carries the sign; a debit-only locked-cash row is exactly the shape the gate reads.
		_, err = tx.Exec(ctx, `INSERT INTO wallet_balance_projection (ledger_account_id, tenant_id, wallet_id, player_account_id, asset_code, account_type, debit_total, credit_total)
			VALUES ($1, $2, $3, $4, 'EUR', 'player_locked_cash', 5, 0)
			ON CONFLICT (ledger_account_id) DO UPDATE SET debit_total = 5, credit_total = 0`, accts[0], tenantID, a.walletID, a.accountID)
		return err
	}); err != nil {
		t.Skipf("the projection row cannot be planted in this database: %v", err)
	}
	d := a5Decisions(t, a, a.walletID)["casino wager"]
	if d.Allowed || d.Code != CodeWalletNegativeBalance {
		t.Fatalf("negative locked cash: allowed=%v code=%q", d.Allowed, d.Code)
	}
}
