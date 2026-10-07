//go:build integration

package reconciliation

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-PAYOUT-UNBOUND-STANDING-1 (ADR 0095 §35.2 PO-1 / §35.6): the per-operation
// standing rule must leave DEPOSIT behaviour unchanged. The payout tests live in
// internal/payments (b11_payout_unbound_hold_integration_test.go, the real park
// paths); these pin the deposit side of the split:
//
//   - a payout clearing signal (a withdrawal_completed posting keyed by the
//     evidencing line's reference) NEVER clears a deposit unbound finding;
//   - a succeeded PAYOUT line naming the reference never evidences a deposit
//     park (linesFor is per operation);
//   - the deposit wording (ADR 0101 F13, allocation) and the deposit clearing
//     (a completed deposit_reversal naming the line reference) are unchanged.
func TestStanding1_DepositUnboundPark_PayoutSignalsNeverClear_DepositClearingUnchanged(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	line := d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)

	m := d2CUFor(t, w.d2Run(t, d2Src(line)), pk.attempt.ID) // in-run; d2CUFor asserts the deposit wording
	if strings.Contains(m.ExpectedValue, "R-K3-8") || !strings.Contains(m.ActualValue, "op=deposit") {
		t.Fatalf("deposit finding misrepresented: expected=%q actual=%q", m.ExpectedValue, m.ActualValue)
	}
	d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID) // standing

	// A payout of this tenant and provider completes under the deposit line's
	// reference (withdrawal_completed keyed by pk.pspRef, attributable to no
	// other deposit attempt): the payout signal must not clear a deposit finding.
	w.payoutFixture(t, payProvA, "s1-instr-"+uuid.NewString()[:8], pk.pspRef, d2Amount, true)
	d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID)

	// Deposit clearing is unchanged: a completed deposit_reversal naming the
	// line reference clears, in-run and from then on.
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("s1-rev-"+uuid.NewString(), pk.pspRef, d2Amount))), "a deposit_reversal on the line reference still clears a deposit park")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and it stays cleared")
	// The payout fixture's own withdrawal_completed is keyed by pk.pspRef by
	// construction, so check "no posting for the parked attempt" on its intent
	// only (SUM equality and the projection rebuild still run).
	w.d2AssertNoMoney(t, d2Parked{attempt: pk.attempt, pspRef: "s1-no-such-ref"})
}

// A deposit park and a succeeded PAYOUT line naming its merchant reference: the
// payout line resolves to no payout attempt (pay_missing_platform_record) and
// never raises a deposit pay_captured_unposted, in-run or standing.
func TestStanding1_PayoutLineNeverEvidencesADepositPark(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	pl := payLineFor(payProvA, pk.pspRef, pk.attempt.MerchantReference, statement.PaymentLinePayout, statement.PaymentStatusSucceeded, d2Amount)
	d2NoCU(t, w.d2Run(t, d2Src(pl)), "a payout line never evidences a deposit park (in-run)")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "a payout line never evidences a deposit park (standing)")
	w.d2AssertNoMoney(t, pk)
}
