//go:build integration

// r21 reconciliation follow-ups to PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 §19),
// through the REAL three-phase payment_statement stream, the REAL M4 four-eyes
// execution and the REAL K2 compensating-entry path, in the runtime-shaped
// non-superuser role of the K3 worlds. MOCK statement sources only.
//
//   - RR-1 (ledger-finance ruling): after an executed m4_evidence_not_paid, a
//     late succeeded line plus a FULL K2 recovery with the right causation
//     stops BOTH pay_declared_not_paid_but_paid and STANDING-1
//     (pay_captured_unposted); anything else keeps raising.
//   - RR-4 / security LOW condition 1: R-1 for an executed not-paid covers
//     EVERY matched decline reference (the verdict's v_rs), inside the I-2
//     budget.
//   - security LOW condition 2: an invisible evidence line (or attempt, or
//     withdrawal) fails the run closed (ErrPaymentEvidenceInvisible) instead of
//     silently reading "no D".
package payments

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

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// newM4WorldOn is newM4World on an existing pool (a second tenant in the same
// scratch database, for the isolation cases).
func newM4WorldOn(t *testing.T, pool *db.Pool) *m4World {
	t.Helper()
	w := newK3WorldOn(t, pool, k3Opts{base: 1})
	keys := m4NewKeys(t)
	w.svc.WithImportSealKeys(keys)
	m := &m4World{k3World: w, keys: keys}
	m.acting3 = w.staffMember(uuid.Nil, "platform_admin")
	w.grantActing(m.acting3, capability.CapabilityPaymentForceResolveApprove)
	return m
}

// rr1NotPaid parks a payout, executes an M4 not-paid on a sealed declaring
// decline, and returns the park and the withdrawal_failed transaction.
func rr1NotPaid(m *m4World, amount int64) (*b11Parked, uuid.UUID) {
	m.t.Helper()
	p, ev := m.notPaidPark(amount)
	out := m.execute(p, ResolutionM4EvidenceNotPaid, ev)
	return p, *out.Resolution.LedgerTransactionID
}

// rr1Line is a payout line with a FIXED occurred_at (so a re-delivery is the
// same statement content).
func (m *m4World) rr1Line(ref, merchant string, amount int64, asset string) statement.PaymentStatementLine {
	l := m.payoutLine(ref, merchant, statement.PaymentStatusSucceeded, amount)
	l.AssetCode = asset
	l.OccurredAt = time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	return l
}

// rr1Raised reports how many pay_captured_unposted (STANDING-1) and
// pay_declared_not_paid_but_paid (R-1) findings name the attempt.
func rr1Raised(ms []reconciliation.Mismatch, attempt uuid.UUID) (cu, r1 int) {
	return len(mismatchesOf(ms, reconciliation.MismatchKindPayCapturedUnposted, "attempt="+attempt.String())),
		len(mismatchesOf(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, "attempt="+attempt.String()))
}

func (m *m4World) rr1RequireRaised(ms []reconciliation.Mismatch, attempt uuid.UUID, what string) {
	m.t.Helper()
	cu, r1 := rr1Raised(ms, attempt)
	if cu == 0 || r1 != 1 {
		m.t.Fatalf("%s: want STANDING-1 and exactly one R-1 for %s, got cu=%d r1=%d:\n%s", what, attempt, cu, r1, render(ms))
	}
	for _, x := range mismatchesOf(ms, reconciliation.MismatchKindPayCapturedUnposted, "attempt="+attempt.String()) {
		if x.ExpectedValue != m4NotPaidHint {
			m.t.Fatalf("%s: a raised STANDING-1 keeps the post-M4 hint, got %q", what, x.ExpectedValue)
		}
	}
}

func (m *m4World) rr1RequireStopped(ms []reconciliation.Mismatch, attempt uuid.UUID, what string) {
	m.t.Helper()
	if cu, r1 := rr1Raised(ms, attempt); cu != 0 || r1 != 0 {
		m.t.Fatalf("%s: want neither STANDING-1 nor R-1 for %s, got cu=%d r1=%d:\n%s", what, attempt, cu, r1, render(ms))
	}
}

// rr1HistoryRows counts every persisted mismatch row (any run) naming the attempt.
func (m *m4World) rr1HistoryRows(attempt uuid.UUID) int {
	return m.countRows(`SELECT count(*) FROM reconciliation_mismatches WHERE tenant_id = $1 AND reconciliation_key LIKE '%attempt=' || $2 || '%'`,
		m.f.tenantID, attempt.String())
}

func (m *m4World) rr1Debit(amount int64, causation uuid.UUID, what string) {
	m.t.Helper()
	if o, err := m.k2Compensate(adjustment.DirectionDebitPlayer, amount, causation); err != nil || !o.Executed {
		m.t.Fatalf("%s: %v %+v", what, err, o)
	}
}

// stmtRunErr is stmtRun returning the match error instead of failing.
func (m *m4World) stmtRunErr(src statement.PaymentStatementSource) ([]reconciliation.Mismatch, error) {
	m.t.Helper()
	stmt, err := reconciliation.FetchPaymentStatement(context.Background(), src, m.f.tenantID, time.Now().Add(-time.Hour), time.Now(), reconciliation.PaymentStatementOptions{})
	if err != nil {
		m.t.Fatalf("FetchPaymentStatement: %v", err)
	}
	var importID uuid.UUID
	if err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		importID, _, err = reconciliation.IngestPaymentStatement(ctx, tx, m.f.tenantID, src, stmt, time.Now())
		return err
	}); err != nil {
		m.t.Fatalf("IngestPaymentStatement: %v", err)
	}
	var ms []reconciliation.Mismatch
	err = m.pool.WithTenantSnapshot(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, ms, _, err = reconciliation.RunPaymentStatement(ctx, tx, m.f.tenantID, importID, reconciliation.PaymentStatementOptions{})
		return err
	})
	return ms, err
}

// --- RR-1 ------------------------------------------------------------------------------------------

// An unresolved M4 not-paid keeps raising both findings on every run (in-run
// and standing), with the post-M4 hint.
func TestRR1_UnresolvedM4NotPaid_KeepsRaisingEveryRun(t *testing.T) {
	m := newM4World(t)
	p, _ := rr1NotPaid(m, 830)
	l := m.rr1Line("rr1-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 830, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "in-run")
	for i := 0; i < 2; i++ {
		m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, fmt.Sprintf("standing run %d", i))
	}
}

// The full legitimate recovery stops BOTH findings; repeated runs and a
// re-delivery of the same line stay stopped (idempotent); earlier mismatch rows
// are kept (history, no delete); nothing about the park changes; a later NEW
// succeeded line raises again (the recovery covered one payout).
func TestRR1_FullRecovery_Stops_Idempotent_HistoryKept_NewLineRaisesAgain(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, failedTx := rr1NotPaid(m, 840)
	l := m.rr1Line("rr1-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 840, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "before recovery")
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "before recovery, standing")
	history := m.rr1HistoryRows(p.fresh.ID)
	if history < 4 {
		t.Fatalf("setup: want the raised rows persisted, got %d", history)
	}
	before := m.attempt(p.fresh.ID)

	m.rr1Debit(840, failedTx, "full recovery")
	for i := 0; i < 3; i++ {
		m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, fmt.Sprintf("after full recovery, run %d", i))
	}
	// Re-delivery: the SAME line in a new import is the same payout.
	m.rr1RequireStopped(m.stmtRun(m.source(false, l)), p.fresh.ID, "re-delivery of the recovered line")
	m.rr1RequireStopped(m.stmtRun(m.source(false, l, l)), p.fresh.ID, "a duplicated re-delivery in one statement")

	// History: nothing was deleted or rewritten; the attempt is untouched.
	if got := m.rr1HistoryRows(p.fresh.ID); got < history {
		t.Fatalf("history shrank: %d -> %d", history, got)
	}
	if n := m.countRows(`SELECT count(*) FROM reconciliation_mismatches WHERE tenant_id = $1 AND reconciliation_key LIKE '%attempt=' || $2 || '%' AND investigation_status <> 'open'`,
		m.f.tenantID, p.fresh.ID.String()); n != 0 {
		t.Fatalf("no mismatch row may be resolved automatically, got %d", n)
	}
	after := m.attempt(p.fresh.ID)
	if after.State != before.State || after.State != AttemptDisputed || ptrStr(after.TerminalReason) != ptrStr(before.TerminalReason) {
		t.Fatalf("the park changed: %s/%s -> %s/%s", before.State, ptrStr(before.TerminalReason), after.State, ptrStr(after.TerminalReason))
	}

	// A later NEW succeeded line raises again - for the new line AND the old one.
	l2 := m.rr1Line("rr1-L2-"+uuid.NewString()[:8], p.fresh.MerchantReference, 840, "EUR")
	ms := m.stmtRun(m.source(false, l2))
	m.rr1RequireRaised(ms, p.fresh.ID, "a new succeeded line after recovery")
	names := func(ref string) bool {
		for _, x := range mismatchesOf(ms, reconciliation.MismatchKindPayCapturedUnposted, "attempt="+p.fresh.ID.String()) {
			if strings.Contains(x.ReconciliationKey+x.ActualValue, ref) {
				return true
			}
		}
		return false
	}
	if !names(l.ProviderReference) || !names(l2.ProviderReference) {
		t.Fatalf("want STANDING-1 on both lines:\n%s", render(ms))
	}
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "standing after the new line")
	m.assertInvariants()
}

func ptrStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// A debit with another causation (even the full amount), a full debit caused
// by an UNRELATED transaction (another park's withdrawal_failed) and a partial
// recovery never stop it; the remaining legitimate unit then does.
func TestRR1_OtherCausation_Unrelated_Partial_KeepRaising(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p, failedTx := rr1NotPaid(m, 850)
	_, otherFailedTx := rr1NotPaid(m, 851)
	l := m.rr1Line("rr1-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 850, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")

	hold := m.wd(p.wr.ID).HoldLedgerTransactionID
	if hold == nil {
		t.Fatal("setup: no hold transaction")
	}
	m.rr1Debit(850, *hold, "another causation (the hold), full amount")
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "a debit with another causation")
	m.rr1Debit(850, otherFailedTx, "an unrelated transaction (another park's withdrawal_failed), full amount")
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "a debit caused by an unrelated transaction")
	// A compensating CREDIT with the right causation is not a recovery.
	if o, err := m.k2Compensate(adjustment.DirectionCreditPlayer, 850, failedTx); err != nil || !o.Executed {
		t.Fatalf("compensating credit with the causation: %v %+v", err, o)
	}
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "a credit with the causation")
	m.rr1Debit(849, failedTx, "partial recovery")
	m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "partial recovery 849 of 850")
	m.rr1Debit(1, failedTx, "the last unit")
	m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, "full recovery (control)")
	m.assertInvariants()
}

// A succeeded line of another amount or another asset keeps raising even after
// a full recovery.
func TestRR1_UnequalAmountOrAssetLine_KeepsRaising(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	for _, c := range []struct {
		name   string
		amount int64
		asset  string
	}{{"other amount", 859, "EUR"}, {"other asset", 860, "USD"}} {
		t.Run(c.name, func(t *testing.T) {
			m.t = t
			p, failedTx := rr1NotPaid(m, 860)
			l := m.rr1Line("rr1-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, c.amount, c.asset)
			m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "before recovery")
			m.rr1Debit(860, failedTx, "full recovery")
			m.rr1RequireRaised(m.stmtRun(m.source(false)), p.fresh.ID, "full recovery, unequal line")
			m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "full recovery, unequal line re-delivered")
		})
	}
	m.t = t
	m.assertInvariants()
}

// Tenant isolation: tenant B's full recovery, B's lines naming A's provider and
// merchant reference, and a B compensation naming A's withdrawal_failed
// transaction can neither stop nor touch A's findings.
func TestRR1_CrossTenantEvidenceCannotStopAnotherTenantsFinding(t *testing.T) {
	a := newM4World(t)
	b := newM4WorldOn(t, a.pool)
	a.k2Setup()
	b.k2Setup()
	pa, failedA := rr1NotPaid(a, 870)
	pb, failedB := rr1NotPaid(b, 870)
	la := a.rr1Line("rr1-X-"+uuid.NewString()[:8], pa.fresh.MerchantReference, 870, "EUR")
	lb := b.rr1Line(la.ProviderReference, pb.fresh.MerchantReference, 870, "EUR")
	a.rr1RequireRaised(a.stmtRun(a.source(false, la)), pa.fresh.ID, "A late success")
	b.rr1RequireRaised(b.stmtRun(b.source(false, lb)), pb.fresh.ID, "B late success")

	// B cannot cite A's transaction as causation (not visible / composite FK).
	if o, err := b.k2Compensate(adjustment.DirectionDebitPlayer, 870, failedA); err == nil && o.Executed {
		t.Fatal("tenant B executed a compensation caused by tenant A's transaction")
	}
	b.rr1Debit(870, failedB, "B's own full recovery")
	// B ingests a copy of A's line under A's PROVIDER id and A's merchant reference.
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
	b.rr1RequireStopped(b.stmtRun(b.source(false)), pb.fresh.ID, "B after its own recovery")
	for i := 0; i < 2; i++ {
		ms := a.stmtRun(a.source(false))
		a.rr1RequireRaised(ms, pa.fresh.ID, fmt.Sprintf("A is unaffected by B, run %d", i))
		for _, x := range ms {
			if strings.Contains(x.ReconciliationKey+x.ActualValue, pb.fresh.ID.String()) {
				t.Fatalf("A's run names B's attempt:\n%s", render(ms))
			}
		}
	}
	a.assertInvariants()
	b.assertInvariants()
}

// RR-1 is a STANDING-1 (unbound, line-keyed) ruling. A BOUND destination_mismatch
// not-paid park keeps its BOUND-CLEAR-1 finding (keyed on X, no line) even after
// R-1 stops: pinned so a change is deliberate (ADR 0111 §19 residual).
func TestRR1_BoundDestinationMismatchPark_BoundFindingOutsideTheRuling(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	wr, a := m.payout(880)
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, a.ID, EvidenceCallback, "destination_mismatch")
	})
	a = m.attempt(a.ID)
	x := *a.ProviderReference
	m.ingest(m4Imp{start: a.CreatedAt.Add(-time.Minute), end: a.LastSentAt.Add(25 * time.Hour)},
		m.line(x, a.MerchantReference, statement.PaymentStatusDeclined, 880, time.Now()))
	ev := m.mustEvidence(a.ID, M4VerdictNotPaid)
	r, err := m.request(m.acting, m.m4In(a.ID, ResolutionM4EvidenceNotPaid, ev.LineID))
	k3RequireNoErr(t, err, "request")
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	k3RequireNoErr(t, err, "approve")
	if !out.Executed || m.wd(wr.ID).State != "failed" {
		t.Fatalf("not executed: %+v", out)
	}
	l := m.rr1Line(x, a.MerchantReference, 880, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), a.ID, "before recovery")
	m.rr1Debit(880, *out.Resolution.LedgerTransactionID, "full recovery")
	ms := m.stmtRun(m.source(false))
	cu, r1 := rr1Raised(ms, a.ID)
	if r1 != 0 || cu != 1 {
		t.Fatalf("want R-1 stopped and the bound finding kept, got cu=%d r1=%d:\n%s", cu, r1, render(ms))
	}
}

// --- RR-4 / security LOW condition 1 --------------------------------------------------------------

// R-1 covers EVERY matched decline reference, not only the evidence line's own
// D: a later succeeded line with NO merchant reference on a DIFFERENT matched
// decline reference raises; duplicates and replays raise exactly once per run;
// references that merely share a prefix or contain D2 never match.
func TestRR4_NotPaid_SucceededOnAnotherMatchedDeclineReference_Raises(t *testing.T) {
	m := newM4World(t)
	p := m.park(890)
	d1 := "rr4-D1-" + uuid.NewString()[:12]
	d2 := "rr4-D2-" + uuid.NewString()[:12]
	m.declineOn(p, d1, m.line(d2, p.fresh.MerchantReference, statement.PaymentStatusDeclined, 890, time.Now()))
	ev := m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
	m.execute(p, ResolutionM4EvidenceNotPaid, ev)
	m.requireNone(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "control: declines only")

	// Partial references (prefix, extension) are different references.
	partial := []statement.PaymentStatementLine{
		m.rr1Line(d2[:len(d2)-1], "", 890, "EUR"), m.rr1Line(d2+"0", "", 890, "EUR"), m.rr1Line("x"+d2, "", 890, "EUR")}
	m.requireNone(m.stmtRun(m.source(false, partial...)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "partial references")
	// The exact reference D2 under ANOTHER provider of the same tenant is another PSP's reference space.
	if err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		other := m.rr1Line(d2, "", 890, "EUR")
		other.ProviderID = "rr4-other-psp"
		src := k3MockSource{k3Source{provider: other.ProviderID, lines: []statement.PaymentStatementLine{other}}}
		st, err := src.Fetch(ctx, statement.PaymentFetchRequest{})
		if err != nil {
			return err
		}
		_, _, err = reconciliation.IngestPaymentStatement(ctx, tx, m.f.tenantID, src, st, time.Now())
		return err
	}); err != nil {
		t.Fatalf("other-provider ingest: %v", err)
	}
	m.requireNone(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "partial references and another provider, standing")

	s2 := m.rr1Line(d2, "", 890, "EUR")
	f := m.requireOne(m.stmtRun(m.source(false, s2)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "succeeded on D2, no merchant")
	if !strings.Contains(f.ActualValue, "reference="+d2) || !strings.Contains(f.ReconciliationKey, "check=m4_not_paid_but_paid") {
		t.Fatalf("finding: %+v", f)
	}
	m.requireOne(m.stmtRun(m.source(false, s2, s2)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "duplicate in one statement")
	m.requireOne(m.stmtRun(m.source(false, s2)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "replay in a new import")
	m.requireOne(m.stmtRun(m.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "standing")
	// Multi-reference: a succeeded line on D1 as well is still one finding per resolution.
	m.requireOne(m.stmtRun(m.source(false, m.rr1Line(d1, "", 890, "EUR"))), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, p.fresh.ID, "D1 and D2")
}

// A matched reference only RAISES: with a full recovery of the one recovered
// payout, a NEW succeeded line on a matched decline reference (no merchant
// reference) makes the finding raise again - it never helps a stop.
func TestRR4_MatchedReferenceLine_AfterRecovery_RaisesAgain(t *testing.T) {
	m := newM4World(t)
	m.k2Setup()
	p := m.park(895)
	d1 := "rr4-D1-" + uuid.NewString()[:12]
	d2 := "rr4-D2-" + uuid.NewString()[:12]
	m.declineOn(p, d1, m.line(d2, p.fresh.MerchantReference, statement.PaymentStatusDeclined, 895, time.Now()))
	out := m.execute(p, ResolutionM4EvidenceNotPaid, m.mustEvidence(p.fresh.ID, M4VerdictNotPaid))
	l := m.rr1Line("rr4-L-"+uuid.NewString()[:8], p.fresh.MerchantReference, 895, "EUR")
	m.rr1RequireRaised(m.stmtRun(m.source(false, l)), p.fresh.ID, "late success")
	m.rr1Debit(895, *out.Resolution.LedgerTransactionID, "full recovery")
	m.rr1RequireStopped(m.stmtRun(m.source(false)), p.fresh.ID, "recovered")
	m.rr1RequireRaised(m.stmtRun(m.source(false, m.rr1Line(d2, "", 895, "EUR"))), p.fresh.ID, "a second payout on D2")
}

// I-2 holds for the matched references: their lines are read within what is
// LEFT of the budget fixed from platform state before the read (here 2 keys:
// the merchant reference and D1 -> cap 128), never with a budget of their own.
// 2 rows by key + 126 matched-only rows = 128 runs; one more refuses the run.
func TestRR4_MatchedReferenceLines_ShareTheI2Budget(t *testing.T) {
	m := newM4World(t)
	p := m.park(896)
	d1 := "rr4-D1-" + uuid.NewString()[:12]
	d2 := "rr4-D2-" + uuid.NewString()[:12]
	m.declineOn(p, d1, m.line(d2, p.fresh.MerchantReference, statement.PaymentStatusDeclined, 896, time.Now()))
	m.execute(p, ResolutionM4EvidenceNotPaid, m.mustEvidence(p.fresh.ID, M4VerdictNotPaid))
	many := func(n int, from int64) []statement.PaymentStatementLine {
		out := make([]statement.PaymentStatementLine, n)
		for i := range out {
			out[i] = m.line(d2, "", statement.PaymentStatusDeclined, from+int64(i), time.Now())
		}
		return out
	}
	// Plus lines on references that only SHARE a prefix with D2 (or extend it):
	// exact matching never reads them, so they spend no budget.
	m.ingest(m4Imp{noDecl: true}, append(many(126, 1),
		m.line(d2+"0", "", statement.PaymentStatusDeclined, 5, time.Now()),
		m.line(d2[:len(d2)-1], "", statement.PaymentStatusDeclined, 6, time.Now()))...)
	if _, err := m.stmtRunErr(m.source(false)); err != nil {
		t.Fatalf("at the cap (128 rows for 2 keys; partial references not read): %v", err)
	}
	m.ingest(m4Imp{noDecl: true}, many(1, 10_000)...)
	before := m.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, m.f.tenantID)
	_, err := m.stmtRunErr(m.source(false))
	if !errors.Is(err, reconciliation.ErrPaymentEvidenceOverflow) || !strings.Contains(err.Error(), "more than 128") {
		t.Fatalf("129 rows for 2 keys: want ErrPaymentEvidenceOverflow at 128, got %v", err)
	}
	if after := m.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, m.f.tenantID); after != before {
		t.Fatalf("a refused run wrote a run row: %d -> %d", before, after)
	}
}

// --- security LOW condition 2 ---------------------------------------------------------------------

// An executed M4 whose evidence line (or attempt, or withdrawal) the run cannot
// see fails the run closed - ErrPaymentEvidenceInvisible, no run row, the
// existing P1 sweep_run_failed through the sweep entry point - instead of
// silently reading "no D". The invisibility is simulated with a RESTRICTIVE
// policy in this world's private scratch database, dropped afterwards.
func TestRR5_InvisibleEvidence_FailsClosed(t *testing.T) {
	m := newM4World(t)
	p, _ := m.notPaidPark(900)
	out := m.execute(p, ResolutionM4EvidenceNotPaid, m.mustEvidence(p.fresh.ID, M4VerdictNotPaid))
	lineID := *out.Resolution.EvidenceLineID
	ctx := context.Background()
	ddl := func(sql string) {
		t.Helper()
		if err := m.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.stmtRunErr(m.source(false)); err != nil {
		t.Fatalf("control: %v", err)
	}
	for _, c := range []struct {
		name, table, id, want string
	}{
		{"evidence line", "payment_statement_lines", lineID.String(), ": evidence line not visible"},
		{"attempt", "payment_attempts", p.fresh.ID.String(), ": attempt not visible"},
		{"withdrawal", "withdrawal_requests", p.wr.ID.String(), ": withdrawal not visible"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ddl(fmt.Sprintf(`CREATE POLICY rr5_hide ON %s AS RESTRICTIVE FOR SELECT USING (id <> '%s'::uuid)`, c.table, c.id))
			defer ddl(fmt.Sprintf(`DROP POLICY rr5_hide ON %s`, c.table))
			runs := m.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, m.f.tenantID)
			_, err := m.stmtRunErr(m.source(false))
			if !errors.Is(err, reconciliation.ErrPaymentEvidenceInvisible) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want ErrPaymentEvidenceInvisible (%q), got %v", c.want, err)
			}
			if got := m.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, m.f.tenantID); got != runs {
				t.Fatalf("a refused run wrote a run row")
			}
			var logBuf strings.Builder
			o := reconciliation.ReconcilePaymentStatementForTenant(ctx, m.pool, slog.New(slog.NewTextHandler(&logBuf, nil)),
				m.f.tenantID, time.Now().Add(-time.Hour), time.Now(), m.source(false), reconciliation.PaymentStatementOptions{})
			if !errors.Is(o.Err, reconciliation.ErrPaymentEvidenceInvisible) || o.Run.ID != uuid.Nil || !strings.Contains(logBuf.String(), "P1") {
				t.Fatalf("sweep entry: want the fail-closed P1, got err=%v run=%v log=%s", o.Err, o.Run.ID, logBuf.String())
			}
			if n := m.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
				AND metadata->>'stream' = 'payment_statement' AND metadata->>'severity' = 'P1' AND metadata->>'error' LIKE '%not visible%'`, m.f.tenantID); n < 1 {
				t.Fatalf("want an audited P1 sweep_run_failed, got %d", n)
			}
		})
	}
	m.t = t
	if _, err := m.stmtRunErr(m.source(false)); err != nil {
		t.Fatalf("control after the policies are dropped: %v", err)
	}
}
