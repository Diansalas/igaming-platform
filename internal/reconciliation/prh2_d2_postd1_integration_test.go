//go:build integration

// PRH-2 D2, after D1 merged (7adb0c5): end-to-end coverage of the dispute
// reasons D1 added or named (ADR 0095 §35, §36).
//
//   - TestD2_10: QA D2-F1 - a REAL D1 poll park (poll_amount_mismatch,
//     poll_reference_mismatch), produced by the payments sweeper polling the
//     MOCK, is reported by the payment_statement stream as
//     pay_captured_unposted, in-run and standing.
//   - TestD2_11: a REAL callback_amount_asset_mismatch park (verified callback
//     with a different amount on a live attempt) - bound, in-run and standing.
//   - TestD2_12: PAY-POLL-DECLINED-ALERT-RECON-1 part (b) - a DECLINED attempt
//     (the T13 shape) plus a succeeded statement line gives pay_status_mismatch.
//
// Time-dependent sweeper behaviour follows plan §5.0 T-1: the attempt is made
// due by writing next_action_at in the fixture, never by sleeping.
package reconciliation

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// d2PollOnce makes the attempt due (fixture timestamp) and runs one real sweep.
func (w *d2World) d2PollOnce(t *testing.T, attemptID uuid.UUID) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET next_action_at = now() WHERE id = $1`, attemptID)
		return err
	}); err != nil {
		t.Fatalf("setup: make due: %v", err)
	}
	sw := payments.NewSweeper(w.pool, w.orch, allowAllKYCGate{}, payments.MockCredentialResolver{})
	if st := sw.RunOnce(context.Background(), []uuid.UUID{w.f.tenantID}); len(st.Errors) > 0 {
		t.Fatalf("sweeper: %v", st.Errors)
	}
}

func (w *d2World) d2DisputeAudits(t *testing.T, attemptID uuid.UUID, reason string) int64 {
	t.Helper()
	var n int64
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed'
		   AND target_id = $2 AND metadata->>'terminal_reason' = $3`, w.f.tenantID, attemptID.String(), reason).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestD2_10_RealPollPark_EndToEnd is QA D2-F1.
func TestD2_10_RealPollPark_EndToEnd(t *testing.T) {
	cases := []struct {
		reason    string
		status    func(ref string) payments.StatusResult
		lineAmt   int64
		amtDiffer bool
	}{
		{payments.TerminalReasonPollAmountMismatch, func(ref string) payments.StatusResult {
			return payments.StatusResult{ProviderReference: ref, Outcome: payments.OutcomeSucceeded, Amount: d2PSPAmount, AssetCode: "EUR"}
		}, d2PSPAmount, true},
		{payments.TerminalReasonPollReferenceMismatch, func(string) payments.StatusResult {
			return payments.StatusResult{ProviderReference: "d2-echo-" + uuid.NewString(), Outcome: payments.OutcomeSucceeded, Amount: d2Amount, AssetCode: "EUR"}
		}, d2Amount, false},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			w := newD2World(t)
			a := w.deposit(t, d2Amount)
			if a.State != payments.AttemptPending || a.ProviderReference == nil {
				t.Fatalf("setup: want a pending deposit with a bound reference, got %s %v", a.State, a.ProviderReference)
			}
			ref := *a.ProviderReference
			w.p.setStatus(ref, c.status(ref))
			w.d2PollOnce(t, a.ID)

			pk := d2Parked{attempt: w.mustParked(t, a.ID, c.reason, false), pspRef: ref}
			if pk.attempt.ProviderReference == nil || *pk.attempt.ProviderReference != ref {
				t.Fatalf("the poll park must keep the bound reference %q, got %v", ref, pk.attempt.ProviderReference)
			}
			if n := w.d2DisputeAudits(t, a.ID, c.reason); n != 1 {
				t.Fatalf("want exactly one payment.attempt_disputed audit for the real D1 park, got %d", n)
			}

			want := map[MismatchKind]int{d2KindCU: 1}
			if c.amtDiffer {
				want[MismatchKindPayAmountMismatch] = 1
			}
			ms := w.d2Run(t, d2Src(d2Line(ref, "", statement.PaymentStatusSucceeded, c.lineAmt)))
			d2Expect(t, ms, want)
			if m := d2CUFor(t, ms, a.ID); !strings.Contains(m.ActualValue, "terminal_reason="+c.reason) {
				t.Errorf("detail must name the D1 reason: %s", m.ActualValue)
			}
			ms = w.d2Run(t, d2Src())
			d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1})
			d2CUFor(t, ms, a.ID)
			w.d2AssertNoMoney(t, pk)
		})
	}
}

// TestD2_11_RealCallbackAmountAssetMismatch_IsBound: classification per the
// D2 review P1 (bound: the same captured-but-unposted exposure as
// sync_amount_mismatch).
func TestD2_11_RealCallbackAmountAssetMismatch_IsBound(t *testing.T) {
	w := newD2World(t)
	a := w.deposit(t, d2Amount)
	ref := *a.ProviderReference
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
		EventType: "deposit", ProviderReference: ref, Outcome: payments.OutcomeSucceeded, Amount: d2PSPAmount, AssetCode: "EUR",
	})
	pk := d2Parked{attempt: w.mustParked(t, a.ID, payments.TerminalReasonCallbackAmountAssetMismatch, false), pspRef: ref}
	ms := w.d2Run(t, d2Src(d2Line(ref, "", statement.PaymentStatusSucceeded, d2PSPAmount)))
	d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1, MismatchKindPayAmountMismatch: 1})
	d2CUFor(t, ms, a.ID)
	ms = w.d2Run(t, d2Src())
	d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1})
	w.d2AssertNoMoney(t, pk)
}

// TestD2_12_DeclinedAttempt_SucceededLine_IsStatusMismatch pins
// PAY-POLL-DECLINED-ALERT-RECON-1 part (b): a declined attempt (D1 audits a
// contradicting poll on it without a state change, the T13 shape) plus a
// succeeded statement line for its reference is pay_status_mismatch - the
// existing "provider succeeded, platform not succeeded" rule; no matcher
// change was needed.
func TestD2_12_DeclinedAttempt_SucceededLine_IsStatusMismatch(t *testing.T) {
	w := newD2World(t)
	a := w.deposit(t, payments.MockAmountPlayerDeclineNoCascade)
	if a.State != payments.AttemptDeclined || a.ProviderReference == nil {
		t.Fatalf("setup: want a declined attempt with a reference, got %s %v", a.State, a.ProviderReference)
	}
	ref := *a.ProviderReference
	ms := w.d2Run(t, d2Src(d2Line(ref, "", statement.PaymentStatusSucceeded, payments.MockAmountPlayerDeclineNoCascade)))
	d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayStatusMismatch: 1})
	mustKeyed(t, ms, MismatchKindPayStatusMismatch, "attempt="+a.ID.String())
	// A declined line for it is clean (control).
	d2Expect(t, w.d2Run(t, d2Src(d2Line(ref, "", statement.PaymentStatusDeclined, payments.MockAmountPlayerDeclineNoCascade))), map[MismatchKind]int{})
	w.d2AssertBalanced(t)
}
