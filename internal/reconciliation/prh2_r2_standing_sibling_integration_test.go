//go:build integration

package reconciliation

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-RECON-PARKED-CAPTURE-STANDING-1 pin (ledger-finance 2.5): a parked second
// capture keeps standing when a SIBLING attempt of the same intent succeeded and
// posted. One logical deposit intent has at most one successful authoritative
// posting; the second capture stays disputed and unposted, with no automatic
// posting, receivable or clawback. It clears only on a PSP reversal/tombstone.
func TestR2_ParkedSecondCaptureStandsWhenSiblingAttemptSucceeded(t *testing.T) {
	w := newD2World(t)
	first := w.deposit(t, d2Amount)
	w.succeed(t, w.mockA, payProvA, first)
	first = w.attempt(t, first.ID)
	if first.LedgerTransactionID == nil {
		t.Fatal("setup: the sibling must be succeeded and posted")
	}
	var second payments.PaymentAttempt
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = payments.InsertCreatedAttempt(ctx, tx, payments.NewCreatedAttempt{
			ID: uuid.New(), TenantID: w.f.tenantID, Operation: payments.AttemptOperationDeposit, DepositIntentID: first.DepositIntentID,
			AttemptNo: 2, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: d2Amount,
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET provider_id = $2 WHERE id = $1`, second.ID, payProvA)
		return err
	}); err != nil {
		t.Fatalf("setup: second attempt on the same intent: %v", err)
	}
	ref := "r2-sib-" + uuid.NewString()
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: second.MerchantReference,
		Outcome: payments.OutcomeSucceeded, Amount: d2Amount, AssetCode: "EUR"})
	parked := w.attempt(t, second.ID)
	if parked.State != payments.AttemptDisputed || parked.LedgerTransactionID != nil {
		t.Fatalf("setup: the second capture must be disputed and unposted, got %s ledger=%v", parked.State, parked.LedgerTransactionID)
	}
	line := d2Line(ref, second.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)
	count := func() int {
		var n int
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, w.f.tenantID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	postedBefore := count() // the sibling's posting (plus any fixture seed)
	d2CUFor(t, w.d2Run(t, d2Src(line)), second.ID)
	for i := 0; i < 3; i++ {
		d2CUFor(t, w.d2Run(t, d2PastSrc()), second.ID)
	}
	if n := count(); n != postedBefore {
		t.Fatalf("one posting per intent: the parked capture must add none (%d before), got %d", postedBefore, n)
	}
	if again := w.attempt(t, second.ID); again.State != payments.AttemptDisputed || again.LedgerTransactionID != nil {
		t.Fatal("the second capture must stay disputed/unposted")
	}
	// Only a PSP-side reversal on the second capture's reference clears it.
	d2NoCU(t, w.d2Run(t, k3Cov(4, d2ReversalLine("r2-rev-"+uuid.NewString(), ref, d2Amount))), "a PSP reversal on the second capture's reference clears")
	w.d2AssertBalanced(t)
}
