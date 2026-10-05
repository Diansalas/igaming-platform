//go:build integration

package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// metricTotal sums every data point of a counter whose attributes contain all of want.
func metricTotal(t *testing.T, name string, want map[string]string) int64 {
	t.Helper()
	var total int64
	for _, m := range collect(t) {
		if m.Name != name {
			continue
		}
		for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
			a := attrs(dp.Attributes, keysOf(want)...)
			match := true
			for k, v := range want {
				if a[k] != v {
					match = false
				}
			}
			if match {
				total += dp.Value
			}
		}
	}
	return total
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// humanFake is a test sink on the mock kind that CLAIMS to notify a person, to
// exercise the notification="human" label path. It is a test double only.
type humanFake struct{ *MockSink }

func (humanFake) HumanNotification() bool { return true }

// Code review #5/#6: the metric CALL SITES (not just the helpers) emit
// alert_delivery_attempts_total and alert_unrouted_total with the right labels.
func TestMetricCallSites_DeliveryAttemptsAndUnrouted(t *testing.T) {
	testMetricReader()
	pool := scratchPool(t, "amcs")
	admin := seedPlatformAdmin(t, pool)
	tenant := seedTenant(t, pool, admin)
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock-target")

	sentNonHuman := map[string]string{"channel_kind": "mock", "result": "sent", "notification": "non_human"}
	sentHuman := map[string]string{"channel_kind": "mock", "result": "sent", "notification": "human"}
	dead := map[string]string{"channel_kind": "mock", "result": "dead", "error_class": "rejected", "notification": "non_human"}
	failed := map[string]string{"channel_kind": "mock", "result": "failed", "error_class": "unavailable", "notification": "non_human"}
	noRoute := map[string]string{"severity": "p1", "reason": "no_route"}
	const att = "alert_delivery_attempts_total"

	b1, b2, b3, b4, b5 := metricTotal(t, att, sentNonHuman), metricTotal(t, att, sentHuman), metricTotal(t, att, dead),
		metricTotal(t, att, failed), metricTotal(t, "alert_unrouted_total", noRoute)

	run := func(sink Sink) {
		d := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, sink)
		if err := d.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	seedOpenAlert(t, pool, tenant, KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	run(&MockSink{})
	seedOpenAlert(t, pool, tenant, KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	run(humanFake{&MockSink{}})
	seedOpenAlert(t, pool, tenant, KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	run(&MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) { return OutcomeFailed, ErrorClassRejected }})
	seedOpenAlert(t, pool, tenant, KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	run(&MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) { return OutcomeFailed, ErrorClassUnavailable }})
	seedOpenAlert(t, pool, tenant, KindPaymentWebhookIntegrity, "d:"+uuid.NewString()) // p1, no p1 route
	run(&MockSink{})

	for name, c := range map[string]struct {
		before int64
		want   map[string]string
		metric string
	}{
		"sent via a non-human channel":  {b1, sentNonHuman, att},
		"sent via a human channel":      {b2, sentHuman, att},
		"permanent failure is dead":     {b3, dead, att},
		"retryable failure is failed":   {b4, failed, att},
		"unrouted carries severity+why": {b5, noRoute, "alert_unrouted_total"},
	} {
		if after := metricTotal(t, c.metric, c.want); after <= c.before {
			t.Errorf("%s: %s%v did not increase (%d -> %d): a call site stopped emitting it", name, c.metric, c.want, c.before, after)
		}
	}
}
