//go:build integration

package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// ledgerTxOf returns (transaction_type, provider_tx_id, idempotency_key,
// correlation_id) of a ledger transaction.
func (w *k3World) ledgerTxOf(id uuid.UUID) (txType string, ptx, key *string, corr uuid.UUID) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT transaction_type, provider_tx_id, idempotency_key, correlation_id FROM ledger_transactions WHERE id = $1`, id).
			Scan(&txType, &ptx, &key, &corr)
	})
	return
}

// legs returns the entries of a ledger transaction as "type/direction/amount".
func (w *k3World) legs(id uuid.UUID) []string {
	w.t.Helper()
	var out []string
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT a.account_type, e.direction, e.amount::text FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id
			WHERE e.ledger_transaction_id = $1 ORDER BY e.direction, a.account_type`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a, d, amt string
			if err := rows.Scan(&a, &d, &amt); err != nil {
				return err
			}
			out = append(out, a+"/"+d+"/"+amt)
		}
		return rows.Err()
	})
	return out
}

// C-4 (R): M2 paid and not-paid, from `ambiguous` and from allow-listed
// `disputed`, withdrawal `submitted`: correct attempt/withdrawal outcomes,
// postings and keys, a reserved id that lives ONLY in the ledger key, a balanced
// ledger, RunLedgerVsProjection = 0 with the psp_clearing delta asserted (C-49).
func TestK3_C4_M2DeclarePaid_FromAmbiguous(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(700)
	clearingBefore, holdBefore := w.pspClearing(), w.walletBalance("player_withdrawal_hold")

	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)

	if res.State != ResolutionExecuted || res.LedgerTransactionID == nil || res.ReservedProviderTxID == nil {
		t.Fatalf("resolution not executed with a link: %+v", res)
	}
	if want := providerref.ReservedOperatorPrefix + res.ID.String(); *res.ReservedProviderTxID != want || want != ReservedDeclaredTxID(res.ID) {
		t.Fatalf("reserved id = %q, want %q", *res.ReservedProviderTxID, want)
	}
	after := w.attempt(a.ID)
	if after.State != AttemptSucceeded || after.LastEvidenceKind != EvidenceOperator {
		t.Fatalf("attempt after = %s/%s, want succeeded/operator", after.State, after.LastEvidenceKind)
	}
	if after.ProviderReference == nil || *after.ProviderReference != *a.ProviderReference {
		t.Fatalf("the attempt's provider reference must be unchanged (the reserved id is never bound to it): %v", after.ProviderReference)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateCompleted || got.ReleaseLedgerTransactionID == nil || *got.ReleaseLedgerTransactionID != *res.LedgerTransactionID {
		t.Fatalf("withdrawal = %s release=%v", got.State, got.ReleaseLedgerTransactionID)
	}
	typ, ptx, key, corr := w.ledgerTxOf(*res.LedgerTransactionID)
	if typ != "withdrawal_completed" || ptx == nil || *ptx != *res.ReservedProviderTxID || key == nil ||
		*key != w.provider+":"+*res.ReservedProviderTxID || corr != wr.ID {
		t.Fatalf("Step B posting keys wrong: type=%s ptx=%v key=%v corr=%s", typ, ptx, key, corr)
	}
	if got := strings.Join(w.legs(*res.LedgerTransactionID), ","); got != "psp_clearing/credit/700,player_withdrawal_hold/debit/700" {
		t.Fatalf("legs = %s", got)
	}
	if d := w.pspClearing() - clearingBefore; d != 700 {
		t.Fatalf("psp_clearing delta = %d, want +700 (the F11 residual: the provider may never have paid)", d)
	}
	if d := w.walletBalance("player_withdrawal_hold") - holdBefore; d != -700 {
		t.Fatalf("hold delta = %d, want -700 (the hold leg is debited by the Step B posting)", d)
	}
	acts := w.auditActions("payment_manual_resolution", res.ID.String())
	for _, want := range []string{"payment.manual_resolution_requested", "payment.manual_resolution_executed"} {
		if !contains(acts, want) {
			t.Fatalf("audit actions %v lack %s", acts, want)
		}
	}
	w.assertInvariants()
	// C-49: the drift stream is clean after an M2 "paid".
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, ms, err := reconciliation.RunLedgerVsProjection(ctx, tx, w.f.tenantID, a.CreatedAt.Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			return err
		}
		if len(ms) != 0 {
			t.Fatalf("RunLedgerVsProjection after M2 paid: %d mismatches", len(ms))
		}
		return nil
	})
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestK3_C4_M2DeclareNotPaid_FromAmbiguous(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(650)
	clearingBefore, cashBefore := w.pspClearing(), w.walletBalance("player_cash")

	res := w.executeM2(a.ID, ResolutionM2DeclareNotPaid)

	if res.ReservedProviderTxID != nil {
		t.Fatalf("a not-paid resolution has no reserved id: %v", *res.ReservedProviderTxID)
	}
	after := w.attempt(a.ID)
	if after.State != AttemptDeclined || after.LastEvidenceKind != EvidenceOperator {
		t.Fatalf("attempt after = %s/%s, want declined/operator", after.State, after.LastEvidenceKind)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateFailed {
		t.Fatalf("withdrawal = %s, want failed", got.State)
	}
	typ, ptx, key, corr := w.ledgerTxOf(*res.LedgerTransactionID)
	if typ != "withdrawal_failed" || ptx != nil || key == nil || *key != wr.ID.String()+":failed" || corr != wr.ID {
		t.Fatalf("Step A reversal keys wrong: type=%s ptx=%v key=%v corr=%s", typ, ptx, key, corr)
	}
	if got := strings.Join(w.legs(*res.LedgerTransactionID), ","); got != "player_cash/credit/650,player_withdrawal_hold/debit/650" {
		t.Fatalf("legs = %s", got)
	}
	if d := w.pspClearing() - clearingBefore; d != 0 {
		t.Fatalf("psp_clearing delta = %d, want 0 for a not-paid release", d)
	}
	if d := w.walletBalance("player_cash") - cashBefore; d != 650 {
		t.Fatalf("player_cash delta = %d, want +650 (hold released back)", d)
	}
	w.assertInvariants()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, ms, err := reconciliation.RunLedgerVsProjection(ctx, tx, w.f.tenantID, a.CreatedAt.Add(-time.Minute), time.Now().Add(time.Hour))
		if err == nil && len(ms) != 0 {
			t.Fatalf("RunLedgerVsProjection after M2 not-paid: %d mismatches", len(ms))
		}
		return err
	})
}

func TestK3_C4_M2_FromAllowListedDisputed(t *testing.T) {
	for _, reason := range []string{"provider_reference_mismatch"} {
		for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
			t.Run(reason+"/"+string(kind), func(t *testing.T) {
				w := newK3World(t, k3Opts{base: 1})
				wr, a := w.disputedPayout(300, reason)
				res := w.executeM2(a.ID, kind)
				after := w.attempt(a.ID)
				want := AttemptSucceeded
				wantWR := withdrawal.StateCompleted
				if kind == ResolutionM2DeclareNotPaid {
					want, wantWR = AttemptDeclined, withdrawal.StateFailed
				}
				if after.State != want || w.withdrawalOf(wr.ID).State != wantWR || res.State != ResolutionExecuted {
					t.Fatalf("attempt=%s withdrawal=%s resolution=%s", after.State, w.withdrawalOf(wr.ID).State, res.State)
				}
				if after.TerminalReason == nil || *after.TerminalReason != reason {
					t.Fatalf("terminal_reason must be unchanged (R-7), got %v", after.TerminalReason)
				}
				w.assertInvariants()
			})
		}
	}
}

// neverSentPayout builds a T15 shape through the real writers: a payout claimed
// and never sent (created, no reference), then a success for a never-sent attempt.
func (w *k3World) neverSentPayout(amount int64) (PaymentAttempt, PaymentAttempt) {
	w.t.Helper()
	w.ensureWithdrawalPolicy()
	_, a := notSentPayoutAttempt(w.t, w.pool, w.orch, w.f, amount, "k3-ns-"+uuid.NewString())
	if a.State != AttemptCreated {
		w.t.Fatalf("setup: want created, got %s", a.State)
	}
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNeverSent(ctx, tx, a.ID, EvidenceCallback, "success_for_never_sent_attempt")
	})
	return a, w.attempt(a.ID)
}

func TestK3_C4_M2_T15NeverSent_NotPaidAdmitted_PaidRefusedForNoReference(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.neverSentPayout(200)
	if a.State != AttemptDisputed || a.ProviderReference != nil {
		t.Fatalf("setup: %s %v", a.State, a.ProviderReference)
	}
	// C-46 / D-3 at the T15 shape: a reference-less attempt cannot be declared paid.
	_, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))
	k3RequireCode(t, err, "MR010")
	// ... but "declare not paid" is admitted (ever_possibly_sent = false).
	res := w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	if w.attempt(a.ID).State != AttemptDeclined || res.State != ResolutionExecuted {
		t.Fatalf("T15 not-paid did not execute")
	}
	w.assertInvariants()
}

// C-5 (ADV): every non-admitted payout dispute reason is refused (MR012) for
// BOTH M2 kinds; the hold is untouched (C-33). The reasons are the classification
// table's refused rows plus the invalid_provider_reference:<reason> family.
func TestK3_C5_C33_M2RefusedReasons_HoldUntouched(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	var refused []string
	for reason, admitted := range PayoutDisputeReasons() {
		if !admitted {
			refused = append(refused, reason)
		}
	}
	for _, r := range []providerref.Reason{providerref.ReasonEmpty, providerref.ReasonTooLong, providerref.ReasonInvalidUTF8, providerref.ReasonControlChar, providerref.ReasonReservedNamespace} {
		refused = append(refused, "invalid_provider_reference:"+string(r))
	}
	for _, reason := range refused {
		wr, a := w.disputedPayout(100, reason)
		for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
			holdBefore := w.walletBalance("player_withdrawal_hold")
			_, err := w.request(w.f1, w.m2In(a.ID, kind))
			k3RequireCode(t, err, "MR012")
			if tok := ResolutionToken(ClassifyResolutionError(err)); tok != TokenForceResolveReasonNotResolved {
				t.Fatalf("%s: token %q", reason, tok)
			}
			if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted || w.walletBalance("player_withdrawal_hold") != holdBefore {
				t.Fatalf("%s: the hold or the withdrawal changed", reason)
			}
		}
	}
	// A NULL dispute reason is refused too (never admitted).
	wr, a := w.payout(100)
	_ = wr
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='disputed', terminal_reason=NULL, last_evidence_kind='callback', resolved_at=now(), next_action_at=NULL WHERE id = $1`, a.ID)
		return err
	})
	_, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	k3RequireCode(t, err, "MR012")
	w.assertInvariants()
}

// C-5 (ADV): M2 on a deposit is refused; M1 on a payout is refused.
func TestK3_C5_KindOperationMismatch(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	dep := w.disputedDeposit(5000)
	_, err := w.request(w.f1, w.m2In(dep.ID, ResolutionM2DeclareNotPaid))
	k3RequireCode(t, err, "MR010")
	_, pa := w.ambiguousPayout(100)
	_, err = w.request(w.f1, w.m1In(pa.ID, "awaiting_psp_refund"))
	k3RequireCode(t, err, "MR010")
}

// C-5 / T14 (LF-15): M2 on a T14 dispute (withdrawal already failed by an
// executed "declare not paid", then a late success) is refused: the withdrawal
// is not submitted and the reason is not admitted.
func TestK3_C5_C7_T14AfterNotPaid_RefusedAndStanding(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(400)
	w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	// The late provider success for the declared-not-paid payout (T14): the real
	// receipt path moves declined -> disputed.
	if _, err := rvApplyReceipt(w.pool, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{
		EventType: "payout", ProviderReference: *a.ProviderReference, Outcome: OutcomeSucceeded, Amount: 400, AssetCode: "EUR",
	}); err != nil {
		t.Fatalf("late success receipt: %v", err)
	}
	after := w.attempt(a.ID)
	if after.State != AttemptDisputed || after.TerminalReason == nil || *after.TerminalReason != "success_after_payout_declined" {
		t.Fatalf("want T14 disputed/success_after_payout_declined, got %s %v", after.State, after.TerminalReason)
	}
	if w.withdrawalOf(wr.ID).State != withdrawal.StateFailed {
		t.Fatal("withdrawal must stay failed")
	}
	for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
		_, err := w.request(w.f1, w.m2In(a.ID, kind))
		if c := k3Code(err); c != "MR012" && c != "MR010" {
			t.Fatalf("M2 on T14: want MR012/MR010, got %v", err)
		}
	}
}

// C-6 (ADV): the DB binding of the executing resolution. A resolution for X used
// on Y, a target mismatch, operator evidence without a resolution, an executed
// resolution reused: all refused.
func TestK3_C6_ResolutionBinding(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, x := w.ambiguousPayout(100)
	_, y := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(x.ID, ResolutionM2DeclareNotPaid))

	err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
		// Y: the resolution is for X.
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='declined', last_evidence_kind='operator', resolved_at=now(), next_action_at=NULL WHERE id = $1`, y.ID)
			return err
		}); err == nil {
			t.Error("a resolution for X was usable on attempt Y")
		}
		// Target mismatch: a not-paid resolution cannot make X succeeded.
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='succeeded', last_evidence_kind='operator', resolved_at=now(), next_action_at=NULL WHERE id = $1`, x.ID)
			return err
		}); err == nil {
			t.Error("a not-paid resolution admitted a succeeded target")
		}
		// The matching target works (sanity: the harness does reach the guard).
		return k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='declined', last_evidence_kind='operator', resolved_at=now(), next_action_at=NULL WHERE id = $1`, x.ID)
			if err != nil {
				t.Errorf("the matching target was refused: %v", err)
			}
			return errK3Rollback
		})
	})
	if err != nil && !strings.Contains(err.Error(), "k3 test rollback") {
		t.Fatal(err)
	}

	// Operator evidence with no resolution at all (tenant session).
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='succeeded', last_evidence_kind='operator', resolved_at=now(), next_action_at=NULL WHERE id = $1`, y.ID)
			return err
		})
		if err == nil {
			t.Error("operator evidence without a resolution moved a payout terminal")
		}
		return nil
	})

	// An EXECUTED resolution is not reusable: not on its own attempt (terminal)
	// and not on another attempt.
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("execute r: %v %+v", err, out)
	}
	res := out.Resolution
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='declined', last_evidence_kind='operator', resolved_at=now(), next_action_at=NULL WHERE id = $1`, y.ID)
			return err
		}); err == nil {
			t.Error("an executed resolution was reused on another attempt")
		}
		return nil
	})
	// C-50: re-approving an executed resolution is refused.
	_, err = w.decide(w.f3, res, ResolutionApprove)
	if err == nil {
		t.Fatal("re-approving an executed resolution succeeded")
	}
}

// C-18: no platform policy -> M1 and M2 are disabled (MR014).
func TestK3_C18_NoPlatformPolicy_Disabled(t *testing.T) {
	w := newK3World(t, k3Opts{base: 0})
	_, a := w.ambiguousPayout(100)
	dep := w.disputedDeposit(5000)
	for _, in := range []ResolutionRequestInput{w.m2In(a.ID, ResolutionM2DeclareNotPaid), w.m1In(dep.ID, "awaiting_psp_refund")} {
		_, err := w.request(w.f1, in)
		k3RequireCode(t, err, "MR014")
		if tok := ResolutionToken(ClassifyResolutionError(err)); tok != TokenForceResolveDisabled {
			t.Fatalf("token %q", tok)
		}
	}
}

// C-41 (LF L-3): "declare not paid" after a possible dispatch with basis
// reconciliation_exhausted is refused; provider_confirmed_out_of_band is admitted.
func TestK3_C41_NotPaidAfterPossibleDispatchNeedsOutOfBandBasis(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.timeoutPayout(100)
	if !a.EverPossiblySent || a.State != AttemptAmbiguous {
		t.Fatalf("setup: the attempt must be ambiguous with ever_possibly_sent, got %s %v", a.State, a.EverPossiblySent)
	}
	in := w.m2In(a.ID, ResolutionM2DeclareNotPaid)
	in.BasisCode = "reconciliation_exhausted"
	_, err := w.request(w.f1, in)
	k3RequireCode(t, err, "MR010")
	in.BasisCode = BasisProviderConfirmedOutOfBand
	if _, err := w.request(w.f1, in); err != nil {
		t.Fatalf("out-of-band basis refused: %v", err)
	}
}

// C-46 (LF D-3): "declare paid" on a reference-less attempt is refused at insert,
// in payment_m2_admits and in the executor.
func TestK3_C46_DeclarePaidNeedsReference(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	// The R-K3-9 shape: an ambiguous timeout BEFORE the provider acknowledged -
	// no reference, ever_possibly_sent.
	_, a := w.timeoutPayout(150)
	if a.ProviderReference != nil || a.State != AttemptAmbiguous {
		t.Fatalf("setup: want an ambiguous reference-less attempt, got %s %v", a.State, a.ProviderReference)
	}
	// (1) insert trigger.
	_, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))
	k3RequireCode(t, err, "MR010")
	// (2) payment_m2_admits returns false for the shape.
	var admits bool
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payment_m2_admits($1, 'ambiguous', NULL, $2, 'succeeded', 'operator')`,
			a.ID, *a.WithdrawalRequestID).Scan(&admits)
	})
	if admits {
		t.Fatal("payment_m2_admits admitted a reference-less declare-paid shape")
	}
	// (3) the executor's Go precondition.
	svc := w.svc
	got := svc.executionRefusal(ManualResolution{Kind: ResolutionM2DeclarePaid, AttemptStateAtSubmission: string(a.State),
		TerminalReasonAtSubmission: a.TerminalReason, ProviderID: a.ProviderID}, a, w.withdrawalOf(*a.WithdrawalRequestID))
	if got != resolutionRefusedPrecond {
		t.Fatalf("executor refusal = %q, want %q", got, resolutionRefusedPrecond)
	}
	// "Declare not paid" stays available for it (with the out-of-band basis, L-3).
	w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	w.assertInvariants()
}

// timeoutPayout dispatches a payout whose phase C outcome is a transport timeout
// (ambiguous): the real writer leaves an ambiguous attempt with NO reference and
// ever_possibly_sent = true.
func (w *k3World) timeoutPayout(amount int64) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	w.t.Helper()
	w.ensureWithdrawalPolicy()
	wr := w.approveWithdrawal(amount, "k3-to-"+uuid.NewString())
	claim, err := w.orch.ClaimForDispatch(context.Background(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		w.t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, claim.Attempt,
		GateResult[WithdrawResult]{Class: ErrorClassAmbiguous, Err: errors.New("k3 simulated timeout")}, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		w.t.Fatalf("ApplyPayoutResult(ambiguous, WithDestinations(pitest.Shared())): %v", err)
	}
	return w.withdrawalOf(wr.ID), w.attempt(claim.Attempt.ID)
}

// LF O-4 + PAY-K3-STATEMENT-SOURCE-WIRING-1: BOTH M2 kinds are refused at
// submission when no statement source is registered for the attempt's provider
// (decided from the process registry), and nothing is written. (Before the
// wiring fix "declare paid" was not source-gated; that old pin is replaced.)
func TestK3_NotPaidRefusedWithoutStatementSource(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1, noStatementSource: true})
	_, a := w.ambiguousPayout(100)
	for _, kind := range []ResolutionKind{ResolutionM2DeclareNotPaid, ResolutionM2DeclarePaid} {
		_, err := w.request(w.f1, w.m2In(a.ID, kind))
		if err == nil || ResolutionToken(ClassifyResolutionError(err)) != TokenForceResolvePreconditionFail || !errors.Is(err, ErrResolutionNoStatementSource) {
			t.Fatalf("%s: want a no-statement-source precondition refusal, got %v", kind, err)
		}
	}
	if n := w.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.f.tenantID); n != 0 {
		t.Fatalf("a refused submission left %d resolution rows", n)
	}
	w.assertInvariants()
}
