package reconciliation

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/payments"
)

// B13-B (LF M-2): the payout-scoped classification table is tied to payments.PayoutDisputeReasons(): every key is a real
// payout dispute reason, and every destination_* payout reason is classified (a new one cannot ship unclassified).
func TestPayoutDisputeReasonClasses_PinnedToPayments(t *testing.T) {
	payout := payments.PayoutDisputeReasons()
	for r := range payoutDisputeReasonClasses {
		if _, ok := payout[r]; !ok {
			t.Errorf("reconciliation classifies payout reason %q which payments.PayoutDisputeReasons() does not list", r)
		}
	}
	for _, r := range []string{payments.TerminalReasonDestinationMismatch, payments.TerminalReasonDestinationIntegrityFailure} {
		if _, ok := payoutDisputeReasonClasses[r]; !ok {
			t.Errorf("payout destination reason %q is not classified for reconciliation", r)
		}
	}
	// The deposit table must NOT carry them (the deposit pins would refuse, and the SQL list is deposit-only).
	for r := range payoutDisputeReasonClasses {
		if _, ok := disputeReasonClasses[r]; ok {
			t.Errorf("%q must live only in the payout-scoped table", r)
		}
	}
	// operation-scoped: a deposit attempt with such a reason is unclassified (it cannot occur).
	if c := (&payAttempt{operation: paymentStatementKindDeposit, terminalReason: "destination_mismatch", providerRef: "x"}).captureClass(); c != reasonUnclassified {
		t.Errorf("deposit + destination_mismatch = %v, want unclassified", c)
	}
	if c := (&payAttempt{operation: "payout", terminalReason: "destination_mismatch", providerRef: "x"}).captureClass(); c != reasonBound {
		t.Errorf("payout + destination_mismatch + reference = %v, want bound", c)
	}
	if c := (&payAttempt{operation: "payout", terminalReason: "destination_integrity_failure"}).captureClass(); c != reasonUnbound {
		t.Errorf("payout + destination_integrity_failure, no reference = %v, want unbound", c)
	}
}
