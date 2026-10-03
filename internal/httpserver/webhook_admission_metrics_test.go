package httpserver

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Diansalas/igaming-platform/internal/observability"
)

// This file is PRH-I4-METRICS-1: ADR 0097 §8's OTel admission metrics.
//
// metricsTestReader is installed exactly once for this whole
// internal/httpserver test binary, for the same reason
// internal/observability's own webhook_admission_metrics_test.go installs
// one via TestMain: go.opentelemetry.io/otel's global meter provider
// delegates to whichever provider is passed to the FIRST
// otel.SetMeterProvider call in the process, permanently, for every
// instrument already created at package-init time. No other file in this
// package ever calls SetMeterProvider (grep confirms), so installing it
// here once is safe and does not disturb any other httpserver test.
//
// Code review J-1: a LATER otel.SetMeterProvider call (e.g. installing a
// "failing" provider mid-test) has NO effect on instruments already
// delegated to metricsTestReader here - that is exactly the bug the
// pre-fix version of TestAdmission_DecisionsIdenticalRegardlessOfMeter
// had (its "failing exporter" run still silently recorded into THIS
// reader). Every test below that needs to simulate a nil/panicking/slow
// instrument uses observability.SetWebhookAdmissionInstrumentsForTest
// instead, which substitutes the package-level instrument variables
// directly and bypasses the OTel global provider indirection entirely.
//
// None of the tests in this file call t.Parallel(), and that is required,
// not incidental: every exact-delta assertion against the SHARED
// metricsTestReader (TestAdmission_InFlightGaugeBalancesToZero,
// TestAdmission_DoubleReleaseGaugeNeverGoesNegative) assumes no other
// test records through the same instruments between its "before" and
// "after" snapshots, and SetWebhookAdmissionInstrumentsForTest itself
// mutates unsynchronized package-level state in internal/observability -
// a future t.Parallel() admission test in this file (or a concurrent one
// in internal/observability, same constraint) would make these
// assertions racy, not just wrong for itself.
var metricsTestReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	metricsTestReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricsTestReader))
	otel.SetMeterProvider(mp)
	os.Exit(m.Run())
}

func collectMetrics(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricsTestReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

func findAdmissionMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// admissionSumFor returns the current cumulative value (and full
// attribute key set) of an int64 Sum metric's data point matching exactly
// wantAttrs - see internal/observability's identical helper for why this
// is baseline/delta based rather than an absolute-value assertion.
func admissionSumFor(t *testing.T, rm metricdata.ResourceMetrics, metricName string, wantAttrs map[string]string) (value int64, found bool, attrKeys map[string]bool) {
	t.Helper()
	m, ok := findAdmissionMetric(rm, metricName)
	if !ok {
		return 0, false, nil
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q: unexpected data type %T", metricName, m.Data)
	}
	for _, dp := range sum.DataPoints {
		keys := map[string]bool{}
		for _, kv := range dp.Attributes.ToSlice() {
			keys[string(kv.Key)] = true
		}
		match := true
		for k, v := range wantAttrs {
			got, ok := dp.Attributes.Value(attribute.Key(k))
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

// metricsUnitTestSettings mirrors testAdmissionSettings (webhook_admission_
// harness_test.go) but is small enough to exhaust A3/A4a/B1/B2 with a
// handful of calls, entirely in the unit lane (no DB, no real time -
// every rate here is small enough that GCRA's burst tolerance, not
// elapsed wall-clock time, is what matters, exactly like
// TestAdmission_OverflowLogged already does in this package).
func metricsUnitTestSettings() WebhookAdmissionSettings {
	rb := func(rate float64, burst int) WebhookRateBurst { return WebhookRateBurst{Rate: rate, Burst: burst} }
	return WebhookAdmissionSettings{
		Enabled: true,
		// payments: burst=1, deliberately tiny so a SECOND call on the same
		// (domain, tenant, provider) key is rejected at A3 (preauth) - used
		// by every "preauth rejection" scenario in this file.
		// casino: burst generous, so A3 never trips - used by every
		// "inflight (A4a) rejection" scenario, where InFlightPerKey=1 must
		// be the ONLY thing that rejects a second concurrent call.
		PreAuthRate:        map[string]WebhookRateBurst{"payments": rb(0.01, 1), "casino": rb(0.01, 1000), "kyc": rb(0.01, 1000)},
		PreAuthUnknownRate: map[string]WebhookRateBurst{"payments": rb(0.01, 1), "casino": rb(0.01, 1000), "kyc": rb(0.01, 1000)},
		VerifiedRate:       map[string]WebhookRateBurst{"payments": rb(0.01, 1000), "casino": rb(0.01, 1000), "kyc": rb(0.01, 1)},
		InFlightGlobal:     100, InFlightPerKey: 1, InFlightUnknown: 1,
		DBGateGlobal: 100, DBGatePerKey: 50, DBGateUnknown: 50, DBGateWait: 50 * time.Millisecond,
		DomainTxPerTenant: 1, DomainWait: 50 * time.Millisecond,
		DirectoryRefresh: time.Hour, DirectoryCap: 1000,
		IdleEvict: time.Hour, VerifiedMaxKeys: 1000, PerIPMaxKeys: 1000,
	}
}

// newWebhookMetricsRequest builds a POST webhook request with tenantSlug/
// providerID path values EXPLICITLY set via (Go 1.22+) SetPathValue - a
// bare httptest.NewRequest never runs through a ServeMux, so
// r.PathValue("tenantSlug")/("providerID") would otherwise always be "",
// which would silently short-circuit preAuthKeys' schemeRegistered call
// (see its `len(providerID) > 0 && ...` guard) and make every "panic
// inside schemeRegistered" scenario below inert.
func newWebhookMetricsRequest(path, tenantSlug, providerID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, nil)
	r.SetPathValue("tenantSlug", tenantSlug)
	r.SetPathValue("providerID", providerID)
	return r
}

func newMetricsTestRuntime(t *testing.T) *webhookAdmissionRuntime {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	rt := newWebhookAdmission(metricsUnitTestSettings(), nil, logger)
	if rt == nil {
		t.Fatal("expected a non-nil runtime")
	}
	return rt
}

// fakeInt64Counter/fakeInt64UpDownCounter are minimal metric.Int64Counter/
// metric.Int64UpDownCounter fakes (mirrors internal/observability's own
// identical test-only types) letting a test control exactly what Add()
// does, independent of any real OTel SDK/exporter behaviour.
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

// admissionOutcome captures everything an admission call site actually
// hands back to its caller - status, body, and the ok/release contract -
// so "identical admission outcomes" (code review J-1's own phrase) can be
// asserted byte-for-byte, not just "both ok" or "both rejected".
type admissionOutcome struct {
	status int
	body   string
	ok     bool
}

// runAdmissionScenarioMatrix drives a fixed battery of admission
// scenarios (admitted, every rejection tier at both stages, both
// panic-recovery paths, the A4b db_gate rejection) through a FRESH
// runtime and returns one outcome per scenario, in order. Used to prove
// byte-for-byte identical behaviour across different metrics-instrument
// states (J-1).
func runAdmissionScenarioMatrix(t *testing.T) []admissionOutcome {
	t.Helper()
	rt := newMetricsTestRuntime(t)
	var outcomes []admissionOutcome
	record := func(w *httptest.ResponseRecorder, ok bool) {
		outcomes = append(outcomes, admissionOutcome{status: w.Code, body: w.Body.String(), ok: ok})
	}

	// 1. Admitted (preauth stage).
	w := httptest.NewRecorder()
	r := newWebhookMetricsRequest("/v1/webhooks/payments/t1/mock", "t1", "mock")
	release, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { return true }, nil)
	record(w, ok)
	if ok && release != nil {
		release()
	}

	// 2. A3 preauth rejection (burst already exhausted for this domain's
	// known-tenant bucket by scenario 1's admitted call).
	w2 := httptest.NewRecorder()
	r2 := newWebhookMetricsRequest("/v1/webhooks/payments/t1/mock", "t1", "mock")
	release2, ok2 := rt.admitPreAuth(w2, r2, domainPayments, 0, func(string) bool { return true }, nil)
	record(w2, ok2)
	if ok2 && release2 != nil {
		release2()
	}

	// 3. A4a in-flight rejection.
	w3a := httptest.NewRecorder()
	r3a := newWebhookMetricsRequest("/v1/webhooks/casino/t2/mock", "t2", "mock")
	release3a, ok3a := rt.admitPreAuth(w3a, r3a, domainCasino, 0, func(string) bool { return true }, nil)
	if !ok3a {
		t.Fatalf("scenario 3 setup: expected first admit to succeed")
	}
	w3b := httptest.NewRecorder()
	r3b := newWebhookMetricsRequest("/v1/webhooks/casino/t2/mock", "t2", "mock")
	release3b, ok3b := rt.admitPreAuth(w3b, r3b, domainCasino, 0, func(string) bool { return true }, nil)
	record(w3b, ok3b)
	if ok3b && release3b != nil {
		release3b()
	}
	release3a()

	// 4. B1 verified rejection (verified stage).
	tenantID := uuid.New()
	w4a := httptest.NewRecorder()
	r4a := newWebhookMetricsRequest("/v1/webhooks/kyc/t3/mock", "t3", "mock")
	relV1, okV1 := rt.admitVerified(w4a, r4a, domainKYC, tenantID, "mock", nil)
	if !okV1 {
		t.Fatalf("scenario 4 setup: expected first verified admit to succeed")
	}
	w4b := httptest.NewRecorder()
	r4b := newWebhookMetricsRequest("/v1/webhooks/kyc/t3/mock", "t3", "mock")
	relV2, okV2 := rt.admitVerified(w4b, r4b, domainKYC, tenantID, "mock", nil)
	record(w4b, okV2)
	if okV2 && relV2 != nil {
		relV2()
	}
	if relV1 != nil {
		relV1()
	}

	// 5. Panic path (preauth stage): schemeRegistered panics inside
	// admitPreAuth.
	w5 := httptest.NewRecorder()
	r5 := newWebhookMetricsRequest("/v1/webhooks/payments/tpanic/mock", "tpanic", "mock")
	panicRel, panicOk := rt.admitPreAuth(w5, r5, domainPayments, 0, func(string) bool { panic("boom") }, nil)
	record(w5, panicOk)
	if panicOk && panicRel != nil {
		panicRel()
	}

	// 6. db_gate rejection (preauth stage; writeDBGateUnavailable is a
	// direct write, not gated by any limiter state above).
	w6 := httptest.NewRecorder()
	r6 := newWebhookMetricsRequest("/v1/webhooks/payments/t6/mock", "t6", "mock")
	rt.writeDBGateUnavailable(w6, r6, domainPayments, nil, "mock")
	record(w6, false)

	// 7. Panic path (verified stage): retries429 panics inside
	// admitVerified's B1-rejection branch.
	tenantID2 := uuid.New()
	w7a := httptest.NewRecorder()
	r7a := newWebhookMetricsRequest("/v1/webhooks/kyc/t4/mock", "t4", "mock")
	rel7a, ok7a := rt.admitVerified(w7a, r7a, domainKYC, tenantID2, "mock", nil)
	if !ok7a {
		t.Fatalf("scenario 7 setup: expected first verified admit to succeed")
	}
	w7b := httptest.NewRecorder()
	r7b := newWebhookMetricsRequest("/v1/webhooks/kyc/t4/mock", "t4", "mock")
	rel7b, ok7b := rt.admitVerified(w7b, r7b, domainKYC, tenantID2, "mock", func(string) (bool, bool) { panic("boom") })
	record(w7b, ok7b)
	if ok7b && rel7b != nil {
		rel7b()
	}
	if rel7a != nil {
		rel7a()
	}

	return outcomes
}

// TestAdmission_DecisionsIdenticalRegardlessOfMeter is the DoD's core
// requirement: "Metrics must never affect admission". Code review J-1:
// the previous version used `otel.SetMeterProvider(failingMP)` to
// simulate a failing exporter, which had NO effect (the global delegate
// binds permanently to TestMain's provider) - the "failing" run silently
// recorded into metricsTestReader too, making the comparison vacuous.
// This version uses observability.SetWebhookAdmissionInstrumentsForTest
// to genuinely substitute the package's instruments with (a) nil
// (no-op), (b) an instrument whose Add() always panics, and (c) an
// instrument whose Add() is merely slow (bounded), and asserts every run
// produces byte-for-byte identical HTTP status/body/ok/release outcomes
// against the real-meter baseline.
func TestAdmission_DecisionsIdenticalRegardlessOfMeter(t *testing.T) {
	baseline := runAdmissionScenarioMatrix(t)

	cases := []struct {
		name    string
		counter metric.Int64Counter
		gauge   metric.Int64UpDownCounter
	}{
		{name: "nil instruments (no-op)", counter: nil, gauge: nil},
		{
			name: "panicking Add",
			counter: fakeInt64Counter{addFn: func(context.Context, int64, ...metric.AddOption) {
				panic("simulated counter Add panic")
			}},
			gauge: fakeInt64UpDownCounter{addFn: func(context.Context, int64, ...metric.AddOption) {
				panic("simulated gauge Add panic")
			}},
		},
		{
			name: "slow (bounded) Add",
			counter: fakeInt64Counter{addFn: func(context.Context, int64, ...metric.AddOption) {
				time.Sleep(10 * time.Millisecond)
			}},
			gauge: fakeInt64UpDownCounter{addFn: func(context.Context, int64, ...metric.AddOption) {
				time.Sleep(10 * time.Millisecond)
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := observability.SetWebhookAdmissionInstrumentsForTest(tc.counter, tc.gauge)
			defer restore()

			got := runAdmissionScenarioMatrix(t)
			if len(baseline) != len(got) {
				t.Fatalf("scenario count differs: %d vs %d", len(baseline), len(got))
			}
			for i := range baseline {
				if baseline[i] != got[i] {
					t.Fatalf("scenario %d differs from the real-meter baseline:\n  baseline: %+v\n  %s: %+v", i, baseline[i], tc.name, got[i])
				}
			}
		})
	}
}

// admissionComboKey identifies one webhook_admission_decisions_total data
// point by its full, closed label set.
type admissionComboKey struct {
	decision, reason, providerKind, stage string
}

// collectDecisionCombos reads every webhook_admission_decisions_total
// data point from reader and returns it keyed by its exact label
// combination - failing the test outright if any data point does NOT
// carry exactly the four expected keys (decision, reason, provider_kind,
// stage), which would itself be a label-shape regression.
func collectDecisionCombos(t *testing.T, reader *sdkmetric.ManualReader) map[admissionComboKey]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	combos := map[admissionComboKey]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "webhook_admission_decisions_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q: unexpected data type %T", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				attrs := dp.Attributes.ToSlice()
				if len(attrs) != 4 {
					t.Fatalf("unexpected label shape (want exactly 4 keys): %v", attrs)
				}
				var k admissionComboKey
				for _, kv := range attrs {
					switch string(kv.Key) {
					case "decision":
						k.decision = kv.Value.AsString()
					case "reason":
						k.reason = kv.Value.AsString()
					case "provider_kind":
						k.providerKind = kv.Value.AsString()
					case "stage":
						k.stage = kv.Value.AsString()
					default:
						t.Fatalf("unexpected label key %q", kv.Key)
					}
				}
				combos[k] += dp.Value
			}
		}
	}
	return combos
}

// newScopedDecisionsInstruments builds a FRESH, isolated SDK counter/
// gauge pair (backed by its own ManualReader, never touched by any other
// test) and installs them as this package's own webhook admission
// instruments via observability.SetWebhookAdmissionInstrumentsForTest.
// Every subtest using this starts from an empty (zero-value) metric
// state, so an assertion can compare EXACT values instead of a
// before/after delta - which is what lets
// TestAdmission_MetricsCoverage_AllReasonsBothStages assert "delta = 1 at
// the exercised point(s) and 0 at every other closed-enum combination" as
// a single map comparison.
func newScopedDecisionsInstruments(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := mp.Meter("webhook-admission-metrics-scoped-test")
	counter, err := meter.Int64Counter("webhook_admission_decisions_total")
	if err != nil {
		t.Fatalf("build scoped counter: %v", err)
	}
	gauge, err := meter.Int64UpDownCounter("webhook_admission_inflight")
	if err != nil {
		t.Fatalf("build scoped gauge: %v", err)
	}
	restore := observability.SetWebhookAdmissionInstrumentsForTest(counter, gauge)
	t.Cleanup(restore)
	return reader
}

// TestAdmission_MetricsCoverage_AllReasonsBothStages is code review J-2:
// every reason at both stages, each asserted as "delta 1 at the exercised
// point(s), 0 at every other closed-enum combination" via a fresh,
// zero-baseline scoped instrument per subtest (so "delta" and "absolute
// value" are the same thing here). This is what actually kills:
//   - MJ2 (admitVerified records "admitted" twice per call) - the
//     "verified admitted" subtest would see combo value 2, not 1.
//   - MJ3 (domain_bulkhead mistakenly recorded as "verified") - the
//     "verified domain_bulkhead" subtest would see an unexpected
//     rejected/verified/verified data point instead of
//     rejected/domain_bulkhead/verified, and the expected
//     rejected/domain_bulkhead combo would be missing (value 0).
func TestAdmission_MetricsCoverage_AllReasonsBothStages(t *testing.T) {
	type subtest struct {
		name     string
		run      func(t *testing.T, rt *webhookAdmissionRuntime)
		expected map[admissionComboKey]int64
	}

	subtests := []subtest{
		{
			name: "preauth admitted",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				w := httptest.NewRecorder()
				r := newWebhookMetricsRequest("/v1/webhooks/payments/c1/mock", "c1", "mock")
				rel, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { return true }, nil)
				if !ok {
					t.Fatalf("expected admission")
				}
				rel()
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "payments", "preauth"}: 1,
			},
		},
		{
			name: "preauth ip rejected",
			run: func(t *testing.T, _ *webhookAdmissionRuntime) {
				settings := metricsUnitTestSettings()
				settings.PerIPRPS = 0.01
				settings.PerIPBurst = 1
				settings.PerIPMaxKeys = 100
				logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
				ipRT := newWebhookAdmission(settings, nil, logger)
				w1 := httptest.NewRecorder()
				r1 := newWebhookMetricsRequest("/v1/webhooks/payments/c2a/mock", "c2a", "mock")
				rel1, ok1 := ipRT.admitPreAuth(w1, r1, domainPayments, 0, func(string) bool { return true }, nil)
				if !ok1 {
					t.Fatalf("expected first (IP-burst-consuming) call to be admitted")
				}
				rel1()
				w2 := httptest.NewRecorder()
				r2 := newWebhookMetricsRequest("/v1/webhooks/payments/c2b/mock", "c2b", "mock")
				_, ok2 := ipRT.admitPreAuth(w2, r2, domainPayments, 0, func(string) bool { return true }, nil)
				if ok2 {
					t.Fatalf("expected second call (same IP, burst exhausted) to be rejected")
				}
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "payments", "preauth"}: 1,
				{"rejected", "ip", "payments", "preauth"}:       1,
			},
		},
		{
			name: "preauth rejected (A3)",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				w1 := httptest.NewRecorder()
				r1 := newWebhookMetricsRequest("/v1/webhooks/payments/c3/mock", "c3", "mock")
				rel1, ok1 := rt.admitPreAuth(w1, r1, domainPayments, 0, func(string) bool { return true }, nil)
				if !ok1 {
					t.Fatalf("expected first call to be admitted")
				}
				rel1()
				w2 := httptest.NewRecorder()
				r2 := newWebhookMetricsRequest("/v1/webhooks/payments/c3/mock", "c3", "mock")
				_, ok2 := rt.admitPreAuth(w2, r2, domainPayments, 0, func(string) bool { return true }, nil)
				if ok2 {
					t.Fatalf("expected second call (burst exhausted) to be rejected")
				}
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "payments", "preauth"}: 1,
				{"rejected", "preauth", "payments", "preauth"}:  1,
			},
		},
		{
			name: "preauth inflight rejected (A4a)",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				w1 := httptest.NewRecorder()
				r1 := newWebhookMetricsRequest("/v1/webhooks/casino/c4/mock", "c4", "mock")
				rel1, ok1 := rt.admitPreAuth(w1, r1, domainCasino, 0, func(string) bool { return true }, nil)
				if !ok1 {
					t.Fatalf("expected first call to be admitted")
				}
				w2 := httptest.NewRecorder()
				r2 := newWebhookMetricsRequest("/v1/webhooks/casino/c4/mock", "c4", "mock")
				_, ok2 := rt.admitPreAuth(w2, r2, domainCasino, 0, func(string) bool { return true }, nil)
				if ok2 {
					t.Fatalf("expected second concurrent call to be rejected (inflight cap 1)")
				}
				rel1()
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "casino", "preauth"}: 1,
				{"rejected", "inflight", "casino", "preauth"}: 1,
			},
		},
		{
			name: "preauth db_gate rejected",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				w := httptest.NewRecorder()
				r := newWebhookMetricsRequest("/v1/webhooks/payments/c5/mock", "c5", "mock")
				rt.writeDBGateUnavailable(w, r, domainPayments, nil, "mock")
			},
			expected: map[admissionComboKey]int64{
				{"rejected", "db_gate", "payments", "preauth"}: 1,
			},
		},
		{
			name: "preauth directory_unloaded rejected",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
				rt.directory = newWebhookTenantDirectory(nil, 10, logger) // constructed, never Load()ed
				w := httptest.NewRecorder()
				r := newWebhookMetricsRequest("/v1/webhooks/payments/c6/mock", "c6", "mock")
				_, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { return true }, nil)
				if ok {
					t.Fatalf("expected rejection: directory never loaded")
				}
			},
			expected: map[admissionComboKey]int64{
				{"rejected", "directory_unloaded", "payments", "preauth"}: 1,
			},
		},
		{
			name: "preauth panic recovered",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				w := httptest.NewRecorder()
				r := newWebhookMetricsRequest("/v1/webhooks/payments/c7/mock", "c7", "mock")
				_, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { panic("boom") }, nil)
				if ok {
					t.Fatalf("expected panic recovery to reject")
				}
			},
			expected: map[admissionComboKey]int64{
				{"rejected", "panic", "payments", "preauth"}: 1,
			},
		},
		{
			name: "verified admitted",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				w := httptest.NewRecorder()
				r := newWebhookMetricsRequest("/v1/webhooks/kyc/c8/mock", "c8", "mock")
				rel, ok := rt.admitVerified(w, r, domainKYC, uuid.New(), "mock", nil)
				if !ok {
					t.Fatalf("expected verified admission")
				}
				rel()
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "kyc", "verified"}: 1,
			},
		},
		{
			name: "verified rejected (B1)",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				tenantID := uuid.New()
				w1 := httptest.NewRecorder()
				r1 := newWebhookMetricsRequest("/v1/webhooks/kyc/c9/mock", "c9", "mock")
				rel1, ok1 := rt.admitVerified(w1, r1, domainKYC, tenantID, "mock", nil)
				if !ok1 {
					t.Fatalf("expected first verified call to be admitted")
				}
				rel1()
				w2 := httptest.NewRecorder()
				r2 := newWebhookMetricsRequest("/v1/webhooks/kyc/c9/mock", "c9", "mock")
				_, ok2 := rt.admitVerified(w2, r2, domainKYC, tenantID, "mock", nil)
				if ok2 {
					t.Fatalf("expected second verified call (burst exhausted) to be rejected")
				}
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "kyc", "verified"}: 1,
				{"rejected", "verified", "kyc", "verified"}: 1,
			},
		},
		{
			name: "verified domain_bulkhead rejected (B2)",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				tenantID := uuid.New()
				w1 := httptest.NewRecorder()
				r1 := newWebhookMetricsRequest("/v1/webhooks/kyc/c10/mockA", "c10", "mockA")
				rel1, ok1 := rt.admitVerified(w1, r1, domainKYC, tenantID, "mockA", nil)
				if !ok1 {
					t.Fatalf("expected first call (holds the only B2 slot) to be admitted")
				}
				// A DIFFERENT provider for the same tenant gets a fresh B1
				// bucket, so this call passes B1 and is rejected at B2
				// specifically (DomainTxPerTenant=1, already held by call 1).
				w2 := httptest.NewRecorder()
				r2 := newWebhookMetricsRequest("/v1/webhooks/kyc/c10/mockB", "c10", "mockB")
				_, ok2 := rt.admitVerified(w2, r2, domainKYC, tenantID, "mockB", nil)
				if ok2 {
					t.Fatalf("expected second call to be rejected at B2 (domain_bulkhead)")
				}
				rel1()
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "kyc", "verified"}:        1,
				{"rejected", "domain_bulkhead", "kyc", "verified"}: 1,
			},
		},
		{
			name: "verified panic recovered",
			run: func(t *testing.T, rt *webhookAdmissionRuntime) {
				tenantID := uuid.New()
				w1 := httptest.NewRecorder()
				r1 := newWebhookMetricsRequest("/v1/webhooks/kyc/c11/mock", "c11", "mock")
				rel1, ok1 := rt.admitVerified(w1, r1, domainKYC, tenantID, "mock", nil)
				if !ok1 {
					t.Fatalf("expected first verified call to be admitted")
				}
				rel1()
				w2 := httptest.NewRecorder()
				r2 := newWebhookMetricsRequest("/v1/webhooks/kyc/c11/mock", "c11", "mock")
				_, ok2 := rt.admitVerified(w2, r2, domainKYC, tenantID, "mock", func(string) (bool, bool) { panic("boom") })
				if ok2 {
					t.Fatalf("expected the retries429-panic call to be rejected")
				}
			},
			expected: map[admissionComboKey]int64{
				{"admitted", "admitted", "kyc", "verified"}: 1,
				{"rejected", "panic", "kyc", "verified"}:    1,
			},
		},
	}

	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			reader := newScopedDecisionsInstruments(t)
			rt := newMetricsTestRuntime(t)
			st.run(t, rt)

			got := collectDecisionCombos(t, reader)
			if len(got) != len(st.expected) {
				t.Fatalf("combo set size = %d, want %d\n  got:  %+v\n  want: %+v", len(got), len(st.expected), got, st.expected)
			}
			for k, wantV := range st.expected {
				gotV, ok := got[k]
				if !ok {
					t.Fatalf("missing expected combo %+v (got: %+v)", k, got)
				}
				if gotV != wantV {
					t.Fatalf("combo %+v = %d, want %d (got: %+v)", k, gotV, wantV, got)
				}
			}
		})
	}
}

// TestAdmission_InFlightGaugeBalancesToZero drives the A4a in-flight
// gauge through several admitted-then-released requests, including the
// panic-recovery path, and asserts it returns to its baseline (0 net
// change) every time.
func TestAdmission_InFlightGaugeBalancesToZero(t *testing.T) {
	rt := newMetricsTestRuntime(t)

	before := collectMetrics(t)
	baseline, _, _ := admissionSumFor(t, before, "webhook_admission_inflight", map[string]string{"provider_kind": "payments"})

	// Normal admit + release.
	w1 := httptest.NewRecorder()
	r1 := newWebhookMetricsRequest("/v1/webhooks/payments/g1/mock", "g1", "mock")
	rel1, ok1 := rt.admitPreAuth(w1, r1, domainPayments, 0, func(string) bool { return true }, nil)
	if !ok1 {
		t.Fatalf("expected admission")
	}

	mid := collectMetrics(t)
	midValue, found, _ := admissionSumFor(t, mid, "webhook_admission_inflight", map[string]string{"provider_kind": "payments"})
	if !found || midValue != baseline+1 {
		t.Fatalf("mid-flight value = %d (found=%v), want %d", midValue, found, baseline+1)
	}
	rel1()

	// A rejection never increments the gauge at all.
	w2 := httptest.NewRecorder()
	r2 := newWebhookMetricsRequest("/v1/webhooks/payments/g1/mock", "g1", "mock")
	_, ok2 := rt.admitPreAuth(w2, r2, domainPayments, 0, func(string) bool { return true }, nil)
	if ok2 {
		t.Fatalf("expected preauth rejection (burst exhausted)")
	}

	// A panic recovered before the A4a acquire never leaves the gauge
	// incremented either.
	w3 := httptest.NewRecorder()
	r3 := newWebhookMetricsRequest("/v1/webhooks/payments/g2/mock", "g2", "mock")
	_, ok3 := rt.admitPreAuth(w3, r3, domainPayments, 0, func(string) bool { panic("boom") }, nil)
	if ok3 {
		t.Fatalf("expected panic path to reject")
	}

	after := collectMetrics(t)
	finalValue, found, _ := admissionSumFor(t, after, "webhook_admission_inflight", map[string]string{"provider_kind": "payments"})
	if !found || finalValue != baseline {
		t.Fatalf("final in-flight value = %d (found=%v), want baseline %d", finalValue, found, baseline)
	}
}

// TestAdmission_DoubleReleaseGaugeNeverGoesNegative is J-5 (code review
// Info): rel() itself is idempotent (internal/admission.Bulkhead's own
// sync.Once), but calling admitPreAuth's own RETURNED WRAPPER twice used
// to call WebhookAdmissionInFlightDec twice too, driving the gauge
// negative even though the underlying A4a slot was only ever released
// once. admitPreAuth now wraps the Dec in its own sync.Once - this test
// proves a double call to the wrapper leaves the gauge at baseline, never
// at baseline-1.
func TestAdmission_DoubleReleaseGaugeNeverGoesNegative(t *testing.T) {
	rt := newMetricsTestRuntime(t)

	before := collectMetrics(t)
	baseline, _, _ := admissionSumFor(t, before, "webhook_admission_inflight", map[string]string{"provider_kind": "payments"})

	w := httptest.NewRecorder()
	r := newWebhookMetricsRequest("/v1/webhooks/payments/dr1/mock", "dr1", "mock")
	rel, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { return true }, nil)
	if !ok {
		t.Fatalf("expected admission")
	}

	rel()
	rel() // deliberate double release

	after := collectMetrics(t)
	finalValue, found, _ := admissionSumFor(t, after, "webhook_admission_inflight", map[string]string{"provider_kind": "payments"})
	if !found || finalValue != baseline {
		t.Fatalf("in-flight value after a double release = %d (found=%v), want baseline %d (must never go negative)", finalValue, found, baseline)
	}
}
