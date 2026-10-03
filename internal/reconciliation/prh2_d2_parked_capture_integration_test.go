//go:build integration

// PRH-2 D2 - PAY-RECON-PARKED-CAPTURE-1 (ADR 0095 §35; ledger-finance ruling
// on QA C-F2, docs/plans/prh2-hardening-round/reviews/c-ledger-finance.md
// "(a)" and "(c) 1-7"; QA F5 of plan §5 D).
//
// Every T10 whose provider evidence says "captured" while the platform
// disputed and posted nothing must be visible to the payment_statement
// stream as pay_captured_unposted:
//
//   - bound reasons (the attempt holds the captured reference):
//     sync_amount_mismatch (PRH-2 C), poll_amount_mismatch and
//     poll_reference_mismatch (PRH-2 D1). In-run AND standing; cleared only
//     by a deposit_reversal line naming the bound reference or a tombstone
//     on it.
//   - unbound parks (provider_reference_conflict,
//     invalid_provider_reference:*): in-run, when a succeeded deposit line
//     resolves to the attempt by merchant reference.
//
// Fixtures: the C reasons are produced through the REAL deposit path
// (InitiateDepositAttempt -> phase C park) with a scripted wrapper around the
// MOCK adapter. In TestD2_1..3 the D1 poll reasons are fixture parks (a real
// pending deposit moved to disputed with payments.ApplyDisputeFromNonTerminal
// and D1's reason constant); TestD2_10 drives REAL D1 poll parks through the
// sweeper end to end (QA D2-F1).
//
// One tenant, the MOCK provider, test-only fixed MOCK statement sources.
package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// The PRH-2 D1 poll T10 reasons, from payments (D1 merged at 7adb0c5).
// Production reconciliation spells them as string literals and never imports
// internal/payments; payment_reason_classification_test.go pins the two.
const (
	d2ReasonPollAmountMismatch    = payments.TerminalReasonPollAmountMismatch
	d2ReasonPollReferenceMismatch = payments.TerminalReasonPollReferenceMismatch
)

const (
	d2Amount     int64 = 5000
	d2PSPAmount  int64 = 5001 // what the provider says it captured on an amount mismatch
	d2KindCU           = MismatchKindPayCapturedUnposted
	d2WideBefore       = -time.Hour
)

// d2Provider wraps the MOCK and lets a test rewrite what Deposit and
// QueryStatus return.
type d2Provider struct {
	*payments.MockProvider
	mu     sync.Mutex
	script func(req payments.DepositRequest) payments.DepositResult
	status map[string]payments.StatusResult // QueryStatus overrides by reference
}

func (p *d2Provider) QueryStatus(ctx context.Context, ref string) (payments.StatusResult, error) {
	p.mu.Lock()
	ov, ok := p.status[ref]
	p.mu.Unlock()
	if ok {
		return ov, nil
	}
	return p.MockProvider.QueryStatus(ctx, ref)
}

func (p *d2Provider) setStatus(ref string, st payments.StatusResult) {
	p.mu.Lock()
	if p.status == nil {
		p.status = map[string]payments.StatusResult{}
	}
	p.status[ref] = st
	p.mu.Unlock()
}

func (p *d2Provider) Deposit(ctx context.Context, req payments.DepositRequest) (payments.DepositResult, error) {
	res, err := p.MockProvider.Deposit(ctx, req)
	p.mu.Lock()
	script := p.script
	p.mu.Unlock()
	if err != nil || script == nil {
		return res, err
	}
	return script(req), nil
}

func (p *d2Provider) setScript(s func(req payments.DepositRequest) payments.DepositResult) {
	p.mu.Lock()
	p.script = s
	p.mu.Unlock()
}

type d2World struct {
	*payWorld
	p *d2Provider
}

// newD2World: one tenant, provider A only (the scripted MOCK, sync success
// allowed so a sync_amount_mismatch is reachable). w.mockA is the wrapped
// MOCK itself, so w.succeed / w.deliverReversal keep working.
func newD2World(t *testing.T) *d2World {
	t.Helper()
	pool := testPool(t)
	mp := payments.NewMockProvider(payProvA, "EUR")
	mp.SetManifest(payments.OperationManifest{SupportsDeposit: true, StatusQuery: "by_provider_reference", IdempotentSubmission: true, SyncSuccessPossible: true})
	p := &d2Provider{MockProvider: mp}
	w := &payWorld{pool: pool, f: seedFixture(t, pool), mockA: mp}
	w.orch = payments.NewOrchestrator(
		map[string]payments.PaymentProvider{payProvA: p},
		payments.MultiWebhookCredentialResolver{payProvA: payments.NewMockWebhookCredentials(mp)})
	w.registerCapability(t, p, 10)
	w.srcA = payments.NewMockStatementSource(mp, payments.MockCredentialResolver{})
	return &d2World{payWorld: w, p: p}
}

func d2Pending(ref string) func(payments.DepositRequest) payments.DepositResult {
	return func(req payments.DepositRequest) payments.DepositResult {
		return payments.DepositResult{Outcome: payments.OutcomePending, ProviderReference: ref,
			RedirectURL: "https://mock-psp.invalid/pay/x", Amount: req.Amount, AssetCode: req.AssetCode}
	}
}

func d2SyncEcho(ref string, amount int64) func(payments.DepositRequest) payments.DepositResult {
	return func(req payments.DepositRequest) payments.DepositResult {
		return payments.DepositResult{Outcome: payments.OutcomeSucceeded, ProviderReference: ref, Amount: amount, AssetCode: req.AssetCode}
	}
}

func d2Reason(a payments.PaymentAttempt) string {
	if a.TerminalReason == nil {
		return ""
	}
	return *a.TerminalReason
}

// d2Parked is one parked deposit attempt, plus the reference the provider
// reported for it (the bound reference for a bound reason; the line
// reference for an unbound park).
type d2Parked struct {
	attempt payments.PaymentAttempt
	pspRef  string
}

func (w *d2World) mustParked(t *testing.T, id uuid.UUID, wantReason string, wantPrefix bool) payments.PaymentAttempt {
	t.Helper()
	a := w.attempt(t, id)
	r := d2Reason(a)
	ok := r == wantReason || (wantPrefix && strings.HasPrefix(r, wantReason+":"))
	if a.State != payments.AttemptDisputed || !ok {
		t.Fatalf("setup: attempt %s state=%s reason=%q, want disputed/%s", id, a.State, r, wantReason)
	}
	if a.LedgerTransactionID != nil {
		t.Fatalf("setup: a parked attempt must carry no ledger link, got %s", *a.LedgerTransactionID)
	}
	return a
}

// parkSyncMismatch: the REAL phase C T10 sync_amount_mismatch. C binds the
// validated reference on this park (§34.8, F-C1), which is what lets
// reconciliation match and clear it by reference.
func (w *d2World) parkSyncMismatch(t *testing.T) d2Parked {
	t.Helper()
	ref := "d2-sm-" + uuid.NewString()
	w.p.setScript(d2SyncEcho(ref, d2PSPAmount))
	defer w.p.setScript(nil)
	a := w.mustParked(t, w.deposit(t, d2Amount).ID, payments.TerminalReasonSyncAmountMismatch, false)
	if a.ProviderReference == nil || *a.ProviderReference != ref {
		t.Fatalf("setup: the sync_amount_mismatch park must bind the validated reference %q, got %v", ref, a.ProviderReference)
	}
	return d2Parked{attempt: a, pspRef: ref}
}

// parkPoll: a real pending deposit (reference bound by T4), then the T10 a
// poll would apply under D1 (fixture: ApplyDisputeFromNonTerminal with the
// plan's reason string; D1's code is not used).
func (w *d2World) parkPoll(t *testing.T, reason string) d2Parked {
	t.Helper()
	a := w.deposit(t, d2Amount)
	if a.State != payments.AttemptPending || a.ProviderReference == nil {
		t.Fatalf("setup: expected a pending deposit with a bound reference, got state=%s ref=%v", a.State, a.ProviderReference)
	}
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return payments.ApplyDisputeFromNonTerminal(ctx, tx, a.ID, payments.EvidenceQueryStatus, reason)
	}); err != nil {
		t.Fatalf("setup: T10 %s: %v", reason, err)
	}
	a = w.mustParked(t, a.ID, reason, false)
	return d2Parked{attempt: a, pspRef: *a.ProviderReference}
}

// parkPayoutConflict: the REAL phase C T10 provider_reference_conflict, the
// adapter returning a reference already bound to a payout attempt of this
// tenant. The park never binds it (§34.8).
func (w *d2World) parkPayoutConflict(t *testing.T) (d2Parked, uuid.UUID) {
	t.Helper()
	payoutRef := "d2-payout-" + uuid.NewString()
	payoutID := w.payoutFixture(t, payProvA, payoutRef, "", d2Amount, false)
	w.p.setScript(d2Pending(payoutRef))
	defer w.p.setScript(nil)
	a := w.mustParked(t, w.deposit(t, d2Amount).ID, payments.TerminalReasonProviderReferenceConflict, false)
	if a.ProviderReference != nil {
		t.Fatalf("setup: a conflict park must not bind the reference, got %s", *a.ProviderReference)
	}
	return d2Parked{attempt: a, pspRef: payoutRef}, payoutID
}

// parkInvalidRef: the REAL phase C T10 invalid_provider_reference:<reason>
// (a control character). Nothing of the reference is persisted.
func (w *d2World) parkInvalidRef(t *testing.T) (d2Parked, string) {
	t.Helper()
	bad := "d2-bad\x01" + uuid.NewString()[:8]
	w.p.setScript(d2Pending(bad))
	defer w.p.setScript(nil)
	a := w.mustParked(t, w.deposit(t, d2Amount).ID, payments.TerminalReasonInvalidProviderReference, true)
	if a.ProviderReference != nil {
		t.Fatalf("setup: an invalid-reference park must not bind anything, got %q", *a.ProviderReference)
	}
	// The provider's own (valid) reference for the capture, as a statement
	// would carry it alongside the merchant reference.
	return d2Parked{attempt: a, pspRef: "d2-psp-own-" + uuid.NewString()}, bad
}

func d2Line(ref, merchant, status string, amount int64) statement.PaymentStatementLine {
	return payLineFor(payProvA, ref, merchant, statement.PaymentLineDeposit, status, amount)
}

func d2ReversalLine(ref, original string, amount int64) statement.PaymentStatementLine {
	l := payLineFor(payProvA, ref, "", statement.PaymentLineDepositReversal, statement.PaymentStatusSucceeded, amount)
	l.OriginalProviderReference = original
	return l
}

func d2Src(lines ...statement.PaymentStatementLine) payFixedSource {
	return payFixedSource{provider: payProvA, stmt: wideCoverage(lines...)}
}

// d2PastSrc: a later run whose coverage window lies entirely in the past
// (it excludes the parked attempt and its line) and which carries no line.
func d2PastSrc() payFixedSource {
	return payFixedSource{provider: payProvA, stmt: statement.PaymentStatement{
		CoverageStart: time.Now().Add(-48 * time.Hour).UTC(), CoverageEnd: time.Now().Add(-47 * time.Hour).UTC()}}
}

func d2Kinds(ms []Mismatch) map[MismatchKind]int {
	out := map[MismatchKind]int{}
	for _, m := range ms {
		out[m.MismatchKind]++
	}
	return out
}

// d2Expect asserts the run reported EXACTLY these kinds and counts.
func d2Expect(t *testing.T, ms []Mismatch, want map[MismatchKind]int) {
	t.Helper()
	got := d2Kinds(ms)
	same := len(got) == len(want)
	for k, n := range want {
		if got[k] != n {
			same = false
		}
	}
	if !same {
		t.Fatalf("mismatch kinds = %v, want exactly %v:\n%s", got, want, renderMismatches(ms))
	}
}

// d2CUFor asserts exactly one pay_captured_unposted names the attempt.
func d2CUFor(t *testing.T, ms []Mismatch, attemptID uuid.UUID) Mismatch {
	t.Helper()
	var found []Mismatch
	for _, m := range ms {
		if m.MismatchKind == d2KindCU && strings.Contains(m.ReconciliationKey, "attempt="+attemptID.String()) {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one pay_captured_unposted for attempt %s, got %d:\n%s", attemptID, len(found), renderMismatches(ms))
	}
	if !strings.Contains(found[0].ReconciliationKey, "check=captured_unposted") {
		t.Fatalf("pay_captured_unposted key lacks check=captured_unposted: %s", found[0].ReconciliationKey)
	}
	// D2 review P2 (ADR 0101 F13 / C-19): operators act on this text. It
	// must name allocation (LEDGER-SUSPENSE-B-1), say M1 only acknowledges,
	// and never suggest that M1 clears the finding.
	if ev := found[0].ExpectedValue; !strings.Contains(ev, "allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges") || strings.Contains(ev, "M1/allocation") {
		t.Fatalf("pay_captured_unposted detail must use the F13 wording, got %q", ev)
	}
	return found[0]
}

func d2NoCU(t *testing.T, ms []Mismatch, why string) {
	t.Helper()
	if d2Kinds(ms)[d2KindCU] != 0 {
		t.Fatalf("%s: expected NO pay_captured_unposted, got:\n%s", why, renderMismatches(ms))
	}
}

// d2Run is LF (c) 6's "the reconciliation run posts nothing": fetch and
// ingest, snapshot, match, snapshot - the match writes one run and its
// mismatch rows and nothing else (ledger, SUM equality, projections,
// attempts, intents, receipts, withdrawals untouched).
func (w *d2World) d2Run(t *testing.T, src statement.PaymentStatementSource) []Mismatch {
	t.Helper()
	id, _ := w.fetchIngest(t, src, PaymentStatementOptions{})
	before := capturePay(t, w.pool, w.f.tenantID)
	run, ms, _, err := w.matchErr(t, id, PaymentStatementOptions{})
	if err != nil {
		t.Fatalf("RunPaymentStatement: %v", err)
	}
	if (len(ms) == 0) != (run.Status == StatusClean) {
		t.Fatalf("run status %s inconsistent with %d mismatches", run.Status, len(ms))
	}
	assertOnlyRunWritten(t, before, capturePay(t, w.pool, w.f.tenantID), len(ms))
	return ms
}

// d2AssertNoMoney is LF (c) 6 for one parked attempt: zero ledger
// transactions for it (no deposit under its intent or its reported
// reference, no ledger link), SUM(D)=SUM(C) for the tenant, and
// RunLedgerVsProjection reports 0 mismatches.
func (w *d2World) d2AssertNoMoney(t *testing.T, pk d2Parked) {
	t.Helper()
	a := w.attempt(t, pk.attempt.ID)
	if a.State != payments.AttemptDisputed || a.LedgerTransactionID != nil {
		t.Fatalf("parked attempt changed: state=%s ledger=%v", a.State, a.LedgerTransactionID)
	}
	var n, debits, credits int64
	var drift []Mismatch
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM ledger_transactions
			 WHERE tenant_id = $1 AND transaction_type <> 'tombstone'
			   AND (correlation_id = $2 OR (provider_id = $3 AND provider_tx_id = $4))`,
			w.f.tenantID, *a.DepositIntentID, payProvA, pk.pspRef).Scan(&n); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint,
			       COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint
			  FROM ledger_entries WHERE tenant_id = $1`, w.f.tenantID).Scan(&debits, &credits); err != nil {
			return err
		}
		var err error
		_, drift, err = RunLedgerVsProjection(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("LF (c) 6: %d ledger transaction(s) for the parked attempt %s (intent or reference %q); want 0", n, a.ID, pk.pspRef)
	}
	if debits != credits {
		t.Errorf("LF (c) 6: SUM(debits)=%d != SUM(credits)=%d", debits, credits)
	}
	if len(drift) != 0 {
		t.Errorf("LF (c) 6: RunLedgerVsProjection reported %d mismatches, want 0: %v", len(drift), drift)
	}
}

// d2BoundCase is one bound reason with how to build it and what its
// statement line reports.
type d2BoundCase struct {
	name      string
	build     func(t *testing.T, w *d2World) d2Parked
	lineAmt   int64 // the provider's amount on the statement line
	amtDiffer bool  // pay_amount_mismatch also expected in-window
}

func d2BoundCases() []d2BoundCase {
	return []d2BoundCase{
		{payments.TerminalReasonSyncAmountMismatch, func(t *testing.T, w *d2World) d2Parked { return w.parkSyncMismatch(t) }, d2PSPAmount, true},
		{d2ReasonPollAmountMismatch, func(t *testing.T, w *d2World) d2Parked { return w.parkPoll(t, d2ReasonPollAmountMismatch) }, d2PSPAmount, true},
		{d2ReasonPollReferenceMismatch, func(t *testing.T, w *d2World) d2Parked { return w.parkPoll(t, d2ReasonPollReferenceMismatch) }, d2Amount, false},
	}
}

func (c d2BoundCase) inRunWant() map[MismatchKind]int {
	want := map[MismatchKind]int{d2KindCU: 1}
	if c.amtDiffer {
		want[MismatchKindPayAmountMismatch] = 1
	}
	return want
}

// TestD2_1_BoundPark_MatchedSucceededLine_ReportsCapturedUnposted is LF (c) 1
// (plus the D1 poll reasons): a park, plus a statement line matched by the
// bound reference with status succeeded and the provider's amount, gives
// pay_captured_unposted for that attempt (and pay_amount_mismatch where the
// amounts differ). Not silently excluded. Mutant M-PRED kills it.
func TestD2_1_BoundPark_MatchedSucceededLine_ReportsCapturedUnposted(t *testing.T) {
	for _, c := range d2BoundCases() {
		t.Run(c.name, func(t *testing.T) {
			w := newD2World(t)
			pk := c.build(t, w)
			ms := w.d2Run(t, d2Src(d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, c.lineAmt)))
			d2Expect(t, ms, c.inRunWant())
			m := d2CUFor(t, ms, pk.attempt.ID)
			if !strings.Contains(m.ActualValue, "terminal_reason="+c.name) || !strings.Contains(m.ActualValue, "status=succeeded") {
				t.Errorf("detail must name the reason and the succeeded line: %s", m.ActualValue)
			}
			if c.amtDiffer {
				mustKeyed(t, ms, MismatchKindPayAmountMismatch, "attempt="+pk.attempt.ID.String())
			}
			w.d2AssertNoMoney(t, pk)
		})
	}
	// poll_reference_mismatch where the statement line carries the
	// provider's ECHOED reference (not the bound one): it resolves by
	// merchant reference, pay_reference_mismatch fires, and the capture is
	// still reported.
	t.Run("poll_reference_mismatch_line_carries_echo", func(t *testing.T) {
		w := newD2World(t)
		pk := w.parkPoll(t, d2ReasonPollReferenceMismatch)
		echo := "d2-echo-" + uuid.NewString()
		ms := w.d2Run(t, d2Src(d2Line(echo, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1, MismatchKindPayReferenceMismatch: 1})
		d2CUFor(t, ms, pk.attempt.ID)
		w.d2AssertNoMoney(t, pk)
	})
	// Control: a non-succeeded line (pending, declined, reversed) on a bound
	// park is not a captured_unposted finding in-run (R1: `reversed` is the
	// PSP's own refund).
	t.Run("non_succeeded_line_is_not_flagged_in_run", func(t *testing.T) {
		for _, st := range []string{statement.PaymentStatusPending, statement.PaymentStatusDeclined, statement.PaymentStatusReversed} {
			w := newD2World(t)
			pk := w.parkSyncMismatch(t)
			ms := w.d2Run(t, d2Src(d2Line(pk.pspRef, "", st, d2PSPAmount)))
			d2NoCU(t, ms, "line status "+st)
			w.d2AssertNoMoney(t, pk)
		}
	})
}

func mustKeyed(t *testing.T, ms []Mismatch, kind MismatchKind, keyContains string) {
	t.Helper()
	for _, m := range ms {
		if m.MismatchKind == kind && strings.Contains(m.ReconciliationKey, keyContains) {
			return
		}
	}
	t.Fatalf("no %s mismatch keyed %q:\n%s", kind, keyContains, renderMismatches(ms))
}

// TestD2_2_BoundPark_Standing_ReportedWhenCoverageExcludesTheLine is LF (c) 2:
// after the matched run, a later run whose statement carries no line for
// the attempt (an empty wide window, and a window entirely in the past that
// excludes both the attempt and its line) still reports
// pay_captured_unposted - unwindowed, every run. Mutant M-PRED kills it.
func TestD2_2_BoundPark_Standing_ReportedWhenCoverageExcludesTheLine(t *testing.T) {
	for _, c := range d2BoundCases() {
		t.Run(c.name, func(t *testing.T) {
			w := newD2World(t)
			pk := c.build(t, w)
			d2Expect(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, c.lineAmt))), c.inRunWant())

			for name, src := range map[string]payFixedSource{"empty_wide_window": d2Src(), "past_window": d2PastSrc()} {
				ms := w.d2Run(t, src)
				d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1})
				m := d2CUFor(t, ms, pk.attempt.ID)
				if !strings.Contains(m.ActualValue, "no statement line") || !strings.Contains(m.ReconciliationKey, "provider_reference="+pk.pspRef) {
					t.Errorf("%s: standing finding must be the no-line form keyed by the bound reference: key=%s actual=%s", name, m.ReconciliationKey, m.ActualValue)
				}
			}
			w.d2AssertNoMoney(t, pk)
		})
	}
}

// TestD2_3_BoundPark_ClearsOnlyOnReversalLineOrTombstoneOnBoundRef is LF
// (c) 3: the finding clears only after a deposit_reversal line naming the
// bound reference, or a tombstone on the bound reference. Marking the
// mismatch row resolved (investigation_status, the only operator surface
// today), a reversal line naming a DIFFERENT reference, and a tombstone on
// a DIFFERENT reference do not clear it. (M1 is NOT IMPLEMENTED - ADR 0101,
// K3; ADR 0101 §4 LF-3 says M1 writes nothing reconciliation reads, and the
// emission predicate reads nothing M1 could write. Asserted structurally by
// the investigation_status case: no row state of a prior finding is read.)
func TestD2_3_BoundPark_ClearsOnlyOnReversalLineOrTombstoneOnBoundRef(t *testing.T) {
	builders := map[string]func(t *testing.T, w *d2World) d2Parked{
		payments.TerminalReasonSyncAmountMismatch: func(t *testing.T, w *d2World) d2Parked { return w.parkSyncMismatch(t) },
		d2ReasonPollAmountMismatch:                func(t *testing.T, w *d2World) d2Parked { return w.parkPoll(t, d2ReasonPollAmountMismatch) },
	}
	for name, build := range builders {
		t.Run(name+"/investigation_status_resolved_does_not_clear", func(t *testing.T) {
			w := newD2World(t)
			pk := build(t, w)
			ms := w.d2Run(t, d2Src())
			m := d2CUFor(t, ms, pk.attempt.ID)
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return ResolveMismatch(ctx, tx, m.ID, uuid.New(), "d2: operator marks it resolved", nil)
			}); err != nil {
				t.Fatalf("ResolveMismatch: %v", err)
			}
			var status string
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT investigation_status FROM reconciliation_mismatches WHERE id = $1`, m.ID).Scan(&status)
			}); err != nil || status != "resolved" {
				t.Fatalf("setup: investigation_status=%q err=%v", status, err)
			}
			d2CUFor(t, w.d2Run(t, d2Src()), pk.attempt.ID)
			w.d2AssertNoMoney(t, pk)
		})
		t.Run(name+"/reversal_line_or_tombstone_on_another_reference_does_not_clear", func(t *testing.T) {
			w := newD2World(t)
			pk := build(t, w)
			other := "d2-other-" + uuid.NewString()
			// A tombstone on an unrelated reference (a reversal of a
			// never-seen original), through the real receipt path.
			w.deliverReversal(t, "d2-rev-"+uuid.NewString()[:8], other, 1234)
			ms := w.d2Run(t, d2Src(d2ReversalLine("d2-rev2-"+uuid.NewString()[:8], "d2-other2-"+uuid.NewString(), d2PSPAmount)))
			d2CUFor(t, ms, pk.attempt.ID)
			w.d2AssertNoMoney(t, pk)
		})
		t.Run(name+"/reversal_line_on_bound_reference_clears", func(t *testing.T) {
			w := newD2World(t)
			pk := build(t, w)
			d2CUFor(t, w.d2Run(t, d2Src()), pk.attempt.ID)
			ms := w.d2Run(t, d2Src(d2ReversalLine("d2-rev-"+uuid.NewString()[:8], pk.pspRef, d2PSPAmount)))
			d2NoCU(t, ms, "a reversal line naming the bound reference")
			// The same reversal line alongside the parked capture's own
			// succeeded line also clears the in-run finding.
			ms = w.d2Run(t, d2Src(d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, d2PSPAmount),
				d2ReversalLine("d2-rev3-"+uuid.NewString()[:8], pk.pspRef, d2PSPAmount)))
			d2NoCU(t, ms, "in-run with a reversal line naming the bound reference")
			w.d2AssertNoMoney(t, pk)
		})
		t.Run(name+"/tombstone_on_bound_reference_clears", func(t *testing.T) {
			w := newD2World(t)
			pk := build(t, w)
			d2CUFor(t, w.d2Run(t, d2Src()), pk.attempt.ID)
			w.deliverReversal(t, "d2-tomb-"+uuid.NewString()[:8], pk.pspRef, d2PSPAmount)
			var tombs int64
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone' AND provider_id = $2 AND provider_tx_id = $3`,
					w.f.tenantID, payProvA, pk.pspRef).Scan(&tombs)
			}); err != nil {
				t.Fatal(err)
			}
			if tombs != 1 {
				t.Fatalf("setup: expected exactly one tombstone on the bound reference, got %d", tombs)
			}
			d2NoCU(t, w.d2Run(t, d2Src()), "a tombstone on the bound reference (standing)")
			d2NoCU(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, d2PSPAmount))), "a tombstone on the bound reference (in-run)")
			w.d2AssertNoMoney(t, pk)
		})
	}
}

// TestD2_4_ConflictParks is LF (c) 4.
func TestD2_4_ConflictParks(t *testing.T) {
	// A provider_reference_conflict bound to a PAYOUT attempt, plus a
	// deposit line resolving to the parked attempt by merchant reference
	// with status succeeded: flagged in-run. The payout attempt is not
	// touched, and an unbound park is not a standing finding.
	t.Run("payout_bound_conflict_merchant_resolved_succeeded_line_is_flagged", func(t *testing.T) {
		w := newD2World(t)
		pk, payoutID := w.parkPayoutConflict(t)
		payoutBefore := w.attempt(t, payoutID)
		ms := w.d2Run(t, d2Src(d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1})
		m := d2CUFor(t, ms, pk.attempt.ID)
		if !strings.Contains(m.ActualValue, "terminal_reason=provider_reference_conflict") || strings.Contains(m.ExpectedValue, "M1/allocation") {
			t.Errorf("unbound-park detail: expected=%s actual=%s", m.ExpectedValue, m.ActualValue)
		}
		if after := w.attempt(t, payoutID); after.State != payoutBefore.State || !after.UpdatedAt.Equal(payoutBefore.UpdatedAt) {
			t.Errorf("the payout attempt changed: %s -> %s", payoutBefore.State, after.State)
		}
		// Not succeeded: not flagged.
		d2NoCU(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusPending, d2Amount))), "a pending merchant-resolved line")
		// In-run clearing on the LINE's reference: a reversal line naming it.
		d2NoCU(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount),
			d2ReversalLine("d2-rev-"+uuid.NewString()[:8], pk.pspRef, d2Amount))), "a reversal line naming the line's reference")
		// Unbound parks are in-run only (standing: ADR 0095 §35.4, NOT IMPLEMENTED).
		d2NoCU(t, w.d2Run(t, d2Src()), "an unbound park with no line this run")
		w.d2AssertNoMoney(t, pk)
	})
	// Defence in depth: an unbound-park reason on an attempt that DOES hold
	// a reference (not producible by C's code - fixture via T10 on a real
	// pending deposit) and a succeeded line naming that reference: still
	// flagged, never silently excluded. Pins the rule as not gated on how
	// the line resolved.
	t.Run("unbound_reason_on_a_reference_holding_attempt_is_still_flagged", func(t *testing.T) {
		w := newD2World(t)
		pk := w.parkPoll(t, payments.TerminalReasonProviderReferenceConflict)
		ms := w.d2Run(t, d2Src(d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1})
		d2CUFor(t, ms, pk.attempt.ID)
		w.d2AssertNoMoney(t, pk)
	})
	// A conflict bound to ANOTHER DEPOSIT attempt, plus a second succeeded
	// line with the same reference: pay_duplicate (check=duplicate_line),
	// reported once. The first line resolves to the holder, which is
	// succeeded, so it is otherwise clean. (LF ruling: "adequate as an
	// in-window signal"; verifies the existing matcher.)
	t.Run("deposit_bound_conflict_second_succeeded_line_is_pay_duplicate", func(t *testing.T) {
		w := newD2World(t)
		holder := w.deposit(t, d2Amount)
		if holder.ProviderReference == nil {
			t.Fatalf("setup: holder has no reference (state %s)", holder.State)
		}
		ref := *holder.ProviderReference
		w.succeed(t, w.mockA, payProvA, holder)
		w.p.setScript(d2Pending(ref))
		parked := w.mustParked(t, w.deposit(t, d2Amount).ID, payments.TerminalReasonProviderReferenceConflict, false)
		w.p.setScript(nil)
		if parked.ProviderReference != nil {
			t.Fatalf("setup: the conflict park must not bind the reference")
		}
		ms := w.d2Run(t, d2Src(
			d2Line(ref, holder.MerchantReference, statement.PaymentStatusSucceeded, d2Amount),
			d2Line(ref, parked.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{MismatchKindPayDuplicate: 1})
		mustOnePay(t, ms, MismatchKindPayDuplicate, "provider_reference="+ref, "check=duplicate_line")
		if a := w.attempt(t, holder.ID); a.State != payments.AttemptSucceeded {
			t.Fatalf("holder state %s, want succeeded", a.State)
		}
		w.d2AssertNoMoney(t, d2Parked{attempt: parked, pspRef: "d2-no-such-ref"})
		// Exactly one deposit posting of this provider: the holder's (the
		// fixture's own seed deposit carries no provider).
		var deposits int64
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit' AND provider_id = $2`, w.f.tenantID, payProvA).Scan(&deposits)
		}); err != nil || deposits != 1 {
			t.Fatalf("want exactly one provider deposit posting (the holder's), got %d err=%v", deposits, err)
		}
	})
}

// TestD2_5_InvalidReferencePark is LF (c) 5.
func TestD2_5_InvalidReferencePark(t *testing.T) {
	// A statement line carrying the invalid reference is refused at import:
	// the run fails, nothing is stored, and the failure is audited as P1.
	t.Run("line_with_invalid_reference_refused_run_failed_audited_p1", func(t *testing.T) {
		w := newD2World(t)
		pk, bad := w.parkInvalidRef(t)
		src := d2Src(d2Line(bad, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount))
		before := capturePay(t, w.pool, w.f.tenantID)
		var logBuf strings.Builder
		out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, slog.New(slog.NewTextHandler(&logBuf, nil)),
			w.f.tenantID, time.Now().Add(d2WideBefore), time.Now(), src, PaymentStatementOptions{})
		if !errors.Is(out.Err, ErrPaymentStatementInvalid) {
			t.Fatalf("expected ErrPaymentStatementInvalid, got %v", out.Err)
		}
		after := capturePay(t, w.pool, w.f.tenantID)
		if after.imports != before.imports || after.lines != before.lines || after.runs != before.runs || after.mismatches != before.mismatches {
			t.Fatal("a refused statement must store nothing and record no run")
		}
		if after.ledgerTx != before.ledgerTx || after.debits != after.credits {
			t.Fatal("a refused statement must not touch the ledger")
		}
		if !strings.Contains(logBuf.String(), "P1") {
			t.Errorf("expected a P1 error log, got %s", logBuf.String())
		}
		var n int
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			  AND metadata->>'stream' = 'payment_statement' AND metadata->>'phase' = 'fetch' AND metadata->>'severity' = 'P1'`, w.f.tenantID).Scan(&n)
		}); err != nil || n != 1 {
			t.Fatalf("expected one audited P1 run failure, got n=%d err=%v", n, err)
		}
		w.d2AssertNoMoney(t, pk)
	})
	// A line resolving to the parked attempt by merchant reference with
	// status succeeded is flagged in-run.
	t.Run("merchant_resolved_succeeded_line_is_flagged", func(t *testing.T) {
		w := newD2World(t)
		pk, _ := w.parkInvalidRef(t)
		ms := w.d2Run(t, d2Src(d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)))
		d2Expect(t, ms, map[MismatchKind]int{d2KindCU: 1})
		m := d2CUFor(t, ms, pk.attempt.ID)
		if !strings.Contains(m.ActualValue, "terminal_reason=invalid_provider_reference:") {
			t.Errorf("detail must name the reason: %s", m.ActualValue)
		}
		if strings.Contains(m.ActualValue, "\x01") {
			t.Errorf("the invalid reference must never reach a mismatch row")
		}
		// In-run clearing on the line's reference: a tombstone on it.
		w.deliverReversal(t, "d2-tomb-"+uuid.NewString()[:8], pk.pspRef, d2Amount)
		d2NoCU(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount))), "a tombstone on the line's reference")
		w.d2AssertNoMoney(t, pk)
	})
}

// TestD2_6_ExistingReasonsUnchanged pins that the widened rule leaves the
// other disputed reasons where they were: reversal_tombstone_precedes_success
// (net zero at the PSP) and any unknown reason are never pay_captured_unposted,
// in-run or standing, even with a succeeded line resolving by reference or
// by merchant reference.
func TestD2_6_ExistingReasonsUnchanged(t *testing.T) {
	for _, reason := range []string{payments.TerminalReasonTombstonePrecedesSuccess, "d2_some_other_reason"} {
		t.Run(reason, func(t *testing.T) {
			w := newD2World(t)
			pk := w.parkPoll(t, reason)
			d2NoCU(t, w.d2Run(t, d2Src(d2Line(pk.pspRef, "", statement.PaymentStatusSucceeded, d2Amount))), "matched by reference")
			d2NoCU(t, w.d2Run(t, d2Src(d2Line("d2-x-"+uuid.NewString(), pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount))), "matched by merchant")
			d2NoCU(t, w.d2Run(t, d2Src()), "standing")
			w.d2AssertNoMoney(t, pk)
		})
	}
}
