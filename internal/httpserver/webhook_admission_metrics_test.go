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
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// This file is PRH-I4-METRICS-1: ADR 0097 §8's OTel admission metrics.
//
// metricsTestReader is installed exactly once for this whole
// internal/httpserver test binary, for the same reason
// internal/observability's own webhook_admission_metrics_test.go installs
// one via TestMain: go.opentelemetry.io/otel's global meter provider
// delegates to whichever provider is passed to the FIRST
// otel.SetMeterProvider call in the process, permanently, for every
// instrument already created at package-init time (this includes
// internal/observability's webhook_admission_decisions_total/
// webhook_admission_inflight instruments, created when that package was
// imported into this test binary). No other file in this package ever
// calls SetMeterProvider (grep confirms), so installing it here once is
// safe and does not disturb any other httpserver test.
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

// TestAdmission_DecisionsIdenticalRegardlessOfMeter is the DoD's core
// requirement: "Metrics must never affect admission" - it drives the
// exact same admission scenarios twice, once against whatever meter
// provider TestMain already installed (a real, working meter/reader -
// the ambient state every other test in this file also runs under) and
// once against a meter provider backed by an ALREADY-SHUT-DOWN reader
// (a stand-in for "the exporter is failing"; Collect on it errors, but
// Add() must still never block/panic/change behaviour), and asserts
// every HTTP status code, body, and release()/ok outcome is byte-for-byte
// identical between the two runs.
func TestAdmission_DecisionsIdenticalRegardlessOfMeter(t *testing.T) {
	type outcome struct {
		status int
		body   string
		ok     bool
	}

	runScenarios := func(t *testing.T) []outcome {
		t.Helper()
		rt := newMetricsTestRuntime(t)
		var outcomes []outcome

		record := func(w *httptest.ResponseRecorder, ok bool) {
			outcomes = append(outcomes, outcome{status: w.Code, body: w.Body.String(), ok: ok})
		}

		// 1. Admitted.
		w := httptest.NewRecorder()
		r := newWebhookMetricsRequest("/v1/webhooks/payments/t1/mock", "t1", "mock")
		release, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { return true }, nil)
		record(w, ok)
		if ok && release != nil {
			release()
		}

		// 2. A3 preauth rejection (burst already exhausted for this
		// domain's known-tenant bucket by scenario 1's admitted call,
		// since PreAuthRate burst is 1 here).
		w2 := httptest.NewRecorder()
		r2 := newWebhookMetricsRequest("/v1/webhooks/payments/t1/mock", "t1", "mock")
		release2, ok2 := rt.admitPreAuth(w2, r2, domainPayments, 0, func(string) bool { return true }, nil)
		record(w2, ok2)
		if ok2 && release2 != nil {
			release2()
		}

		// 3. A4a in-flight rejection: admit once (holding the slot open,
		// InFlightPerKey=1) on a distinct tenant key, then a second
		// concurrent admit on the same key must be rejected.
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

		// 4. B1 verified rejection (burst 1, two calls same key).
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

		// 5. Panic path: schemeRegistered panics inside admitPreAuth.
		w5 := httptest.NewRecorder()
		r5 := newWebhookMetricsRequest("/v1/webhooks/payments/tpanic/mock", "tpanic", "mock")
		panicRel, panicOk := rt.admitPreAuth(w5, r5, domainPayments, 0, func(string) bool { panic("boom") }, nil)
		record(w5, panicOk)
		if panicOk && panicRel != nil {
			panicRel()
		}

		// 6. db_gate rejection (writeDBGateUnavailable is a direct write,
		// not gated by any limiter state above).
		w6 := httptest.NewRecorder()
		r6 := newWebhookMetricsRequest("/v1/webhooks/payments/t6/mock", "t6", "mock")
		rt.writeDBGateUnavailable(w6, r6, domainPayments, nil, "mock")
		record(w6, false)

		return outcomes
	}

	baseline := runScenarios(t)

	failingReader := sdkmetric.NewManualReader()
	failingMP := sdkmetric.NewMeterProvider(sdkmetric.WithReader(failingReader))
	if err := failingReader.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failing reader: %v", err)
	}
	prevMP := otel.GetMeterProvider()
	otel.SetMeterProvider(failingMP)
	t.Cleanup(func() { otel.SetMeterProvider(prevMP) })

	underFailingExporter := runScenarios(t)

	if len(baseline) != len(underFailingExporter) {
		t.Fatalf("scenario count differs: %d vs %d", len(baseline), len(underFailingExporter))
	}
	for i := range baseline {
		if baseline[i] != underFailingExporter[i] {
			t.Fatalf("scenario %d differs between meter states:\n  real meter:    %+v\n  failing meter: %+v", i, baseline[i], underFailingExporter[i])
		}
	}
}

// TestAdmission_MetricsLabelsAndCounts drives each admission decision
// class through the real runtime and asserts
// webhook_admission_decisions_total increments with exactly the closed
// {decision,reason,provider_kind} label set - no tenant label, no raw
// provider/tenant identifier of any kind.
func TestAdmission_MetricsLabelsAndCounts(t *testing.T) {
	rt := newMetricsTestRuntime(t)

	before := collectMetrics(t)
	baseline := map[string]int64{}
	want := []map[string]string{
		{"decision": "admitted", "reason": "admitted", "provider_kind": "payments"},
		{"decision": "rejected", "reason": "preauth", "provider_kind": "payments"},
		{"decision": "rejected", "reason": "inflight", "provider_kind": "casino"},
		{"decision": "rejected", "reason": "verified", "provider_kind": "kyc"},
		{"decision": "rejected", "reason": "panic", "provider_kind": "payments"},
		{"decision": "rejected", "reason": "db_gate", "provider_kind": "payments"},
	}
	keyFor := func(m map[string]string) string { return m["decision"] + "|" + m["reason"] + "|" + m["provider_kind"] }
	for _, w := range want {
		v, _, _ := admissionSumFor(t, before, "webhook_admission_decisions_total", w)
		baseline[keyFor(w)] = v
	}

	// Admitted.
	w1 := httptest.NewRecorder()
	r1 := newWebhookMetricsRequest("/v1/webhooks/payments/m1/mock", "m1", "mock")
	rel1, ok1 := rt.admitPreAuth(w1, r1, domainPayments, 0, func(string) bool { return true }, nil)
	if !ok1 {
		t.Fatalf("expected admission")
	}
	rel1()

	// Preauth rejection (same tenant key, burst already spent).
	w2 := httptest.NewRecorder()
	r2 := newWebhookMetricsRequest("/v1/webhooks/payments/m1/mock", "m1", "mock")
	_, ok2 := rt.admitPreAuth(w2, r2, domainPayments, 0, func(string) bool { return true }, nil)
	if ok2 {
		t.Fatalf("expected preauth rejection")
	}

	// Inflight rejection.
	w3a := httptest.NewRecorder()
	r3a := newWebhookMetricsRequest("/v1/webhooks/casino/m2/mock", "m2", "mock")
	rel3a, ok3a := rt.admitPreAuth(w3a, r3a, domainCasino, 0, func(string) bool { return true }, nil)
	if !ok3a {
		t.Fatalf("expected first admit to succeed")
	}
	w3b := httptest.NewRecorder()
	r3b := newWebhookMetricsRequest("/v1/webhooks/casino/m2/mock", "m2", "mock")
	_, ok3b := rt.admitPreAuth(w3b, r3b, domainCasino, 0, func(string) bool { return true }, nil)
	if ok3b {
		t.Fatalf("expected inflight rejection")
	}
	rel3a()

	// Verified rejection.
	tenantID := uuid.New()
	w4a := httptest.NewRecorder()
	r4a := newWebhookMetricsRequest("/v1/webhooks/kyc/m3/mock", "m3", "mock")
	rel4a, ok4a := rt.admitVerified(w4a, r4a, domainKYC, tenantID, "mock", nil)
	if !ok4a {
		t.Fatalf("expected first verified admit to succeed")
	}
	w4b := httptest.NewRecorder()
	r4b := newWebhookMetricsRequest("/v1/webhooks/kyc/m3/mock", "m3", "mock")
	_, ok4b := rt.admitVerified(w4b, r4b, domainKYC, tenantID, "mock", nil)
	if ok4b {
		t.Fatalf("expected verified rejection")
	}
	rel4a()

	// Panic.
	w5 := httptest.NewRecorder()
	r5 := newWebhookMetricsRequest("/v1/webhooks/payments/m4/mock", "m4", "mock")
	_, ok5 := rt.admitPreAuth(w5, r5, domainPayments, 0, func(string) bool { panic("boom") }, nil)
	if ok5 {
		t.Fatalf("expected panic recovery to reject")
	}

	// db_gate.
	w6 := httptest.NewRecorder()
	r6 := newWebhookMetricsRequest("/v1/webhooks/payments/m5/mock", "m5", "mock")
	rt.writeDBGateUnavailable(w6, r6, domainPayments, nil, "mock")

	after := collectMetrics(t)
	for _, w := range want {
		v, found, keys := admissionSumFor(t, after, "webhook_admission_decisions_total", w)
		if !found {
			t.Fatalf("expected a data point for %v", w)
		}
		if v-baseline[keyFor(w)] != 1 {
			t.Fatalf("%v: delta = %d, want 1", w, v-baseline[keyFor(w)])
		}
		for _, forbidden := range []string{"tenant_id", "tenant_key", "provider_id", "provider_key", "domain"} {
			if keys[forbidden] {
				t.Fatalf("forbidden label %q present on webhook_admission_decisions_total for %v", forbidden, w)
			}
		}
		if len(keys) != 3 {
			t.Fatalf("label set not bounded to exactly {decision,reason,provider_kind}: %v", keys)
		}
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
