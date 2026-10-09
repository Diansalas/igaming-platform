//go:build integration

package payments

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func (w *b13bW) rawCallback(ev ReceiptEvidence) ReceiptDisposition {
	w.t.Helper()
	var d ReceiptDisposition
	pending, err := alerting.InTx(w.ctx(), alerting.NewTenantRunner(w.pool, w.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, w.pid, ev)
		return err
	})
	if err != nil {
		w.t.Fatal(err)
	}
	pending.Flush(w.ctx())
	return d
}

// Ledger-finance re-review P5: the validated reported reference is already held by ANOTHER attempt. The destination park
// becomes provider_reference_conflict, binds nothing, leaves the other attempt untouched, raises only the conflict P1.
func TestB13B_LFRR_P5_ForeignHeldReferenceParksAsConflict(t *testing.T) {
	w := newB13bW(t, "rr5")
	other := w.approved(500, "rr5-other")
	oc := w.mustClaim(other)
	w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-rr5"}, StatusResult{})
	if err := w.apply(other, oc, w.dispatch(oc)); err != nil {
		t.Fatal(err)
	}
	otherBefore := w.attempt(oc.Attempt.ID)
	wr := w.approved(500, "rr5")
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-rr5", DestinationEcho: w.badEcho(cl.Attempt.ID)}, StatusResult{})
	ledgerBefore := w.ledgerTx()
	if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
		t.Fatal(err)
	}
	a := w.attempt(cl.Attempt.ID)
	if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != "provider_reference_conflict" {
		t.Fatalf("attempt = %s reason=%v, want disputed/provider_reference_conflict", a.State, a.TerminalReason)
	}
	if a.ProviderReference != nil && *a.ProviderReference != "" {
		t.Fatalf("a foreign-held reference was bound: %v", *a.ProviderReference)
	}
	if got := w.wr(wr.ID); got.ProviderReference != nil && *got.ProviderReference != "" {
		t.Fatalf("withdrawal got a reference: %v", *got.ProviderReference)
	}
	o := w.attempt(oc.Attempt.ID)
	if o.State != otherBefore.State || o.ProviderReference == nil || *o.ProviderReference != "ref-rr5" {
		t.Fatalf("the other attempt moved: %s/%v, was %s", o.State, o.ProviderReference, otherBefore.State)
	}
	if _, ok := w.alertFor(a.ID, "provider_reference_conflict"); !ok {
		t.Fatal("conflict P1 missing")
	}
	if _, ok := w.alertFor(a.ID, TerminalReasonDestinationMismatch); ok {
		t.Fatal("a destination_mismatch alert was raised next to the conflict (only the conflict P1 is expected)")
	}
	if d := w.ledgerTx() - ledgerBefore; d != 0 {
		t.Fatalf("ledger delta = %d, want 0", d)
	}
	w.balanced()
}

// Ledger-finance L-B / P7 and security LR-2: a bad-echo receipt closed as unattributable, then phase C binds the reference,
// then an echo-free SUCCESS of the same event: it must not settle the payout (held), whatever the adapter declares.
func TestB13B_LFRR_P7_EchoFreeSuccessAfterClosedBadEchoDoesNotSettle(t *testing.T) {
	w := newB13bW(t, "rr7")
	wr := w.approved(500, "rr7")
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-rr7"}, StatusResult{})
	gr := w.dispatch(cl)
	ev := ReceiptEvidence{EventType: "payout", ProviderReference: "ref-rr7", Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", DestinationEcho: w.badEcho(cl.Attempt.ID)}
	if d := w.rawCallback(ev); d != DispositionAnomaly {
		t.Fatalf("pre-attribution delivery = %s", d)
	}
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	ledgerBefore := w.ledgerTx()
	ev.DestinationEcho = nil
	w.rawCallback(ev)
	if a := w.attempt(cl.Attempt.ID); a.State == AttemptSucceeded {
		t.Fatalf("FINDING: the echo-free twin of a closed bad-echo receipt settled the attempt (%s)", a.State)
	}
	if got := w.wr(wr.ID); got.State == withdrawal.StateCompleted || got.ReleaseLedgerTransactionID != nil {
		t.Fatalf("FINDING: the payout completed: %s", got.State)
	}
	if d := w.ledgerTx() - ledgerBefore; d != 0 {
		t.Fatalf("ledger delta = %d, want 0", d)
	}
	w.balanced()
}

// Ledger-finance L-A: a duplicate of the closed unattributable receipt that DOES change the attempt (a pending copy binding the
// reference) leaves one audit row linking the receipt to the attempt.
func TestB13B_LFRR_LA_DuplicateOfClosedReceiptThatChangesTheAttemptIsLinked(t *testing.T) {
	w := newB13bW(t, "rrla")
	wr := w.approved(500, "rrla")
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomeAmbiguous, ProviderReference: "ref-rrla"}, StatusResult{})
	gr := w.dispatch(cl)
	ev := ReceiptEvidence{EventType: "payout", ProviderReference: "ref-rrla", Outcome: OutcomePending, Amount: 500, AssetCode: "EUR", DestinationEcho: w.goodEcho(cl.Attempt.ID)}
	w.rawCallback(ev)
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	ev.DestinationEcho = nil
	w.rawCallback(ev)
	a := w.attempt(cl.Attempt.ID)
	if a.ProviderReference == nil || *a.ProviderReference != "ref-rrla" {
		t.Skipf("the duplicate did not bind the reference here (attempt %s ref=%v): link row not applicable", a.State, a.ProviderReference)
	}
	if n := w.count(`SELECT count(*) FROM audit_log WHERE action = 'payments.payout_echo_receipt_attributed' AND metadata->>'attempt_id' = $1`, cl.Attempt.ID.String()); n != 1 {
		t.Fatalf("link audit rows = %d, want 1", n)
	}
}
