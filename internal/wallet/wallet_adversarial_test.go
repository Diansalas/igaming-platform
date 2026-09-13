//go:build integration

// Adversarial financial tests for the wallet read surface, sharing
// wallet_integration_test.go's testPool/seedFixture helpers and its
// //go:build integration tag.

package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// TestInsufficientFunds_OverDebitRejectedAndSummaryUnchanged proves a
// debit that would drive player_cash negative is rejected rather than
// silently applied, observed from the wallet's own read surface: the
// balance a player is shown never reflects a debit the platform refused,
// and never goes negative.
//
// internal/withdrawal is imported here (test-only; it does not import
// this package, so there is no cycle) because the withdrawal request is
// the only path in Stage 3B that debits player_cash on a player's
// instruction, and therefore the only place the sufficiency check that
// protects this wallet lives - its own locked-read/reject mechanism is
// tested in that package (TestRequestWithdrawal_InsufficientFundsRejected
// AndAtomic, TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds).
// internal/ledger deliberately has no balance check of its own: a
// house-level account such as psp_clearing is legitimately negative
// (ledger-accounting-model.md §5), so sufficiency is a policy decision
// belonging to the debiting flow, not to the posting engine.
func TestInsufficientFunds_OverDebitRejectedAndSummaryUnchanged(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var w Wallet
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w, err = GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "EUR")
		if err != nil {
			return err
		}
		cashID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		clearingID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		provider, providerTx := "mockpsp", "dep-insufficient"
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "dep-insufficient",
			ProviderID: &provider, ProviderTxID: &providerTx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearingID, Direction: ledger.Debit, Amount: 100},
				{LedgerAccountID: cashID, Direction: ledger.Credit, Amount: 100},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// One minor unit more than the wallet holds - the smallest possible
	// over-debit, not a comfortably large one.
	debitErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID:        f.tenantID,
			BrandID:         f.brandID,
			PlayerAccountID: f.playerAccountID,
			WalletID:        w.ID,
			AssetCode:       "EUR",
			Amount:          101,
			IdempotencyKey:  "wd-over-balance",
		})
		return err
	})
	if !errors.Is(debitErr, withdrawal.ErrInsufficientFunds) {
		t.Fatalf("expected ErrInsufficientFunds for a debit of 101 against a balance of 100, got %v", debitErr)
	}

	assertUntouchedBalance(t, pool, f, w)

	// The exact balance must still be spendable - the rejection above is
	// not a symptom of an off-by-one that also blocks legitimate debits.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID:        f.tenantID,
			BrandID:         f.brandID,
			PlayerAccountID: f.playerAccountID,
			WalletID:        w.ID,
			AssetCode:       "EUR",
			Amount:          100,
			IdempotencyKey:  "wd-exact-balance",
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected a debit of exactly the available balance to succeed, got %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		summary, err := GetSummary(ctx, tx, w)
		if err != nil {
			return err
		}
		if summary.CashBalance != 0 || summary.AvailableBalance != 0 {
			t.Fatalf("expected cash/available 0 after debiting the full balance, got %+v", summary)
		}
		if summary.HeldForWithdrawal != 100 {
			t.Fatalf("expected 100 held for withdrawal, got %d", summary.HeldForWithdrawal)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// assertUntouchedBalance checks that the rejected over-debit left neither
// the projection nor the ledger behind it changed - a rejected debit must
// not be partially applied, and must not leave the wallet negative.
func assertUntouchedBalance(t *testing.T, pool *db.Pool, f fixture, w Wallet) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		summary, err := GetSummary(ctx, tx, w)
		if err != nil {
			return err
		}
		if summary.CashBalance != 100 {
			t.Fatalf("expected cash balance to remain 100 after a rejected debit, got %d", summary.CashBalance)
		}
		if summary.AvailableBalance != 100 {
			t.Fatalf("expected available balance to remain 100 after a rejected debit, got %d", summary.AvailableBalance)
		}
		if summary.HeldForWithdrawal != 0 {
			t.Fatalf("expected no withdrawal hold after a rejected debit, got %d", summary.HeldForWithdrawal)
		}

		cashID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		rebuilt, err := ledger.RebuildBalance(ctx, tx, cashID)
		if err != nil {
			return err
		}
		if rebuilt.Signed() != 100 || rebuilt.DebitTotal != 0 {
			t.Fatalf("expected the ledger itself to show 100 with no debit entry, got %+v", rebuilt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}
