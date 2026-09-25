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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
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
// still passes and the call reaches lockAndPost, where ledger.Post is
// asked to post the SAME key and type (sportsbook_settlement) with
// DIFFERENT entries (the fixture's Dr HOUSE 1 / Cr CASH 1 versus the real
// settle-won shape). STALE COMMENT FIXED (code review finding 4,
// docs/governance/stage-10-w1-code-review.md): before the F-7 remediation
// (36616f1) this returned AlreadyPosted because Post's replay compared
// only transaction_type; after F-7, Post also compares the canonical
// entry set and returns ErrIdempotencyPayloadMismatch here instead - a
// DIFFERENT one of the three replay-shaped outcomes lockAndPost must
// classify as an integrity failure (§4.7), exercised via the
// errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) branch
// (settlement.go's lockAndPost). This must abort as ErrSettlementIntegrity
// and commit NOTHING - no history row, no status change, no second ledger
// row under the same key. lockAndPost's other two replay-shaped branches -
// a full match (res.AlreadyPosted) and a differing TYPE
// (ErrIdempotencyKeyReused) - are exercised by
// TestSettlementFaultInjection_TombstoneBackstop_AlreadyPosted and
// TestSettlementFaultInjection_TombstoneBackstop_KeyReused below.
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

// fabricateReservedTombstoneFixture posts a bare TxTombstone directly under
// generation g's reserved settlement key, WITH NO backing
// sportsbook_bet_settlements row - the fixture code review finding 4
// (docs/governance/stage-10-w1-code-review.md) prescribes to exercise
// lockAndPost's two remaining untested replay-shaped branches. Its
// CorrelationID is a fresh uuid.New(), never betID: F-7's replay comparison
// (internal/ledger/replay.go) deliberately EXEMPTS correlation_id for
// TxTombstone, so this is still a legitimate "same fact" as far as Post's
// replay check is concerned, exactly like the historical tombstone writers
// (casino/payments) it documents.
func fabricateReservedTombstoneFixture(t *testing.T, pool *db.Pool, f sbFixture, betID uuid.UUID, generation int) {
	t.Helper()
	reservedKey := settlementIdempotencyKey(betID, generation)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: reservedKey, CorrelationID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("fabricate the reserved tombstone fixture: %v", err)
	}
}

// TestSettlementFaultInjection_TombstoneBackstop_AlreadyPosted exercises
// lockAndPost's res.AlreadyPosted branch (settlement.go:934, code review
// finding 4): a bare tombstone already occupies generation 1's reserved
// key with no history row. rollback(1) on a never-settled bet is §4.3's
// "bet open, g = G+1, no row for g" case, which itself tries to post a
// TxTombstone under that SAME key with the SAME (empty) entries -
// correlation is exempt for tombstones, so Post reports a FULL replay
// match (res.AlreadyPosted = true) for a posting §4.3 classified as NEW.
// This must abort as ErrSettlementIntegrity and commit nothing.
func TestSettlementFaultInjection_TombstoneBackstop_AlreadyPosted(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	fabricateReservedTombstoneFixture(t, pool, f, betID, 1)

	_, callErr := simulateSettlement(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if !errors.Is(callErr, ErrSettlementIntegrity) {
		t.Fatalf("expected ErrSettlementIntegrity, got %v", callErr)
	}

	if got := len(settlementHistory(t, pool, f.tenantID, betID)); got != 0 {
		t.Fatalf("expected 0 history rows, got %d", got)
	}
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusOpen {
		t.Fatalf("bet status = %q, want open (unchanged)", got)
	}
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxTombstone)); got != 1 {
		t.Fatalf("expected exactly 1 tombstone ledger transaction (the fixture; the call must not add a second), got %d", got)
	}
}

// TestSettlementFaultInjection_TombstoneBackstop_KeyReused exercises
// lockAndPost's ErrIdempotencyKeyReused branch (settlement.go:916, code
// review finding 4): the SAME bare-tombstone fixture as above, but this
// time settle(1) is attempted. §4.3 sees no history row for generation 1
// (the fixture has none), so it classifies settle(1) as NEW and tries to
// post a sportsbook_settlement transaction under the SAME reserved key a
// tombstone already occupies - a TYPE mismatch, which Post rejects as
// ErrIdempotencyKeyReused before any payload comparison even runs. This
// must abort as ErrSettlementIntegrity and commit nothing.
func TestSettlementFaultInjection_TombstoneBackstop_KeyReused(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	fabricateReservedTombstoneFixture(t, pool, f, betID, 1)

	_, callErr := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if !errors.Is(callErr, ErrSettlementIntegrity) {
		t.Fatalf("expected ErrSettlementIntegrity, got %v", callErr)
	}

	if got := len(settlementHistory(t, pool, f.tenantID, betID)); got != 0 {
		t.Fatalf("expected 0 history rows, got %d", got)
	}
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusOpen {
		t.Fatalf("bet status = %q, want open (unchanged)", got)
	}
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxSportsbookSettlement)); got != 0 {
		t.Fatalf("expected 0 sportsbook_settlement ledger transactions, got %d", got)
	}
	if got := countLedgerTransactionsByType(t, pool, f, string(ledger.TxTombstone)); got != 1 {
		t.Fatalf("expected exactly 1 tombstone ledger transaction (the fixture), got %d", got)
	}
}
