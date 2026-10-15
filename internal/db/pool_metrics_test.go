package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// An unconnected pool (MinConns 0 dials nothing at construction) is enough to read Stat().
func newLazyPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPoolMetrics_EmitTheSaturationSignals(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	if err := registerPoolMetrics(mp.Meter("test"), newLazyPool(t, 7)); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				got[m.Name] = float64(d.DataPoints[0].Value)
			case metricdata.Sum[int64]:
				got[m.Name] = float64(d.DataPoints[0].Value)
			case metricdata.Sum[float64]:
				got[m.Name] = d.DataPoints[0].Value
			}
			// No instrument may carry any attribute: the pool is process-wide, there is nothing to label.
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				if d.DataPoints[0].Attributes.Len() != 0 {
					t.Errorf("%s has attributes", m.Name)
				}
			}
		}
	}
	for name, want := range map[string]float64{
		"db_pool_max_connections":      7,
		"db_pool_acquired_connections": 0,
		"db_pool_idle_connections":     0,
	} {
		v, ok := got[name]
		if !ok {
			t.Errorf("metric %s not emitted (have %v)", name, got)
			continue
		}
		if v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	for _, name := range []string{"db_pool_acquire_wait_seconds_total", "db_pool_empty_acquires_total"} {
		if _, ok := got[name]; !ok {
			t.Errorf("metric %s not emitted (have %v)", name, got)
		}
	}
}

func TestRegisterPoolMetrics_NilPoolIsRefused(t *testing.T) {
	if err := RegisterPoolMetrics(nil); err == nil {
		t.Fatal("a nil pool must be refused, not silently skipped")
	}
}
