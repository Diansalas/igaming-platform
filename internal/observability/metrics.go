package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// InitMetrics configures the global OpenTelemetry meter provider,
// mirroring InitTracing's exporter selection. It exposes nothing over the
// network; InitMetricsExposed adds the Prometheus scrape handler.
func InitMetrics(ctx context.Context, serviceName, exporterKind string) (metric.Meter, Shutdown, error) {
	meter, _, shutdown, err := initMetrics(ctx, serviceName, exporterKind, false)
	return meter, shutdown, err
}

// httpServerView bounds the label set of every otelhttp server metric (SR9-A9-1). otelhttp records
// server.address and server.port from the client-controlled Host header, so an unauthenticated caller could
// mint series without limit until the SDK cardinality limit routes everything (including the 5xx and /readyz
// series the alert rules select on) into an overflow series with no status or route label. Only the four
// attributes below, all closed sets, are kept; everything else is dropped and aggregated away.
func httpServerView() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: "http.server.*"},
		sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(
			"http.request.method",
			"http.response.status_code",
			"http.route",
			"url.scheme",
		)},
	)
}

// initMetrics builds the meter provider; with expose it also attaches a Prometheus reader and returns
// the scrape handler (prometheus.go).
func initMetrics(ctx context.Context, serviceName, exporterKind string, expose bool) (metric.Meter, *scrapeHandler, Shutdown, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("observability: build resource: %w", err)
	}

	opts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithView(httpServerView())}
	if exporterKind != "none" {
		exp, err := stdoutmetric.New()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("observability: build stdout metric exporter: %w", err)
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	}
	var scrape *scrapeHandler
	if expose {
		scrape, err = newScrapeHandler()
		if err != nil {
			return nil, nil, nil, err
		}
		opts = append(opts, sdkmetric.WithReader(scrape.reader))
	}

	mp := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(mp)
	return mp.Meter(serviceName), scrape, func(ctx context.Context) error { return mp.Shutdown(ctx) }, nil
}
