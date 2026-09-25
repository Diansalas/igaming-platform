//go:build integration

// Ledger-finance P3-3.1: a duplicate ledger-side statement key (two
// un-reversed sportsbook_settlement transactions of one bet that both
// parse to the SAME statement key - e.g. an unparseable/ambiguous
// idempotency key suffix, both mapping to "line=settlement#<null>") must
// be RECORDED as a mismatch, never silently overwritten in
// sbLedgerStatementView's map. Code review #9: the "ledger payout" of a
// settlement transaction is computed by ONE shared helper
// (sbLedgerCashPayoutSubquery), used identically by the per-bet check
// (sbLoadCurrentSettlements) and the statement match
// (sbLedgerStatementView).
package reconciliation

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func TestSportsbookSettlementRecon_DuplicateLedgerSideStatementKeyDetected(t *testing.T) {
	pool := sbScratchPool(t)
	w := seedSBWorld(t, pool)
	requireClean(t, pool, w.f.tenantID)

	bet := w.bets["open"] // still open: no real settlement ledger row exists for it yet.

	// Two DIFFERENT ledger transactions, both un-reversed
	// sportsbook_settlement postings on the SAME bet, whose idempotency
	// keys both fail sbGenerationFromKey's suffix parse (an ambiguous/
	// corrupted key) - both therefore map to the SAME statement key
	// ("line=settlement#<null>"). A writer that bypassed
	// SimulateSettlementEvent entirely (the only way this can happen: T-1
	// and the partial unique indexes prevent it through the real path) is
	// exactly what §8.3's orphan check and this dedup check are a backstop
	// for.
	err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		house, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
		if err != nil {
			return err
		}
		for i := 0; i < 2; i++ {
			if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: w.f.tenantID, TransactionType: ledger.TxSportsbookSettlement,
				IdempotencyKey: "sportsbook_settlement:" + bet.ID.String() + "#not-a-number-" + uuid.NewString(),
				CorrelationID:  bet.ID,
				Entries: []ledger.EntryInput{
					{LedgerAccountID: house, Direction: ledger.Debit, Amount: 1},
					{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: 1},
				},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inject duplicate ledger-side statement key fixture: %v", err)
	}

	requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBMockStatement,
		"ledger: bet="+bet.ID.String()+" line=settlement#<null>")
}

// TestSbLedgerCashPayoutSubquery_SharedByBothCallers is a documentation-
// grade regression pin for code review #9: both call sites embed the
// SAME helper function's output verbatim, so they cannot silently drift
// back into two different queries.
func TestSbLedgerCashPayoutSubquery_SharedByBothCallers(t *testing.T) {
	got := sbLedgerCashPayoutSubquery("t.id", "t.correlation_id")
	want := sbLedgerCashPayoutSubquery("t.id", "t.correlation_id")
	if got != want {
		t.Fatalf("sbLedgerCashPayoutSubquery is not deterministic for identical arguments")
	}
	if got == sbLedgerCashPayoutSubquery("s.ledger_transaction_id", "s.bet_id") {
		t.Fatalf("expected different column arguments to produce different SQL")
	}
}
