//go:build integration

package adjustment

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// B-1 (R): a credit and a debit, each executed in the FINAL approval's own
// transaction (LF-13), with the §5.3 keys and exactly the §4 shape; the
// executed request links to its transaction; invariants hold.
func TestB1_CreditAndDebitExecuteInFinalApprovalTx(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(10_000)

	r, err := w.submit(w.F1, w.credit(2_500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("submit credit: %v", err)
	}
	if r.State != StatePending || r.RequiredAtSubmission != 1 || len(r.ContributingPolicyIDs) != 1 {
		t.Fatalf("unexpected pinned request: %+v", r)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil {
		t.Fatalf("approve credit: %v", err)
	}
	if !out.Executed || out.Request.State != StateExecuted || out.Request.LedgerTransactionID == nil {
		t.Fatalf("expected executed in the final approval's tx, got %+v", out)
	}
	if got := w.playerCash(); got != 12_500 {
		t.Fatalf("player_cash after credit: want 12500 got %d", got)
	}
	w.assertPostingKeys(out.Request)

	d, err := w.submit(w.F2, w.debit(4_000, ReasonExternalInstruction))
	if err != nil {
		t.Fatalf("submit debit: %v", err)
	}
	out, err = w.decide(w.F3, d, DecisionApprove)
	if err != nil {
		t.Fatalf("approve debit: %v", err)
	}
	if !out.Executed {
		t.Fatalf("debit not executed: %+v", out)
	}
	if got := w.playerCash(); got != 8_500 {
		t.Fatalf("player_cash after debit: want 8500 got %d", got)
	}
	w.assertPostingKeys(out.Request)
	w.assertInvariants()
}

// assertPostingKeys pins §5.3 and §4 on the linked ledger transaction and
// that executed_txid equals the approval's decided_txid ("same
// transaction" by txid, never xmin).
func (w *world) assertPostingKeys(r Request) {
	w.t.Helper()
	if err := w.pool.WithPrincipalScope(context.Background(), w.Tenant, w.F1.ID, func(ctx context.Context, tx pgx.Tx) error {
		var ttype, idem, reason string
		var corr uuid.UUID
		var cause *uuid.UUID
		var provider *string
		if err := tx.QueryRow(ctx, `SELECT transaction_type, idempotency_key, correlation_id, causation_id, reason_code, provider_id
			FROM ledger_transactions WHERE id = $1`, *r.LedgerTransactionID).Scan(&ttype, &idem, &corr, &cause, &reason, &provider); err != nil {
			return err
		}
		if ttype != "manual_adjustment" || idem != "manual_adjustment:"+r.ID.String() || corr != r.ID || reason != string(r.ReasonCode) || provider != nil {
			w.t.Fatalf("posting keys wrong: type=%s idem=%s corr=%s reason=%s provider=%v", ttype, idem, corr, reason, provider)
		}
		if (cause == nil) != (r.CausationTransactionID == nil) || (cause != nil && *cause != *r.CausationTransactionID) {
			w.t.Fatalf("causation mismatch: %v vs %v", cause, r.CausationTransactionID)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
			WHERE e.ledger_transaction_id = $1 AND la.account_type IN ('player_cash','manual_adjustment')`, *r.LedgerTransactionID).Scan(&n); err != nil {
			return err
		}
		var total int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE ledger_transaction_id = $1`, *r.LedgerTransactionID).Scan(&total); err != nil {
			return err
		}
		if n != 2 || total != 2 {
			w.t.Fatalf("expected exactly the two §4 entries, got %d of %d", n, total)
		}
		var sameTx bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_adjustment_requests r JOIN ledger_adjustment_approvals a ON a.request_id = r.id
			WHERE r.id = $1 AND a.decision = 'approve' AND a.decided_txid = r.executed_txid)`, r.ID).Scan(&sameTx); err != nil {
			return err
		}
		if !sameTx {
			w.t.Fatal("executed_txid is not the final approval's decided_txid")
		}
		return nil
	}); err != nil {
		w.t.Fatal(err)
	}
}
