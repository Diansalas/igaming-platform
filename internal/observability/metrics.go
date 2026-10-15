package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
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

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
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
