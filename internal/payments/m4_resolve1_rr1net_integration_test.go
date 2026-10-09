//go:build integration

// r30 (owner decisions 1 and 2 of 2026-10-09, ADR 0095 §48; ADR 0111 §21),
// through the REAL payment_statement stream, the REAL M4 four-eyes execution
// and the REAL K2 path, in the runtime-shaped non-superuser role of the K3
// worlds. MOCK statement sources only.
//
//   - Decision 2: the recovery of M2 (d) and RR-1 is NET - executed
//     compensating_entry debits under the causation (withdrawal_failed), same
//     wallet and asset, none reversed, minus every executed credit under that
//     causation. "A debit occurred" is not enough.
//   - Decision 1 (Q-R21-1): RR-1 also applies to the BOUND finding of a
//     recovered destination_mismatch park, which stops ONLY when the complete
//     predicate positively holds for that park.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// rr30K2 runs one K2 request (f3 initiates, f4 approves) on any wallet, asset,
// direction and reason code, and returns the outcome and the request's ledger
// transaction (uuid.Nil when it did not execute).
func (w *k3World) rr30K2(wallet uuid.UUID, asset string, dir adjustment.Direction, reason adjustment.ReasonCode, amount int64, causation uuid.UUID) (adjustment.Outcome, adjustment.Request, uuid.UUID, error) {
	w.t.Helper()
	h := k3EvidenceHash()
	r, err := w.k2Service().Submit(k3Ctx(w.f3), w.k2Target(w.f3), adjustment.SubmitInput{
		WalletID: wallet, AssetCode: asset, Direction: dir, Amount: amount, ReasonCode: reason,
		CausationTransactionID: &causation, EvidenceRefHash: &h, Note: "r30 net recovery"}, adjustment.Meta{RequestID: "r30-k2"})
	if err != nil {
		return adjustment.Outcome{}, adjustment.Request{}, uuid.Nil, err
	}
	o, err := w.k2Decide(w.f4, r)
	if err != nil || !o.Executed {
		return o, r, uuid.Nil, err
	}
	var tx uuid.UUID
	w.tx(func(ctx context.Context, t pgx.Tx) error {
		return t.QueryRow(ctx, `SELECT ledger_transaction_id FROM ledger_adjustment_requests WHERE id = $1`, r.ID).Scan(&tx)
	})
	return o, r, tx, nil
}

// rr30Must runs rr30K2 and requires execution.
func (w *k3World) rr30Must(dir adjustment.Direction, reason adjustment.ReasonCode, amount int64, causation uuid.UUID, what string) uuid.UUID {
	w.t.Helper()
	o, _, tx, err := w.rr30K2(w.f.walletID, "EUR", dir, reason, amount, causation)
	if err != nil || !o.Executed {
		w.t.Fatalf("%s: %v %+v", what, err, o)
	}
	return tx
}

// rr30PlantReversal posts, in the system session (ADR 0110 T5: no K2 path
// writes one), a ledger transaction naming orig in reverses_transaction_id. It
// moves no player cash: a reversal is a reversal whatever its legs.
func (w *k3World) rr30PlantReversal(orig uuid.UUID) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.f.tenantID,
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		pid, ptx := "r30-planted", uuid.NewString()
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxCasinoWin,
			IdempotencyKey: "r30-planted:" + ptx, ProviderID: &pid, ProviderTxID: &ptx, CorrelationID: uuid.New(), ReversesTransactionID: &orig,
			Entries: []ledger.EntryInput{{LedgerAccountID: ids[0], Direction: ledger.Debit, Amount: 1},
				{LedgerAccountID: ids[1], Direction: ledger.Credit, Amount: 1}}})
		return err
	})
}

// rr30R1Detail returns the single R-1 finding's detail for the attempt.
func (m *m4World) rr30R1Detail(ms []reconciliation.Mismatch, attempt uuid.UUID) string {
	m.t.Helper()
	xs := mismatchesOf(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, "attempt="+attempt.String())
	if len(xs) != 1 {
		m.t.Fatalf("want one R-1 for %s, got %d:\n%s", attempt, len(xs), render(ms))
	}
	return xs[0].ActualValue
}

// --- decision 2: NET recovery (RR-1 over an unbound M4 not-paid park) ---------------------------

// A debit later offset by a credit with the same causation keeps raising (the
// debit-only false positive of security LOW-1): the full recovery stops both
// findings, a compensating credit of ONE unit re-raises them on every run, the
// detail reports the net, and - INV-ADJ-6 counts the earlier debit - the debit
// cannot be redone under the causation, so it stays raised.
func TestRR30_DebitOffsetByCredit_KeepsRaising_NetReported(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, failedTx := rr1NotPaid(m, 840)
	l := m.rr1Line("rr30-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 840, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")
	m.rr30Must(adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 840, failedTx, "full debit")
	m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, "full net recovery")

	m.rr30Must(adjustment.DirectionCreditPlayer, adjustment.ReasonCompensatingEntry, 1, failedTx, "credit of one unit, same causation")
	for i := 0; i < 2; i++ {
		ms := m.stmtRun(m.source(false))
		m.rr1RequireRaised(ms, p.fresh.ID, fmt.Sprintf("debit offset by a credit, run %d", i))
		if d := m.rr30R1Detail(ms, p.fresh.ID); !strings.Contains(d, "recovered=839 of 840") {
			t.Fatalf("the detail must report the NET recovery: %s", d)
		}
	}
	// The cap counts the original debit: the unit cannot be re-debited under F.
	if o, _, _, err := m.rr30K2(m.f.walletID, "EUR", adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 1, failedTx); err == nil && o.Executed {
		t.Fatal("INV-ADJ-6 must refuse a debit beyond the causation's leg")
	}
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "still raised after the refused re-debit")
	m.assertInvariants()
}

// A credit of ANOTHER reason code (operational_error_correction, uncapped by
// INV-ADJ-6) with the causation is subtracted too: the subtraction side is not
// limited to compensating entries.
func TestRR30_CreditOfAnotherReasonCode_IsSubtracted(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, failedTx := rr1NotPaid(m, 845)
	l := m.rr1Line("rr30-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 845, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")
	m.rr30Must(adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 845, failedTx, "full debit")
	m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, "full net recovery")
	m.rr30Must(adjustment.DirectionCreditPlayer, adjustment.ReasonOperationalErrorCorrection, 2, failedTx, "operational credit, same causation")
	ms := m.stmtRun(m.source(false))
	m.rr1RequireRaised(ms, p.fresh.ID, "operational credit with the causation")
	if d := m.rr30R1Detail(ms, p.fresh.ID); !strings.Contains(d, "recovered=843 of 845") {
		t.Fatalf("net: %s", d)
	}
}

// Only compensating_entry debits are recovery (the M2 (d) rule): a full debit
// of ANOTHER reason code (operational_error_correction, causation allowed,
// not capped by INV-ADJ-6) under the causation keeps raising; the
// compensating_entry debit then stops it (non-vacuity).
func TestRR30_DebitOfAnotherReasonCode_IsNotRecovery(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, failedTx := rr1NotPaid(m, 846)
	l := m.rr1Line("rr30-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 846, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")
	m.rr30Must(adjustment.DirectionDebitPlayer, adjustment.ReasonOperationalErrorCorrection, 846, failedTx, "operational debit, same causation")
	ms := m.stmtRun(m.source(false))
	m.rr1RequireRaised(ms, p.fresh.ID, "an operational debit is not a recovery")
	if d := m.rr30R1Detail(ms, p.fresh.ID); !strings.Contains(d, "recovered=0 of 846") {
		t.Fatalf("net: %s", d)
	}
	m.rr1Debit(846, failedTx, "compensating debit")
	m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, "control")
}

// A REVERSED debit does not count: (i) a K2 credit whose causation is the
// debit's own manual_adjustment transaction, (ii) a planted ledger reversal of
// it (reverses_transaction_id). Each keeps raising with the net excluding it.
func TestRR30_ReversedDebit_DoesNotCount(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	for _, c := range []struct {
		name    string
		reverse func(debitTx uuid.UUID)
	}{
		{"K2 credit caused by the debit", func(d uuid.UUID) {
			m.rr30Must(adjustment.DirectionCreditPlayer, adjustment.ReasonCompensatingEntry, 850, d, "credit reversing the debit")
		}},
		{"planted ledger reversal of the debit", func(d uuid.UUID) { m.rr30PlantReversal(d) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			p, failedTx := rr1NotPaid(m, 850)
			l := m.rr1Line("rr30-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 850, "EUR")
			m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")
			d := m.rr30Must(adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 850, failedTx, "full debit")
			m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, "control: full net recovery")
			c.reverse(d)
			ms := m.stmtRun(m.source(false))
			m.rr1RequireRaised(ms, p.fresh.ID, "the debit is reversed")
			if det := m.rr30R1Detail(ms, p.fresh.ID); !strings.Contains(det, "recovered=0 of 850") {
				t.Fatalf("a reversed debit must count zero: %s", det)
			}
		})
	}
	m.t = t
}

// Wrong wallet and wrong asset cannot carry recovery: K2 refuses a debit under
// the causation on another player's wallet or in another asset (MA022
// causation_not_on_wallet), and the finding keeps raising; the right wallet and
// asset then stops it (non-vacuity), and a credit under an unrelated causation
// does not offset it. Wrong causation (even the full amount) is
// pinned by TestRR1_OtherCausation_Unrelated_Partial_KeepRaising.
func TestRR30_WrongWalletOrAsset_NoRecovery(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, failedTx := rr1NotPaid(m, 855)
	l := m.rr1Line("rr30-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 855, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")
	other := m.otherWallet()
	if o, _, _, err := m.rr30K2(other, "EUR", adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 855, failedTx); err == nil && o.Executed {
		t.Fatal("a debit under the causation on another wallet executed")
	}
	if o, _, _, err := m.rr30K2(m.f.walletID, "USD", adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 855, failedTx); err == nil && o.Executed {
		t.Fatal("a debit under the causation in another asset executed")
	}
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "wrong wallet / wrong asset attempts")
	if n := m.countRows(`SELECT count(*) FROM ledger_adjustment_requests WHERE tenant_id = $1 AND causation_transaction_id = $2 AND state = 'executed'`,
		m.f.tenantID, failedTx); n != 0 {
		t.Fatalf("no recovery may have executed, got %d", n)
	}
	// A credit under an UNRELATED causation (the withdrawal's hold) is not
	// subtracted: only credits naming F (or a counted debit) offset the recovery.
	hold := m.wd(p.wr.ID).HoldLedgerTransactionID
	if hold == nil {
		t.Fatal("setup: no hold transaction")
	}
	m.rr30Must(adjustment.DirectionCreditPlayer, adjustment.ReasonCompensatingEntry, 5, *hold, "credit under an unrelated causation")
	m.rr1Debit(855, failedTx, "right wallet and asset")
	m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, "control (an unrelated credit does not offset)")
}

// Duplicate recovery: a replayed final approval posts nothing (ErrNotPending)
// and the recovery is not counted twice; a second full debit is refused by the
// cap; only the real remaining unit completes the net.
func TestRR30_DuplicateRecovery_ReplayIsIdempotent_NoDoubleCount(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, failedTx := rr1NotPaid(m, 860)
	l := m.rr1Line("rr30-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 860, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")
	o, r, _, err := m.rr30K2(m.f.walletID, "EUR", adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 859, failedTx)
	if err != nil || !o.Executed {
		t.Fatalf("partial debit: %v %+v", err, o)
	}
	before := m.ledgerTxCount("manual_adjustment")
	if _, err := m.k2Decide(m.f4, r); !errors.Is(err, adjustment.ErrNotPending) {
		t.Fatalf("a replayed decision must be refused as not pending, got %v", err)
	}
	if got := m.ledgerTxCount("manual_adjustment"); got != before {
		t.Fatalf("a replay posted: %d -> %d", before, got)
	}
	for i := 0; i < 2; i++ {
		ms := m.stmtRun(m.source(false))
		m.rr1RequireRaised(ms, p.fresh.ID, fmt.Sprintf("partial after a replay, run %d", i))
		if d := m.rr30R1Detail(ms, p.fresh.ID); !strings.Contains(d, "recovered=859 of 860") {
			t.Fatalf("double count: %s", d)
		}
	}
	if o, _, _, err := m.rr30K2(m.f.walletID, "EUR", adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 860, failedTx); err == nil && o.Executed {
		t.Fatal("a second full debit under the causation executed (INV-ADJ-6)")
	}
	m.rr1Debit(1, failedTx, "the remaining unit")
	for i := 0; i < 2; i++ {
		m.rr1RequireStopped(m.stmtRun(m.source(false, l)), p.fresh.ID, fmt.Sprintf("complete, re-delivered line, run %d", i))
	}
}

// --- decision 2 for M2 (d) ----------------------------------------------------------------------------

// M2 (d) uses the same NET recovery: a full debit clears (d); a credit with the
// causation re-raises it; a reversed debit counts zero.
func TestRR30_M2d_NetRecovery(t *testing.T) {
	t.Run("debit offset by a credit", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		w.k2Setup()
		_, a := w.ambiguousPayout(400)
		res := w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
		line := w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 400)
		w.requireOne(w.stmtRun(w.source(false, line)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "late success")
		w.rr30Must(adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 400, *res.LedgerTransactionID, "full debit")
		w.requireNone(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "full net recovery")
		w.rr30Must(adjustment.DirectionCreditPlayer, adjustment.ReasonCompensatingEntry, 400, *res.LedgerTransactionID, "credit, same causation")
		f := w.requireOne(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "offset by a credit")
		if !strings.Contains(f.ActualValue, "recovered=0 of 400") || !strings.Contains(f.ExpectedValue, "NET") {
			t.Fatalf("finding: %+v", f)
		}
		w.requireOne(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "still raised")
	})
	t.Run("reversed debit", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		w.k2Setup()
		_, a := w.ambiguousPayout(410)
		res := w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
		line := w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 410)
		w.requireOne(w.stmtRun(w.source(false, line)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "late success")
		d := w.rr30Must(adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 410, *res.LedgerTransactionID, "full debit")
		w.requireNone(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "full net recovery")
		w.rr30Must(adjustment.DirectionCreditPlayer, adjustment.ReasonCompensatingEntry, 410, d, "credit reversing the debit")
		f := w.requireOne(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "reversed debit")
		if !strings.Contains(f.ActualValue, "recovered=0 of 410") {
			t.Fatalf("finding: %+v", f)
		}
	})
}

// --- decision 1: the BOUND destination_mismatch site -------------------------------------------------

type rr30Bound struct {
	a        PaymentAttempt
	x        string
	wrID     uuid.UUID
	failedTx uuid.UUID
}

// rr30BoundNotPaid parks a payout on destination_mismatch holding X (the B13-B
// shape, ApplyDisputeFromNonTerminal), declines it on X (with its merchant
// reference) plus a declined line on every extra reference, and executes an M4
// not-paid on it.
func (m *m4World) rr30BoundNotPaid(amount int64, extraDeclined ...string) rr30Bound {
	m.t.Helper()
	wr, a := m.payout(amount)
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "destination_mismatch")
	})
	a = m.attempt(a.ID)
	x := *a.ProviderReference
	lines := []statement.PaymentStatementLine{m.line(x, a.MerchantReference, statement.PaymentStatusDeclined, amount, time.Now())}
	for _, d := range extraDeclined {
		lines = append(lines, m.line(d, a.MerchantReference, statement.PaymentStatusDeclined, amount, time.Now()))
	}
	m.ingest(m4Imp{start: a.CreatedAt.Add(-time.Minute), end: a.LastSentAt.Add(25 * time.Hour)}, lines...)
	ev := m.mustEvidence(a.ID, M4VerdictNotPaid)
	r, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(m.t, err, "request")
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(m.t, err, "approve")
	if !out.Executed || m.wd(wr.ID).State != "failed" {
		m.t.Fatalf("not executed: %+v", out)
	}
	return rr30Bound{a: a, x: x, wrID: wr.ID, failedTx: *out.Resolution.LedgerTransactionID}
}

// rr30CU counts the bound pay_captured_unposted findings naming the attempt and
// requires the post-M4 hint on each.
func (m *m4World) rr30CU(ms []reconciliation.Mismatch, attempt uuid.UUID) int {
	m.t.Helper()
	xs := mismatchesOf(ms, reconciliation.MismatchKindPayCapturedUnposted, "attempt="+attempt.String())
	for _, x := range xs {
		if x.ExpectedValue != m4NotPaidHint {
			m.t.Fatalf("a raised bound finding keeps the post-M4 hint, got %q", x.ExpectedValue)
		}
	}
	return len(xs)
}

func (m *m4World) rr30RequireCU(ms []reconciliation.Mismatch, attempt uuid.UUID, what string) {
	m.t.Helper()
	if m.rr30CU(ms, attempt) == 0 {
		m.t.Fatalf("%s: want the bound pay_captured_unposted for %s:\n%s", what, attempt, render(ms))
	}
}

func (m *m4World) rr30RequireNoCU(ms []reconciliation.Mismatch, attempt uuid.UUID, what string) {
	m.t.Helper()
	if n := m.rr30CU(ms, attempt); n != 0 {
		m.t.Fatalf("%s: want no pay_captured_unposted for %s, got %d:\n%s", what, attempt, n, render(ms))
	}
}

// The recovered destination_mismatch M4: the bound finding (keyed on X, no
// line) keeps raising - in-run and standing - until the COMPLETE predicate
// holds (a later callback, a debit alone and a partial recovery never stop it);
// then it stops on every run and on a re-delivery; history rows are kept and
// none is resolved; the park, the attempt and the withdrawal are untouched; a
// later NEW payout raises again.
func TestRR30_BoundDestinationMismatch_RecoveredStops_OnlyOnCompleteRecovery(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	b := m.rr30BoundNotPaid(880)
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "after the M4 not-paid, no payout yet")
	// A debit alone (no evidenced payout) is not recovery.
	m.rr1Debit(880, b.failedTx, "debit before any payout")
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "a debit alone")

	// A later succeeded callback on the disputed attempt changes nothing here.
	_, _ = rvApplyReceipt(m.pool, m.orch, m.f.tenantID, m.provider, ReceiptEvidence{
		EventType: "payout", ProviderReference: b.x, Outcome: OutcomeSucceeded, Amount: 880, AssetCode: "EUR"})
	if got := m.attempt(b.a.ID); got.State != AttemptDisputed || ptrStr(got.TerminalReason) != "destination_mismatch" {
		t.Fatalf("a callback changed the park: %s/%s", got.State, ptrStr(got.TerminalReason))
	}
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "after a later callback, no statement payout")
	history := m.rr1HistoryRows(b.a.ID)

	// The PSP's payout on X (same amount and asset) with the full NET recovery
	// already executed: the complete predicate holds.
	l := m.rr1Line(b.x, b.a.MerchantReference, 880, "EUR")
	ms := m.stmtRun(m.source(false, l))
	m.rr30RequireNoCU(ms, b.a.ID, "complete recovery, in-run line on X")
	if _, r1 := rr1Raised(ms, b.a.ID); r1 != 0 {
		t.Fatalf("R-1 must stop under the same rule:\n%s", render(ms))
	}
	for i := 0; i < 2; i++ {
		m.rr30RequireNoCU(m.stmtRun(m.source(false)), b.a.ID, fmt.Sprintf("standing run %d", i))
	}
	m.rr30RequireNoCU(m.stmtRun(m.source(false, l, l)), b.a.ID, "re-delivery, duplicated")

	if got := m.rr1HistoryRows(b.a.ID); got < history || history < 3 {
		t.Fatalf("history: %d -> %d", history, got)
	}
	if n := m.countRows(`SELECT count(*) FROM reconciliation_mismatches WHERE tenant_id = $1 AND reconciliation_key LIKE '%attempt=' || $2 || '%' AND investigation_status <> 'open'`,
		m.f.tenantID, b.a.ID.String()); n != 0 {
		t.Fatalf("no mismatch row may be resolved automatically, got %d", n)
	}
	if got := m.attempt(b.a.ID); got.State != AttemptDisputed || ptrStr(got.TerminalReason) != "destination_mismatch" || ptrStr(got.ProviderReference) != b.x {
		t.Fatalf("the park changed: %+v", got)
	}
	if st := m.wd(b.wrID).State; st != "failed" {
		t.Fatalf("withdrawal changed: %s", st)
	}

	// A NEW payout (same reference, another time) is a second payout: raises.
	l2 := m.rr1Line(b.x, b.a.MerchantReference, 880, "EUR")
	l2.OccurredAt = l.OccurredAt.Add(time.Second)
	m.rr30RequireCU(m.stmtRun(m.source(false, l2)), b.a.ID, "a second payout")
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "a second payout, standing")
	m.assertInvariants()
}

// Partial, offset, wrong-causation and reversed recoveries never stop the
// bound finding; the remaining legitimate step does (non-vacuity).
func TestRR30_BoundDestinationMismatch_IncompleteRecoveries_KeepRaising(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	b := m.rr30BoundNotPaid(890)
	other := m.rr30BoundNotPaid(890)
	l := m.rr1Line(b.x, b.a.MerchantReference, 890, "EUR")
	m.rr30RequireCU(m.stmtRun(m.source(false, l)), b.a.ID, "payout before recovery")
	m.rr1Debit(890, other.failedTx, "wrong causation: another park's withdrawal_failed, full amount")
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "wrong causation")
	m.rr1Debit(889, b.failedTx, "partial")
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "partial 889 of 890")
	m.rr1Debit(1, b.failedTx, "the last unit")
	m.rr30RequireNoCU(m.stmtRun(m.source(false)), b.a.ID, "complete")
	m.rr30Must(adjustment.DirectionCreditPlayer, adjustment.ReasonCompensatingEntry, 1, b.failedTx, "offsetting credit")
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "debit offset by a credit")
}

// The payout must be positively THIS park's: an unrelated payout (on another
// matched decline reference, with no merchant reference) lets the shared RR-1
// predicate hold - R-1 stops - but the bound finding keeps raising; a payout on
// X naming another merchant reference keeps raising; two payouts keep raising.
func TestRR30_BoundDestinationMismatch_UnattributedPayouts_KeepRaising(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()

	d2 := "rr30-D2-" + uuid.NewString()[:12]
	b := m.rr30BoundNotPaid(900, d2)
	m.rr1Debit(900, b.failedTx, "full recovery")
	ms := m.stmtRun(m.source(false, m.rr1Line(d2, "", 900, "EUR")))
	m.rr30RequireCU(ms, b.a.ID, "unrelated payout on another reference without the merchant reference")
	if _, r1 := rr1Raised(ms, b.a.ID); r1 != 0 {
		t.Fatalf("R-1 stops (one recovered payout) while the bound finding keeps raising:\n%s", render(ms))
	}
	m.rr30RequireCU(m.stmtRun(m.source(false)), b.a.ID, "unrelated payout, standing")

	c := m.rr30BoundNotPaid(905)
	m.rr1Debit(905, c.failedTx, "full recovery")
	m.rr30RequireCU(m.stmtRun(m.source(false, m.rr1Line(c.x, "rr30-other-merchant", 905, "EUR"))), c.a.ID, "payout on X naming another merchant reference")

	e := m.rr30BoundNotPaid(910)
	m.rr1Debit(910, e.failedTx, "full recovery")
	m.rr30RequireNoCU(m.stmtRun(m.source(false, m.rr1Line(e.x, e.a.MerchantReference, 910, "EUR"))), e.a.ID, "control: one attributed payout")
	m.rr30RequireCU(m.stmtRun(m.source(false, m.rr1Line("rr30-L2-"+uuid.NewString()[:8], e.a.MerchantReference, 910, "EUR"))), e.a.ID, "a second payout under another reference")

	f := m.rr30BoundNotPaid(915)
	m.rr1Debit(915, f.failedTx, "full recovery")
	m.rr30RequireCU(m.stmtRun(m.source(false, m.rr1Line(f.x, f.a.MerchantReference, 914, "EUR"))), f.a.ID, "unequal amount")
	g := m.rr30BoundNotPaid(920)
	m.rr1Debit(920, g.failedTx, "full recovery")
	m.rr30RequireCU(m.stmtRun(m.source(false, m.rr1Line(g.x, g.a.MerchantReference, 920, "USD"))), g.a.ID, "other asset")
}

// Tenant isolation at the bound site: tenant B's recovered park, B's copy of
// A's payout line under A's provider and merchant reference, can neither stop
// nor touch A's bound finding; A's own complete recovery then stops it.
func TestRR30_BoundDestinationMismatch_CrossTenantIsolation(t *testing.T) {
	a := newM4World(t)
	b := newM4WorldOn(t, a.pool)
	a.k2Setup()
	b.k2Setup()
	pa := a.rr30BoundNotPaid(930)
	pb := b.rr30BoundNotPaid(930)
	la := a.rr1Line(pa.x, pa.a.MerchantReference, 930, "EUR")
	b.rr1Debit(930, pb.failedTx, "B's own full recovery")
	if err := b.pool.WithTenant(context.Background(), b.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		src := k3MockSource{k3Source{provider: a.provider, lines: []statement.PaymentStatementLine{la}}}
		st, err := src.Fetch(ctx, statement.PaymentFetchRequest{})
		if err != nil {
			return err
		}
		_, _, err = reconciliation.IngestPaymentStatement(ctx, tx, b.f.tenantID, src, st, time.Now())
		return err
	}); err != nil {
		t.Fatalf("tenant B ingest: %v", err)
	}
	b.rr30RequireNoCU(b.stmtRun(b.source(false, b.rr1Line(pb.x, pb.a.MerchantReference, 930, "EUR"))), pb.a.ID, "B after its own complete recovery")
	if o, _, _, err := b.rr30K2(b.f.walletID, "EUR", adjustment.DirectionDebitPlayer, adjustment.ReasonCompensatingEntry, 930, pa.failedTx); err == nil && o.Executed {
		t.Fatal("tenant B executed a compensation caused by tenant A's transaction")
	}
	for i := 0; i < 2; i++ {
		ms := a.stmtRun(a.source(false))
		a.rr30RequireCU(ms, pa.a.ID, fmt.Sprintf("A is unaffected by B, run %d", i))
		for _, x := range ms {
			if strings.Contains(x.ReconciliationKey+x.ActualValue, pb.a.ID.String()) {
				t.Fatalf("A's run names B's attempt:\n%s", render(ms))
			}
		}
	}
	a.rr1Debit(930, pa.failedTx, "A's own full recovery")
	a.rr30RequireNoCU(a.stmtRun(a.source(false, la)), pa.a.ID, "A after its own complete recovery")
	a.assertInvariants()
	b.assertInvariants()
}
