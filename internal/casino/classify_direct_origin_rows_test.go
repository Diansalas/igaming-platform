package casino

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// TestClassifyDirectOriginRows_G1 is the unit-level case matrix for G-1
// (Stage 10.3, docs/plans/stage-10.3-planning/02-casino-financial-
// analysis.md §3), exercising classifyDirectOriginRows directly rather
// than through a full HTTP/DB round trip - the same "conformance suite +
// pure-function unit test" split this codebase uses elsewhere for
// classification logic.
func TestClassifyDirectOriginRows_G1(t *testing.T) {
	wallet1, wallet2 := uuid.New(), uuid.New()
	tx1, tx2 := uuid.New(), uuid.New()

	t.Run("single cash bet resolves normally", func(t *testing.T) {
		row, err := classifyDirectOriginRows([]originRow{
			{BetTransactionID: tx1, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if row.WalletID != wallet1 {
			t.Fatalf("expected wallet1, got %v", row.WalletID)
		}
	})

	t.Run("two cash bets on the SAME wallet resolve to that wallet (G-1 fix)", func(t *testing.T) {
		row, err := classifyDirectOriginRows([]originRow{
			{BetTransactionID: tx1, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
			{BetTransactionID: tx2, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
		})
		if err != nil {
			t.Fatalf("expected a resolved wallet for a multi-bet cash round, got error: %v", err)
		}
		if row.WalletID != wallet1 {
			t.Fatalf("expected wallet1, got %v", row.WalletID)
		}
	})

	t.Run("three cash bets on the same wallet still resolve", func(t *testing.T) {
		tx3 := uuid.New()
		if _, err := classifyDirectOriginRows([]originRow{
			{BetTransactionID: tx1, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
			{BetTransactionID: tx2, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
			{BetTransactionID: tx3, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
		}); err != nil {
			t.Fatalf("expected three same-wallet cash bets to resolve, got: %v", err)
		}
	})

	t.Run("two cash bets on DIFFERENT wallets is a wallet collision (409)", func(t *testing.T) {
		_, err := classifyDirectOriginRows([]originRow{
			{BetTransactionID: tx1, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
			{BetTransactionID: tx2, AccountType: ledger.AccountPlayerCash, WalletID: wallet2, AssetCode: "EUR"},
		})
		if err != ErrCorrelationWalletCollision {
			t.Fatalf("expected ErrCorrelationWalletCollision, got %v", err)
		}
	})

	t.Run("mixed cash and bonus account types on one wallet is mixed-funding (409), not resolved as cash", func(t *testing.T) {
		_, err := classifyDirectOriginRows([]originRow{
			{BetTransactionID: tx1, AccountType: ledger.AccountPlayerCash, WalletID: wallet1, AssetCode: "EUR"},
			{BetTransactionID: tx2, AccountType: ledger.AccountPlayerBonus, WalletID: wallet1, AssetCode: "EUR"},
		})
		if err != ErrMixedFundingUnsupported {
			t.Fatalf("expected ErrMixedFundingUnsupported, got %v", err)
		}
	})

	t.Run("two bare player_bonus rows (no lock) on one wallet stay ambiguous, not silently resolved", func(t *testing.T) {
		_, err := classifyDirectOriginRows([]originRow{
			{BetTransactionID: tx1, AccountType: ledger.AccountPlayerBonus, WalletID: wallet1, AssetCode: "EUR"},
			{BetTransactionID: tx2, AccountType: ledger.AccountPlayerBonus, WalletID: wallet1, AssetCode: "EUR"},
		})
		if err != ErrAmbiguousMultiOriginRound {
			t.Fatalf("expected ErrAmbiguousMultiOriginRound for a non-cash multi-row shape, got %v", err)
		}
	})
}
