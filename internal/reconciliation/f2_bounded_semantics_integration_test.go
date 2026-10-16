//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2 (platform-freeze work list): LF-A11-1 (bounded-mode matcher windowing) and LF-A11-2/6 (settle lag, completeness
// horizon, time-ordered pages).

func f2MissingRecords(t *testing.T, w *payWorld, runID uuid.UUID) []string {
	t.Helper()
	var keys []string
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT reconciliation_key FROM reconciliation_mismatches WHERE tenant_id = $1 AND reconciliation_run_id = $2
			AND mismatch_kind = 'pay_missing_provider_record'`, w.f.tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return err
			}
			keys = append(keys, k)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read mismatches: %v", err)
	}
	return keys
}

// THE LF-A11-1 PROBE (ledger-finance review 9.2): three deposits are sent AFTER the cursor, the provider settles
// all three (its lines are dated by those settlements), and only then does the platform record the three successes.
// Under a bound of one line per run, the first (truncated) run covers only the first settlement. The pre-F2 rule
// windowed succeeded attempts by sentAt, so the other two (sent inside the window, settled after its cut) were
// reported pay_missing_provider_record: a false P1 at every truncation boundary, peaking during a backlog. Now no run
// raises it: an attempt is windowed by its own last change, and one whose line an earlier window already imported
// (platform-side latency across a cut) is evidenced by the persisted line.
func TestF2_LFA11_1_TruncationBoundary_SettledAttemptsAreNotFalselyMissing(t *testing.T) {
	w := newPayWorld(t)
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 1}
	if a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA); a.Err != nil {
		t.Fatalf("establish the cursor: %+v", a)
	}
	var deps []payments.PaymentAttempt
	for i := 0; i < 3; i++ {
		deps = append(deps, w.deposit(t, int64(300+i)))
	}
	for _, a := range deps { // the provider settles all three, in order, at distinct instants
		time.Sleep(3 * time.Millisecond)
		w.mockA.Resolve(*a.ProviderReference, payments.OutcomeSucceeded, "", false)
	}
	time.Sleep(3 * time.Millisecond)
	for _, a := range deps { // ... and only then does the platform hear about them (callback latency across the cuts)
		w.applyReceipt(t, payProvA, payments.ReceiptEvidence{EventType: "deposit", ProviderReference: *a.ProviderReference,
			Outcome: payments.OutcomeSucceeded, Amount: a.Amount, AssetCode: a.AssetCode})
	}
	truncated := 0
	for i := 0; i < 8; i++ {
		a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
		if a.Err != nil {
			t.Fatalf("sweep %d: %+v", i, a)
		}
		if a.Run.ID == uuid.Nil {
			break
		}
		if keys := f2MissingRecords(t, w, a.Run.ID); len(keys) != 0 {
			t.Fatalf("sweep %d (truncated=%t): settled attempts must not be reported missing at a truncation boundary: %v", i, a.Truncated, keys)
		}
		if !a.Truncated {
			break
		}
		truncated++
	}
	if truncated < 2 {
		t.Fatalf("the probe needs at least two truncated runs (a cut between settlements), got %d", truncated)
	}
	for _, a := range deps {
		if got := w.attempt(t, a.ID); got.State != payments.AttemptSucceeded {
			t.Fatalf("setup: %s must be succeeded, got %s", a.ID, got.State)
		}
	}
	if refs := importedRefs(t, w.pool, w.f.tenantID, payProvA); len(refs) < 3 {
		t.Fatalf("every settlement line must have been imported: %v", refs)
	}
}

// The persisted-line exemption also resolves by MERCHANT reference (a provider whose lines carry another reference
// than the platform holds): the LF-A11-1 probe with every statement line re-referenced.
func TestF2_LFA11_1_ExemptionByMerchantReference(t *testing.T) {
	w := newPayWorld(t)
	rereferenced := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		for i := range s.Lines {
			s.Lines[i].ProviderReference = "alt-" + s.Lines[i].ProviderReference[:20]
		}
		return s
	}}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: rereferenced, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 1}
	if a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA); a.Err != nil {
		t.Fatalf("establish the cursor: %+v", a)
	}
	var deps []payments.PaymentAttempt
	for i := 0; i < 3; i++ {
		deps = append(deps, w.deposit(t, int64(310+i)))
	}
	for _, a := range deps {
		time.Sleep(3 * time.Millisecond)
		w.mockA.Resolve(*a.ProviderReference, payments.OutcomeSucceeded, "", false)
	}
	time.Sleep(3 * time.Millisecond)
	for _, a := range deps {
		w.applyReceipt(t, payProvA, payments.ReceiptEvidence{EventType: "deposit", ProviderReference: *a.ProviderReference,
			Outcome: payments.OutcomeSucceeded, Amount: a.Amount, AssetCode: a.AssetCode})
	}
	for i := 0; i < 8; i++ {
		a := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
		if a.Err != nil {
			t.Fatalf("sweep %d: %+v", i, a)
		}
		if a.Run.ID == uuid.Nil {
			break
		}
		if keys := f2MissingRecords(t, w, a.Run.ID); len(keys) != 0 {
			t.Fatalf("sweep %d: an attempt whose line (by merchant reference) an earlier window imported must not be missing: %v", i, keys)
		}
		if !a.Truncated {
			break
		}
	}
}

// "Never silently missing": in bounded mode an attempt the platform says succeeded and of which the provider has NO
// record in any statement is still reported, in the window of its last change.
func TestF2_LFA11_1_SucceededWithNoProviderRecordIsStillReported(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 410)
	ref := *a.ProviderReference
	hiding := payDivergentSource{inner: w.srcA, mutate: func(s statement.PaymentStatement) statement.PaymentStatement {
		out := s.Lines[:0]
		for _, l := range s.Lines {
			if l.ProviderReference != ref {
				out = append(out, l)
			}
		}
		s.Lines = out
		return s
	}}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: hiding, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 100}
	scopedSweep(t, w.pool, w.f, cfg)
	w.succeed(t, w.mockA, payProvA, a) // the platform records success; the provider's statement never shows it
	o := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if o.Err != nil || o.Run.ID == uuid.Nil {
		t.Fatalf("sweep: %+v", o)
	}
	keys := f2MissingRecords(t, w, o.Run.ID)
	if len(keys) != 1 || !strings.Contains(keys[0], a.ID.String()) {
		t.Fatalf("a succeeded attempt with no provider record must be reported in the window of its success: %v", keys)
	}
}

// The persisted-line exemption needs a line that records the SUCCESS: a provider that only ever listed the attempt
// as pending (an earlier import) while the platform recorded success is still reported.
func TestF2_LFA11_1_PendingOnlyAtTheProviderIsStillReported(t *testing.T) {
	w := newPayWorld(t)
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 100}
	scopedSweep(t, w.pool, w.f, cfg)
	a := w.deposit(t, 430)
	scopedSweep(t, w.pool, w.f, cfg) // imports the provider's PENDING line
	if refs := importedRefs(t, w.pool, w.f.tenantID, payProvA); refs[*a.ProviderReference] == 0 {
		t.Fatal("setup: the pending line must be imported")
	}
	// The platform records success; the provider's record stays pending (no new line is dated after the cursor).
	w.applyReceipt(t, payProvA, payments.ReceiptEvidence{EventType: "deposit", ProviderReference: *a.ProviderReference,
		Outcome: payments.OutcomeSucceeded, Amount: a.Amount, AssetCode: a.AssetCode})
	o := outcomeFor(t, scopedSweep(t, w.pool, w.f, cfg), payProvA)
	if o.Err != nil || o.Run.ID == uuid.Nil {
		t.Fatalf("sweep: %+v", o)
	}
	if keys := f2MissingRecords(t, w, o.Run.ID); len(keys) != 1 || !strings.Contains(keys[0], a.ID.String()) {
		t.Fatalf("a success the provider never recorded must be reported even though a pending line exists: %v", keys)
	}
}

// The legacy (creation-dated, non-continuation) mode is unchanged: still windowed by sentAt, no persisted-line
// exemption (its pins stay). A succeeded attempt sent inside the window and absent from the statement is reported.
func TestF2_LFA11_1_LegacyModeStillWindowsBySentAt(t *testing.T) {
	w := newPayWorld(t)
	a := w.deposit(t, 420)
	w.succeed(t, w.mockA, payProvA, a)
	empty := payFixedSource{provider: payProvA, stmt: wideCoverage()}
	_, ms := w.run(t, empty, PaymentStatementOptions{})
	found := false
	for _, m := range ms {
		if m.MismatchKind == MismatchKindPayMissingProviderRecord && strings.Contains(m.ReconciliationKey, a.ID.String()) {
			found = true
		}
	}
	if !found {
		t.Fatalf("legacy mode must still report the missing provider record:\n%s", renderMismatches(ms))
	}
}

// lagSource declares a settle lag (statement.SettleLagDeclarer) and states its fetch time as CoverageEnd unless
// honour is set.
type lagSource struct {
	*recSource
	lag    time.Duration
	honour bool
}

func (s lagSource) StatementSettleLag() time.Duration { return s.lag }

// LF-A11-2: the request carries PeriodEnd = now - settle lag, and a statement claiming coverage beyond it is refused
// with nothing stored (the cursor never passes the provider's completeness horizon).
func TestF2_LFA11_2_SettleLag_HorizonRequestedAndEnforced(t *testing.T) {
	w := newPayWorld(t)
	const lag = 10 * time.Minute
	honest := lagSource{lag: lag, honour: true}
	honest.recSource = &recSource{provider: payProvA, fn: func(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		start := req.ContinueFrom
		if start.IsZero() {
			start = time.Now().Add(-time.Hour).UTC()
		}
		return statement.PaymentStatement{CoverageStart: start, CoverageEnd: req.PeriodEnd}, nil
	}}
	before := time.Now().UTC()
	if a := outcomeFor(t, scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: honest, payProvB: w.srcB}, AllowSynthetic: true}), payProvA); a.Err != nil {
		t.Fatalf("an honest lagged source: %+v", a)
	}
	reqs := honest.requests()
	if len(reqs) != 1 || !reqs[0].Continuation || reqs[0].PeriodEnd.After(before.Add(-lag).Add(time.Second)) || reqs[0].PeriodEnd.Before(before.Add(-lag).Add(-time.Minute)) {
		t.Fatalf("the request must carry the horizon now - lag (%s): %+v", before.Add(-lag), reqs)
	}
	c := mustCursor(t, w.pool, w.f.tenantID, payProvA)
	if c.After(time.Now().Add(-lag)) {
		t.Fatalf("the cursor %s passed now - lag", c)
	}
	runsBefore := len(paymentRuns(t, w.pool, w.f.tenantID, payProvA))
	greedy := lagSource{lag: lag}
	greedy.recSource = &recSource{provider: payProvA, fn: func(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		return statement.PaymentStatement{CoverageStart: req.ContinueFrom, CoverageEnd: time.Now().UTC()}, nil // ignores the horizon
	}}
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: greedy, payProvB: w.srcB}, AllowSynthetic: true}), payProvA)
	if !errors.Is(a.Err, ErrPaymentStatementCoverageGap) {
		t.Fatalf("a CoverageEnd beyond now - lag must be refused: %+v", a)
	}
	if got := mustCursor(t, w.pool, w.f.tenantID, payProvA); !got.Equal(c) || len(paymentRuns(t, w.pool, w.f.tenantID, payProvA)) != runsBefore {
		t.Fatal("a refused statement records nothing and leaves the cursor")
	}
}

// A declared lag outside [0, MaxStatementSettleLag] is a source error: the run fails (P1), nothing is fetched.
func TestF2_LFA11_2_InvalidDeclaredLagFailsTheRun(t *testing.T) {
	for _, lag := range []time.Duration{-time.Second, statement.MaxStatementSettleLag + time.Second} {
		w := newPayWorld(t)
		bad := lagSource{lag: lag, recSource: &recSource{provider: payProvA, fn: contiguousEmpty}}
		a := outcomeFor(t, scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: bad, payProvB: w.srcB}, AllowSynthetic: true}), payProvA)
		if !errors.Is(a.Err, ErrPaymentStatementInvalid) || len(bad.requests()) != 0 {
			t.Fatalf("lag %s: want a source failure with no fetch, got %+v (%d requests)", lag, a, len(bad.requests()))
		}
		if err := ValidatePaymentSettleLag(bad); err == nil {
			t.Fatalf("lag %s: the registry check must refuse it", lag)
		}
	}
}

// LF-A11-6: a truncated page that is not in time order (a source that pages by reference, say) is refused with
// nothing stored; the MOCK's bounded pages are in time order and capped at the horizon.
func TestF2_LFA11_6_OutOfOrderPageRefused_MockPagesInTimeOrder(t *testing.T) {
	w := newPayWorld(t)
	t0 := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Microsecond)
	unordered := &recSource{provider: payProvA, fn: func(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		l1 := payLineFor(payProvA, "f2-ooo-b", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1)
		l2 := payLineFor(payProvA, "f2-ooo-a", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1)
		l1.OccurredAt, l2.OccurredAt = t0.Add(2*time.Second), t0.Add(time.Second) // listed newest first
		return statement.PaymentStatement{CoverageStart: t0.Add(-time.Minute), CoverageEnd: t0.Add(3 * time.Second), Truncated: true,
			Lines: []statement.PaymentStatementLine{l1, l2}}, nil
	}}
	a := outcomeFor(t, scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: unordered, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 2}), payProvA)
	if !errors.Is(a.Err, ErrPaymentStatementOutOfOrder) {
		t.Fatalf("an out-of-order page must be refused: %+v", a)
	}
	if refs := importedRefs(t, w.pool, w.f.tenantID, payProvA); len(refs) != 0 {
		t.Fatalf("nothing stored: %v", refs)
	}

	// The MOCK: twelve deposits whose references sort differently from their creation time ("<p>-10-..." sorts before
	// "<p>-2-..."); a bounded fetch lists them by time and never past the requested horizon.
	const n = 12
	for i := 0; i < n; i++ {
		w.deposit(t, int64(500+i))
		time.Sleep(2 * time.Millisecond)
	}
	horizon := time.Now().UTC().Truncate(time.Microsecond)
	late := w.deposit(t, 599) // after the horizon: must not be listed
	st, err := w.srcA.Fetch(context.Background(), statement.PaymentFetchRequest{TenantID: w.f.tenantID, ProviderID: payProvA,
		PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: horizon, Continuation: true})
	if err != nil {
		t.Fatalf("mock fetch: %v", err)
	}
	if st.CoverageEnd.After(horizon) || len(st.Lines) != n {
		t.Fatalf("the MOCK must cap at the horizon %s: end %s, %d lines", horizon, st.CoverageEnd, len(st.Lines))
	}
	for i := 1; i < len(st.Lines); i++ {
		if st.Lines[i].OccurredAt.Before(st.Lines[i-1].OccurredAt) {
			t.Fatalf("bounded MOCK lines must be in time order: %+v", st.Lines)
		}
	}
	for _, l := range st.Lines {
		if l.ProviderReference == *late.ProviderReference {
			t.Fatal("a line dated after the horizon was listed")
		}
	}
}
