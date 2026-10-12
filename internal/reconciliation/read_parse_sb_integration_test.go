//go:build integration

package reconciliation

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// ADR 0114 R2: the REAL sportsbook_settlement emitter (RunSportsbookSettlement over injected drift)
// run through the strict parsers. The subject is the wallet / bet the key names; the asset and
// amounts are null because the rendered segments of these kinds carry no asset (the key is never
// consulted for it).
func TestSportsbookEmitters_ThroughTheStrictParsers(t *testing.T) {
	pool := sbScratchPool(t)
	w := seedSBWorld(t, pool)
	requireClean(t, pool, w.f.tenantID)
	bet := w.bets["won"]
	err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		locked, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerLockedCash, "EUR")
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxSportsbookBet, IdempotencyKey: "drift-" + uuid.NewString(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: 7},
				{LedgerAccountID: locked, Direction: ledger.Credit, Amount: 7},
			},
		}); err != nil {
			return err
		}
		house, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxSportsbookBet, IdempotencyKey: "drift-" + uuid.NewString(), CorrelationID: bet.ID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: house, Direction: ledger.Debit, Amount: 3},
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: 3},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	_, ms := runSB(t, pool, w.f.tenantID, nil)
	sawLocked, sawNet := false, false
	for _, m := range ms {
		s := MismatchSubject(StreamSportsbookSettlement, m.MismatchKind, m.ReconciliationKey, m.ExpectedValue)
		switch m.MismatchKind {
		case MismatchKindSBLocked:
			sawLocked = true
			if subj(s) != "wallet:"+w.f.walletID.String() {
				t.Errorf("locked subject = %q (key %q)", subj(s), m.ReconciliationKey)
			}
			if got := MismatchAssetCandidate(m.ExpectedValue, m.ActualValue); got != nil {
				t.Errorf("locked asset candidate = %q, want nil (the key is not consulted)", *got)
			}
		case MismatchKindSBBetNet:
			sawNet = true
			if subj(s) != "bet:"+bet.ID.String() {
				t.Errorf("bet-net subject = %q (key %q)", subj(s), m.ReconciliationKey)
			}
			if MismatchAmount(m.ExpectedValue) != nil || MismatchAmount(m.ActualValue) != nil {
				t.Errorf("multi-value cash/locked/house texts must yield no amount: %q / %q", m.ExpectedValue, m.ActualValue)
			}
		}
	}
	if !sawLocked || !sawNet {
		t.Fatalf("the injected drift was not detected:%s", sbDump(ms))
	}
}
