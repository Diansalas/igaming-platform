//go:build integration

// CAS-STMT-IO-1 (A3): the casino statement is obtained OUTSIDE the
// reconciliation transaction and only compared inside it. The provider shape
// (statement.CasinoStatementFetcher) is the only shape a real source may have;
// the Synthetic in-house ledger renderer (the MOCK) is the one other accepted
// shape. These tests pin: the fetch holds no pooled connection (proved with a
// one-connection pool the fetcher itself needs), the fetch is refused under a
// held transaction, a non-Synthetic ledger renderer and an unknown shape are
// refused, CoverageEnd keeps a posting that commits between fetch and match
// out of the comparison (with a no-bound kill control), and a failing fetch is
// a failed, audited run with no partial rows.
package reconciliation

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

const casFetcherLabel = "MOCK external-shape test fetcher (test-only; renders the ledger through its own pool)"

// casFetcher is a TEST-ONLY provider-shaped (Fetch) source. It is deliberately
// NOT Synthetic: the stream must treat it as a provider source.
type casFetcher struct {
	pool *db.Pool
	// withCoverage sets CoverageEnd to the instant the lines were read.
	withCoverage bool
	// coverage, when non-zero, is the CoverageEnd returned verbatim (SR3-3
	// validation tests; it wins over withCoverage).
	coverage time.Time
	// emptyStatement returns no lines and no totals (the 1970 repro shape).
	emptyStatement bool
	// after runs once the statement is fetched, before the fetch returns.
	after func()
	err   error
	// heldSeen records txscope.Held at Fetch.
	heldSeen *atomic.Bool
	calls    *atomic.Int64
}

func (casFetcher) Label() string { return casFetcherLabel }

func (f casFetcher) Fetch(ctx context.Context, req statement.CasinoFetchRequest) (statement.CasinoStatement, error) {
	if f.calls != nil {
		f.calls.Add(1)
	}
	if f.heldSeen != nil && txscope.Held(ctx) {
		f.heldSeen.Store(true)
	}
	if f.err != nil {
		return statement.CasinoStatement{}, f.err
	}
	// A real source would do network I/O here. This one needs a pooled
	// connection of its OWN: on a one-connection pool that only works if the
	// caller holds none.
	var out statement.CasinoStatement
	err := f.pool.WithTenantSnapshot(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out.Lines, out.Totals, err = casino.MockStatementSource{}.RenderFromLedger(ctx, tx, req.TenantID)
		return err
	})
	if err != nil {
		return statement.CasinoStatement{}, err
	}
	if f.emptyStatement {
		out.Lines, out.Totals = nil, nil
	}
	if f.withCoverage {
		out.CoverageEnd = time.Now()
	}
	if !f.coverage.IsZero() {
		out.CoverageEnd = f.coverage
	}
	if f.after != nil {
		f.after()
	}
	return out, nil
}

// renderOnlySource is a ledger renderer that does NOT mark itself Synthetic.
type renderOnlySource struct{}

func (renderOnlySource) Label() string { return "MOCK non-synthetic renderer (test-only)" }
func (renderOnlySource) RenderFromLedger(context.Context, pgx.Tx, uuid.UUID) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal, error) {
	return nil, nil, nil
}

// labelOnlySource has neither shape.
type labelOnlySource struct{}

func (labelOnlySource) Label() string { return "MOCK label-only source (test-only)" }

func oneConnPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 1, 3*time.Second)
	if err != nil {
		t.Fatalf("connect one-connection pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCasinoStatementFetch_RefusedUnderHeldTransactionAndSourceNeverCalled(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	var calls atomic.Int64
	src := casFetcher{pool: w.pool, calls: &calls}
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FetchCasinoStatement(ctx, src, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	})
	if !errors.Is(err, ErrCasinoStatementFetchUnderTx) {
		t.Fatalf("a fetch under a held transaction must be refused with ErrCasinoStatementFetchUnderTx, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("the source must never be called under a held transaction, got %d calls", calls.Load())
	}
	// Outside a transaction the same call works.
	if _, err := FetchCasinoStatement(context.Background(), src, w.f.tenantID, time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatalf("fetch outside a transaction: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one call, got %d", calls.Load())
	}
}

// The sweep fetches with no pooled connection held: on a ONE-connection pool
// the fetcher can only obtain the connection it needs for its own read if the
// sweep is not holding it. Under the original shape (source read inside the
// run's transaction) this would be a connection-acquire timeout.
func TestCasinoStatementFetch_SweepHoldsNoConnectionDuringFetch(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	one := oneConnPool(t)
	var heldSeen atomic.Bool
	var calls atomic.Int64
	// withCoverage: A3 SR3-3 requires a provider-shaped source to state its
	// CoverageEnd; the assertions below are unchanged.
	src := casFetcher{pool: one, heldSeen: &heldSeen, calls: &calls, withCoverage: true}
	now := time.Now()
	outs, err := RunSweepTenants(context.Background(), one, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, src)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	o := findOutcome(t, outs, w.f)
	if o.CasinoStatement.Err != nil || o.CasinoStatement.Run.Status != StatusClean {
		t.Fatalf("expected a clean casino_statement run through the external-shaped source, got %+v", o.CasinoStatement)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one fetch per run, got %d", calls.Load())
	}
	if heldSeen.Load() {
		t.Fatal("the sweep called Fetch with a context marked as holding a transaction")
	}
}

func TestCasinoStatementFetch_CoverageEndKeepsLaterPostingsOutOfTheComparison(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	var n atomic.Int64
	interleave := func() {
		time.Sleep(25 * time.Millisecond) // strictly after CoverageEnd
		i := n.Add(1)
		round := "cov-" + string(rune('a'+i))
		w.mustDeliver(t, casino.CallbackEventBet, round+"-b", "", round, 25)
		w.mustDeliver(t, casino.CallbackEventWin, round+"-w", "", round, 30)
	}
	// With a CoverageEnd the later bet and win are out of scope: clean.
	w.mustStmtClean(t, casFetcher{pool: w.pool, withCoverage: true, after: interleave})
	// Kill control: the identical interleaving with NO tight coverage bound
	// reports the false findings the bound exists to prevent: the bet and the
	// win as ledger postings with no statement line, and the GGR total they
	// move. A3 SR3-3: a provider statement may no longer omit CoverageEnd, so
	// the "no bound" control uses the loosest bound still accepted (the maximum
	// permitted clock skew past now), which includes the interleaved postings.
	_, ms, _ := w.runStmt(t, casFetcher{pool: w.pool, coverage: time.Now().Add(CasinoCoverageMaxClockSkew - time.Minute), after: interleave})
	var noLine, total int
	for _, m := range ms {
		switch {
		case strings.Contains(m.ReconciliationKey, "check=key") && strings.Contains(m.ActualValue, "no statement line"):
			noLine++
		case strings.Contains(m.ReconciliationKey, "check=total"):
			total++
		}
	}
	if len(ms) != 3 || noLine != 2 || total != 1 {
		t.Fatalf("control: without a CoverageEnd the interleaved bet and win must surface as 2 key findings and 1 total finding, got %+v", ms)
	}
}

func TestCasinoStatementFetch_UnacceptableSourceShapesAreRefused(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	for name, src := range map[string]statement.CasinoStatementSource{
		"non_synthetic_ledger_renderer": renderOnlySource{},
		"neither_shape":                 labelOnlySource{},
	} {
		_, err := FetchCasinoStatement(context.Background(), src, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		if !errors.Is(err, ErrCasinoStatementSourceShape) {
			t.Fatalf("%s: expected ErrCasinoStatementSourceShape, got %v", name, err)
		}
	}
	// Through the sweep: a failed, audited run, never clean, no run row.
	now := time.Now()
	outs, err := RunSweepTenants(context.Background(), w.pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, renderOnlySource{})
	if err != nil {
		t.Fatal(err)
	}
	o := findOutcome(t, outs, w.f)
	if o.CasinoStatement.Err == nil || o.CasinoStatement.Run.Status == StatusClean {
		t.Fatalf("a non-synthetic ledger renderer must fail the run, never clean: %+v", o.CasinoStatement)
	}
	if runs, _ := w.stmtRunCount(t); runs != 0 {
		t.Fatalf("a refused source must leave no run row, got %d", runs)
	}
}

func TestCasinoStatementFetch_SweepFetchFailureIsAFailedAuditedRun(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	now := time.Now()
	outs, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casFetcher{pool: pool, err: errors.New("provider statement API down")})
	if err != nil {
		t.Fatal(err)
	}
	o := findOutcome(t, outs, w.f)
	if o.CasinoStatement.Err == nil || !strings.Contains(o.CasinoStatement.Err.Error(), "provider statement API down") {
		t.Fatalf("a failed fetch must fail the stream with the cause, got %+v", o.CasinoStatement)
	}
	var failed int
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			AND metadata->>'stream' = 'casino_statement' AND metadata->>'statement_source' = $2`, w.f.tenantID, casFetcherLabel).Scan(&failed)
	}); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("expected one audited failure naming the source label, got %d", failed)
	}
	if runs, mm := w.stmtRunCount(t); runs != 0 || mm != 0 {
		t.Fatalf("a failed fetch must leave no partial rows, got runs=%d mismatches=%d", runs, mm)
	}
	// The other streams ran regardless.
	if o.Err != nil || o.Casino.Err != nil {
		t.Fatalf("other streams must be unaffected: %+v", o)
	}
}

// ---------------------------------------------------------------------
// A3 SR3-3: a provider statement's CoverageEnd is validated; an unchecked
// value is an unchecked scope.

func (w *casWorld) requireCoverageRefusal(t *testing.T, name string, src statement.CasinoStatementSource) {
	t.Helper()
	run, ms, _, err := w.runStmtErr(t, src)
	if !errors.Is(err, ErrCasinoStatementCoverage) {
		t.Fatalf("%s: expected ErrCasinoStatementCoverage, got err=%v run=%s mismatches=%d", name, err, run.Status, len(ms))
	}
	if run.Status == StatusClean {
		t.Fatalf("%s: a refused coverage must never be clean", name)
	}
}

func TestCasinoStatementCoverage_InvalidProviderCoverageFailsClosed(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	now := time.Now()
	// The repro from the security review: a 1970 CoverageEnd with an EMPTY
	// statement over a world that has ledger activity. Unchecked, the ledger
	// side is bounded to nothing, both sides are empty and the run is "clean".
	w.requireCoverageRefusal(t, "1970 coverage with an empty statement",
		casFetcher{pool: w.pool, emptyStatement: true, coverage: time.Unix(0, 0)})
	w.requireCoverageRefusal(t, "zero coverage from a provider source", casFetcher{pool: w.pool})
	w.requireCoverageRefusal(t, "zero coverage with an empty statement", casFetcher{pool: w.pool, emptyStatement: true})
	w.requireCoverageRefusal(t, "earlier than period end minus max lag",
		casFetcher{pool: w.pool, coverage: now.Add(-time.Hour - CasinoCoverageMaxLag)})
	w.requireCoverageRefusal(t, "in the future", casFetcher{pool: w.pool, coverage: now.Add(CasinoCoverageMaxClockSkew + time.Minute)})
	if runs, mm := w.stmtRunCount(t); runs != 0 || mm != 0 {
		t.Fatalf("a refused coverage must leave no run rows, got runs=%d mismatches=%d", runs, mm)
	}
	// Controls: the same source with a credible bound is clean, so the refusals
	// above are caused by the coverage and nothing else.
	w.mustStmtClean(t, casFetcher{pool: w.pool, withCoverage: true})
	w.mustStmtClean(t, casFetcher{pool: w.pool, coverage: now.Add(time.Minute)})
}

// A Synthetic in-house source has no provider to vouch for anything and keeps
// working without a bound (the MOCK).
func TestCasinoStatementCoverage_SyntheticSourceNeedsNoBound(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	info := w.mustStmtClean(t, casino.MockStatementSource{})
	if !info.CoverageEnd.IsZero() {
		t.Fatalf("a synthetic source states no coverage, got %s", info.CoverageEnd)
	}
	if _, ok := info.AuditMetadata()["coverage_end"]; ok {
		t.Fatal("no coverage_end may be recorded for a source that stated none")
	}
}

// Through the sweep: coverage_end is recorded in the run's audit metadata, and
// the next run refuses a statement whose coverage moves backwards.
func TestCasinoStatementCoverage_SweepRecordsCoverageAndRefusesRegression(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	sweep := func(src statement.CasinoStatementSource) StreamOutcome {
		now := time.Now()
		outs, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
			sportsbook.MockSettlementStatementSource{}, src)
		if err != nil {
			t.Fatal(err)
		}
		return findOutcome(t, outs, w.f).CasinoStatement
	}
	first := sweep(casFetcher{pool: pool, withCoverage: true})
	if first.Err != nil || first.Run.Status != StatusClean {
		t.Fatalf("first sweep: %+v", first)
	}
	var recorded string
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'coverage_end' FROM audit_log
			WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run' AND target_id = $2`, w.f.tenantID, first.Run.ID.String()).Scan(&recorded)
	}); err != nil || recorded == "" {
		t.Fatalf("coverage_end must be recorded in the run's audit metadata: %q %v", recorded, err)
	}
	// Earlier than the previous run's coverage (but inside the lag window).
	back := sweep(casFetcher{pool: pool, coverage: time.Now().Add(-30 * time.Minute)})
	if !errors.Is(back.Err, ErrCasinoStatementCoverage) || back.Run.Status == StatusClean {
		t.Fatalf("a coverage earlier than the previous run's must fail closed, got %+v", back)
	}
	// Control: a later coverage proceeds.
	if again := sweep(casFetcher{pool: pool, withCoverage: true}); again.Err != nil || again.Run.Status != StatusClean {
		t.Fatalf("a later coverage must proceed: %+v", again)
	}
}

// A3 SR3B-3: the monotonicity anchor is read from the stream's own run rows
// (reconciliation_runs.coverage_end), never from forgeable audit rows, and a
// forged run row cannot lock the stream out or lower the anchor.
func TestCasinoStatementCoverage_AnchorForgeriesAreIneffective(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	sweep := func(src statement.CasinoStatementSource) StreamOutcome {
		now := time.Now()
		outs, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
			sportsbook.MockSettlementStatementSource{}, src)
		if err != nil {
			t.Fatal(err)
		}
		return findOutcome(t, outs, w.f).CasinoStatement
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		}); err != nil {
			t.Fatalf("forge: %v", err)
		}
	}
	forgeAudit := func(created time.Time, coverage string) {
		exec(`INSERT INTO audit_log (tenant_id, actor_type, action, outcome, metadata, created_at)
			VALUES ($1, 'system', 'reconciliation.sweep_run', 'success', $2::jsonb, $3)`,
			w.f.tenantID, `{"stream":"casino_statement","coverage_end":"`+coverage+`"}`, created)
	}
	forgeRun := func(runAt, coverage time.Time) {
		exec(`INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, run_at, status, coverage_end)
			VALUES ($1, 'casino_statement', $2, $2, $3, 'clean', $4)`, w.f.tenantID, runAt, runAt, coverage)
	}
	far := time.Now().AddDate(100, 0, 0)

	// Forged audit rows (future-dated 2999, later-created 1970, malformed): none is read.
	forgeAudit(far, "2999-01-01T00:00:00Z")
	forgeAudit(far.Add(time.Hour), "1970-01-01T00:00:00Z")
	forgeAudit(far.Add(2*time.Hour), "not-a-timestamp")
	if o := sweep(casFetcher{pool: pool, withCoverage: true}); o.Err != nil || o.Run.Status != StatusClean {
		t.Fatalf("forged audit rows must not affect the stream: %+v", o)
	}
	// The legitimate anchor (the run row just written) still refuses a regression,
	// although the forged 1970 audit row is "later".
	if o := sweep(casFetcher{pool: pool, coverage: time.Now().Add(-30 * time.Minute)}); !errors.Is(o.Err, ErrCasinoStatementCoverage) {
		t.Fatalf("a regression must still be refused despite forged audit rows: %+v", o)
	}

	// Forged run rows: a future-dated run (run_at and coverage 2999) cannot lock the stream out ...
	forgeRun(far, far)
	if o := sweep(casFetcher{pool: pool, withCoverage: true}); o.Err != nil || o.Run.Status != StatusClean {
		t.Fatalf("a future-dated forged run row must not lock the stream out: %+v", o)
	}
	// ... a plausible run_at with an implausible coverage (2999) is ignored too ...
	forgeRun(time.Now().Add(-time.Minute), far)
	if o := sweep(casFetcher{pool: pool, withCoverage: true}); o.Err != nil || o.Run.Status != StatusClean {
		t.Fatalf("an implausible forged coverage must be ignored: %+v", o)
	}
	// ... a future-dated run row with a plausible-looking coverage is ignored (run_at > now()) ...
	forgeRun(far, time.Now().Add(CasinoCoverageMaxClockSkew-time.Minute))
	if o := sweep(casFetcher{pool: pool, withCoverage: true}); o.Err != nil || o.Run.Status != StatusClean {
		t.Fatalf("a future-dated run row must not become the anchor: %+v", o)
	}
	// ... and a later-created run row with a 1970 coverage cannot lower the anchor.
	forgeRun(time.Now().Add(-time.Second), time.Unix(0, 0))
	if o := sweep(casFetcher{pool: pool, coverage: time.Now().Add(-30 * time.Minute)}); !errors.Is(o.Err, ErrCasinoStatementCoverage) {
		t.Fatalf("a forged 1970 run row must not lower the anchor: %+v", o)
	}
}

// SR3B-8: a Synthetic Fetch-shaped source needs no CoverageEnd, but a non-zero
// one it states is validated like a provider's.
type syntheticFetcher struct {
	casFetcher
}

func (syntheticFetcher) SyntheticComponent() {}

func TestCasinoStatementCoverage_SyntheticFetcherNonZeroCoverageIsValidated(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	w.mustStmtClean(t, syntheticFetcher{casFetcher{pool: w.pool}}) // zero: allowed
	w.requireCoverageRefusal(t, "synthetic 1970", syntheticFetcher{casFetcher{pool: w.pool, emptyStatement: true, coverage: time.Unix(0, 0)}})
}
