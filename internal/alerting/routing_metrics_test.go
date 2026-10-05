package alerting

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collect(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

func attrs(set attribute.Set, keys ...string) map[string]string {
	m := map[string]string{}
	for _, k := range keys {
		if v, ok := set.Value(attribute.Key(k)); ok {
			m[k] = v.AsString()
		}
	}
	return m
}

// alert_delivery_attempts_total labels a log/mock delivery non_human, never human.
func TestMetric_DeliveryAttempts_LabelNonHuman_NoTenant(t *testing.T) {
	ctx := context.Background()
	recordDeliveryAttempt(ctx, ChannelMock, "sent", ErrorClassNone, false)
	recordDeliveryAttempt(ctx, ChannelLog, "failed", ErrorClassTimeout, false)
	recordDeliveryAttempt(ctx, ChannelMock, "dead", ErrorClassRejected, false)
	var seen int
	for _, m := range collect(t) {
		if m.Name != "alert_delivery_attempts_total" {
			continue
		}
		for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
			if dp.Attributes.Len() != 4 {
				t.Fatalf("labels must be {channel_kind,result,error_class,notification}: %v", dp.Attributes.ToSlice())
			}
			a := attrs(dp.Attributes, "channel_kind", "result", "error_class", "notification")
			if a["notification"] != "non_human" {
				t.Fatalf("a log/mock delivery must be labelled non_human: %v", a)
			}
			seen++
		}
	}
	if seen < 3 {
		t.Fatalf("expected the three recorded series, saw %d", seen)
	}
}

func TestMetric_Unrouted_CarriesSeverityAndReason(t *testing.T) {
	recordUnrouted(context.Background(), SeverityP1, UnroutedNoSink)
	var ok bool
	for _, m := range collect(t) {
		if m.Name != "alert_unrouted_total" {
			continue
		}
		for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
			a := attrs(dp.Attributes, "severity", "reason")
			if dp.Attributes.Len() != 2 {
				t.Fatalf("labels must be {severity, reason}: %v", dp.Attributes.ToSlice())
			}
			if a["severity"] == "p1" && a["reason"] == "no_sink" {
				ok = true
			}
		}
	}
	if !ok {
		t.Fatal("alert_unrouted_total{severity=p1,reason=no_sink} missing")
	}
}

// alert_routing_ready{severity}: observed at scrape time, stale or unevaluated reads 0.
func TestMetric_RoutingReadyGauge_StaleReadsZero(t *testing.T) {
	prev := latestReadiness.Load()
	defer latestReadiness.Store(prev)

	clk := &manualClockUnit{now: time.Now()}
	tr := &readinessTracker{clock: clk, staleAfter: time.Minute}
	tr.snap = ReadinessSnapshot{Evaluated: true, EvaluatedAt: clk.now, Severities: []SeverityReadiness{
		{Severity: SeverityP1, Required: true, Ready: true, Reason: ReadinessReady},
		{Severity: SeverityP2, Required: true, Ready: false, Reason: ReadinessNotHuman},
	}}
	latestReadiness.Store(tr)

	read := func() map[string]int64 {
		out := map[string]int64{}
		for _, m := range collect(t) {
			if m.Name != "alert_routing_ready" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
				v, _ := dp.Attributes.Value("severity")
				out[v.AsString()] = dp.Value
			}
		}
		return out
	}
	if got := read(); got["p1"] != 1 || got["p2"] != 0 || len(got) != 2 {
		t.Fatalf("fresh gauge: %v", got)
	}
	clk.now = clk.now.Add(2 * time.Minute)
	if got := read(); got["p1"] != 0 || got["p2"] != 0 {
		t.Fatalf("a stale evaluation must read 0, got %v", got)
	}
}
