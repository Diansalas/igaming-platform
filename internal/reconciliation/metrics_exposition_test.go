package reconciliation

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/observability"
)

// The package-level tracker registers against the global meter provider at init; this proves the series the
// alert rules use (ReconciliationStale, ReconciliationSweepStalled, ReconciliationMismatchesOpen,
// ReconciliationRunFailures, ReconciliationListingFailed) really appear, under those exact names and label
// values, in the Prometheus exposition cmd/platform-api serves on METRICS_ADDR.
func TestExposition_ReconciliationSeriesMatchTheAlertRules(t *testing.T) {
	_, handler, shutdown, err := observability.InitMetricsExposed(context.Background(), "platform-api", "none")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	tenant := uuid.New()
	gen := defaultTracker.beginSweep()
	defaultTracker.record(gen, tenant, string(StreamLedgerVsProjection), "", StreamOutcome{Run: Run{Status: StatusMismatchesFound}, Mismatches: 2})
	defaultTracker.record(gen, tenant, string(StreamCasinoConsistency), "", StreamOutcome{Err: context.DeadlineExceeded})
	defaultTracker.endSweep(gen, true)
	gen = defaultTracker.beginSweep()
	defaultTracker.endSweep(gen, false)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	body := string(b)
	for _, want := range []string{
		`reconciliation_last_success_unix_seconds{stream="ledger_vs_projection"}`,
		`reconciliation_last_success_unix_seconds{stream="casino_consistency"} 0`,
		`reconciliation_mismatches_open{stream="ledger_vs_projection"} 2`,
		`reconciliation_runs_total{result="failed",stream="casino_consistency"} 1`,
		`reconciliation_runs_total{result="mismatch",stream="ledger_vs_projection"} 1`,
		`reconciliation_sweeps_total{result="listing_failed"} 1`,
		`reconciliation_sweeps_total{result="ok"} 1`,
		`reconciliation_last_sweep_unix_seconds`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition lacks %q", want)
		}
	}
	// No tenant identifier may ever reach a label.
	if strings.Contains(body, tenant.String()) {
		t.Error("a tenant id leaked into the exposition")
	}
}
