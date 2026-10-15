package observability

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// scrapeHandler couples the OTel Prometheus reader with the private registry it feeds.
type scrapeHandler struct {
	reader  sdkmetric.Reader
	handler http.Handler
}

// newScrapeHandler builds a PRIVATE Prometheus registry (never the process-global default one, so no
// third-party package can register a collector into the exposition) fed by the OpenTelemetry meter
// provider, plus the Go runtime and process collectors. Translation keeps instrument names unchanged
// (the platform's counters already end in _total and are not suffixed twice; a unit "s" histogram
// becomes <name>_seconds), which is what deploy/observability/prometheus-alert-rules.yml assumes and
// deploy/observability's parity test proves.
func newScrapeHandler() (*scrapeHandler, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	exp, err := otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutScopeInfo())
	if err != nil {
		return nil, fmt.Errorf("observability: build prometheus exporter: %w", err)
	}
	return &scrapeHandler{
		reader: exp,
		handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorHandling:       promhttp.HTTPErrorOnError,
			Timeout:             8 * time.Second,
			MaxRequestsInFlight: 4,
		}),
	}, nil
}

// InitMetricsExposed is InitMetrics plus a Prometheus-format scrape handler over the same meter
// provider. The handler is NOT mounted anywhere by this package: cmd/platform-api serves it on a
// SEPARATE listener (METRICS_ADDR) via NewMetricsServer, never on the public API mux.
func InitMetricsExposed(ctx context.Context, serviceName, exporterKind string) (metric.Meter, http.Handler, Shutdown, error) {
	meter, scrape, shutdown, err := initMetrics(ctx, serviceName, exporterKind, true)
	if err != nil {
		return nil, nil, nil, err
	}
	return meter, scrape.handler, shutdown, nil
}

// metricsOnly serves exactly "/metrics" for GET and HEAD and nothing else. It is deliberately a plain handler
// function, not a ServeMux: there is no route table to extend, and no route registration outside internal/httpserver
// (a repository static test pins that route registration lives only in the API package).
func metricsOnly(metrics http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		metrics.ServeHTTP(w, r)
	})
}

// NewMetricsServer returns the dedicated metrics http.Server: GET and HEAD on exactly /metrics, 404 for
// every other path, no other handler of any kind (no pprof, no API route), and bounded header, read,
// write and idle times. It performs no authentication: exposure is controlled by the bind address and
// the network (docs/runbooks/observability-and-alerting.md), and config refuses a non-loopback bind in
// production without an explicit acknowledgement.
func NewMetricsServer(addr string, metrics http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           metricsOnly(metrics),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
}
