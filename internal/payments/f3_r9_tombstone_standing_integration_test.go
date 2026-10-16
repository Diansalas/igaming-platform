//go:build integration

package payments

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// F3 / A5 R9 (LF-A5-1 (b)): the standing payment_statement finding pay_payout_reversal_tombstone_but_paid for a payout
// the A5 tombstone branch failed while a statement line says the PSP paid it. Raised on every run until PSP return
// evidence or a K2 net recovery clears it; never raised for a branch the PSP never paid.

const r9Kind = reconciliation.MismatchKindPayPayoutReversalTombstoneButPaid

// r9Branch takes a pending payout through the A5 tombstone branch (two grant holders) and returns the request (now
// failed, hold released) and the original reference.
func r9Branch(t *testing.T, amount int64) (rbEnv, *k3World, withdrawal.WithdrawalRequest, string) {
	t.Helper()
	e, w := a5K3(t)
	wr, a := w.payout(amount)
	ref := *a.ProviderReference
	p := e.a5Payload(ref)
	p.Amount = amount
	e.a5MustDecide(t, w.f1.ID, wr.ID, p, PayoutReversalRecorded)
	e.a5MustDecide(t, w.f2.ID, wr.ID, p, PayoutReversalTombstoned)
	got := fpReqState(t, e.pool, e.f, wr.ID)
	if got.State != withdrawal.StateFailed || got.ReleaseLedgerTransactionID == nil {
		t.Fatalf("setup: the branch must fail the request with a release: %s", got.State)
	}
	return e, w, got, ref
}

func r9Findings(ms []reconciliation.Mismatch, requestID uuid.UUID) []reconciliation.Mismatch {
	return mismatchesOf(ms, r9Kind, "withdrawal_request="+requestID.String())
}

func r9Line(w *k3World, ref, status string, amount int64, at time.Time) statement.PaymentStatementLine {
	l := w.payoutLine(ref, "", status, amount)
	l.OccurredAt = at.UTC()
	return l
}

// The legitimate branch the PSP never paid: no succeeded line, no finding, on any run.
func TestF3_R9_NoSucceededLine_NeverRaised(t *testing.T) {
	_, w, wr, ref := r9Branch(t, 500)
	for _, src := range []statement.PaymentStatementSource{w.source(false), w.source(false, r9Line(w, ref, statement.PaymentStatusPending, 500, time.Now())),
		w.source(false, r9Line(w, ref, statement.PaymentStatusDeclined, 500, time.Now()))} {
		if got := r9Findings(w.stmtRun(src), wr.ID); len(got) != 0 {
			t.Fatalf("a branch the PSP never paid must not raise: %s", render(got))
		}
	}
}

// A succeeded line raises; it STANDS on later runs without the line (unwindowed, persisted); a reversed line on the
// original reference with the same amount and asset, dated no earlier, clears it; a later succeeded line re-raises.
func TestF3_R9_SucceededLineRaises_StandsUntilPSPReturnEvidence(t *testing.T) {
	_, w, wr, ref := r9Branch(t, 500)
	t0 := time.Now().Add(-10 * time.Minute)
	ms := w.stmtRun(w.source(false, r9Line(w, ref, statement.PaymentStatusSucceeded, 500, t0)))
	if got := r9Findings(ms, wr.ID); len(got) != 1 {
		t.Fatalf("a succeeded line on the tombstoned reference must raise exactly one finding:\n%s", render(ms))
	}
	for i := 0; i < 2; i++ {
		if got := r9Findings(w.stmtRun(w.source(false)), wr.ID); len(got) != 1 {
			t.Fatalf("run %d without the line: the finding must stand", i)
		}
	}
	// Not return evidence: an older reversed line, another amount, another asset.
	older := r9Line(w, ref, statement.PaymentStatusReversed, 500, t0.Add(-time.Minute))
	other := r9Line(w, ref, statement.PaymentStatusReversed, 499, t0.Add(time.Minute))
	asset := r9Line(w, ref, statement.PaymentStatusReversed, 500, t0.Add(time.Minute))
	asset.AssetCode = "USD"
	for name, l := range map[string]statement.PaymentStatementLine{"older": older, "other amount": other, "other asset": asset} {
		if got := r9Findings(w.stmtRun(w.source(false, l)), wr.ID); len(got) != 1 {
			t.Fatalf("%s reversed line must not clear", name)
		}
	}
	// PSP return evidence clears.
	if got := r9Findings(w.stmtRun(w.source(false, r9Line(w, ref, statement.PaymentStatusReversed, 500, t0.Add(2*time.Minute)))), wr.ID); len(got) != 0 {
		t.Fatalf("a reversed line on the original reference must clear: %s", render(got))
	}
	if got := r9Findings(w.stmtRun(w.source(false)), wr.ID); len(got) != 0 {
		t.Fatal("cleared stays cleared on later runs")
	}
	// The PSP then reports it paid AGAIN (a newer succeeded line): re-raised.
	if got := r9Findings(w.stmtRun(w.source(false, r9Line(w, ref, statement.PaymentStatusSucceeded, 500, t0.Add(3*time.Minute)))), wr.ID); len(got) != 1 {
		t.Fatal("a newer succeeded line after the return must raise again")
	}
}

// D-4: a MOCK reversed line does not clear once a non-MOCK import exists for the pair (raising still uses any import).
func TestF3_R9_MockReturnEvidenceIneligibleOnceARealImportExists(t *testing.T) {
	_, w, wr, ref := r9Branch(t, 500)
	t0 := time.Now().Add(-10 * time.Minute)
	if got := r9Findings(w.stmtRun(w.source(true, r9Line(w, ref, statement.PaymentStatusSucceeded, 500, t0))), wr.ID); len(got) != 1 {
		t.Fatal("a real import's succeeded line raises")
	}
	if got := r9Findings(w.stmtRun(w.source(false, r9Line(w, ref, statement.PaymentStatusReversed, 500, t0.Add(time.Minute)))), wr.ID); len(got) != 1 {
		t.Fatal("a MOCK return line must not clear once a real import exists")
	}
	if got := r9Findings(w.stmtRun(w.source(true, r9Line(w, ref, statement.PaymentStatusReversed, 500, t0.Add(2*time.Minute)))), wr.ID); len(got) != 0 {
		t.Fatal("a real import's return line clears")
	}
}

// K2 net recovery under the branch's withdrawal_failed clears it; a partial recovery does not.
func TestF3_R9_ClearedByK2NetRecoveryOnly(t *testing.T) {
	_, w, wr, ref := r9Branch(t, 500)
	w.k2Setup()
	if got := r9Findings(w.stmtRun(w.source(false, r9Line(w, ref, statement.PaymentStatusSucceeded, 500, time.Now()))), wr.ID); len(got) != 1 {
		t.Fatal("setup: raised")
	}
	if out, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 499, *wr.ReleaseLedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("partial recovery: %v %+v", err, out)
	}
	if got := r9Findings(w.stmtRun(w.source(false)), wr.ID); len(got) != 1 {
		t.Fatal("a partial recovery must not clear")
	}
	if out, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 1, *wr.ReleaseLedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("final recovery: %v %+v", err, out)
	}
	if got := r9Findings(w.stmtRun(w.source(false)), wr.ID); len(got) != 0 {
		t.Fatalf("a full net recovery clears: %s", render(got))
	}
	w.assertInvariants()
}

// Only the A5 branch's request-correlated tombstone counts: a deposit-style tombstone (fresh correlation) on some
// reference with a succeeded payout line raises nothing of this kind.
func TestF3_R9_OnlyRequestCorrelatedTombstones(t *testing.T) {
	e, w := a5K3(t)
	ref := "f3-r9-foreign-" + uuid.NewString()[:8]
	f3ForeignTombstone(t, e, ref)
	ms := w.stmtRun(w.source(false, r9Line(w, ref, statement.PaymentStatusSucceeded, 500, time.Now())))
	if got := mismatchesOf(ms, r9Kind, ""); len(got) != 0 {
		t.Fatalf("a tombstone not correlated to a withdrawal request must not raise R9: %s", render(got))
	}
}
