//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-PAYOUT-UNBOUND-RESOLVE-1 - the no-migration tightenings ADR 0111 §7.3
// says "may start now" (§4.6):
//
//   - I-1 (security I-1 / ledger-finance C-4): payoutCompletedRef clears only
//     when the attempt's OWN positively attributed withdrawal_completed has the
//     attempt's amount and asset, and - where the finding is keyed on an
//     evidencing line (unbound in-run, cross-check B step, standing) - that
//     line's amount and asset equal the attempt's too. Bound sites
//     (capturedUnposted: matchPayment's bound case and checkUnmatchedAttempts)
//     compare the completion.
//   - I-2 (security I-2): loadK3Evidence reads at most a hard per-run cap of
//     persisted lines (64 per lookup key); the (cap+1)-th row FAILS the run with
//     ErrPaymentEvidenceOverflow, reported as the existing P1
//     reconciliation.sweep_run_failed. Never a silent truncation.
//   - L-4: an unbound payout park inside RESOLVE-1's scope (no provider
//     reference) names M4, labelled NOT IMPLEMENTED.
//
// Every run executes as the RUNTIME role (bcWorld.rt) and asserts it wrote only
// its run and mismatch rows plus SUM(debits) = SUM(credits) and a clean
// projection (bcRun). The "own completion" is the BOUND-CLEAR-1 TEST STAND-IN
// (withdrawal.Complete called directly): the governed M4 completion is NOT
// IMPLEMENTED.

const rsAmount = int64(900)

// rsPayout writes a payout attempt for a SUBMITTED withdrawal whose hold and
// amount are wrAmount EUR, while the ATTEMPT carries attAmount / attAsset (the
// I-1 shapes: a completion of another amount or asset than the attempt's).
// x != "" binds x (T4, pending); x == "" leaves the attempt submitting.
func (w *bcWorld) rsPayout(t *testing.T, x string, wrAmount, attAmount int64, attAsset string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	wrID, attemptID := uuid.New(), uuid.New()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		hold, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRequested, IdempotencyKey: "rs-wreq-" + wrID.String(), CorrelationID: wrID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: wrAmount},
				{LedgerAccountID: hold, Direction: ledger.Credit, Amount: wrAmount},
			},
		}); err != nil {
			return fmt.Errorf("post hold: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key, provider_id, provider_reference)
			 VALUES ($1,$2,$3,$4,$5,'EUR',$6,'submitted',$7,$8,$9)`,
			wrID, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, wrAmount, "rs-wd-"+wrID.String(), payProvA, "rs-instr-"+wrID.String()[:8]); err != nil {
			return fmt.Errorf("insert withdrawal: %w", err)
		}
		if _, err := payments.InsertSubmittingAttempt(ctx, tx, payments.NewSubmittingAttempt{
			ID: attemptID, TenantID: w.f.tenantID, Operation: payments.AttemptOperationPayout, WithdrawalRequestID: &wrID,
			ProviderID: payProvA, PaymentMethod: "bank_transfer", AssetCode: attAsset, Amount: attAmount, Interactive: false,
			ClaimToken: uuid.New(), LeaseOwner: "rs-test", LeaseUntil: time.Now().Add(time.Minute),
		}); err != nil {
			return err
		}
		if x == "" {
			return nil
		}
		return payments.MarkAccepted(ctx, tx, attemptID, payments.EvidenceSync, x, time.Now().Add(time.Hour))
	}); err != nil {
		t.Fatalf("rs payout fixture: %v", err)
	}
	return wrID, attemptID
}

// rsUnboundPark parks a submitting payout (no reference) with the unbound
// reason invalid_provider_reference:control_char (T10) - inside RESOLVE-1's
// scope (ADR 0111 §4.1: unbound reason, provider_reference NULL).
func (w *bcWorld) rsUnboundPark(t *testing.T, wrAmount, attAmount int64, attAsset string) bcPark {
	t.Helper()
	wrID, attemptID := w.rsPayout(t, "", wrAmount, attAmount, attAsset)
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return payments.ApplyDisputeFromNonTerminal(ctx, tx, attemptID, payments.EvidenceSync,
			payments.TerminalReasonInvalidProviderReference+":"+string(providerref.ReasonControlChar))
	}); err != nil {
		t.Fatalf("unbound park: %v", err)
	}
	a := w.attempt(t, attemptID)
	if a.State != payments.AttemptDisputed || a.ProviderReference != nil || a.Amount != attAmount || a.AssetCode != attAsset {
		t.Fatalf("setup: want an unbound disputed payout with no reference, got %s ref=%v amount=%d asset=%s", a.State, a.ProviderReference, a.Amount, a.AssetCode)
	}
	return bcPark{attempt: a, wrID: wrID}
}

// rsBoundPark parks a payout holding X through the REAL callback T10
// (callback_amount_asset_mismatch), with the withdrawal of wrAmount EUR and the
// attempt of attAmount attAsset.
func (w *bcWorld) rsBoundPark(t *testing.T, wrAmount, attAmount int64, attAsset string) bcPark {
	t.Helper()
	x := "rs-x-" + uuid.NewString()
	wrID, attemptID := w.rsPayout(t, x, wrAmount, attAmount, attAsset)
	pend := w.attempt(t, attemptID)
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{
		EventType: "payout", ProviderReference: x, MerchantReference: pend.MerchantReference,
		Outcome: payments.OutcomeSucceeded, Amount: attAmount - 1, AssetCode: attAsset,
	})
	a := w.attempt(t, attemptID)
	if a.State != payments.AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != payments.TerminalReasonCallbackAmountAssetMismatch ||
		a.ProviderReference == nil || *a.ProviderReference != x {
		t.Fatalf("setup: want a disputed callback_amount_asset_mismatch payout holding %s, got %s %v %v", x, a.State, a.TerminalReason, a.ProviderReference)
	}
	return bcPark{attempt: a, wrID: wrID, x: x}
}

// rsLine is a payout line under ref naming merchant, of amount/asset.
func rsLine(ref, merchant string, amount int64, asset string) statement.PaymentStatementLine {
	l := payLineFor(payProvA, ref, merchant, statement.PaymentLinePayout, statement.PaymentStatusSucceeded, amount)
	l.AssetCode = asset
	return l
}

// rsUnboundCU asserts exactly one unbound payout finding for p keyed on ref,
// with the L-4 M4-scope wording; standing distinguishes checkStandingUnbound
// from the in-run site.
func rsUnboundCU(t *testing.T, ms []Mismatch, p bcPark, ref string, standing bool, what string) {
	t.Helper()
	got := bcCU(ms, p.attempt.ID)
	if len(got) != 1 {
		t.Fatalf("%s: want exactly one pay_captured_unposted for the unbound payout park %s, got %d:\n%s", what, p.attempt.ID, len(got), renderMismatches(ms))
	}
	m := got[0]
	if m.ExpectedValue != m4ScopePayoutCapturedUnpostedResolutionHint || !strings.Contains(m.ExpectedValue, "M4") || !strings.Contains(m.ExpectedValue, "NOT IMPLEMENTED") ||
		strings.Contains(m.ExpectedValue, "LEDGER-SUSPENSE") || !strings.Contains(m.ReconciliationKey, "provider_reference="+ref) || !strings.Contains(m.ActualValue, "op=payout") {
		t.Fatalf("%s: unbound payout finding misrepresented: %s | %s | %s", what, m.ReconciliationKey, m.ExpectedValue, m.ActualValue)
	}
	if got := strings.Contains(m.ActualValue, "standing: persisted line"); got != standing {
		t.Fatalf("%s: standing=%t but the detail is %q", what, standing, m.ActualValue)
	}
}

// --- I-1: unbound payout parks -------------------------------------------

// Control: the own completion keyed by the line reference, the completion AND
// the line of the attempt's amount and asset, clears in-run and standing.
func TestRes1_I1_UnboundPayoutPark_OwnCompletionAmountAssetEqual_Clears(t *testing.T) {
	w := newBCWorld(t)
	p := w.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	r := "rs-psp-" + uuid.NewString()
	line := rsLine(r, p.attempt.MerchantReference, rsAmount, "EUR")
	rsUnboundCU(t, w.bcRun(t, d2Src(line)), p, r, false, "setup in-run")
	rsUnboundCU(t, w.bcRun(t, d2PastSrc()), p, r, true, "setup standing")
	w.bcComplete(t, p.wrID, payProvA, r)
	bcNone(t, w.bcRun(t, d2PastSrc()), p, "equal completion, standing")
	bcNone(t, w.bcRun(t, k3Cov(2, line)), p, "equal completion, in-run")
	bcNone(t, w.bcRun(t, bcReal(3, line)), p, "equal completion, in-run, real import")
	bcNone(t, w.bcRun(t, d2PastSrc()), p, "equal completion, stays cleared")
}

// The completion of ANOTHER amount (partial or over) or ANOTHER asset than the
// attempt's - still the attempt's own, positively attributed release keyed by
// the line reference - never clears, in-run or standing.
func TestRes1_I1_UnboundPayoutPark_OwnCompletionOtherAmountOrAsset_NeverClears(t *testing.T) {
	for _, c := range []struct {
		name               string
		wrAmount, attAmt   int64
		attAsset, lineAsst string
		lineAmt            int64
	}{
		{"completion_partial_amount", rsAmount - 100, rsAmount, "EUR", "EUR", rsAmount},
		{"completion_over_amount", rsAmount + 100, rsAmount, "EUR", "EUR", rsAmount},
		{"completion_other_asset", rsAmount, rsAmount, "USD", "USD", rsAmount},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newBCWorld(t)
			p := w.rsUnboundPark(t, c.wrAmount, c.attAmt, c.attAsset)
			r := "rs-psp-" + uuid.NewString()
			line := rsLine(r, p.attempt.MerchantReference, c.lineAmt, c.lineAsst)
			rsUnboundCU(t, w.bcRun(t, d2Src(line)), p, r, false, "setup in-run")
			w.bcComplete(t, p.wrID, payProvA, r)
			// Positive attribution holds (the release is the attempt's own, keyed by r):
			// only the I-1 amount/asset comparison keeps the finding.
			var rel string
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT t.provider_tx_id FROM withdrawal_requests wr JOIN ledger_transactions t ON t.id = wr.release_ledger_transaction_id WHERE wr.id = $1`, p.wrID).Scan(&rel)
			}); err != nil || rel != r {
				t.Fatalf("setup: the own completion must be keyed by %s, got %q err=%v", r, rel, err)
			}
			rsUnboundCU(t, w.bcRun(t, d2PastSrc()), p, r, true, "other-amount/asset completion, standing")
			rsUnboundCU(t, w.bcRun(t, k3Cov(2, line)), p, r, false, "other-amount/asset completion, in-run")
			rsUnboundCU(t, w.bcRun(t, bcReal(3, line)), p, r, false, "other-amount/asset completion, in-run, real import")
			rsUnboundCU(t, w.bcRun(t, d2PastSrc()), p, r, true, "other-amount/asset completion, standing after a real import")
		})
	}
}

// The completion equals the attempt, but the EVIDENCING LINE reports another
// amount (partial) or another asset: the PSP's line is not the payout the
// platform completed - never clears, in-run or standing.
func TestRes1_I1_UnboundPayoutPark_EvidencingLineOtherAmountOrAsset_NeverClears(t *testing.T) {
	for _, c := range []struct {
		name    string
		lineAmt int64
		asset   string
	}{
		{"line_partial_amount", rsAmount - 1, "EUR"},
		{"line_over_amount", rsAmount + 1, "EUR"},
		{"line_other_asset", rsAmount, "USD"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newBCWorld(t)
			p := w.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
			r := "rs-psp-" + uuid.NewString()
			line := rsLine(r, p.attempt.MerchantReference, c.lineAmt, c.asset)
			rsUnboundCU(t, w.bcRun(t, d2Src(line)), p, r, false, "setup in-run")
			w.bcComplete(t, p.wrID, payProvA, r)
			rsUnboundCU(t, w.bcRun(t, d2PastSrc()), p, r, true, "line of another amount/asset, standing")
			rsUnboundCU(t, w.bcRun(t, k3Cov(2, line)), p, r, false, "line of another amount/asset, in-run")
			// An EQUAL line under the same reference arriving later clears that line
			// only: the persisted unequal line keeps the standing finding loud.
			equal := rsLine(r, p.attempt.MerchantReference, rsAmount, "EUR")
			rsUnboundCU(t, w.bcRun(t, k3Cov(3, equal)), p, r, true, "an equal line does not hide the persisted unequal one")
			rsUnboundCU(t, w.bcRun(t, d2PastSrc()), p, r, true, "standing")
		})
	}
}

// The merchant cross-check B step (a line resolved by settlement reference to
// another attempt P1, naming the unbound park B by merchant reference): B's own
// release cannot be keyed by that reference (P1 holds it), so it never clears,
// whatever the amounts - the I-1 comparison at that site narrows a rule that is
// already closed there (disclosed as equivalent in the mutation evidence).
func TestRes1_I1_MerchantCrossCheckB_NeverClears(t *testing.T) {
	w := newBCWorld(t)
	s1 := "rs-settle-" + uuid.NewString()[:8]
	p1 := w.payoutFixture(t, payProvA, "rs-instr-1-"+uuid.NewString()[:8], s1, rsAmount, true)
	b := w.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	line := rsLine(s1, b.attempt.MerchantReference, rsAmount, "EUR")
	ms := w.bcRun(t, d2Src(line))
	rsUnboundCU(t, ms, b, s1, false, "cross-check B in-run")
	if m := bcCU(ms, b.attempt.ID)[0]; !strings.Contains(m.ActualValue, "line resolved by reference to attempt="+p1.String()) {
		t.Fatalf("want the in-run cross-check finding for B, got %s", m.ActualValue)
	}
	rsUnboundCU(t, w.bcRun(t, d2PastSrc()), b, s1, true, "cross-check B standing")
}

// --- I-1: bound payout parks (both capturedUnposted sites) ----------------

// The bound park's own completion keyed by X, of another amount (partial or
// over) or another asset than the attempt's: never clears at matchPayment's
// bound site (a line names X) nor at checkUnmatchedAttempts (no line).
func TestRes1_I1_BoundPayoutPark_OwnCompletionOtherAmountOrAsset_NeverClears(t *testing.T) {
	for _, c := range []struct {
		name     string
		wrAmount int64
		asset    string
	}{
		{"completion_partial_amount", rsAmount - 100, "EUR"},
		{"completion_over_amount", rsAmount + 100, "EUR"},
		{"completion_other_asset", rsAmount, "USD"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newBCWorld(t)
			p := w.rsBoundPark(t, c.wrAmount, rsAmount, c.asset)
			line := rsLine(p.x, p.attempt.MerchantReference, rsAmount, c.asset)
			bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "setup standing")
			w.bcComplete(t, p.wrID, payProvA, p.x)
			bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "other-amount/asset own completion, standing (checkUnmatchedAttempts)")
			bcRequire(t, w.bcRun(t, d2Src(line)), p, true, "other-amount/asset own completion, in-run (matchPayment bound site)")
			bcRequire(t, w.bcRun(t, bcReal(3, line)), p, true, "other-amount/asset own completion, in-run, real import")
			bcRequire(t, w.bcRun(t, d2PastSrc()), p, false, "standing again")
		})
	}
	t.Run("control_equal_completion_clears_both_sites", func(t *testing.T) {
		w := newBCWorld(t)
		q := w.rsBoundPark(t, rsAmount, rsAmount, "EUR")
		bcRequire(t, w.bcRun(t, d2PastSrc()), q, false, "setup standing")
		w.bcComplete(t, q.wrID, payProvA, q.x)
		bcNone(t, w.bcRun(t, d2PastSrc()), q, "equal completion, standing")
		bcNone(t, w.bcRun(t, d2Src(rsLine(q.x, q.attempt.MerchantReference, rsAmount, "EUR"))), q, "equal completion, in-run")
	})
}

// --- deposits unchanged ----------------------------------------------------

// A deposit unbound park whose evidencing line differs in amount from the
// attempt still clears on a completed deposit_reversal naming the line
// reference (deposits ignore the I-1 line comparison: payout-only tightening).
func TestRes1_I1_DepositUnboundPark_LineAmountIrrelevant_ClearingUnchanged(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	line := d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount-1)
	d2CUFor(t, w.d2Run(t, d2Src(line)), pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID)
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("rs-rev-"+uuid.NewString(), pk.pspRef, d2Amount))), "a deposit_reversal on the line reference still clears a deposit park")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and it stays cleared")
	w.d2AssertNoMoney(t, pk)
}

// --- tenant isolation ------------------------------------------------------

// Tenant B, under tenant A's provider id and A's line reference, completes its
// OWN unbound park of an amount EQUAL to A's attempt: B clears, A does not
// (A's own completion is of another amount); then nothing B does changes A.
func TestRes1_I1_TenantIsolation(t *testing.T) {
	a := newBCWorld(t)
	b := newBCWorldOn(t, a.pool, a.rt.pool)
	pa := a.rsUnboundPark(t, rsAmount-100, rsAmount, "EUR")
	pb := b.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	r := "rs-psp-" + uuid.NewString()
	rsUnboundCU(t, a.bcRun(t, d2Src(rsLine(r, pa.attempt.MerchantReference, rsAmount, "EUR"))), pa, r, false, "A setup")
	rsUnboundCU(t, b.bcRun(t, d2Src(rsLine(r, pb.attempt.MerchantReference, rsAmount, "EUR"))), pb, r, false, "B setup")
	a.bcComplete(t, pa.wrID, payProvA, r)
	b.bcComplete(t, pb.wrID, payProvA, r)
	bcNone(t, b.bcRun(t, d2PastSrc()), pb, "B's equal own completion clears B")
	ms := b.bcRun(t, d2PastSrc())
	if len(bcCU(ms, pa.attempt.ID)) != 0 {
		t.Fatalf("tenant B reported tenant A's attempt:\n%s", renderMismatches(ms))
	}
	rsUnboundCU(t, a.bcRun(t, d2PastSrc()), pa, r, true, "A's partial own completion does not clear A (B's equal one is not A's)")
}

// --- I-2: bounded persisted-lines read, refusal on overflow ---------------

// rsManyLines is n distinct succeeded payout lines naming merchant.
func rsManyLines(merchant string, n int) []statement.PaymentStatementLine {
	out := make([]statement.PaymentStatementLine, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, rsLine(fmt.Sprintf("rs-many-%03d-%s", i, uuid.NewString()[:8]), merchant, rsAmount, "EUR"))
	}
	return out
}

// Exactly the cap (64 lines for one lookup key) runs; the 65th persisted line
// read - here a second import repeating ONE of them (overlapping imports count:
// the cap bounds rows READ) - refuses the run: ErrPaymentEvidenceOverflow, no
// run row, no mismatch row, ledger untouched; through the sweep entry point it
// is the existing P1 reconciliation.sweep_run_failed (phase match). It stays
// refused on every later run (never truncated into a clean or partial verdict).
func TestRes1_I2_PersistedLinesCap_OverflowRefusesTheRun(t *testing.T) {
	w := newBCWorld(t)
	p := w.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	if k3PersistedLinesCap(1) != 64 {
		t.Fatalf("cap for one lookup key = %d, want 64 (ADR 0111 §4.4/§4.6)", k3PersistedLinesCap(1))
	}
	lines := rsManyLines(p.attempt.MerchantReference, 64)
	ms := w.bcRun(t, d2Src(lines...)) // exactly the cap: the run completes
	if got := len(bcCU(ms, p.attempt.ID)); got != 64 {
		t.Fatalf("at the cap: want 64 findings (one per evidencing reference), got %d", got)
	}

	// The 65th row read.
	id, _ := w.rt.fetchIngest(t, k3Cov(2, lines[0]), PaymentStatementOptions{})
	before := capturePay(t, w.pool, w.f.tenantID)
	_, got, _, err := w.rt.matchErr(t, id, PaymentStatementOptions{})
	if !errors.Is(err, ErrPaymentEvidenceOverflow) || len(got) != 0 {
		t.Fatalf("65 persisted lines for one key: want ErrPaymentEvidenceOverflow and no findings, got err=%v findings=%d", err, len(got))
	}
	if after := capturePay(t, w.pool, w.f.tenantID); after != before {
		t.Fatalf("a refused run must write nothing (no run, no mismatch, no ledger): %+v -> %+v", before, after)
	}
	w.bcInvariants(t)

	// The sweep entry point: the existing P1 run failure, audited, every run.
	for i := 0; i < 2; i++ {
		before := capturePay(t, w.pool, w.f.tenantID)
		var logBuf strings.Builder
		out := ReconcilePaymentStatementForTenant(context.Background(), w.rt.pool, slog.New(slog.NewTextHandler(&logBuf, nil)),
			w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), d2PastSrc(), PaymentStatementOptions{})
		if !errors.Is(out.Err, ErrPaymentEvidenceOverflow) || out.Run.ID != uuid.Nil {
			t.Fatalf("sweep run %d: want the overflow refusal, got err=%v run=%v", i, out.Err, out.Run.ID)
		}
		if !strings.Contains(logBuf.String(), "P1") {
			t.Fatalf("sweep run %d: want a P1 error log, got %s", i, logBuf.String())
		}
		after := capturePay(t, w.pool, w.f.tenantID)
		if after.runs != before.runs || after.mismatches != before.mismatches || after.ledgerTx != before.ledgerTx || after.debits != after.credits {
			t.Fatalf("sweep run %d: a refused run must record no run/mismatch and touch no ledger", i)
		}
		var n int
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			  AND metadata->>'stream' = 'payment_statement' AND metadata->>'phase' = 'match' AND metadata->>'severity' = 'P1'
			  AND metadata->>'error' LIKE '%per-run cap%'`, w.f.tenantID).Scan(&n)
		}); err != nil || n != i+1 {
			t.Fatalf("sweep run %d: want %d audited P1 sweep_run_failed (phase match, overflow), got %d err=%v", i, i+1, n, err)
		}
	}
	a := w.attempt(t, p.attempt.ID)
	var state string
	var release *uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, p.wrID).Scan(&state, &release)
	}); err != nil {
		t.Fatal(err)
	}
	if a.State != payments.AttemptDisputed || *a.TerminalReason != *p.attempt.TerminalReason || state != "submitted" || release != nil {
		t.Fatalf("after the refused runs the park moved: attempt %s %v, withdrawal %s release=%v", a.State, a.TerminalReason, state, release)
	}
}

// The cap is per RUN, scaled by the lookup keys fixed before the read (64 per
// key): with two parks (two keys, cap 128) 65 lines on ONE key are all read -
// nothing is dropped and the run completes with every finding; 129 rows refuse.
// Another tenant's lines never count (tenant_id predicate).
func TestRes1_I2_PersistedLinesCap_PerRunAcrossKeys_TenantScoped(t *testing.T) {
	w := newBCWorld(t)
	p := w.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	q := w.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	lines := rsManyLines(p.attempt.MerchantReference, 65)
	ms := w.bcRun(t, d2Src(lines...))
	if got := len(bcCU(ms, p.attempt.ID)); got != 65 {
		t.Fatalf("65 lines under a 128 cap: want all 65 findings (nothing truncated), got %d", got)
	}
	if got := len(bcCU(ms, q.attempt.ID)); got != 0 {
		t.Fatalf("q has no line: want no finding, got %d", got)
	}

	// Tenant B on the same provider id: A's 65 lines never count toward B's cap.
	b := newBCWorldOn(t, w.pool, w.rt.pool)
	pb := b.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	bl := rsManyLines(pb.attempt.MerchantReference, 64)
	if got := len(bcCU(b.bcRun(t, d2Src(bl...)), pb.attempt.ID)); got != 64 {
		t.Fatalf("tenant B at its own cap: want 64 findings, got %d", got)
	}

	// A: 65 + 64 (q's key) = 129 rows > 128.
	id, _ := w.rt.fetchIngest(t, k3Cov(2, rsManyLines(q.attempt.MerchantReference, 64)...), PaymentStatementOptions{})
	if _, _, _, err := w.rt.matchErr(t, id, PaymentStatementOptions{}); !errors.Is(err, ErrPaymentEvidenceOverflow) {
		t.Fatalf("129 rows for two keys: want ErrPaymentEvidenceOverflow, got %v", err)
	}
	// B is unaffected by A's overflow.
	if got := len(bcCU(b.bcRun(t, d2PastSrc()), pb.attempt.ID)); got != 64 {
		t.Fatalf("tenant B after A's overflow: want its 64 standing findings, got %d", got)
	}
}

// --- L-4 wording -------------------------------------------------------------

// Only an unbound payout park WITHOUT a provider reference (ADR 0111 §4.1, C-6)
// names M4; a bound payout park and an unbound reason that holds a reference
// keep the R-K3-8 payout wording; deposits keep F13.
func TestRes1_L4_M4HintOnlyInResolveScope(t *testing.T) {
	w := newBCWorld(t)
	p := w.rsUnboundPark(t, rsAmount, rsAmount, "EUR")
	r := "rs-psp-" + uuid.NewString()
	rsUnboundCU(t, w.bcRun(t, d2Src(rsLine(r, p.attempt.MerchantReference, rsAmount, "EUR"))), p, r, false, "unbound, no reference")

	bp := w.rsBoundPark(t, rsAmount, rsAmount, "EUR")
	bcRequire(t, w.bcRun(t, d2PastSrc()), bp, false, "bound keeps the R-K3-8 wording")

	// invalid_provider_reference:* that HOLDS a reference (unbound class, outside
	// the C-6 scope): the R-K3-8 wording.
	x := "rs-held-" + uuid.NewString()
	_, held := w.rsPayout(t, x, rsAmount, rsAmount, "EUR")
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return payments.ApplyDisputeFromNonTerminal(ctx, tx, held, payments.EvidenceSync,
			payments.TerminalReasonInvalidProviderReference+":"+string(providerref.ReasonControlChar))
	}); err != nil {
		t.Fatal(err)
	}
	ha := w.attempt(t, held)
	r2 := "rs-psp-" + uuid.NewString()
	got := bcCU(w.bcRun(t, d2Src(rsLine(r2, ha.MerchantReference, rsAmount, "EUR"))), held)
	if len(got) != 1 || got[0].ExpectedValue != payoutCapturedUnpostedResolutionHint {
		t.Fatalf("unbound reason holding a reference: want the R-K3-8 payout wording, got %+v", got)
	}
}
