package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Shutdown stops all telemetry providers started by Init. Callers must
// defer it and pass a context with a reasonable timeout so buffered spans
// /metrics are flushed on process exit.
type Shutdown func(ctx context.Context) error

// InitTracing configures the global OpenTelemetry trace provider.
// exporterKind: "stdout" writes human-readable spans to stdout (Stage 1
// default); "none" installs a no-op provider (used in tests so test
// output isn't full of span dumps).
func InitTracing(ctx context.Context, serviceName, exporterKind string) (trace.Tracer, Shutdown, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("observability: build resource: %w", err)
	}

	if exporterKind == "none" {
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		return tp.Tracer(serviceName), func(ctx context.Context) error { return tp.Shutdown(ctx) }, nil
	}

	exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, nil, fmt.Errorf("observability: build stdout trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Tracer(serviceName), func(ctx context.Context) error { return tp.Shutdown(ctx) }, nil
}
