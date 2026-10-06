//go:build integration

package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// MA020-SYNC-MISMATCH-1 end to end with B3 (PAY-CALLBACK-MISMATCH-BIND-1): a
// live deposit attempt with NO provider reference is resolved by a verified
// callback by MERCHANT reference whose amount mismatches. The real receipt path
// binds the reported reference and parks callback_amount_asset_mismatch (T10);
// migration 0119's MA020 then refuses a K2 credit for that player through the
// real Service session dispatch, and a refund tombstone on the bound reference
// lifts it (exactly one posting). Before the bind (B3) such a park was
// reference-less and, under R-MA-2, would not block.
func TestMA020_B3_CallbackMismatchParkBlocksK2Credit(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	w.prov.setDeposit(scriptOutcome(OutcomeAmbiguous, ""))
	res := rvInit(t, w.pool, w.orch, w.f.orchFixture, 3000, "ma020-b3-"+uuid.NewString())
	w.prov.setDeposit(nil)
	a := w.attempt(res.Attempt.ID)
	if a.ProviderReference != nil || (a.State != AttemptAmbiguous && a.State != AttemptSubmitting && a.State != AttemptPending) {
		t.Fatalf("setup: state=%s ref=%v, want a live reference-less attempt", a.State, a.ProviderReference)
	}
	if w.hasExposure() {
		t.Fatal("setup: no exposure before the callback")
	}
	ref := "ma020-b3-ref-" + uuid.NewString()
	d, err := rvApplyReceipt(w.pool, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{EventType: "deposit", ProviderReference: ref,
		MerchantReference: a.MerchantReference, Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"})
	if err != nil || d != DispositionApplied {
		t.Fatalf("mismatch callback: %v %v", d, err)
	}
	a = w.attempt(a.ID)
	if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != TerminalReasonCallbackAmountAssetMismatch ||
		a.ProviderReference == nil || *a.ProviderReference != ref || a.LedgerTransactionID != nil {
		t.Fatalf("want a bound callback_amount_asset_mismatch park holding %q, got state=%s reason=%v ref=%v", ref, a.State, a.TerminalReason, a.ProviderReference)
	}
	credit := adjustment.SubmitInput{WalletID: w.f.walletID, AssetCode: "EUR", Direction: adjustment.DirectionCreditPlayer, Amount: 10,
		ReasonCode: adjustment.ReasonOperationalErrorCorrection, Note: "ma020 b3 e2e"}
	if _, err := w.k2Service().Submit(k3Ctx(w.f3), w.k2Target(w.f3), credit, adjustment.Meta{RequestID: "ma020-b3"}); k3Code(err) != "MA020" {
		t.Fatalf("K2 credit under the bound callback-mismatch park: want MA020, got %v", err)
	}
	if n := w.ledgerTxCount("manual_adjustment"); n != 0 {
		t.Fatalf("no manual adjustment may post, got %d", n)
	}
	// The refund tombstone on the bound reference lifts the exposure.
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		p := w.provider
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "ma020-b3-tomb:" + ref, ProviderID: &p, ProviderTxID: &ref, CorrelationID: uuid.New()})
		return err
	})
	r, err := w.k2Service().Submit(k3Ctx(w.f3), w.k2Target(w.f3), credit, adjustment.Meta{RequestID: "ma020-b3"})
	if err != nil {
		t.Fatalf("credit after the tombstone must be admitted: %v", err)
	}
	if out, err := w.k2Decide(w.f4, r); err != nil || !out.Executed {
		t.Fatalf("credit after the tombstone must execute: %v %+v", err, out)
	}
	if n := w.ledgerTxCount("manual_adjustment"); n != 1 {
		t.Fatalf("want exactly one manual adjustment posting, got %d", n)
	}
	w.assertInvariants()
}
