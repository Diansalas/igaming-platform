//go:build integration

// ADR 0113 slice 1 (migration 0132) fixture support for this package. internal/withdrawal imports this
// package, so the shared legal-path helper (internal/testsupport/wdpath) cannot be imported here; these
// two functions are its package-local copy (the repo's per-package fixture convention):
//
//   - plantHold is E0b: the withdrawal_requested hold leg keyed and correlated by the request id
//     (Dr player_cash / Cr player_withdrawal_hold, the request's wallet, asset and amount) and the hold
//     link, in the SAME transaction as the E0 INSERT, as withdrawal.RequestWithdrawal does. Since 0132 a
//     request without it is refused at COMMIT (check A, WD031).
//   - cancelLegally is E2 as withdrawal.Cancel writes it (the `:cancelled` reversal of the hold, then
//     the UPDATE with the release link), as the RUNTIME role. It replaces the owner tamper
//     `UPDATE ... SET state='cancelled'` these tests used to end a request with: the tests need a
//     TERMINAL request (the instrument is then no longer in use), not a tampered one.
package payoutinstrument

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func plantHold(ctx context.Context, tx pgx.Tx, tenantID, walletID, wid uuid.UUID, asset string, amount int64) error {
	accts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: asset},
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: asset})
	if err != nil {
		return err
	}
	post, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxWithdrawalRequested, IdempotencyKey: wid.String(), CorrelationID: wid,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: amount},
		},
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET hold_ledger_transaction_id = $1, updated_at = now() WHERE id = $2`, post.TransactionID, wid)
	return err
}

func (w *world) cancelLegally(wid uuid.UUID) {
	w.t.Helper()
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var walletID, hold uuid.UUID
		var asset string
		var amount int64
		if err := tx.QueryRow(ctx, `SELECT wallet_id, asset_code, amount::bigint, hold_ledger_transaction_id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, wid).
			Scan(&walletID, &asset, &amount, &hold); err != nil {
			return err
		}
		accts, err := ledger.GetOrCreateAccounts(ctx, tx, w.tenantID,
			ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: asset},
			ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: asset})
		if err != nil {
			return err
		}
		post, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.tenantID, TransactionType: ledger.TxWithdrawalRejected, IdempotencyKey: wid.String() + ":cancelled",
			CorrelationID: wid, ReversesTransactionID: &hold,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: amount},
			},
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'cancelled', release_ledger_transaction_id = $2, updated_at = now() WHERE id = $1 AND state = 'requested'`, wid, post.TransactionID)
		return err
	}); err != nil {
		w.t.Fatalf("cancel withdrawal %s (legal E2): %v", wid, err)
	}
}
