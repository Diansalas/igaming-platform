package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// The exposition must keep the exact series names the alert rules use: counters that already end in
// _total are not suffixed twice, an observable gauge keeps its name, and attribute keys are label keys.
func TestPrometheusExposition_NamesMatchTheAlertRules(t *testing.T) {
	meter, handler, shutdown, err := InitMetricsExposed(context.Background(), "platform-api-test", "none")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	c, err := meter.Int64Counter("expo_test_items_total")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(context.Background(), 3, metric.WithAttributes(attribute.String("result", "error")))
	if _, err := meter.Int64ObservableGauge("expo_test_last_pass_unix_seconds",
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(42); return nil })); err != nil {
		t.Fatal(err)
	}
	h, err := meter.Float64Histogram("expo_test_duration", metric.WithUnit("s"))
	if err != nil {
		t.Fatal(err)
	}
	h.Record(context.Background(), 0.2)

	body := scrape(t, handler)
	for _, want := range []string{
		`expo_test_items_total{`,
		`result="error"`,
		`expo_test_last_pass_unix_seconds`,
		`expo_test_duration_seconds_bucket`,
		`expo_test_duration_seconds_count`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "expo_test_items_total_total") {
		t.Error("counter suffix applied twice")
	}
}

func TestMetricsServer_ServesOnlyMetricsAndOnlyGET(t *testing.T) {
	_, handler, shutdown, err := InitMetricsExposed(context.Background(), "platform-api-test", "none")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	srv := NewMetricsServer("127.0.0.1:0", handler)
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatal("metrics server must bound every phase of a connection")
	}
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/metrics", http.StatusOK},
		{http.MethodHead, "/metrics", http.StatusOK},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/v1/me", http.StatusNotFound},
		{http.MethodGet, "/debug/pprof/", http.StatusNotFound},
		{http.MethodGet, "/metrics/extra", http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// A scrape must never block a slow reader forever and must not leak tenant or secret text: the resource
// carries only service.name.
func TestPrometheusExposition_NoResourceLabelsOtherThanTargetInfo(t *testing.T) {
	_, handler, shutdown, err := InitMetricsExposed(context.Background(), "platform-api-test", "none")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	done := make(chan string, 1)
	go func() { done <- scrape(t, handler) }()
	select {
	case body := <-done:
		for _, bad := range []string{"password", "secret", "token", "DATABASE_URL"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(bad)) {
				t.Errorf("exposition mentions %q", bad)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scrape did not complete")
	}
}
