package observability

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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
// would get, not these already-delegated instruments (code review J-1:
// this is exactly why the pre-fix version of
// TestAdmission_DecisionsIdenticalRegardlessOfMeter's
// `otel.SetMeterProvider(failingMP)` had no effect at all - see
// SetWebhookAdmissionInstrumentsForTest, the actual fix, for the seam
// that works instead). So every test in this file asserting real
// recorded data must read from the SAME reader, and must compute a DELTA
// against a baseline snapshot (the Sum aggregation is cumulative-since-
// creation, and other tests in this package may also record through the
// same global instruments).
//
// None of the tests in this package call t.Parallel(), and that is
// required, not incidental: every exact-delta/exact-value assertion here
// (and in internal/httpserver/webhook_admission_metrics_test.go) assumes
// no OTHER test is concurrently recording through these same shared
// instruments between its "before" and "after" snapshots. A future test
// added with t.Parallel() would make every such assertion racy/flaky,
// not just wrong for itself - if admission testing ever needs real
// parallelism, it must first move to SetWebhookAdmissionInstrumentsForTest's
// per-test scoped instrument pattern (see this file's
// TestRecordWebhookAdmissionDecision_ClosedLabelSet for an example)
// instead of sharing testReader.
var testReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	testReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader))
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
// for that data point (for label-shape assertions).
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

// TestRecordWebhookAdmissionDecision_ClosedLabelSet asserts the counter
// increments per decision class and carries exactly the closed
// decision/reason/provider_kind/stage label set (J-3: `stage` added) - no
// tenant label, no raw provider id.
func TestRecordWebhookAdmissionDecision_ClosedLabelSet(t *testing.T) {
	ctx := context.Background()

	before := collect(t)
	baseline, _, _ := sumValueFor(t, before, "webhook_admission_decisions_total", map[string]string{
		"decision": "rejected", "reason": "preauth", "provider_kind": "payments", "stage": "preauth",
	})

	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionRejected, WebhookAdmissionReasonPreAuth, WebhookProviderKindPayments, WebhookAdmissionStagePreAuth)
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionRejected, WebhookAdmissionReasonPreAuth, WebhookProviderKindPayments, WebhookAdmissionStagePreAuth)
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionAdmitted, WebhookAdmissionReasonAdmitted, WebhookProviderKindCasino, WebhookAdmissionStageVerified)

	after := collect(t)

	gotRejected, found, keys := sumValueFor(t, after, "webhook_admission_decisions_total", map[string]string{
		"decision": "rejected", "reason": "preauth", "provider_kind": "payments", "stage": "preauth",
	})
	if !found {
		t.Fatalf("expected a data point for rejected/preauth/payments/preauth")
	}
	if gotRejected-baseline != 2 {
		t.Fatalf("rejected/preauth/payments/preauth delta = %d, want 2", gotRejected-baseline)
	}
	for _, want := range []string{"decision", "reason", "provider_kind", "stage"} {
		if !keys[want] {
			t.Fatalf("missing expected label %q; saw %v", want, keys)
		}
	}
	if len(keys) != 4 {
		t.Fatalf("label set not bounded to exactly {decision,reason,provider_kind,stage}: %v", keys)
	}
	for forbidden := range map[string]bool{"tenant_id": true, "tenant_key": true, "provider_id": true, "provider_key": true, "domain": true} {
		if keys[forbidden] {
			t.Fatalf("forbidden label %q present on webhook_admission_decisions_total", forbidden)
		}
	}

	gotAdmitted, found, _ := sumValueFor(t, after, "webhook_admission_decisions_total", map[string]string{
		"decision": "admitted", "reason": "admitted", "provider_kind": "casino", "stage": "verified",
	})
	if !found || gotAdmitted < 1 {
		t.Fatalf("expected at least 1 admitted/admitted/casino/verified, got %d (found=%v)", gotAdmitted, found)
	}

	// A stage mix-up (recording the preauth-stage admit as "verified" or
	// vice versa) must NOT show up under the wrong stage.
	if v, found, _ := sumValueFor(t, after, "webhook_admission_decisions_total", map[string]string{
		"decision": "admitted", "reason": "admitted", "provider_kind": "casino", "stage": "preauth",
	}); found && v > 0 {
		t.Fatalf("admitted/admitted/casino leaked into stage=preauth: %d", v)
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

// fakeInt64Counter/fakeInt64UpDownCounter are minimal metric.Int64Counter/
// metric.Int64UpDownCounter implementations (embedding the OTel
// `embedded.*` marker types, per the API's own "API Implementations"
// convention) that let a test control exactly what Add() does - nothing,
// panic, or block - independent of any real OTel SDK/exporter behaviour.
// This is code review J-1's actual fix: the previous "failing exporter"
// test exercised a throwaway scratch counter from an unrelated meter,
// never this package's own webhookAdmissionDecisionsTotal/
// webhookAdmissionInFlight instruments.
type fakeInt64Counter struct {
	embedded.Int64Counter
	addFn func(ctx context.Context, incr int64, opts ...metric.AddOption)
}

func (f fakeInt64Counter) Add(ctx context.Context, incr int64, opts ...metric.AddOption) {
	if f.addFn != nil {
		f.addFn(ctx, incr, opts...)
	}
}
func (f fakeInt64Counter) Enabled(context.Context) bool { return true }

type fakeInt64UpDownCounter struct {
	embedded.Int64UpDownCounter
	addFn func(ctx context.Context, incr int64, opts ...metric.AddOption)
}

func (f fakeInt64UpDownCounter) Add(ctx context.Context, incr int64, opts ...metric.AddOption) {
	if f.addFn != nil {
		f.addFn(ctx, incr, opts...)
	}
}
func (f fakeInt64UpDownCounter) Enabled(context.Context) bool { return true }

// TestRecordWebhookAdmissionDecision_NilInstrumentsAreNoOp is the true
// no-op case (J-1): this package's OWN counter/gauge variables set to
// nil - the documented state before any meter provider is ever
// installed. Every Record*/Inc/Dec call must simply return.
func TestRecordWebhookAdmissionDecision_NilInstrumentsAreNoOp(t *testing.T) {
	restore := SetWebhookAdmissionInstrumentsForTest(nil, nil)
	defer restore()

	ctx := context.Background()
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionAdmitted, WebhookAdmissionReasonAdmitted, WebhookProviderKindPayments, WebhookAdmissionStagePreAuth)
	WebhookAdmissionInFlightInc(ctx, WebhookProviderKindPayments)
	WebhookAdmissionInFlightDec(ctx, WebhookProviderKindPayments)
}

// TestRecordWebhookAdmissionDecision_PanickingInstrumentNeverEscapes
// substitutes THIS package's real instruments with fakes whose Add()
// always panics (J-1's "an Add that ... errors" arm, modeled as a panic
// since OTel's synchronous Add() has no error return - a panic is the
// only way a real Add() implementation could actually misbehave towards
// its caller). Record*/Inc/Dec must not let that escape.
func TestRecordWebhookAdmissionDecision_PanickingInstrumentNeverEscapes(t *testing.T) {
	panicCounter := fakeInt64Counter{addFn: func(context.Context, int64, ...metric.AddOption) {
		panic("simulated counter Add panic")
	}}
	panicGauge := fakeInt64UpDownCounter{addFn: func(context.Context, int64, ...metric.AddOption) {
		panic("simulated gauge Add panic")
	}}
	restore := SetWebhookAdmissionInstrumentsForTest(panicCounter, panicGauge)
	defer restore()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RecordWebhookAdmissionDecision let a panic escape: %v", r)
			}
		}()
		RecordWebhookAdmissionDecision(context.Background(), WebhookAdmissionAdmitted, WebhookAdmissionReasonAdmitted, WebhookProviderKindPayments, WebhookAdmissionStagePreAuth)
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("WebhookAdmissionInFlightInc let a panic escape: %v", r)
			}
		}()
		WebhookAdmissionInFlightInc(context.Background(), WebhookProviderKindPayments)
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("WebhookAdmissionInFlightDec let a panic escape: %v", r)
			}
		}()
		WebhookAdmissionInFlightDec(context.Background(), WebhookProviderKindPayments)
	}()
}

// TestRecordWebhookAdmissionDecision_SlowInstrumentStillReturns is J-1's
// "an Add that blocks" arm: a fake whose Add() takes a bounded-but-real
// amount of time (simulating a badly behaved instrumentation/exporter
// path) still returns normally, on a bounded budget - proving latency
// here cannot turn into an indefinite hang that would need its own
// timeout/circuit breaker. This does NOT claim Add() has a hard deadline
// (the real OTel API contract has none); it demonstrates the actual
// shape every call site in this package uses (a direct, synchronous
// call) completes for a merely-slow instrument, which is what a
// mis-configured but non-adversarial real exporter can plausibly do.
func TestRecordWebhookAdmissionDecision_SlowInstrumentStillReturns(t *testing.T) {
	slow := fakeInt64Counter{addFn: func(context.Context, int64, ...metric.AddOption) {
		time.Sleep(20 * time.Millisecond)
	}}
	restore := SetWebhookAdmissionInstrumentsForTest(slow, nil)
	defer restore()

	done := make(chan struct{})
	go func() {
		RecordWebhookAdmissionDecision(context.Background(), WebhookAdmissionAdmitted, WebhookAdmissionReasonAdmitted, WebhookProviderKindPayments, WebhookAdmissionStagePreAuth)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordWebhookAdmissionDecision did not return within 2s against a merely-slow instrument")
	}
}

// TestRecordWebhookAdmissionDecision_FailedReaderInstrumentIsSafe is J-1's
// corrected version of the original "failing exporter" test: it builds a
// REAL SDK counter from a REAL meter (unlike the previous version's
// unrelated scratch counter), installs it as THIS package's own
// instrument via SetWebhookAdmissionInstrumentsForTest, shuts down its
// reader (so any Collect against it errors), and proves Add() through
// this package's exported Record functions still doesn't block/panic/
// error back to the caller.
func TestRecordWebhookAdmissionDecision_FailedReaderInstrumentIsSafe(t *testing.T) {
	failingReader := sdkmetric.NewManualReader()
	failingMP := sdkmetric.NewMeterProvider(sdkmetric.WithReader(failingReader))
	meter := failingMP.Meter("failing-exporter-test")
	counter, err := meter.Int64Counter("webhook_admission_decisions_total_scoped_for_test")
	if err != nil {
		t.Fatalf("build scoped counter: %v", err)
	}
	gauge, err := meter.Int64UpDownCounter("webhook_admission_inflight_scoped_for_test")
	if err != nil {
		t.Fatalf("build scoped gauge: %v", err)
	}
	restore := SetWebhookAdmissionInstrumentsForTest(counter, gauge)
	defer restore()

	if err := failingReader.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	ctx := context.Background()
	RecordWebhookAdmissionDecision(ctx, WebhookAdmissionRejected, WebhookAdmissionReasonDBGate, WebhookProviderKindKYC, WebhookAdmissionStagePreAuth)
	WebhookAdmissionInFlightInc(ctx, WebhookProviderKindKYC)
	WebhookAdmissionInFlightDec(ctx, WebhookProviderKindKYC)

	var rm metricdata.ResourceMetrics
	if err := failingReader.Collect(context.Background(), &rm); err == nil {
		t.Fatalf("expected Collect on a shut-down reader to error (test setup sanity check)")
	}
}
