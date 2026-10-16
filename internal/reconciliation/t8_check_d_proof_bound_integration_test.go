//go:build integration

// ADR 0110 T8 amendment (migration 0165): check D's WD026 twin counts ONLY proof-bound 'reverse' decisions.
// A well-shaped reversal whose two distinct-Person decisions carry no proof (the shape of decisions recorded
// before 0165, planted by the owner with the proof guard and the state guard switched off: ADR 0110 s9.4) is
// a standing finding (violation=WD026); the governed (proof-bound) reversal in the same sweep is clean.
package reconciliation

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func TestT8D_ReversedOnUnprovenDecisions_IsWD026(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	governed := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	a5DReverse(t, rt, f, governed, a5DStaff(t, pool, f.tenantID), a5DStaff(t, pool, f.tenantID))
	wdpath.SkipOnPersistentDatabase(t, pool)

	planted := dWithdrawal(t, pool, f, 10, withdrawal.StateCompleted)
	u1, u2 := a5DStaff(t, pool, f.tenantID), a5DStaff(t, pool, f.tenantID)
	reason := withdrawal.ReversalReasonPrefix + strings.Repeat("d8", 32)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE withdrawal_approvals DISABLE TRIGGER zz_actor_proof_guard`); err != nil {
			return err
		}
		for _, s := range []uuid.UUID{u1, u2} {
			if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id,
				is_automated_approval, decision, reason_code, threshold_amount_at_decision, request_amount_at_decision)
				VALUES ($1, $2, $3, false, 'reverse', $4, 0, 10)`, f.tenantID, planted, s, reason); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE withdrawal_approvals ENABLE TRIGGER zz_actor_proof_guard`); err != nil {
			return err
		}
		var completion uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, planted).Scan(&completion); err != nil {
			return err
		}
		// Non-vacuity of the guard: with the unproven decisions in place, E11 is refused by WD026 itself.
		if _, err := tx.Exec(ctx, `SAVEPOINT e11`); err != nil {
			return err
		}
		pid, ret := "wdpath-psp", "t8-ret-"+planted.String()
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalReversed,
			IdempotencyKey: planted.String() + ":reversed", ProviderID: &pid, ProviderTxID: &ret, CorrelationID: planted,
			ReversesTransactionID: &completion,
			Entries: []ledger.EntryInput{{LedgerAccountID: f.clearingID, Direction: ledger.Debit, Amount: 10},
				{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: 10}}})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, res.TransactionID, planted)
		if err == nil || !strings.Contains(err.Error(), "WD026") {
			t.Fatalf("E11 on two unproven decisions must be refused WD026 by the guard, got %v", err)
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT e11`); err != nil {
			return err
		}
		// The owner plant past the guard.
		if _, err := tx.Exec(ctx, `ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_aa_state_transition_guard`); err != nil {
			return err
		}
		res, err = ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalReversed,
			IdempotencyKey: planted.String() + ":reversed", ProviderID: &pid, ProviderTxID: &ret, CorrelationID: planted,
			ReversesTransactionID: &completion,
			Entries: []ledger.EntryInput{{LedgerAccountID: f.clearingID, Direction: ledger.Debit, Amount: 10},
				{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: 10}}})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, res.TransactionID, planted); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_aa_state_transition_guard`)
		return err
	}); err != nil {
		t.Fatalf("owner tamper plant (reversal on unproven decisions): %v", err)
	}
	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("the plant must be found: %s err=%v", o.Run.Status, o.Err)
	}
	rows := dKind(dMismatches(t, pool, f), MismatchKindWithdrawalRequestLedgerLinkInvalid)
	if len(rows) != 1 || rows[0].key != planted.String() || !strings.HasPrefix(rows[0].actual, "violation=WD026 ") {
		t.Fatalf("want exactly the request reversed on unproven decisions reported WD026 (the proof-bound one is clean): %+v", rows)
	}
}
