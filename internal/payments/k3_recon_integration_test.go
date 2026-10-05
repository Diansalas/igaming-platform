//go:build integration

package payments

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// k3Source is a fixed payment statement source. real = false is a MOCK source
// (SyntheticComponent, label contains MOCK -> is_mock = true); real = true is a
// non-synthetic source (is_mock = false), the substrate for the D-4 / RC-3 cases.
type k3Source struct {
	provider string
	real     bool
	start    time.Time
	end      time.Time
	lines    []statement.PaymentStatementLine
}

func (s k3Source) Label() string {
	if s.real {
		return "k3 fixed real-shaped payment statement (test-only)"
	}
	return "MOCK k3 fixed payment statement (test-only)"
}
func (s k3Source) ProviderID() string { return s.provider }
func (s k3Source) Fetch(ctx context.Context, _ statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	st, en := s.start, s.end
	if st.IsZero() {
		st = time.Now().Add(-time.Hour).UTC()
	}
	if en.IsZero() {
		en = time.Now().Add(time.Minute).UTC()
	}
	return statement.PaymentStatement{CoverageStart: st, CoverageEnd: en, Lines: append([]statement.PaymentStatementLine(nil), s.lines...)}, nil
}

// markSynthetic is implemented only for MOCK sources.
type k3MockSource struct{ k3Source }

func (k3MockSource) SyntheticComponent() {}

func (w *k3World) source(real bool, lines ...statement.PaymentStatementLine) statement.PaymentStatementSource {
	s := k3Source{provider: w.provider, real: real, lines: lines}
	if real {
		return s
	}
	return k3MockSource{s}
}

func (w *k3World) pastSource(real bool) statement.PaymentStatementSource {
	s := k3Source{provider: w.provider, real: real,
		start: time.Now().Add(-48 * time.Hour).UTC(), end: time.Now().Add(-47 * time.Hour).UTC()}
	if real {
		return s
	}
	return k3MockSource{s}
}

func (w *k3World) payoutLine(ref, merchant, status string, amount int64) statement.PaymentStatementLine {
	return statement.PaymentStatementLine{ProviderID: w.provider, ProviderReference: ref, MerchantReference: merchant,
		Kind: statement.PaymentLinePayout, Status: status, Amount: amount, AssetCode: "EUR", OccurredAt: time.Now().UTC()}
}

func (w *k3World) depositLine(ref, merchant, status string, amount int64) statement.PaymentStatementLine {
	l := w.payoutLine(ref, merchant, status, amount)
	l.Kind = statement.PaymentLineDeposit
	return l
}

// stmtRun is the real three-phase stream (fetch, ingest, snapshot match) in the
// REAL WithTenantSnapshot session shape. It returns the run's mismatches.
func (w *k3World) stmtRun(src statement.PaymentStatementSource) []reconciliation.Mismatch {
	w.t.Helper()
	stmt, err := reconciliation.FetchPaymentStatement(context.Background(), src, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), reconciliation.PaymentStatementOptions{})
	if err != nil {
		w.t.Fatalf("FetchPaymentStatement: %v", err)
	}
	var importID uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		importID, _, err = reconciliation.IngestPaymentStatement(ctx, tx, w.f.tenantID, src, stmt, time.Now())
		return err
	}); err != nil {
		w.t.Fatalf("IngestPaymentStatement: %v", err)
	}
	var ms []reconciliation.Mismatch
	if err := w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, ms, _, err = reconciliation.RunPaymentStatement(ctx, tx, w.f.tenantID, importID, reconciliation.PaymentStatementOptions{})
		return err
	}); err != nil {
		w.t.Fatalf("RunPaymentStatement: %v", err)
	}
	return ms
}

func mismatchesOf(ms []reconciliation.Mismatch, kind reconciliation.MismatchKind, keyContains string) []reconciliation.Mismatch {
	var out []reconciliation.Mismatch
	for _, m := range ms {
		if m.MismatchKind == kind && strings.Contains(m.ReconciliationKey, keyContains) {
			out = append(out, m)
		}
	}
	return out
}

func render(ms []reconciliation.Mismatch) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "  %s | %s | expected=%s | actual=%s\n", m.MismatchKind, m.ReconciliationKey, m.ExpectedValue, m.ActualValue)
	}
	return b.String()
}

func (w *k3World) requireOne(ms []reconciliation.Mismatch, kind reconciliation.MismatchKind, attempt uuid.UUID, what string) reconciliation.Mismatch {
	w.t.Helper()
	got := mismatchesOf(ms, kind, "attempt="+attempt.String())
	if len(got) != 1 {
		w.t.Fatalf("%s: want exactly one %s for attempt %s, got %d:\n%s", what, kind, attempt, len(got), render(ms))
	}
	return got[0]
}

func (w *k3World) requireNone(ms []reconciliation.Mismatch, kind reconciliation.MismatchKind, attempt uuid.UUID, what string) {
	w.t.Helper()
	if got := mismatchesOf(ms, kind, "attempt="+attempt.String()); len(got) != 0 {
		w.t.Fatalf("%s: want no %s for attempt %s, got:\n%s", what, kind, attempt, render(ms))
	}
}

// --- K2 compensation helpers (the Step B arm) ----------------------------------

// k2Setup gives the world a K2 ledger_adjustment platform baseline and
// initiate/approve grants for f3 and f4 (distinct Persons from the M2 requester f1
// and approver f2).
func (w *k3World) k2Setup() {
	w.t.Helper()
	w.approvePolicy(adjustment.PolicyChangeInput{
		ChangeKind: adjustment.ChangeKindPolicy, OperationKind: adjustment.OperationKind, Level: adjustment.LevelPlatform,
		AssetCode: k3StrPtr("EUR"), BaseRequiredApprovals: k3IntPtr(1),
	}, w.adminA, w.adminB)
	for _, s := range []k3Staff{w.f1, w.f2, w.f3, w.f4} {
		w.grantTenant(s, capability.CapabilityLedgerAdjustmentInitiate)
		w.grantTenant(s, capability.CapabilityLedgerAdjustmentApprove)
	}
}

func (w *k3World) k2Service() *adjustment.Service { return adjustment.NewService(w.pool) }

func (w *k3World) k2Target(actor k3Staff) adjustment.Target {
	tg, err := adjustment.NewTarget(tenant.Context{TenantID: actor.TenantID}, w.f.tenantID)
	if err != nil {
		w.t.Fatal(err)
	}
	return tg
}

func (w *k3World) k2Submit(actor k3Staff, direction adjustment.Direction, amount int64, causation uuid.UUID) (adjustment.Request, error) {
	h := k3EvidenceHash()
	return w.k2Service().Submit(k3Ctx(actor), w.k2Target(actor), adjustment.SubmitInput{
		WalletID: w.f.walletID, AssetCode: "EUR", Direction: direction, Amount: amount, ReasonCode: adjustment.ReasonCompensatingEntry,
		CausationTransactionID: &causation, EvidenceRefHash: &h, Note: "k3 compensation"}, adjustment.Meta{RequestID: "k3-k2"})
}

func (w *k3World) k2Decide(actor k3Staff, r adjustment.Request) (adjustment.Outcome, error) {
	return w.k2Service().Decide(k3Ctx(actor), w.k2Target(actor), r.ID,
		adjustment.DecisionInput{Decision: adjustment.DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "k3-test"}, adjustment.Meta{RequestID: "k3-k2"})
}

// k2Compensate runs a full K2 compensating entry (f3 initiates, f4 approves).
func (w *k3World) k2Compensate(direction adjustment.Direction, amount int64, causation uuid.UUID) (adjustment.Outcome, error) {
	r, err := w.k2Submit(w.f3, direction, amount, causation)
	if err != nil {
		return adjustment.Outcome{}, err
	}
	return w.k2Decide(w.f4, r)
}

// --- tests ----------------------------------------------------------------------

// C-7 (R): "declare not paid", then a late provider success: T14 AND the standing
// pay_declared_not_paid_but_paid on EVERY run (including runs whose coverage
// excludes the payout), cleared only when recovery debits total the withdrawn
// amount (a partial recovery does not clear: mutant). LF test 10.
func TestK3_C7_LateSuccessAfterNotPaid_StandingUntilFullRecovery(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(400)
	res := w.executeM2(a.ID, ResolutionM2DeclareNotPaid)

	// Before any evidence of payment: no finding.
	w.requireNone(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "before the late success")

	if _, err := rvApplyReceipt(w.pool, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{
		EventType: "payout", ProviderReference: *a.ProviderReference, Outcome: OutcomeSucceeded, Amount: 400, AssetCode: "EUR"}); err != nil {
		t.Fatalf("late success: %v", err)
	}
	if got := w.attempt(a.ID); got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != "success_after_payout_declined" {
		t.Fatalf("want T14, got %s %v", got.State, got.TerminalReason)
	}
	// Standing on every run, including one whose coverage excludes the payout.
	for i, src := range []statement.PaymentStatementSource{w.source(false), w.pastSource(false), w.source(false), w.pastSource(false)} {
		ms := w.stmtRun(src)
		m := w.requireOne(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, fmt.Sprintf("run %d", i))
		if strings.Contains(m.ExpectedValue+m.ActualValue, "M1 clears") {
			t.Fatalf("detail claims M1 clears: %s", render(ms))
		}
	}
	// A partial recovery (399 of 400) does NOT clear it.
	if out, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 399, *res.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("partial recovery debit: %v %+v", err, out)
	}
	w.requireOne(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "after a partial recovery")
	// The remaining 1 totals the withdrawn amount: it clears.
	if out, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 1, *res.LedgerTransactionID); err != nil || !out.Executed {
		t.Fatalf("final recovery debit: %v %+v", err, out)
	}
	w.requireNone(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "after full recovery")
	w.assertInvariants()
}

// C-7b: a statement SUCCEEDED line for a declared-not-paid payout (no T14 yet) is
// the raising predicate too - any import, any amount.
func TestK3_C7b_NotPaidThenSucceededStatementLine(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(300)
	w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	ms := w.stmtRun(w.source(false, w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 300)))
	w.requireOne(ms, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a.ID, "succeeded line")
	// The raising predicate ignores the amount: a different amount raises it too.
	w2 := newK3World(t, k3Opts{base: 1})
	_, a2 := w2.ambiguousPayout(300)
	w2.executeM2(a2.ID, ResolutionM2DeclareNotPaid)
	ms2 := w2.stmtRun(w2.source(false, w2.payoutLine(*a2.ProviderReference, a2.MerchantReference, statement.PaymentStatusSucceeded, 299)))
	w2.requireOne(ms2, reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a2.ID, "succeeded line with another amount")
	// Persisted: a LATER run without the line still reports it (unwindowed, any import).
	w2.requireOne(w2.stmtRun(w2.source(false)), reconciliation.MismatchKindPayDeclaredNotPaidButPaid, a2.ID, "later run with no line")
}

var k3MetricReader = sdkmetric.NewManualReader()

func init() { otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(k3MetricReader))) }

func k3ConfirmedCount(t *testing.T) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := k3MetricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "payment_m2_declared_paid_confirmed_total" {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// C-8 (R): "declare paid", then a real success: no second Step B; a real matching
// statement line gives no pay_reference_mismatch and no missing-record finding,
// and the confirmation metric increments (LF test 10). C-20: a late provider
// decline after "declare paid" is recorded with no state change.
func TestK3_C8_C20_LateEvidenceAfterPaid(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(500)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)
	if n := w.ledgerTxCount("withdrawal_completed"); n != 1 {
		t.Fatalf("withdrawal_completed count = %d", n)
	}

	// Unconfirmed: the standing finding, annotated, with no compensation.
	ms := w.stmtRun(w.source(false))
	w.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "no line yet")

	// A real provider success arrives: recorded, no second Step B.
	if _, err := rvApplyReceipt(w.pool, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{
		EventType: "payout", ProviderReference: *a.ProviderReference, Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}); err != nil {
		t.Fatalf("late real success: %v", err)
	}
	if n := w.ledgerTxCount("withdrawal_completed"); n != 1 {
		t.Fatalf("a second Step B was posted: %d", n)
	}
	if got := w.attempt(a.ID); got.State != AttemptSucceeded || got.LastEvidenceKind != EvidenceOperator {
		t.Fatalf("attempt %s/%s changed", got.State, got.LastEvidenceKind)
	}

	// The real statement line confirms: clean (no unconfirmed, no mismatch), metric +1.
	before := k3ConfirmedCount(t)
	// The provider's own settlement reference differs from the reserved id the
	// Step B posted under: the F4 settlement-reference check must be EXEMPT for a
	// reserved-prefix Step B (mutant: exemption removed -> pay_reference_mismatch).
	confirm := w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 500)
	confirm.SettlementReference = "k3-provider-settlement-" + uuid.NewString()
	ms = w.stmtRun(w.source(false, confirm))
	if len(ms) != 0 {
		t.Fatalf("a matching confirming line must give a clean run, got:\n%s", render(ms))
	}
	if d := k3ConfirmedCount(t) - before; d != 1 {
		t.Fatalf("confirmation metric delta = %d, want 1", d)
	}
	// Persisted: a later run with no line stays clean (the confirmation is evidence).
	if ms := w.stmtRun(w.source(false)); len(mismatchesOf(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID.String())) != 0 {
		t.Fatalf("a persisted confirming line did not keep the finding cleared:\n%s", render(ms))
	}

	// C-20: a late provider DECLINE after "declare paid": recorded, no state change,
	// the withdrawal stays completed (its audit is the record).
	w2 := newK3World(t, k3Opts{base: 1})
	wr2, a2 := w2.ambiguousPayout(500)
	w2.executeM2(a2.ID, ResolutionM2DeclarePaid)
	if _, err := rvApplyReceipt(w2.pool, w2.orch, w2.f.tenantID, w2.provider, ReceiptEvidence{
		EventType: "payout", ProviderReference: *a2.ProviderReference, Outcome: OutcomeDeclined, Amount: 500, AssetCode: "EUR",
		DeclineStage: "after_acceptance"}); err != nil {
		t.Fatalf("late decline: %v", err)
	}
	if got := w2.attempt(a2.ID); got.State != AttemptSucceeded {
		t.Fatalf("a late decline moved a declared-paid attempt to %s", got.State)
	}
	if got := w2.withdrawalOf(wr2.ID); got.State != "completed" {
		t.Fatalf("withdrawal %s", got.State)
	}
	ms2 := w2.stmtRun(w2.source(false, w2.payoutLine(*a2.ProviderReference, a2.MerchantReference, statement.PaymentStatusDeclined, 500)))
	w2.requireOne(ms2, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a2.ID, "declined line keeps it standing")
	if n := len(mismatchesOf(ms2, reconciliation.MismatchKindPayStatusMismatch, "attempt="+a2.ID.String())); n != 1 {
		t.Fatalf("a declined statement line must give pay_status_mismatch:\n%s", render(ms2))
	}
	_ = wr
	_ = res
}

// C-45b (D-4 / RC-3, persisted lookup): once a non-MOCK import exists, a MOCK
// import's reversal line - in the run that carries it AND, as persisted
// evidence, in every later run - cannot clear a finding; a non-MOCK reversal can.
func TestK3_C45b_MockReversalCannotClearAFindingOnceARealImportExists(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	a := w.disputedDeposit(5000)
	ref := *a.ProviderReference
	line := w.depositLine(ref, a.MerchantReference, statement.PaymentStatusSucceeded, 5000)
	rev := func(real bool) statement.PaymentStatementSource {
		l := w.depositLine("k3-rev-"+uuid.NewString(), "", statement.PaymentStatusSucceeded, 5000)
		l.Kind = statement.PaymentLineDepositReversal
		l.OriginalProviderReference = ref
		return w.source(real, l)
	}
	w.requireOne(w.stmtRun(w.source(true, line)), reconciliation.MismatchKindPayCapturedUnposted, a.ID, "raised against a REAL import")
	w.requireOne(w.stmtRun(rev(false)), reconciliation.MismatchKindPayCapturedUnposted, a.ID, "a MOCK reversal in the run that carries it")
	w.requireOne(w.stmtRun(w.pastSource(true)), reconciliation.MismatchKindPayCapturedUnposted, a.ID, "a persisted MOCK reversal in a later run")
	w.requireNone(w.stmtRun(rev(true)), reconciliation.MismatchKindPayCapturedUnposted, a.ID, "a REAL reversal clears")
	w.requireNone(w.stmtRun(w.pastSource(true)), reconciliation.MismatchKindPayCapturedUnposted, a.ID, "and stays cleared")
}

// C-45c: the same payout line persisted by a MOCK import and a later non-MOCK
// import is one line that is eligible if ANY copy is (the dedupe ORs eligibility).
func TestK3_C45c_OverlappingMockAndRealCopiesConfirmOnTheRealOne(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(500)
	w.executeM2(a.ID, ResolutionM2DeclarePaid)
	line := w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 500)
	// Import 1 (MOCK, no real import yet): the line confirms.
	w.requireNone(w.stmtRun(w.source(false, line)), reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "MOCK-only world")
	// Import 2 (non-MOCK): the MOCK copy is now ineligible, the real copy is eligible.
	w.requireNone(w.stmtRun(w.source(true, line)), reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "the real copy confirms")
	w.requireNone(w.stmtRun(w.pastSource(true)), reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "and it stays confirmed")
}

// C-42 (LF D-5): a succeeded line with a DIFFERENT amount does not clear
// pay_declared_paid_unconfirmed, and it raises pay_amount_mismatch.
func TestK3_C42_DifferentAmountLineDoesNotConfirm(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(500)
	w.executeM2(a.ID, ResolutionM2DeclarePaid)
	ms := w.stmtRun(w.source(false, w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 499)))
	w.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "a different-amount line")
	if len(mismatchesOf(ms, reconciliation.MismatchKindPayAmountMismatch, "attempt="+a.ID.String())) != 1 {
		t.Fatalf("pay_amount_mismatch missing:\n%s", render(ms))
	}
	// A different ASSET does not confirm either.
	w2 := newK3World(t, k3Opts{base: 1})
	_, a2 := w2.ambiguousPayout(500)
	w2.executeM2(a2.ID, ResolutionM2DeclarePaid)
	l := w2.payoutLine(*a2.ProviderReference, a2.MerchantReference, statement.PaymentStatusSucceeded, 500)
	l.AssetCode = "USD"
	ms2 := w2.stmtRun(w2.source(false, l))
	w2.requireOne(ms2, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a2.ID, "a different-asset line")
}

// C-45 (LF D-4, section 26 RC-3): a MOCK import cannot clear or confirm a finding
// when ANY non-MOCK import exists for (tenant, provider); a MOCK-only history
// can.
func TestK3_C45_MockEvidenceEligibility(t *testing.T) {
	// (1) MOCK-only: a MOCK confirming line confirms.
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(500)
	w.executeM2(a.ID, ResolutionM2DeclarePaid)
	line := w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 500)
	if ms := w.stmtRun(w.source(false, line)); len(ms) != 0 {
		t.Fatalf("a MOCK-only environment's confirming line must clear:\n%s", render(ms))
	}
	// (2) A real (non-MOCK) import exists, then a MOCK confirming line: does NOT confirm.
	w2 := newK3World(t, k3Opts{base: 1})
	_, a2 := w2.ambiguousPayout(500)
	w2.executeM2(a2.ID, ResolutionM2DeclarePaid)
	w2.stmtRun(w2.source(true)) // an empty real-shaped import: its existence is what matters
	line2 := w2.payoutLine(*a2.ProviderReference, a2.MerchantReference, statement.PaymentStatusSucceeded, 500)
	ms := w2.stmtRun(w2.source(false, line2))
	w2.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a2.ID, "a MOCK line after a real import")
	// ... while a REAL confirming line does.
	ms = w2.stmtRun(w2.source(true, line2))
	w2.requireNone(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a2.ID, "a real confirming line")
}

// C-22 / C-29 (R): a compensation ANNOTATES but does not clear
// pay_declared_paid_unconfirmed across N runs; a confirming line clears it and
// raises pay_declared_paid_compensated_but_paid; a partial recovery does not clear
// that, a full recovery does. C-23..C-28: the Step B arm's own conditions.
func TestK3_C22_C29_CompensationAnnotatesNeverClears(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a := w.ambiguousPayout(500)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)
	creditsBefore := w.walletBalance("player_cash")

	// The compensating credit (f3 initiates, f4 approves): allowed even though the
	// player has an open captured-unposted exposure? (none here) - the arm admits.
	out, err := w.k2Compensate(adjustment.DirectionCreditPlayer, 500, *res.LedgerTransactionID)
	if err != nil || !out.Executed {
		t.Fatalf("Step B compensating credit: %v %+v", err, out)
	}
	if d := w.walletBalance("player_cash") - creditsBefore; d != 500 {
		t.Fatalf("player_cash delta = %d, want +500", d)
	}
	// Across N runs the finding stands, annotated with the compensation.
	for i := 0; i < 3; i++ {
		ms := w.stmtRun(w.source(false))
		m := w.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, fmt.Sprintf("run %d", i))
		if !strings.Contains(m.ActualValue, "compensating credits executed=500") {
			t.Fatalf("no compensation annotation: %s", m.ActualValue)
		}
		w.requireNone(ms, reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, a.ID, "no confirming line yet")
	}
	// A confirming line: (c) clears, (c2) is raised.
	line := w.payoutLine(*a.ProviderReference, a.MerchantReference, statement.PaymentStatusSucceeded, 500)
	ms := w.stmtRun(w.source(false, line))
	w.requireNone(ms, reconciliation.MismatchKindPayDeclaredPaidUnconfirmed, a.ID, "confirmed")
	w.requireOne(ms, reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, a.ID, "compensated and paid")
	// Standing on later runs (the line is persisted).
	w.requireOne(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, a.ID, "later run")

	// Recovery: debits whose causation is the credit's own transaction.
	creditTx := w.countTx("manual_adjustment")
	_ = creditTx
	var creditLedger uuid.UUID
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ledger_transaction_id FROM ledger_adjustment_requests WHERE tenant_id = $1 AND direction = 'credit_player' AND state = 'executed'`,
			w.f.tenantID).Scan(&creditLedger)
	})
	if o, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 499, creditLedger); err != nil || !o.Executed {
		t.Fatalf("partial recovery: %v %+v", err, o)
	}
	w.requireOne(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, a.ID, "partial recovery")
	if o, err := w.k2Compensate(adjustment.DirectionDebitPlayer, 1, creditLedger); err != nil || !o.Executed {
		t.Fatalf("final recovery: %v %+v", err, o)
	}
	w.requireNone(w.stmtRun(w.source(false)), reconciliation.MismatchKindPayDeclaredPaidCompensatedButPaid, a.ID, "full recovery")
	w.assertInvariants()
}

func (w *k3World) countTx(txType string) int { return w.ledgerTxCount(txType) }
