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
	if f.withCoverage {
		out.CoverageEnd = time.Now()
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
	src := casFetcher{pool: one, heldSeen: &heldSeen, calls: &calls}
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
	// Kill control: the identical interleaving with NO coverage bound reports
	// the false findings the bound exists to prevent: the bet and the win as
	// ledger postings with no statement line, and the GGR total they move.
	_, ms, _ := w.runStmt(t, casFetcher{pool: w.pool, withCoverage: false, after: interleave})
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
