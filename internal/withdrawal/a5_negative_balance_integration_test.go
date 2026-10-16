//go:build integration

// A5 / owner decision 13: a withdrawal from a wallet with a negative balance is refused by the WD-RG-1
// gate (WD050, reason wallet_negative_balance) before any row or hold is written, the denial audit is
// committed and the idempotency key is not consumed; once a deposit (or a governed credit) brings the
// balance back to zero or above, the same key succeeds.
package withdrawal

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
)

func TestA5Neg_WithdrawalRefusedOnNegativeWallet_UntilCleared(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := negbal.Chargeback(ctx, tx, f.tenantID, f.walletID, "EUR", 1_300) // spent, then charged back: -300
		return err
	})
	key := "a5neg-" + uuid.NewString()
	_, err := reqCommitDenial(t, pool, f, baseParams(f, key, 100))
	var denied *GateDeniedError
	if !errors.As(err, &denied) || denied.GateCode != GateCodeRG || denied.Reason != rg.CodeWalletNegativeBalance {
		t.Fatalf("want a WD050 denial %s, got %v", rg.CodeWalletNegativeBalance, err)
	}
	if n := fpRequestCount(t, f); n != 0 {
		t.Fatalf("no request row may be written, got %d", n)
	}
	rows := auditRows(t, pool, f, "withdrawal.denied_by_rg")
	if len(rows) != 1 || rows[0]["reason_code"] != rg.CodeWalletNegativeBalance {
		t.Fatalf("the denial audit must be committed: %v", rows)
	}
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return negbal.Deposit(ctx, tx, f.tenantID, f.walletID, "EUR", 500) // back to +200
	})
	if wr, err := req(t, pool, f, baseParams(f, key, 100)); err != nil || wr.State != StateRequested {
		t.Fatalf("once cleared, the same key succeeds: %v %+v", err, wr)
	}
}

func fpRequestCount(t *testing.T, f fixture) int {
	t.Helper()
	var n int
	mustRunTx(t, testPool(t), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE player_account_id = $1`, f.playerAccountID).Scan(&n)
	})
	return n
}
