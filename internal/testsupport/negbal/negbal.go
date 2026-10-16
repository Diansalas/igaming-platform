//go:build integration

// Package negbal (integration builds ONLY) drives a wallet's player_cash below zero the way production
// does it: a deposit_reversal (a chargeback) posted against funds the player already spent. The
// deposit-reversal path posts regardless of the balance (financial-transaction-flows.md Flow 2), which is
// exactly the case owner decision 13 (negative-balance policy "block until cleared", A5) addresses. The
// posting is the real, balanced, ungoverned Flow 2 shape (Dr player_cash / Cr psp_clearing); nothing is
// disabled. Never compiled into an application binary.
package negbal

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// Chargeback posts a deposit_reversal of amount against the wallet's player_cash in tx (the tenant scope
// of tenantID) and returns its transaction id.
func Chargeback(ctx context.Context, tx pgx.Tx, tenantID, walletID uuid.UUID, asset string, amount int64) (uuid.UUID, error) {
	accts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: asset},
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: asset})
	if err != nil {
		return uuid.Nil, fmt.Errorf("negbal: accounts: %w", err)
	}
	provider, ref := "negbal-psp", "negbal-chargeback-"+uuid.NewString()
	res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxDepositReversal,
		IdempotencyKey: provider + ":" + ref, ProviderID: &provider, ProviderTxID: &ref, CorrelationID: uuid.New(),
		Entries: []ledger.EntryInput{
			{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: amount},
		},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("negbal: post chargeback: %w", err)
	}
	return res.TransactionID, nil
}

// Deposit posts a plain Flow 1 deposit of amount to the wallet's player_cash (a player curing the
// balance by depositing, which the negative-balance gate never blocks).
func Deposit(ctx context.Context, tx pgx.Tx, tenantID, walletID uuid.UUID, asset string, amount int64) error {
	accts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: asset},
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: asset})
	if err != nil {
		return fmt.Errorf("negbal: accounts: %w", err)
	}
	provider, ref := "negbal-psp", "negbal-deposit-"+uuid.NewString()
	_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxDeposit,
		IdempotencyKey: provider + ":" + ref, ProviderID: &provider, ProviderTxID: &ref, CorrelationID: uuid.New(),
		Entries: []ledger.EntryInput{
			{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: amount},
		},
	})
	if err != nil {
		return fmt.Errorf("negbal: post deposit: %w", err)
	}
	return nil
}
