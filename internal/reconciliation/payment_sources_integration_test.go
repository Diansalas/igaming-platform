//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// A11: PAY-K3-STATEMENT-SOURCE-WIRING-1 (per-(tenant, provider) source resolution, missing source = failed run +
// durable alert + unreconciled gauge, never clean) and R3-OBS-BOUND-1 (bounded, resumable fetch: truncated
// status + alert, persisted cursor, no skipping, no double counting).

type mapResolver map[string]statement.PaymentStatementSource

func (m mapResolver) SourceFor(p string) (statement.PaymentStatementSource, bool) {
	s, ok := m[p]
	return s, ok
}

// recSource is a Synthetic source that records every request it receives and states whatever fn returns.
type recSource struct {
	provider string
	mu       sync.Mutex
	reqs     []statement.PaymentFetchRequest
	fn       func(n int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error)
}

func (*recSource) SyntheticComponent()  {}
func (*recSource) Label() string        { return "MOCK recording payment statement (test-only)" }
func (s *recSource) ProviderID() string { return s.provider }
func (s *recSource) Fetch(_ context.Context, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	n := len(s.reqs)
	s.mu.Unlock()
	if s.fn == nil {
		return statement.PaymentStatement{CoverageStart: time.Now().Add(-time.Hour).UTC(), CoverageEnd: time.Now().UTC()}, nil
	}
	return s.fn(n, req)
}
func (s *recSource) requests() []statement.PaymentFetchRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]statement.PaymentFetchRequest(nil), s.reqs...)
}

// contiguousEmpty states an empty statement that continues exactly from the cursor (or the last hour).
func contiguousEmpty(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	start := req.ContinueFrom
	if start.IsZero() {
		start = time.Now().Add(-time.Hour).UTC()
	}
	return statement.PaymentStatement{CoverageStart: start, CoverageEnd: time.Now().UTC()}, nil
}

func scopedSweep(t *testing.T, pool *db.Pool, f fixture, cfg PaymentSweepConfig) SweepOutcome {
	t.Helper()
	now := time.Now()
	outs, err := RunSweepTenantsScoped(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}, cfg)
	if err != nil {
		t.Fatalf("RunSweepTenantsScoped: %v", err)
	}
	return findOutcome(t, outs, f)
}

func outcomeFor(t *testing.T, o SweepOutcome, provider string) StreamOutcome {
	t.Helper()
	for _, ps := range o.PaymentStatement {
		if ps.ProviderID == provider {
			return ps
		}
	}
	t.Fatalf("no payment_statement outcome for provider %q in %+v", provider, o.PaymentStatement)
	return StreamOutcome{}
}

type runRow struct {
	periodStart, periodEnd time.Time
	coverageEnd            *time.Time
	truncated              *bool
	status                 string
}

func paymentRuns(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider string) []runRow {
	t.Helper()
	var out []runRow
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT period_start, period_end, coverage_end, window_truncated, status
			FROM reconciliation_runs WHERE tenant_id = $1 AND stream = 'payment_statement' AND provider_id = $2 ORDER BY run_at, id`, tenantID, provider)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r runRow
			if err := rows.Scan(&r.periodStart, &r.periodEnd, &r.coverageEnd, &r.truncated, &r.status); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read runs: %v", err)
	}
	return out
}

func importedRefs(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider string) map[string]int {
	t.Helper()
	refs := map[string]int{}
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT provider_reference, count(*) FROM payment_statement_lines WHERE tenant_id = $1 AND provider_id = $2 GROUP BY 1`, tenantID, provider)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref string
			var n int
			if err := rows.Scan(&ref, &n); err != nil {
				return err
			}
			refs[ref] = n
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read imported lines: %v", err)
	}
	return refs
}

func pairState(tenantID uuid.UUID, provider string) (reason string, present bool) {
	defaultTracker.mu.Lock()
	defer defaultTracker.mu.Unlock()
	st := defaultTracker.states[runKey{tenant: tenantID, stream: string(StreamPaymentStatement), source: provider}]
	if st == nil {
		return "", false
	}
	return st.unreconciled, true
}

func countAudit(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, provider string) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND metadata->>'provider_id' = $3`,
			tenantID, action, provider).Scan(&n)
	}); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------------------------------------
// Source resolution per (tenant, provider).

func TestScoped_ResolvesTheSourceOfEachConfiguredProvider(t *testing.T) {
	w := newPayWorld(t)
	w.deposit(t, 1000)
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true}
	o := scopedSweep(t, w.pool, w.f, cfg)
	if len(o.PaymentStatement) != 2 || o.PaymentStatement[0].ProviderID != payProvA || o.PaymentStatement[1].ProviderID != payProvB {
		t.Fatalf("want one outcome per configured provider in id order, got %+v", o.PaymentStatement)
	}
	for _, ps := range o.PaymentStatement {
		if ps.Err != nil || ps.NotReconciled != "" || ps.Truncated {
			t.Fatalf("a registered source must reconcile its provider: %+v", ps)
		}
		runs := paymentRuns(t, w.pool, w.f.tenantID, ps.ProviderID)
		if len(runs) != 1 || runs[0].coverageEnd == nil || runs[0].truncated == nil || *runs[0].truncated {
			t.Fatalf("%s: the run must carry its provider, coverage_end (the cursor) and window_truncated=false: %+v", ps.ProviderID, runs)
		}
		if reason, ok := pairState(w.f.tenantID, ps.ProviderID); !ok || reason != "" {
			t.Fatalf("%s: a reconciled pair must not be listed unreconciled (present=%v reason=%q)", ps.ProviderID, ok, reason)
		}
	}
	// The statement of A carries A's deposit; B's carries nothing: each source was asked about its own provider only.
	if refs := importedRefs(t, w.pool, w.f.tenantID, payProvA); len(refs) != 1 {
		t.Fatalf("A's statement must hold A's one deposit, got %v", refs)
	}
	if refs := importedRefs(t, w.pool, w.f.tenantID, payProvB); len(refs) != 0 {
		t.Fatalf("B's statement must hold nothing, got %v", refs)
	}
}

func TestScoped_ConfiguredProviderWithoutSource_IsAFailedRunAndADurableAlert_NeverClean(t *testing.T) {
	w := newPayWorld(t)
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA}, AllowSynthetic: true} // B is configured but has no source
	o := scopedSweep(t, w.pool, w.f, cfg)

	b := outcomeFor(t, o, payProvB)
	if !errors.Is(b.Err, ErrPaymentStatementSourceMissing) || b.NotReconciled != NotReconciledNoSource || b.Run.ID != uuid.Nil {
		t.Fatalf("B must be reported as not reconciled with no run: %+v", b)
	}
	if a := outcomeFor(t, o, payProvA); a.Err != nil || a.NotReconciled != "" {
		t.Fatalf("A (with a source) must still reconcile: %+v", a)
	}
	if runs := paymentRuns(t, w.pool, w.f.tenantID, payProvB); len(runs) != 0 {
		t.Fatalf("a provider with no source must record NO reconciliation run (a run row would read as clean): %+v", runs)
	}
	if n := countAudit(t, w.pool, w.f.tenantID, "reconciliation.sweep_run_failed", payProvB); n != 1 {
		t.Fatalf("exactly one audited failed run for B, got %d", n)
	}
	rows := alertinject.Find(alertinject.WaitForKind(t, w.pool, w.f.tenantID, "reconciliation.run_failed", 1), "reconciliation.run_failed")
	found := false
	for _, r := range rows {
		if r.Discriminator == "stream:payment_statement:provider:"+payProvB+":no_statement_source" && r.Severity == "p1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a durable P1 reconciliation.run_failed (phase no_statement_source) for B must exist, got %+v", rows)
	}
	if reason, ok := pairState(w.f.tenantID, payProvB); !ok || reason != NotReconciledNoSource {
		t.Fatalf("B must be visible in the unreconciled gauge as no_source, got present=%v reason=%q", ok, reason)
	}
	// A second sweep attaches an occurrence to the SAME open alert (no alert flood).
	scopedSweep(t, w.pool, w.f, cfg)
	for _, r := range alertinject.Find(alertinject.ForSubject(t, w.pool, w.f.tenantID), "reconciliation.run_failed") {
		if r.Discriminator == "stream:payment_statement:provider:"+payProvB+":no_statement_source" && r.Occurrences < 2 {
			t.Fatalf("a persisting condition must be ONE alert with growing occurrences, got %+v", r)
		}
	}
}

func TestScoped_NoResolver_NilAndTypedNilSource_AllFailClosed(t *testing.T) {
	w := newPayWorld(t)
	var typedNil *payments.MockStatementSource
	cases := map[string]PaymentSweepConfig{
		"nil resolver":    {Resolver: nil, AllowSynthetic: true},
		"empty resolver":  {Resolver: mapResolver{}, AllowSynthetic: true},
		"nil source":      {Resolver: mapResolver{payProvA: nil, payProvB: nil}, AllowSynthetic: true},
		"typed nil":       {Resolver: mapResolver{payProvA: typedNil, payProvB: typedNil}, AllowSynthetic: true},
		"both disallowed": {Resolver: mapResolver{}, AllowSynthetic: false},
	}
	for name, cfg := range cases {
		o := scopedSweep(t, w.pool, w.f, cfg)
		if len(o.PaymentStatement) != 2 {
			t.Fatalf("%s: both configured providers must be reported, got %+v", name, o.PaymentStatement)
		}
		for _, ps := range o.PaymentStatement {
			if ps.Err == nil || ps.NotReconciled != NotReconciledNoSource || ps.Run.ID != uuid.Nil {
				t.Fatalf("%s: %s must be not reconciled (no source), got %+v", name, ps.ProviderID, ps)
			}
		}
	}
	for _, p := range []string{payProvA, payProvB} {
		if runs := paymentRuns(t, w.pool, w.f.tenantID, p); len(runs) != 0 {
			t.Fatalf("%s: no source must never produce a run row: %+v", p, runs)
		}
	}
}

// A Synthetic (MOCK) source is ineligible unless the config allows it (production: never): the source is not even
// called, the pair is reported as source_not_eligible, and no run row exists.
func TestScoped_SyntheticSourceIsIneligibleWhereMocksAreNotAllowed_AndIsNeverFetched(t *testing.T) {
	w := newPayWorld(t)
	spyA := &recSource{provider: payProvA, fn: contiguousEmpty}
	spyB := &recSource{provider: payProvB, fn: contiguousEmpty}
	o := scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: spyA, payProvB: spyB}, AllowSynthetic: false})
	for _, ps := range o.PaymentStatement {
		if !errors.Is(ps.Err, ErrPaymentStatementSourceMissing) || ps.NotReconciled != NotReconciledIneligible {
			t.Fatalf("%s: a MOCK source where mocks are not allowed must be ineligible, got %+v", ps.ProviderID, ps)
		}
	}
	if len(spyA.requests())+len(spyB.requests()) != 0 {
		t.Fatal("an ineligible source must never be fetched (no credential resolution, no provider call)")
	}
	rows := alertinject.WaitForKind(t, w.pool, w.f.tenantID, "reconciliation.run_failed", 2)
	got := map[string]bool{}
	for _, r := range alertinject.Find(rows, "reconciliation.run_failed") {
		got[r.Discriminator] = true
	}
	for _, p := range []string{payProvA, payProvB} {
		if !got["stream:payment_statement:provider:"+p+":source_not_eligible"] {
			t.Fatalf("missing the source_not_eligible alert for %s: %v", p, got)
		}
	}
}

func TestScoped_SourceForAnotherProviderCanNeverVouch(t *testing.T) {
	w := newPayWorld(t)
	// The resolver maps A to B's source (a wiring bug): A must not be reconciled with B's statement.
	o := scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcB, payProvB: w.srcB}, AllowSynthetic: true})
	if a := outcomeFor(t, o, payProvA); a.NotReconciled != NotReconciledIneligible || a.Err == nil {
		t.Fatalf("A must be ineligible when its 'source' speaks for B: %+v", a)
	}
	if b := outcomeFor(t, o, payProvB); b.Err != nil {
		t.Fatalf("B keeps its own source: %+v", b)
	}
}

// Per-tenant isolation: a tenant is reconciled only for ITS providers, with a request carrying ITS tenant id, and
// one tenant's missing source never touches another tenant's outcome or cursor.
func TestScoped_PerTenantIsolation(t *testing.T) {
	w1 := newPayWorld(t)
	f2 := seedFixture(t, w1.pool)
	w2 := &payWorld{pool: w1.pool, f: f2, mockA: w1.mockA, mockB: w1.mockB}
	w2.registerCapability(t, w1.mockA, 10) // tenant 2 has ONLY provider A

	spyA := &recSource{provider: payProvA, fn: contiguousEmpty}
	spyB := &recSource{provider: payProvB, fn: contiguousEmpty}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: spyA, payProvB: spyB}, AllowSynthetic: true}

	now := time.Now()
	outs, err := RunSweepTenantsScoped(context.Background(), w1.pool, nil, []uuid.UUID{w1.f.tenantID, f2.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	o1, o2 := findOutcome(t, outs, w1.f), findOutcome(t, outs, f2)
	if len(o1.PaymentStatement) != 2 || len(o2.PaymentStatement) != 1 || o2.PaymentStatement[0].ProviderID != payProvA {
		t.Fatalf("tenant 1 has providers A+B, tenant 2 only A: %+v / %+v", o1.PaymentStatement, o2.PaymentStatement)
	}
	// B's source was asked about tenant 1 only; A's about both, each with its own tenant id.
	for _, r := range spyB.requests() {
		if r.TenantID != w1.f.tenantID {
			t.Fatalf("B's source must never be fetched for tenant 2: %+v", r)
		}
	}
	seen := map[uuid.UUID]int{}
	for _, r := range spyA.requests() {
		seen[r.TenantID]++
	}
	if seen[w1.f.tenantID] != 1 || seen[f2.tenantID] != 1 || len(seen) != 2 {
		t.Fatalf("A's source must be fetched once per tenant with that tenant's id: %v", seen)
	}

	// Now only tenant 1 loses B's source: tenant 2's result and cursor are untouched.
	cfg2 := PaymentSweepConfig{Resolver: mapResolver{payProvA: spyA}, AllowSynthetic: true}
	outs, err = RunSweepTenantsScoped(context.Background(), w1.pool, nil, []uuid.UUID{w1.f.tenantID, f2.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if b := outcomeFor(t, findOutcome(t, outs, w1.f), payProvB); b.NotReconciled != NotReconciledNoSource {
		t.Fatalf("tenant 1 / B must be not reconciled: %+v", b)
	}
	if o := findOutcome(t, outs, f2); len(o.PaymentStatement) != 1 || o.PaymentStatement[0].Err != nil {
		t.Fatalf("tenant 2 must be unaffected by tenant 1's missing source: %+v", o.PaymentStatement)
	}
	if runs := paymentRuns(t, w1.pool, f2.tenantID, payProvB); len(runs) != 0 {
		t.Fatalf("tenant 2 has no provider B: %+v", runs)
	}
	if c1, c2 := mustCursor(t, w1.pool, w1.f.tenantID, payProvA), mustCursor(t, w1.pool, f2.tenantID, payProvA); c1.IsZero() || c2.IsZero() || c1.Equal(c2) {
		t.Fatalf("each tenant keeps its own cursor: %v vs %v", c1, c2)
	}
}

func TestScoped_ProviderListingFailureIsNotReconciledAndAlerted(t *testing.T) {
	w := newPayWorld(t)
	orig := listConfiguredPaymentProviders
	listConfiguredPaymentProviders = func(context.Context, *db.Pool, uuid.UUID) ([]string, error) {
		return nil, errors.New("forced listing failure")
	}
	t.Cleanup(func() { listConfiguredPaymentProviders = orig })
	o := scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true})
	if len(o.PaymentStatement) != 1 || o.PaymentStatement[0].NotReconciled != NotReconciledScope || o.PaymentStatement[0].Err == nil {
		t.Fatalf("a failed provider listing must be one failed, not-reconciled outcome: %+v", o.PaymentStatement)
	}
	rows := alertinject.WaitForKind(t, w.pool, w.f.tenantID, "reconciliation.run_failed", 1)
	ok := false
	for _, r := range alertinject.Find(rows, "reconciliation.run_failed") {
		if r.Discriminator == "stream:payment_statement:scope" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("want the scope alert, got %+v", rows)
	}
}

// A closed tenant is observed with the same rule: a configured provider with no source is a failed, flagged run.
func TestScoped_NonActiveTenantObservation_MissingSourceIsVisible(t *testing.T) {
	w := newPayWorld(t)
	if err := launchfix.TrySetTenantStatusOn(context.Background(), t, w.pool, w.f.tenantID, "closed"); err != nil {
		t.Fatalf("close tenant: %v", err)
	}
	o := scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA}, AllowSynthetic: true})
	if !o.ObservationOnly || o.TenantStatus != "closed" {
		t.Fatalf("expected an observation outcome: %+v", o)
	}
	if b := outcomeFor(t, o, payProvB); b.NotReconciled != NotReconciledNoSource || b.Err == nil {
		t.Fatalf("closed tenant / B must be not reconciled: %+v", b)
	}
	if a := outcomeFor(t, o, payProvA); a.Err != nil {
		t.Fatalf("closed tenant / A reconciles with its source: %+v", a)
	}
	var meta string
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'non_active_tenant_observation' FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed' AND metadata->>'provider_id' = $2`,
			w.f.tenantID, payProvB).Scan(&meta)
	}); err != nil || meta != "true" {
		t.Fatalf("the failed-run audit must carry the observation flag: %q %v", meta, err)
	}
}

// ---------------------------------------------------------------------------------------------------------
// R3-OBS-BOUND-1.

func mustCursor(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider string) time.Time {
	t.Helper()
	c, err := readPaymentCursor(context.Background(), pool, tenantID, provider)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBound_TruncatedRunContinuesFromTheCursor_NoSkippingNoDoubleCounting(t *testing.T) {
	w := newPayWorld(t)
	const n = 7
	for i := 0; i < n; i++ {
		w.deposit(t, int64(1000+i))
	}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 3}

	var truncatedRuns int
	var prevEnd time.Time
	for i := 0; i < 10; i++ {
		o := scopedSweep(t, w.pool, w.f, cfg)
		a := outcomeFor(t, o, payProvA)
		if a.Err != nil || a.Skipped {
			t.Fatalf("sweep %d: %+v", i, a)
		}
		runs := paymentRuns(t, w.pool, w.f.tenantID, payProvA)
		last := runs[len(runs)-1]
		if last.coverageEnd == nil || !last.coverageEnd.Equal(last.periodEnd) {
			t.Fatalf("sweep %d: run coverage %+v", i, last)
		}
		if !prevEnd.IsZero() && !last.periodStart.Equal(prevEnd) {
			t.Fatalf("sweep %d: window [%s, ...) is not contiguous with the previous run's end %s (a gap skips lines, an overlap double counts)",
				i, last.periodStart, prevEnd)
		}
		prevEnd = last.periodEnd
		if got := mustCursor(t, w.pool, w.f.tenantID, payProvA); !got.Equal(last.coverageEnd.UTC().Truncate(time.Microsecond)) {
			t.Fatalf("sweep %d: cursor %s != last run coverage_end %s", i, got, last.coverageEnd)
		}
		if a.Truncated {
			truncatedRuns++
			if a.NotReconciled != NotReconciledTruncated || last.truncated == nil || !*last.truncated {
				t.Fatalf("sweep %d: a truncated outcome must say so everywhere: %+v / %+v", i, a, last)
			}
			if reason, _ := pairState(w.f.tenantID, payProvA); reason != NotReconciledTruncated {
				t.Fatalf("sweep %d: the pair must read as truncated in the gauge, got %q", i, reason)
			}
			continue
		}
		break // a complete run: caught up
	}
	if truncatedRuns != 2 {
		t.Fatalf("7 lines under a bound of 3 must take 2 truncated runs and then a complete one, got %d truncated", truncatedRuns)
	}
	refs := importedRefs(t, w.pool, w.f.tenantID, payProvA)
	if len(refs) != n {
		t.Fatalf("every one of the %d lines must be imported exactly once across the runs, got %d distinct: %v", n, len(refs), refs)
	}
	for ref, c := range refs {
		if c != 1 {
			t.Fatalf("line %s imported %d times (double counted)", ref, c)
		}
	}
	if reason, _ := pairState(w.f.tenantID, payProvA); reason != "" {
		t.Fatalf("after catching up the pair is reconciled, got %q", reason)
	}
	// The truncated runs raised ONE durable alert (phase truncated) that grew.
	found := false
	for _, r := range alertinject.Find(alertinject.WaitForKind(t, w.pool, w.f.tenantID, "reconciliation.run_failed", 1), "reconciliation.run_failed") {
		if r.Discriminator == "stream:payment_statement:provider:"+payProvA+":truncated" {
			found = true
			if r.Occurrences != 2 || r.Severity != "p1" {
				t.Fatalf("two truncated runs must be one P1 alert with 2 occurrences: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("no durable truncation alert")
	}
	// Caught up and nothing new: the next run is empty, not truncated, and continues from the cursor.
	o := scopedSweep(t, w.pool, w.f, cfg)
	if a := outcomeFor(t, o, payProvA); a.Err != nil || a.Truncated {
		t.Fatalf("idle sweep: %+v", a)
	}
	if got := importedRefs(t, w.pool, w.f.tenantID, payProvA); len(got) != n {
		t.Fatalf("an idle sweep must import nothing new: %v", got)
	}
}

func TestBound_HittingTheBoundIsNeverACleanClaim(t *testing.T) {
	w := newPayWorld(t)
	for i := 0; i < 4; i++ {
		w.deposit(t, int64(500+i))
	}
	before := observedLastSuccess(defaultTracker, w.f.tenantID, payProvA)
	o := scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 2})
	a := outcomeFor(t, o, payProvA)
	if !a.Truncated || a.NotReconciled != NotReconciledTruncated {
		t.Fatalf("4 lines under a bound of 2 must be truncated: %+v", a)
	}
	if after := observedLastSuccess(defaultTracker, w.f.tenantID, payProvA); after != before || after != 0 {
		t.Fatalf("a truncated run must not advance the pair's freshness: before=%d after=%d", before, after)
	}
}

func observedLastSuccess(tr *runTracker, tenant uuid.UUID, provider string) int64 {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if st := tr.states[runKey{tenant: tenant, stream: string(StreamPaymentStatement), source: provider}]; st != nil {
		return st.lastSuccessUnix
	}
	return 0
}

func TestBound_SourceThatIgnoresTheBoundIsRefused_NothingStored(t *testing.T) {
	w := newPayWorld(t)
	now := time.Now().UTC()
	src := &recSource{provider: payProvA, fn: func(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		st := statement.PaymentStatement{CoverageStart: now.Add(-time.Hour), CoverageEnd: now}
		for i := 0; i < 5; i++ { // 5 lines against a bound of 3, not flagged Truncated
			st.Lines = append(st.Lines, payLineFor(payProvA, "ign-"+uuid.NewString()[:8], "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1))
		}
		return st, nil
	}}
	o := scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 3})
	a := outcomeFor(t, o, payProvA)
	if !errors.Is(a.Err, ErrPaymentStatementTooLarge) {
		t.Fatalf("a source exceeding the per-run bound must be refused: %+v", a)
	}
	if refs := importedRefs(t, w.pool, w.f.tenantID, payProvA); len(refs) != 0 {
		t.Fatalf("a refused statement stores nothing, got %v", refs)
	}
	if runs := paymentRuns(t, w.pool, w.f.tenantID, payProvA); len(runs) != 0 {
		t.Fatalf("a refused statement records no run: %+v", runs)
	}
	if reason, _ := pairState(w.f.tenantID, payProvA); reason != "run_failed" {
		t.Fatalf("a failed fetch is visible as run_failed, got %q", reason)
	}
}

func TestBound_NonContiguousCoverageIsRefusedAndTheCursorStays(t *testing.T) {
	w := newPayWorld(t)
	first := &recSource{provider: payProvA, fn: contiguousEmpty}
	scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: first, payProvB: w.srcB}, AllowSynthetic: true})
	c1 := mustCursor(t, w.pool, w.f.tenantID, payProvA)
	if c1.IsZero() {
		t.Fatal("the first run must set the cursor")
	}
	// A source that starts LATER than the cursor skips [cursor, start); one that starts EARLIER overlaps.
	for name, shift := range map[string]time.Duration{"gap": 10 * time.Microsecond, "overlap": -10 * time.Microsecond} {
		bad := &recSource{provider: payProvA, fn: func(_ int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
			return statement.PaymentStatement{CoverageStart: req.ContinueFrom.Add(shift), CoverageEnd: time.Now().UTC()}, nil
		}}
		o := scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: bad, payProvB: w.srcB}, AllowSynthetic: true})
		if a := outcomeFor(t, o, payProvA); !errors.Is(a.Err, ErrPaymentStatementCoverageGap) {
			t.Fatalf("%s: want the coverage refusal, got %+v", name, a)
		}
		if got := mustCursor(t, w.pool, w.f.tenantID, payProvA); !got.Equal(c1) {
			t.Fatalf("%s: a refused statement must leave the cursor at %s, got %s", name, c1, got)
		}
	}
	if runs := paymentRuns(t, w.pool, w.f.tenantID, payProvA); len(runs) != 1 {
		t.Fatalf("only the first run may exist: %+v", runs)
	}
}

func TestBound_FailedFetchDoesNotMoveTheCursor_NextRunRetriesTheSameWindow(t *testing.T) {
	w := newPayWorld(t)
	src := &recSource{provider: payProvA, fn: func(n int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		if n == 2 {
			return statement.PaymentStatement{}, errors.New("provider unavailable")
		}
		return contiguousEmpty(n, req)
	}}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: src, payProvB: w.srcB}, AllowSynthetic: true}
	scopedSweep(t, w.pool, w.f, cfg)
	c1 := mustCursor(t, w.pool, w.f.tenantID, payProvA)
	if o := scopedSweep(t, w.pool, w.f, cfg); outcomeFor(t, o, payProvA).Err == nil {
		t.Fatal("the second fetch was forced to fail")
	}
	if got := mustCursor(t, w.pool, w.f.tenantID, payProvA); !got.Equal(c1) {
		t.Fatalf("a failed run must not move the cursor: %s -> %s", c1, got)
	}
	scopedSweep(t, w.pool, w.f, cfg)
	reqs := src.requests()
	if len(reqs) != 3 || !reqs[1].ContinueFrom.Equal(c1) || !reqs[2].ContinueFrom.Equal(c1) {
		t.Fatalf("the failed window must be requested again from the same cursor %s: %+v", c1, reqs)
	}
}

// Another instance commits a run for the same (tenant, provider) between this run's cursor read and its match: the
// match (under the advisory lock) sees a moved cursor and SKIPS rather than covering the window twice.
func TestBound_StaleCursorUnderTheLock_IsSkippedNotDoubleCovered(t *testing.T) {
	w := newPayWorld(t)
	inner := payFixedSource{provider: payProvA, stmt: wideCoverage()}
	src := &recSource{provider: payProvA}
	src.fn = func(n int, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
		if n == 1 {
			// "the other instance": a complete run for the same pair commits while this fetch is in flight.
			if o := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), inner,
				PaymentStatementOptions{Continuation: true}); o.Err != nil {
				t.Errorf("the concurrent run must succeed: %+v", o)
			}
		}
		return contiguousEmpty(n, req)
	}
	out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src,
		PaymentStatementOptions{Continuation: true})
	if out.Err != nil || !out.Skipped || out.Run.ID != uuid.Nil {
		t.Fatalf("a run whose cursor went stale must be skipped, not failed and not recorded: %+v", out)
	}
	if runs := paymentRuns(t, w.pool, w.f.tenantID, payProvA); len(runs) != 1 {
		t.Fatalf("exactly the concurrent run may exist, got %d: %+v", len(runs), runs)
	}
}

func TestBound_ForgedCursorsAreIgnored(t *testing.T) {
	w := newPayWorld(t)
	scopedSweep(t, w.pool, w.f, PaymentSweepConfig{Resolver: mapResolver{payProvA: &recSource{provider: payProvA, fn: contiguousEmpty}, payProvB: w.srcB}, AllowSynthetic: true})
	real := mustCursor(t, w.pool, w.f.tenantID, payProvA)
	// The runtime role may INSERT a run row (0152 accepts that): a coverage_end far in the future must not move the cursor.
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated)
			VALUES ($1, 'payment_statement', now() - interval '1 hour', now(), 'clean', now() + interval '1 day', $2, false)`, w.f.tenantID, payProvA)
		return err
	}); err != nil {
		t.Fatalf("seed forged run: %v", err)
	}
	if got := mustCursor(t, w.pool, w.f.tenantID, payProvA); !got.Equal(real) {
		t.Fatalf("a forged far-future coverage_end must be ignored: %s vs %s", got, real)
	}
	// A forged run for ANOTHER provider never moves this provider's cursor, nor another tenant's.
	if got := mustCursor(t, w.pool, w.f.tenantID, payProvB); got.IsZero() == false && got.Equal(real) {
		t.Fatalf("provider cursors are independent: B=%s", got)
	}
	other := seedFixture(t, w.pool)
	if got := mustCursor(t, w.pool, other.tenantID, payProvA); !got.IsZero() {
		t.Fatalf("another tenant has no cursor: %s", got)
	}
}

// Several overlapping sweeps for one tenant (multi-instance): no run may leave a gap behind the previous one, and
// nothing errors; the advisory lock and the stale-cursor check serialize the windows.
func TestBound_ConcurrentSweepsNeverLeaveAGap(t *testing.T) {
	w := newPayWorld(t)
	for i := 0; i < 5; i++ {
		w.deposit(t, int64(200+i))
	}
	cfg := PaymentSweepConfig{Resolver: mapResolver{payProvA: w.srcA, payProvB: w.srcB}, AllowSynthetic: true, RunLineBound: 2}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				now := time.Now()
				outs, err := RunSweepTenantsScoped(context.Background(), w.pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
					sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}, cfg)
				if err != nil {
					t.Errorf("sweep: %v", err)
					return
				}
				for _, o := range outs {
					for _, ps := range o.PaymentStatement {
						if ps.Err != nil {
							t.Errorf("concurrent sweep outcome: %+v", ps)
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	runs := paymentRuns(t, w.pool, w.f.tenantID, payProvA)
	if len(runs) == 0 {
		t.Fatal("no runs recorded")
	}
	var maxEnd time.Time
	for i, r := range runs {
		if i > 0 && r.periodStart.After(maxEnd) {
			t.Fatalf("run %d starts at %s, after every earlier run ended (%s): a window was skipped", i, r.periodStart, maxEnd)
		}
		if r.periodEnd.After(maxEnd) {
			maxEnd = r.periodEnd
		}
	}
	// Every line ends up imported at least once (nothing skipped). Duplicates here would be an unmatched loser's evidence import only.
	if refs := importedRefs(t, w.pool, w.f.tenantID, payProvA); len(refs) != 5 {
		t.Fatalf("want all 5 lines imported, got %d: %v", len(refs), refs)
	}
}
