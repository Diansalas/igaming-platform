package reconciliation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeNow) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeNow) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// observed collects the metrics of one meter provider as {name -> {stream -> value}} (stream "" = no label).
func observed(t *testing.T, r metric.Reader) map[string]map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if out[m.Name] == nil {
				out[m.Name] = map[string]float64{}
			}
			add := func(stream string, v float64) { out[m.Name][stream] += v }
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					s, _ := dp.Attributes.Value("stream")
					add(s.AsString(), float64(dp.Value))
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					s, _ := dp.Attributes.Value("stream")
					add(s.AsString(), float64(dp.Value))
				}
			}
		}
	}
	return out
}

func newTestTracker(t *testing.T) (*runTracker, metric.Reader, *fakeNow) {
	t.Helper()
	clock := &fakeNow{t: time.Unix(1_700_000_000, 0)}
	tr := newRunTracker(clock.Now)
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := tr.register(mp.Meter("test")); err != nil {
		t.Fatal(err)
	}
	return tr, reader, clock
}

func TestRunTracker_LastSuccessIsTheOldestTenantNotTheNewest(t *testing.T) {
	tr, reader, clock := newTestTracker(t)
	a, b := uuid.New(), uuid.New()

	gen := tr.beginSweep()
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Run: Run{Status: StatusClean}})
	tr.endSweep(gen, true)
	oldA := clock.Now().Unix()

	clock.Advance(time.Hour)
	gen = tr.beginSweep()
	// Tenant a now FAILS; tenant b succeeds. The gauge must stay at a's old success, not jump to b's.
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Err: errors.New("boom")})
	tr.record(gen, b, "ledger_vs_projection", "", StreamOutcome{Run: Run{Status: StatusClean}})
	tr.endSweep(gen, true)

	got := observed(t, reader)["reconciliation_last_success_unix_seconds"]["ledger_vs_projection"]
	if int64(got) != oldA {
		t.Fatalf("last_success = %v, want the failing tenant's old success %d (min over tenants)", got, oldA)
	}
	if f := observed(t, reader)["reconciliation_runs_total"]; f["ledger_vs_projection"] != 3 {
		t.Fatalf("runs_total = %v, want 3", f)
	}
}

func TestRunTracker_NeverSucceededTenantReadsAsEpochZero(t *testing.T) {
	tr, reader, _ := newTestTracker(t)
	gen := tr.beginSweep()
	tr.record(gen, uuid.New(), "casino_statement", "", StreamOutcome{Err: errors.New("x")})
	tr.endSweep(gen, true)
	if got := observed(t, reader)["reconciliation_last_success_unix_seconds"]["casino_statement"]; got != 0 {
		t.Fatalf("a stream that has never succeeded must read 0 (so time()-value is huge and the stale rule fires), got %v", got)
	}
}

func TestRunTracker_LockSkipIsNotASuccess(t *testing.T) {
	tr, reader, _ := newTestTracker(t)
	gen := tr.beginSweep()
	tr.record(gen, uuid.New(), "ledger_vs_projection", "", StreamOutcome{Skipped: true})
	tr.endSweep(gen, true)
	if got, present := observed(t, reader)["reconciliation_last_success_unix_seconds"]["ledger_vs_projection"]; present || got != 0 {
		t.Fatalf("a lock-contention skip must create no state (an instance that always loses the lock must not read as stale): present=%v value=%v", present, got)
	}
}

func TestRunTracker_MismatchGaugeClearsOnACleanRun(t *testing.T) {
	tr, reader, clock := newTestTracker(t)
	a := uuid.New()
	gen := tr.beginSweep()
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Run: Run{Status: StatusMismatchesFound}, Mismatches: 3})
	tr.endSweep(gen, true)
	if got := observed(t, reader)["reconciliation_mismatches_open"]["ledger_vs_projection"]; got != 3 {
		t.Fatalf("mismatches_open = %v, want 3", got)
	}
	// A failed run neither clears nor changes the last known mismatch count (we do not know it is fixed).
	clock.Advance(time.Hour)
	gen = tr.beginSweep()
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Err: errors.New("x")})
	tr.endSweep(gen, true)
	if got := observed(t, reader)["reconciliation_mismatches_open"]["ledger_vs_projection"]; got != 3 {
		t.Fatalf("a failed run must not clear the mismatch gauge, got %v", got)
	}
	clock.Advance(time.Hour)
	gen = tr.beginSweep()
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Run: Run{Status: StatusClean}})
	tr.endSweep(gen, true)
	if got := observed(t, reader)["reconciliation_mismatches_open"]["ledger_vs_projection"]; got != 0 {
		t.Fatalf("a later clean run must clear the gauge, got %v", got)
	}
}

func TestRunTracker_TenantsThatLeftTheSweepArePruned(t *testing.T) {
	tr, reader, clock := newTestTracker(t)
	a, b := uuid.New(), uuid.New()
	gen := tr.beginSweep()
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Run: Run{Status: StatusClean}})
	tr.record(gen, b, "ledger_vs_projection", "", StreamOutcome{Err: errors.New("x")}) // b has never succeeded
	tr.endSweep(gen, true)
	clock.Advance(time.Hour)
	// b is closed/removed and no longer appears in a COMPLETE sweep: it must stop dragging the minimum to 0.
	gen = tr.beginSweep()
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Run: Run{Status: StatusClean}})
	tr.endSweep(gen, true)
	if got := observed(t, reader)["reconciliation_last_success_unix_seconds"]["ledger_vs_projection"]; int64(got) != clock.Now().Unix() {
		t.Fatalf("last_success = %v, want %d after b left the sweep", got, clock.Now().Unix())
	}
}

func TestRunTracker_IncompleteSweepDoesNotPrune(t *testing.T) {
	tr, reader, _ := newTestTracker(t)
	a, b := uuid.New(), uuid.New()
	gen := tr.beginSweep()
	tr.record(gen, a, "ledger_vs_projection", "", StreamOutcome{Run: Run{Status: StatusClean}})
	tr.record(gen, b, "ledger_vs_projection", "", StreamOutcome{Err: errors.New("x")})
	tr.endSweep(gen, true)
	// A sweep whose tenant listing failed saw nothing: it must not erase what we know.
	gen = tr.beginSweep()
	tr.endSweep(gen, false)
	if got := observed(t, reader)["reconciliation_last_success_unix_seconds"]["ledger_vs_projection"]; got != 0 {
		t.Fatalf("an incomplete sweep pruned state (got %v, want b's never-succeeded 0 still present)", got)
	}
	if got := observed(t, reader)["reconciliation_sweeps_total"]; got[""] != 2 {
		t.Fatalf("sweeps_total = %v", got)
	}
}

func TestRunTracker_UnknownStreamLabelIsBounded(t *testing.T) {
	tr, reader, _ := newTestTracker(t)
	gen := tr.beginSweep()
	tr.record(gen, uuid.New(), "tenant-supplied-free-text-"+uuid.NewString(), "", StreamOutcome{Run: Run{Status: StatusClean}})
	tr.endSweep(gen, true)
	for stream := range observed(t, reader)["reconciliation_last_success_unix_seconds"] {
		if stream != "other" {
			t.Fatalf("unknown stream leaked into a label: %q", stream)
		}
	}
}

func TestRunTracker_SweepHeartbeatOnlyAdvancesOnCompleteSweeps(t *testing.T) {
	tr, reader, clock := newTestTracker(t)
	gen := tr.beginSweep()
	tr.endSweep(gen, true)
	first := clock.Now().Unix()
	clock.Advance(time.Hour)
	gen = tr.beginSweep()
	tr.endSweep(gen, false) // listing failed: the sweep did not complete
	if got := observed(t, reader)["reconciliation_last_sweep_unix_seconds"][""]; int64(got) != first {
		t.Fatalf("last_sweep = %v, want the last COMPLETE sweep %d", got, first)
	}
	if r := observed(t, reader)["reconciliation_sweeps_total"]; r[""] != 2 {
		t.Fatalf("sweeps_total = %v, want both attempts counted (ok and listing_failed)", r)
	}
}
