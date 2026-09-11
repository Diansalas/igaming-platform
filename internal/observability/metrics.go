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
// mirroring InitTracing's exporter selection.
func InitMetrics(ctx context.Context, serviceName, exporterKind string) (metric.Meter, Shutdown, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("observability: build resource: %w", err)
	}

	if exporterKind == "none" {
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res))
		otel.SetMeterProvider(mp)
		return mp.Meter(serviceName), func(ctx context.Context) error { return mp.Shutdown(ctx) }, nil
	}

	exp, err := stdoutmetric.New()
	if err != nil {
		return nil, nil, fmt.Errorf("observability: build stdout metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	return mp.Meter(serviceName), func(ctx context.Context) error { return mp.Shutdown(ctx) }, nil
}
