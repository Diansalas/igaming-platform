package alerting

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func dispatcherPassCount(t *testing.T, result string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "alert_dispatcher_passes_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data type %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, _ := dp.Attributes.Value("result"); v.AsString() == result {
					if dp.Attributes.Len() != 1 {
						t.Fatalf("alert_dispatcher_passes_total must carry only the result label, got %v", dp.Attributes.ToSlice())
					}
					n += dp.Value
				}
			}
		}
	}
	return n
}

// CR-2: alert_dispatcher_passes_total{result} is the runbook's only "dispatcher
// stalled" signal. Drive the real loop through an ok, an error and a panic pass
// and assert each result counter moves by exactly one.
func TestMetric_DispatcherPasses_CountsOkErrorPanic(t *testing.T) {
	_ = testMetricReader()
	before := map[string]int64{}
	for _, r := range []string{"ok", "error", "panic"} {
		before[r] = dispatcherPassCount(t, r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{fn: func(_ context.Context, call int) error {
		switch call {
		case 2:
			return errors.New("db down")
		case 3:
			panic("boom")
		}
		return nil
	}}
	tick, results, done := runLoop(ctx, runner, 0)
	want := []string{"ok", "error", "panic"}
	for i, w := range want {
		if i > 0 {
			tick <- time.Time{}
		}
		if got := <-results; got != w {
			t.Fatalf("pass %d result %q, want %q", i+1, got, w)
		}
	}
	cancel()
	<-done
	for _, r := range want {
		if got := dispatcherPassCount(t, r) - before[r]; got != 1 {
			t.Fatalf("alert_dispatcher_passes_total{result=%q} moved by %d, want exactly 1", r, got)
		}
	}
}
