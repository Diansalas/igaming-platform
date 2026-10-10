//go:build integration

// PAY-PAYOUT-UNBOUND-RESOLVE-1 review amendments (ADR 0111 §17.7): security
// H-1 / H-2 / L-3, ledger-finance C-1 / C-2 / F-2 / F-3. Every amendment
// tightens toward refusal; owner decisions 9-12 stay the oracle (no automatic
// resolution, positive attribution only, the park stays held otherwise).
// MOCK statement sources only.
package payments

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// declineOn ingests ONE sealed, declaring MOCK import covering [created_at,
// last_sent_at + 25h] with a declined line on the park's merchant reference
// under the PSP reference d, plus any extra lines in the SAME import.
func (m *m4World) declineOn(p *b11Parked, d string, extra ...statement.PaymentStatementLine) uuid.UUID {
	m.t.Helper()
	ls := append([]statement.PaymentStatementLine{m.line(d, p.fresh.MerchantReference, statement.PaymentStatusDeclined, p.fresh.Amount, time.Now())}, extra...)
	return m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)}, ls...)
}

func m4Verdict(t *testing.T, m *m4World, attempt uuid.UUID) M4Evidence {
	t.Helper()
	ev, err := m.evidence(attempt)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	return ev
}

// H-1 (security, double payout), CROSS-IMPORT: the not-paid verdict also looks
// up the declined line's OWN PSP reference D. A succeeded, pending or reversed
// payout line on D that carries NO merchant reference, in ANY other import,
// forbids not-paid. Before the amendment each of these read not_paid.
func TestM4_H1_NotPaid_LineOnTheDeclinedReference_CrossImport(t *testing.T) {
	m := newM4World(t)
	cases := []struct {
		name   string
		status string
		other  m4Imp
		want   string
	}{
		{"control: no other line", "", m4Imp{}, M4VerdictNotPaid},
		// A succeeded line on D makes the paid branch read the declined line on
		// the same reference: contradictory (never not_paid, never paid).
		{"succeeded on D, no merchant, sealed non-declaring import", statement.PaymentStatusSucceeded, m4Imp{noDecl: true}, M4VerdictContradictory},
		{"succeeded on D, no merchant, UNSEALED import", statement.PaymentStatusSucceeded, m4Imp{unsealed: true}, M4VerdictContradictory},
		{"pending on D, no merchant, unsealed import", statement.PaymentStatusPending, m4Imp{unsealed: true}, M4VerdictInsufficient},
		{"reversed on D, no merchant, sealed non-declaring import", "reversed", m4Imp{noDecl: true}, M4VerdictInsufficient},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			p := m.park(int64(720 + i))
			d := "m4-D-" + uuid.NewString()[:12]
			m.declineOn(p, d)
			if c.status != "" {
				m.ingest(c.other, m.line(d, "", c.status, p.fresh.Amount, time.Now()))
			}
			if ev := m4Verdict(t, m, p.fresh.ID); ev.Verdict != c.want {
				t.Fatalf("verdict: want %s, got %s", c.want, ev.Verdict)
			}
		})
	}
	m.t = t
	// At execution: a succeeded line on D that arrives after the request (no
	// merchant reference, another import) refuses the final approval; nothing
	// is posted; the park stays held.
	p := m.park(729)
	d := "m4-D-" + uuid.NewString()[:12]
	m.declineOn(p, d)
	ev := m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	m.ingest(m4Imp{noDecl: true}, m.line(d, "", statement.PaymentStatusSucceeded, 729, time.Now()))
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if out.Executed || !out.Refused || *out.Resolution.RefusalCode != resolutionRefusedEvidence {
		t.Fatalf("want refused/%s, got %+v", resolutionRefusedEvidence, out)
	}
	m.b11Held(p, "H-1 cross-import at execution")
	m.assertInvariants()
}

// H-1 (security), SAME IMPORT: an import that declares its payout lines carry
// the merchant reference but holds a payout line WITHOUT one contradicts its
// own declaration: insufficient. A deposit line without one is not a payout
// line and does not count (control).
func TestM4_H1_NotPaid_DeclaringImportWithNullMerchantPayoutLine(t *testing.T) {
	m := newM4World(t)
	t.Run("null-merchant payout line in the declaring import", func(t *testing.T) {
		m.t = t
		p := m.park(731)
		m.declineOn(p, "m4-D-"+uuid.NewString()[:12], m.line(m4Ref(), "", statement.PaymentStatusDeclined, 5, time.Now()))
		if ev := m4Verdict(t, m, p.fresh.ID); ev.Verdict != M4VerdictInsufficient {
			t.Fatalf("verdict: %s", ev.Verdict)
		}
	})
	t.Run("control: a null-merchant DEPOSIT line does not count", func(t *testing.T) {
		m.t = t
		p := m.park(732)
		dep := m.line(m4Ref(), "", statement.PaymentStatusSucceeded, 5, time.Now())
		dep.Kind = statement.PaymentLineDeposit
		m.declineOn(p, "m4-D-"+uuid.NewString()[:12], dep)
		if ev := m4Verdict(t, m, p.fresh.ID); ev.Verdict != M4VerdictNotPaid {
			t.Fatalf("verdict: %s", ev.Verdict)
		}
	})
}

// H-2 (security) / C-1 (LF): R must be unambiguously THIS attempt's. Two parks
// P and Q of the same amount; ONE import holds a succeeded line on R naming
// P's merchant reference and an otherwise identical one naming Q's. The
// cross-import dedupe makes it a single group, so before the amendment both
// parks read paid on the same R (a double completion of one PSP payout).
func TestM4_H2_Paid_OneReferenceNamingTwoMerchants_Contradictory(t *testing.T) {
	m := newM4World(t)
	pP, pQ := m.park(740), m.park(740)
	r := "m4-R-" + uuid.NewString()[:12]
	at := time.Now().UTC().Truncate(time.Microsecond)
	m.ingest(m4Imp{}, m.line(r, pP.fresh.MerchantReference, statement.PaymentStatusSucceeded, 740, at),
		m.line(r, pQ.fresh.MerchantReference, statement.PaymentStatusSucceeded, 740, at))
	for _, p := range []*b11Parked{pP, pQ} {
		if ev := m4Verdict(t, m, p.fresh.ID); ev.Verdict != M4VerdictContradictory {
			t.Fatalf("park %s: want contradictory, got %s (ref %v)", p.fresh.ID, ev.Verdict, ev.Reference)
		}
	}
	// Any status, any import: a later UNSEALED pending line on R naming another
	// merchant also contradicts a clean paid verdict, and refuses its execution.
	p := m.park(741)
	r2 := "m4-R-" + uuid.NewString()[:12]
	at2 := time.Now().UTC().Truncate(time.Microsecond)
	m.ingest(m4Imp{}, m.line(r2, p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 741, at2))
	ev := m.mustEvidence(p.fresh.ID, M4VerdictPaid)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	m.ingest(m4Imp{unsealed: true}, m.line(r2, "m4-other-merchant", statement.PaymentStatusSucceeded, 741, at2))
	m.mustEvidence(p.fresh.ID, M4VerdictContradictory)
	out, err := m.decide(m.acting2, res, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if out.Executed || !out.Refused {
		t.Fatalf("want refused, got %+v", out)
	}
	m.b11Held(p, "H-2 at execution")
	m.assertInvariants()
}

// L-3 (security): a TENANT-STAFF final approval when the platform floor is
// already met. The verdict is evaluated in the approving session, which cannot
// see the attempt's whole statement scope: MR060 (an error, never a verdict),
// and the WHOLE approval transaction rolls back - no approval row, still
// pending, nothing posted.
func TestM4_L3_TenantStaffFinalApproval_FloorMet_MR060_RolledBack(t *testing.T) {
	m := newM4WorldBase(t, 2)
	p, _, ev := m.paidPark(750)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidencePaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	out, err := m.decide(m.acting2, res, ResolutionApprove)
	k3RequireNoErr(t, err, "platform approve")
	if out.Executed || out.Counted != 1 || out.Required != 2 {
		t.Fatalf("setup: want 1 of 2 counted with the floor met, got %+v", out)
	}
	approvals := func() int {
		return m.countRows(`SELECT count(*) FROM payment_manual_resolution_approvals WHERE tenant_id = $1 AND resolution_id = $2`, m.f.tenantID, res.ID)
	}
	before := approvals()
	_, err = m.decide(m.f1, res, ResolutionApprove)
	k3RequireCode(t, err, "MR060")
	if got := approvals(); got != before {
		t.Fatalf("the refused tenant approval persisted: %d -> %d", before, got)
	}
	if got := m.resolution(res.ID); got.State != ResolutionPending {
		t.Fatalf("state: %s", got.State)
	}
	m.b11Held(p, "L-3")
	// A platform_acting final approval then executes.
	out, err = m.decide(m.acting3, res, ResolutionApprove)
	k3RequireNoErr(t, err, "platform final approve")
	if !out.Executed {
		t.Fatalf("not executed: %+v", out)
	}
	m.assertInvariants()
}

// F-2 (LF): two concurrent FINAL approvals of one m4_evidence_not_paid: exactly
// one executes, exactly one withdrawal_failed posting.
func TestM4_Concurrency_TwoFinalApprovals_NotPaid(t *testing.T) {
	m := newM4World(t)
	for i := 0; i < 3; i++ {
		p, ev := m.notPaidPark(int64(760 + i))
		res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
		k3RequireNoErr(t, err, "request")
		var wg sync.WaitGroup
		outs := make([]ResolutionOutcome, 2)
		errs := make([]error, 2)
		for j, s := range []k3Staff{m.acting2, m.acting3} {
			wg.Add(1)
			go func(j int, s k3Staff) {
				defer wg.Done()
				outs[j], errs[j] = m.decide(s, res, ResolutionApprove)
			}(j, s)
		}
		wg.Wait()
		executed := 0
		for j := range outs {
			if errs[j] == nil && outs[j].Executed {
				executed++
			}
		}
		if executed != 1 {
			t.Fatalf("iteration %d: executed %d times (errs %v)", i, executed, errs)
		}
		if n := m.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`, m.f.tenantID, p.wr.ID.String()+":failed"); n != 1 {
			t.Fatalf("iteration %d: withdrawal_failed postings %d", i, n)
		}
		if wr := m.wd(p.wr.ID); wr.State != withdrawal.StateFailed {
			t.Fatalf("iteration %d: withdrawal %s", i, wr.State)
		}
	}
	m.assertInvariants()
}

// F-2 (LF): the REQUEST-time evidence races a concurrent contradicting ingest.
// Whatever the interleaving the park is never released on contradicted
// evidence: either the request is refused, or it commits on the evidence it
// saw and its final approval is then refused at execution.
func TestM4_Concurrency_RequestVsConcurrentIngest(t *testing.T) {
	m := newM4World(t)
	for i := 0; i < 4; i++ {
		paid := i%2 == 0
		var p *b11Parked
		var ev M4Evidence
		var kind ResolutionKind
		var contra statement.PaymentStatementLine
		if paid {
			p, _, ev = m.paidPark(int64(770 + i))
			kind = ResolutionM4EvidencePaid
			contra = m.line(m4Ref(), p.fresh.MerchantReference, statement.PaymentStatusDeclined, p.fresh.Amount, time.Now())
		} else {
			p = m.park(int64(770 + i))
			d := "m4-D-" + uuid.NewString()[:12]
			m.declineOn(p, d)
			ev = m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
			kind = ResolutionM4EvidenceNotPaid
			contra = m.line(d, "", statement.PaymentStatusSucceeded, p.fresh.Amount, time.Now())
		}
		var wg sync.WaitGroup
		var res ManualResolution
		var rerr error
		wg.Add(2)
		go func() { defer wg.Done(); res, rerr = m.request(m.acting, m.m4In(p.fresh.ID, kind, ev.LineID)) }()
		go func() { defer wg.Done(); m.ingest(m4Imp{unsealed: true}, contra) }()
		wg.Wait()
		if rerr == nil {
			out, err := m.decide(m.acting2, res, ResolutionApprove)
			if err == nil && out.Executed {
				t.Fatalf("iteration %d (%s): executed on contradicted evidence: %+v", i, kind, out)
			}
		}
		m.b11Held(p, "request vs ingest")
	}
	m.assertInvariants()
}

// F-3 (LF): S17 is NOT equivalent for not-paid. A REAL import of the tenant and
// provider that matches nothing of the attempt leaves the import set
// unchanged but makes the MOCK decline ineligible (v_has_real): the verdict
// becomes insufficient, and the database's own re-check at -> executing
// (S-2) is what refuses (MR061). The Go path refuses too.
func TestM4_DBRecheck_AtExecuting_NotPaid_RealImportMakesMockIneligible(t *testing.T) {
	m := newM4World(t)
	p, ev := m.notPaidPark(780)
	res, err := m.request(m.acting, m.m4In(p.fresh.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	if err := m.inExecutingActing(res, m.acting2, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("control: executing with unchanged evidence: %v", err)
	}
	m.ingest(m4Imp{real: true}, m.line(m4Ref(), "m4-unrelated-merchant", statement.PaymentStatusSucceeded, 1, time.Now()))
	now := m4Verdict(t, m, p.fresh.ID)
	if now.Verdict != M4VerdictInsufficient || strings.Join(uuidStrings(now.ImportIDs), ",") != strings.Join(uuidStrings(ev.ImportIDs), ",") {
		t.Fatalf("setup: want insufficient over the SAME import set, got %s %v (was %v)", now.Verdict, now.ImportIDs, ev.ImportIDs)
	}
	err = m.inExecutingActing(res, m.acting2, func(context.Context, pgx.Tx) error { return nil })
	k3RequireCode(t, err, "MR061")
	out, err := m.decide(m.acting2, res, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if out.Executed || !out.Refused || *out.Resolution.RefusalCode != resolutionRefusedEvidence {
		t.Fatalf("want refused/%s, got %+v", resolutionRefusedEvidence, out)
	}
	m.b11Held(p, "F-3")
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// --- reconciliation (R-1) ------------------------------------------------------------------

// H-1 in R-1: after an executed M4 not-paid, a succeeded line on the declined
// line's OWN reference D - no merchant reference, no bound reference, no Y -
// raises pay_declared_not_paid_but_paid (D is a lookup key, S-1).
func TestM4Recon_NotPaid_SucceededOnlyOnTheDeclinedReference_Raises(t *testing.T) {
	m := newM4World(t)
	p := m.park(790)
	d := "m4-D-" + uuid.NewString()[:12]
	m.declineOn(p, d)
	ev := m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
	m.execute(p, ResolutionM4EvidenceNotPaid, ev)
	m.requireNone(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "before")
	ms := m.stmtRun(m.source(false, m.payoutLine(d, "", statement.PaymentStatusSucceeded, 790)))
	f := m.requireOne(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "succeeded line on D only")
	if !strings.Contains(f.ReconciliationKey, "check=m4_not_paid_but_paid") || !strings.Contains(f.ActualValue, "reference="+d) {
		t.Fatalf("finding: %+v", f)
	}
	// Standing: persisted, every run.
	m.requireOne(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "standing")
}

// H-2 / C-1 in R-1: after an executed M4 paid, a line on R naming ANOTHER
// merchant reference - identical content otherwise, so neither a contradicting
// status nor a second distinct succeeded line - raises
// pay_declared_paid_unconfirmed.
func TestM4Recon_Paid_LineOnRNamingAnotherMerchant_Raises(t *testing.T) {
	m := newM4World(t)
	p := m.park(800)
	r := "m4-R-" + uuid.NewString()[:12]
	// D-7 (ADR 0111 s25): a paid line must not predate the attempt, so the fixture is dated now, not a minute ago.
	l := m.line(r, p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 800, time.Now().UTC().Truncate(time.Microsecond))
	m.ingest(m4Imp{}, l)
	m.execute(p, ResolutionM4EvidencePaid, m.mustEvidence(p.fresh.ID, M4VerdictPaid))
	m.requireNone(m.stmtRun(m.source(false, l)), reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, p.fresh.ID, "control: the same line again")
	other := l
	other.MerchantReference = "m4-other-merchant"
	ms := m.stmtRun(m.source(false, other))
	f := m.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, p.fresh.ID, "line on R naming another merchant")
	if !strings.Contains(f.ActualValue, "names another merchant reference") || !strings.Contains(f.ReconciliationKey, "check=m4_paid_contradicted") {
		t.Fatalf("finding: %+v", f)
	}
}

// C-2 (LF): the not-paid recovery clearing. After an executed M4 not-paid and a
// succeeded line, the finding stands until executed compensating_entry debits
// WITH causation = the withdrawal_failed transaction total at least the
// amount: a debit with another causation does not clear (even for the whole
// amount), a partial recovery does not clear, the full recovery does.
func TestM4Recon_NotPaidRecovery_ClearsOnlyOnFullRecoveryWithTheCausation(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, ev := m.notPaidPark(810)
	out := m.execute(p, ResolutionM4EvidenceNotPaid, ev)
	failedTx := *out.Resolution.LedgerTransactionID
	line := m.payoutLine("m4-late-"+uuid.NewString()[:8], p.fresh.MerchantReference, statement.PaymentStatusSucceeded, 810)
	m.requireOne(m.stmtRun(m.source(false, line)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "after the late success")

	hold := m.wd(p.wr.ID).HoldLedgerTransactionID
	if hold == nil {
		t.Fatal("setup: no hold transaction")
	}
	if o, err := m.k2Compensate(adjustment.DirectionDebitPlayer, 810, *hold); err != nil || !o.Executed {
		t.Fatalf("other-causation debit: %v %+v", err, o)
	}
	m.requireOne(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "a debit with another causation")
	if o, err := m.k2Compensate(adjustment.DirectionDebitPlayer, 809, failedTx); err != nil || !o.Executed {
		t.Fatalf("partial recovery: %v %+v", err, o)
	}
	m.requireOne(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "a partial recovery (809 of 810)")
	if o, err := m.k2Compensate(adjustment.DirectionDebitPlayer, 1, failedTx); err != nil || !o.Executed {
		t.Fatalf("final recovery: %v %+v", err, o)
	}
	m.requireNone(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "full recovery")
	m.assertInvariants()
}
