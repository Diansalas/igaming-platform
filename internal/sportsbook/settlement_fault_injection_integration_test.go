//go:build integration

// Fault injection (ADR 0088 §4.7, §14 Q3(b)): (1) the settlementAfterPost
// hook returning an error must roll back the WHOLE operation - no ledger
// rows, no history rows, bet status unchanged; (2) the §4.7 backstop: a
// ledger transaction pre-existing under a reserved settlement key, which
// the §4.3 decision table classified as NEW, must abort with
// ErrSettlementIntegrity and commit nothing.
package sportsbook

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// TestSettlementFaultInjection_HookErrorRollsBackWholeTransaction installs
// a hook that fails after ledger.Post but before the history insert
// (exactly Q3(b)'s named seam) and proves the whole settlement transaction
// rolls back: no ledger row, no history row, bet still open.
func TestSettlementFaultInjection_HookErrorRollsBackWholeTransaction(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	injected := errors.New("injected post-Post failure")
	restore := SetSettlementAfterPostHookForTest(func(context.Context, pgx.Tx) error { return injected })
	defer restore()

	_, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if err == nil {
		t.Fatalf("expected the hook's injected error to abort the call")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("expected the injected error to be wrapped, got %v", err)
	}
	restore()

	if got := len(settlementHistory(t, pool, f.tenantID, betID)); got != 0 {
		t.Fatalf("expected 0 history rows after a rolled-back operation, got %d", got)
	}
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusOpen {
		t.Fatalf("bet status = %q, want open (unchanged)", got)
	}
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxSportsbookSettlement)); got != 0 {
		t.Fatalf("expected 0 sportsbook_settlement ledger rows, got %d", got)
	}
	assertNets(t, pool, f, -stdStake, stdStake, 0)
}

// TestSettlementFaultInjection_HookErrorRollsBackVoidAfterSettlement proves
// the same property for the two-posting void-after-settlement path: the
// hook fires once, after BOTH postings, and its error must roll back
// EVERYTHING (both the rollback and the void), not leave the rollback
// half-applied.
func TestSettlementFaultInjection_HookErrorRollsBackVoidAfterSettlement(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	injected := errors.New("injected post-Post failure (void-after-settlement)")
	restore := SetSettlementAfterPostHookForTest(func(context.Context, pgx.Tx) error { return injected })
	defer restore()

	_, err := simulateSettlement(t, pool, f.tenantID, voidEvent(betID, actor, "data_error"))
	if err == nil || !errors.Is(err, injected) {
		t.Fatalf("expected the injected error, got %v", err)
	}
	restore()

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 1 {
		t.Fatalf("expected only the original settlement row to survive (rollback+void must both roll back), got %d rows", len(hist))
	}
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusSettledWon {
		t.Fatalf("bet status = %q, want settled_won (unchanged)", got)
	}
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxSportsbookRollback)); got != 0 {
		t.Fatalf("expected 0 sportsbook_rollback rows, got %d", got)
	}
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxSportsbookVoid)); got != 0 {
		t.Fatalf("expected 0 sportsbook_void rows, got %d", got)
	}
}

// TestSettlementFaultInjection_LedgerKeyBackstop is ADR 0088 §4.7's
// backstop: a ledger transaction is fabricated directly (bypassing the
// settlement service entirely, via ledger.Post) under the RESERVED
// settlement key for generation 1 of a bet the decision table still
// considers open/un-settled - no sportsbook_bet_settlements row backs it,
// so §4.3 classifies the incoming settle(1) as NEW. Its entries touch only
// HOUSE/CASH (never LOCKED), so §2.2's NetLocked pre-posting assertion
// still passes and the call reaches lockAndPost, where ledger.Post reports
// AlreadyPosted (F-7: replay compares only transaction_type, and both are
// sportsbook_settlement) for a transaction §4.3 never produced. This must
// abort as ErrSettlementIntegrity and commit NOTHING - no history row, no
// status change, no second ledger row under the same key.
func TestSettlementFaultInjection_LedgerKeyBackstop(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	reservedKey := settlementIdempotencyKey(betID, 1)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		houseAccount, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: nil, AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxSportsbookSettlement,
			IdempotencyKey: reservedKey, CorrelationID: betID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: houseAccount[0], Direction: ledger.Debit, Amount: 1},
				{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: 1},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fabricate the pre-existing ledger transaction fixture: %v", err)
	}

	_, callErr := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if !errors.Is(callErr, ErrSettlementIntegrity) {
		t.Fatalf("expected ErrSettlementIntegrity, got %v", callErr)
	}

	if got := len(settlementHistory(t, pool, f.tenantID, betID)); got != 0 {
		t.Fatalf("expected 0 history rows (the fabricated ledger row has none), got %d", got)
	}
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusOpen {
		t.Fatalf("bet status = %q, want open (unchanged; the fabricated ledger row never touched sportsbook_bets)", got)
	}
	// Exactly the one FABRICATED sportsbook_settlement transaction exists -
	// the call itself must not have added a second one under the same key.
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxSportsbookSettlement)); got != 1 {
		t.Fatalf("expected exactly 1 sportsbook_settlement ledger transaction (the fixture), got %d", got)
	}
}
