package alerting

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func sp(s string) *string { return &s }

func TestUndeliveredClassification(t *testing.T) {
	// An email channel over the MOCK provider reports HumanNotification()==false (channels.EmailSink); a stub with the
	// same answer stands in here because package channels imports alerting.
	d := &Dispatcher{sinks: map[ChannelKind]Sink{ChannelLog: LogSink{}, ChannelEmail: stubSink{kind: ChannelEmail, human: false}}}
	for _, c := range []struct {
		name  string
		event *string
		kind  *string
		want  bool
	}{
		{"no delivery row yet", nil, nil, true},
		{"unrouted", sp("unrouted"), nil, true},
		{"claimed (in flight)", sp("claimed"), sp("email"), true},
		{"failed", sp("failed"), sp("email"), true},
		{"dead", sp("dead"), sp("email"), true},
		{"sent through the log channel reached nobody", sp("sent"), sp("log"), true},
		{"sent through an email channel over the MOCK provider reached nobody", sp("sent"), sp("email"), true},
		{"sent with no recorded channel is not trusted", sp("sent"), nil, true},
		{"sent through a channel this binary has no sink for is not trusted", sp("sent"), sp("webhook"), true},
		{"simulation suppression is not backlog", sp("suppressed_simulation"), nil, false},
		{"an unknown future event is not counted", sp("bogus"), nil, false},
	} {
		if got := d.undelivered(c.event, c.kind); got != c.want {
			t.Errorf("%s: undelivered = %v, want %v", c.name, got, c.want)
		}
	}
	// A real (human) channel's "sent" is delivered.
	d.sinks[ChannelWebhook] = humanStub{}
	if d.undelivered(sp("sent"), sp("webhook")) {
		t.Error("sent through a human-notification channel must not be backlog")
	}
}

type humanStub struct{}

func (humanStub) ChannelKind() ChannelKind { return ChannelWebhook }
func (humanStub) HumanNotification() bool  { return true }
func (humanStub) Deliver(context.Context, Delivery) (Outcome, ErrorClass) {
	return OutcomeSent, ErrorClassNone
}

func TestBacklogTally_CountsAndOldestPerSeverity(t *testing.T) {
	var b backlogTally
	b.add(SeverityP1, 10*time.Second)
	b.add(SeverityP1, 90*time.Second)
	b.add(SeverityP2, 5*time.Second)
	b.add(Severity("bogus"), time.Hour) // an unknown severity is dropped, never mis-bucketed
	if b.count[0] != 2 || b.oldest[0] != 90*time.Second || b.count[1] != 1 || b.count[2] != 0 {
		t.Fatalf("tally = %+v", b)
	}
}

func TestBacklogGauges_ObservedOnlyAfterAPassAndWithBoundedLabels(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	// Re-register the package gauges on a private provider to read them.
	old := meter
	meter = mp.Meter("test")
	defer func() { meter = old }()
	backlogPublished.Store(false)
	registerBacklogGauges()

	read := func() map[string]map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				g, ok := m.Data.(metricdata.Gauge[int64])
				if !ok {
					continue
				}
				out[m.Name] = map[string]int64{}
				for _, dp := range g.DataPoints {
					v, _ := dp.Attributes.Value("severity")
					out[m.Name][v.AsString()] = dp.Value
				}
			}
		}
		return out
	}
	if got := read(); len(got["alert_undelivered_count"]) != 0 {
		t.Fatalf("gauges must not report before the first completed pass (a stale 0 would read as healthy): %v", got)
	}
	var b backlogTally
	b.add(SeverityP1, 120*time.Second)
	publishBacklog(b)
	got := read()
	if got["alert_undelivered_count"]["p1"] != 1 || got["alert_undelivered_oldest_age_seconds"]["p1"] != 120 {
		t.Fatalf("p1 backlog = %v", got)
	}
	if v, ok := got["alert_undelivered_count"]["p2"]; !ok || v != 0 {
		t.Fatalf("p2 must be reported as 0, got %v present=%v", v, ok)
	}
	if len(got["alert_undelivered_count"]) != 3 {
		t.Fatalf("labels must be exactly p1,p2,p3: %v", got["alert_undelivered_count"])
	}
	// a later pass with no backlog resets the gauges
	publishBacklog(backlogTally{})
	if got := read(); got["alert_undelivered_count"]["p1"] != 0 {
		t.Fatalf("backlog did not reset: %v", got)
	}
}
