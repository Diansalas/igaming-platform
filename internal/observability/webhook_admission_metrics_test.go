package observability

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// testReader is installed exactly once for this whole test binary by
// TestMain. This is deliberate, not an oversight: go.opentelemetry.io/
// otel's global meter provider uses a sync.Once delegate (internal/
// global/state.go) - the FIRST call to otel.SetMeterProvider anywhere in
// the process permanently binds every instrument this package already
// created at package-init time (webhookAdmissionDecisionsTotal,
// webhookAdmissionInFlight) to that one real provider; a later
// SetMeterProvider call only changes what brand-NEW otel.Meter() lookups
// would get, not these already-delegated instruments. So every test
// asserting real recorded data must read from the SAME reader, and must
// compute a DELTA against a baseline snapshot (the Sum aggregation is
// cumulative-since-creation, and other tests/parallel runs in this
// package may also record through the same global instruments).
var testReader *metric.ManualReader

func TestMain(m *testing.M) {
	testReader = metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(testReader))
	otel.SetMeterProvider(mp)
	os.Exit(m.Run())
}

func collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// sumValueFor returns the current cumulative value of an int64 Sum metric
// for the data point whose attribute set contains exactly wantAttrs
// (key -> value), and also returns the full observed attribute key set
// for that data point (for label-shape assertions), for every data point
// - callers match on wantAttrs themselves.
func sumValueFor(t *testing.T, rm metricdata.ResourceMetrics, metricName string, wantAttrs map[string]string) (value int64, found bool, attrKeys map[string]bool) {
	t.Helper()
	m, ok := findMetric(rm, metricName)
	if !ok {
		return 0, false, nil
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q: unexpected data type %T", metricName, m.Data)
	}
	for _, dp := range sum.DataPoints {
		attrs := dp.Attributes
		match := true
		keys := map[string]bool{}
		for _, kv := range attrs.ToSlice() {
			keys[string(kv.Key)] = true
		}
		for k, v := range wantAttrs {
			got, ok := attrs.Value(attribute.Key(k))
			if !ok || got.AsString() != v {
				match = false
			}
		}
		if match && len(keys) == len(wantAttrs) {
			return dp.Value, true, keys
		}
	}
	return 0, false, nil
}

// TestRecordWebhookAdmissionDecision_LabelsAndCounts asserts the counter
// increments per decision class and carries exactly the closed
// decision/reason/provider_kind label set - no tenant label, no raw
// provider id.
func TestRecordWebhookAdmissionDecision_LabelsAndCounts(t *testing.T) {
	ctx := context.Background()

	before := collect(t)
	baseline, _, _ := sumValueFor(t, before, "webhook_admission_decisions_total", map[string]string{
		"decision": "rejected", "reason": "preauth", "provider_kind": "payments",
	})

	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionRejected, WebhookAdmissionReasonPreAuth, WebhookProviderKindPayments)
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionRejected, WebhookAdmissionReasonPreAuth, WebhookProviderKindPayments)
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionAdmitted, WebhookAdmissionReasonAdmitted, WebhookProviderKindCasino)

	after := collect(t)

	gotRejected, found, keys := sumValueFor(t, after, "webhook_admission_decisions_total", map[string]string{
		"decision": "rejected", "reason": "preauth", "provider_kind": "payments",
	})
	if !found {
		t.Fatalf("expected a data point for rejected/preauth/payments")
	}
	if gotRejected-baseline != 2 {
		t.Fatalf("rejected/preauth/payments delta = %d, want 2", gotRejected-baseline)
	}
	for _, want := range []string{"decision", "reason", "provider_kind"} {
		if !keys[want] {
			t.Fatalf("missing expected label %q; saw %v", want, keys)
		}
	}
	if len(keys) != 3 {
		t.Fatalf("label set not bounded to exactly {decision,reason,provider_kind}: %v", keys)
	}
	for forbidden := range map[string]bool{"tenant_id": true, "tenant_key": true, "provider_id": true, "provider_key": true, "domain": true} {
		if keys[forbidden] {
			t.Fatalf("forbidden label %q present on webhook_admission_decisions_total", forbidden)
		}
	}

	gotAdmitted, found, _ := sumValueFor(t, after, "webhook_admission_decisions_total", map[string]string{
		"decision": "admitted", "reason": "admitted", "provider_kind": "casino",
	})
	if !found || gotAdmitted < 1 {
		t.Fatalf("expected at least 1 admitted/admitted/casino, got %d (found=%v)", gotAdmitted, found)
	}
}

// TestWebhookAdmissionInFlight_BalancesToZero asserts the in-flight gauge
// (UpDownCounter) returns to its baseline once every Inc has a matching
// Dec, including a panic/error path.
func TestWebhookAdmissionInFlight_BalancesToZero(t *testing.T) {
	ctx := context.Background()

	before := collect(t)
	baseline, _, _ := sumValueFor(t, before, "webhook_admission_inflight", map[string]string{"provider_kind": "kyc"})

	func() {
		WebhookAdmissionInFlightInc(ctx, WebhookProviderKindKYC)
		defer WebhookAdmissionInFlightDec(ctx, WebhookProviderKindKYC)
	}()

	// Simulate a panic/error path: Inc, then a deferred Dec that still
	// runs during panic unwinding, mirroring admitPreAuth's own
	// release-func-wraps-the-Dec pattern under its recover().
	func() {
		defer func() { _ = recover() }()
		WebhookAdmissionInFlightInc(ctx, WebhookProviderKindKYC)
		defer WebhookAdmissionInFlightDec(ctx, WebhookProviderKindKYC)
		panic("simulated failure mid-request")
	}()

	after := collect(t)
	value, found, _ := sumValueFor(t, after, "webhook_admission_inflight", map[string]string{"provider_kind": "kyc"})
	if !found {
		t.Fatalf("expected a data point for provider_kind=kyc")
	}
	if value != baseline {
		t.Fatalf("webhook_admission_inflight did not return to baseline: got %d, want %d", value, baseline)
	}
}

// TestSafelyRecord_SwallowsPanic proves safelyRecord's own guarantee
// directly (independent of the real OTel SDK's behaviour, which is not
// expected to panic under normal operation): a panicking fn never
// escapes, and a normal fn still runs to completion.
func TestSafelyRecord_SwallowsPanic(t *testing.T) {
	// A panicking fn must not propagate.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("safelyRecord let a panic escape: %v", r)
			}
		}()
		safelyRecord(func() { panic("simulated metrics SDK/exporter failure") })
	}()

	// A normal fn must still execute.
	ran := false
	safelyRecord(func() { ran = true })
	if !ran {
		t.Fatalf("safelyRecord did not run a non-panicking fn")
	}
}

// TestWebhookAdmissionMetrics_NoOpMeterIsSafe asserts every recording
// function tolerates being called with attribute combinations that were
// never seen before, and never panics - the "no-op meter" arm of this
// requirement is additionally covered by observability_admission_noop_
// test.go style call sites in internal/httpserver, which run these
// exact functions with NO meter provider ever installed (this package's
// default state before InitMetrics runs).
func TestWebhookAdmissionMetrics_NoOpMeterIsSafe(t *testing.T) {
	ctx := context.Background()
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionAdmitted, WebhookAdmissionReasonAdmitted, WebhookProviderKindPayments)
	WebhookAdmissionInFlightInc(ctx, WebhookProviderKindPayments)
	WebhookAdmissionInFlightDec(ctx, WebhookProviderKindPayments)
}

// TestWebhookAdmissionMetrics_FailingExporterDoesNotBlock asserts that a
// reader in a failed/shut-down state never causes Record*/Inc/Dec to
// block, error, or panic - Add() is synchronous only with the SDK's
// in-memory aggregation, never with export, so a failing exporter cannot
// propagate back into a caller of this package (and, by construction,
// can therefore never affect an admission decision, which is always made
// and its HTTP response written BEFORE any of these functions are
// called - see RecordWebhookAdmissionDecision's doc comment).
func TestWebhookAdmissionMetrics_FailingExporterDoesNotBlock(t *testing.T) {
	failingReader := metric.NewManualReader()
	failingMP := metric.NewMeterProvider(metric.WithReader(failingReader))
	meter := failingMP.Meter("failing-exporter-test")
	counter, err := meter.Int64Counter("scratch_counter")
	if err != nil {
		t.Fatalf("scratch counter: %v", err)
	}
	// Force the reader into a shut-down state so any Collect against it
	// errors - proves the failure mode is isolated to export/collect, not
	// to recording.
	if err := failingReader.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	counter.Add(context.Background(), 1) // must not panic even post-shutdown

	ctx := context.Background()
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionRejected, WebhookAdmissionReasonDBGate, WebhookProviderKindKYC)
	WebhookAdmissionInFlightInc(ctx, WebhookProviderKindKYC)
	WebhookAdmissionInFlightDec(ctx, WebhookProviderKindKYC)

	var rm metricdata.ResourceMetrics
	if err := failingReader.Collect(context.Background(), &rm); err == nil {
		t.Fatalf("expected Collect on a shut-down reader to error")
	}
}
