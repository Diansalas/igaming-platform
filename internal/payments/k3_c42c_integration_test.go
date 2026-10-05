//go:build integration

package payments

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// C-42c (LF D-1): the RAISING predicates stay broad. A succeeded line that carries
// ANOTHER live payout's reference plus A's merchant reference cannot CLEAR A's
// finding (C-42b), but it must still RAISE (d) pay_declared_not_paid_but_paid and
// (c2) pay_declared_paid_compensated_but_paid for A: in that run and in a later
// persisted-only run. (Mutant LFD8: narrowing the raising loops to resolvesTo.)
func TestK3_C42c_ABorrowedLineStillRaisesDAndC2(t *testing.T) {
	// (d)
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(300)
	w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	_, b := w.payout(300) // another live payout with its own reference
	borrowed := w.payoutLine(*b.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 300)
	w.requireOne(w.stmtRun(w.source(false, borrowed)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "(d) in the run that carries the borrowed line")
	w.requireOne(w.stmtRun(w.pastSource(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "(d) in a later persisted-only run")

	// (c2)
	w2 := newK3World(t, k3Opts{base: 1})
	w2.k2Setup()
	_, pa := w2.ambiguousPayout(500)
	res := w2.executeM2(pa.ID, ResolutionM2DeclarePaid)
	if out, err := w2.k2Compensate(adjustment.DirectionCreditPlayer, 200, *res.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("compensating credit: %v %+v", err, out)
	}
	_, pb := w2.payout(500)
	bl := w2.payoutLine(*pb.ProviderReference, pa.MerchantReference, statement.PaymentStatusSucceeded, 500)
	w2.requireOne(w2.stmtRun(w2.source(false, bl)), reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, pa.ID, "(c2) in the run that carries the borrowed line")
	w2.requireOne(w2.stmtRun(w2.pastSource(false)), reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, pa.ID, "(c2) in a later persisted-only run")
}
