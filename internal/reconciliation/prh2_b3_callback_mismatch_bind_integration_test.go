//go:build integration

package reconciliation

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// B3 (PAY-CALLBACK-MISMATCH-BIND-1) end to end with reconciliation: a verified
// amount-mismatch callback that the receipt path resolved by MERCHANT reference
// now parks the attempt holding the reported reference, so the park is BOUND
// (D2F-1): a standing pay_captured_unposted with no statement line at all, which
// only a tombstone (or reversal line) on that reference clears.
func TestB3_CallbackMismatchPark_IsBound_StandingUntilTombstoneOnReference(t *testing.T) {
	w := newD2World(t)
	w.p.setScript(func(req payments.DepositRequest) payments.DepositResult {
		return payments.DepositResult{Outcome: payments.OutcomeAmbiguous, Amount: req.Amount, AssetCode: req.AssetCode}
	})
	a := w.deposit(t, d2Amount)
	w.p.setScript(nil)
	if a.ProviderReference != nil {
		t.Fatalf("setup: the attempt must be reference-less, got %q", *a.ProviderReference)
	}
	ref := "b3-rc-" + uuid.NewString()
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
		EventType: "deposit", ProviderReference: ref, MerchantReference: a.MerchantReference,
		Outcome: payments.OutcomeSucceeded, Amount: d2Amount + 1, AssetCode: a.AssetCode,
	})
	parked := w.mustParked(t, a.ID, payments.TerminalReasonCallbackAmountAssetMismatch, false)
	if parked.ProviderReference == nil || *parked.ProviderReference != ref {
		t.Fatalf("the park must hold the reported reference %q, got %v", ref, parked.ProviderReference)
	}
	pk := d2Parked{attempt: parked, pspRef: ref}

	// Standing with NO statement line: only possible for a bound park.
	d2CUFor(t, w.d2Run(t, d2Src()), parked.ID)
	d2CUFor(t, w.d2Run(t, d2Src()), parked.ID)
	// A tombstone on a different reference does not clear it.
	w.deliverReversal(t, "b3-rev-"+uuid.NewString()[:8], "b3-other-"+uuid.NewString(), d2PSPAmount)
	d2CUFor(t, w.d2Run(t, d2Src()), parked.ID)
	// A tombstone on the bound reference clears it.
	w.deliverReversal(t, "b3-tomb-"+uuid.NewString()[:8], ref, d2PSPAmount)
	d2NoCU(t, w.d2Run(t, d2Src()), "a tombstone on the bound reference (standing)")
	d2NoCU(t, w.d2Run(t, d2Src(d2Line(ref, "", statement.PaymentStatusSucceeded, d2PSPAmount))), "a tombstone on the bound reference (in-run)")
	w.d2AssertNoMoney(t, pk)
}
