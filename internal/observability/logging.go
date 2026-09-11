// Package observability wires structured logging, tracing, and metrics
// for platform-api. Stage 1 foundation: real OpenTelemetry SDK wiring
// with a stdout exporter (no external collector required yet). A
// production-grade OTLP exporter/backend is introduced when there is a
// concrete backend to send to (PROVIDER DEPENDENT).
package observability

import (
	"context"
	"log/slog"
	"os"
)

// contextKey avoids collisions with keys from other packages in
// context.Context.
type contextKey string

const requestStateKey contextKey = "request_state"

// RequestState is a per-request record shared across the whole
// middleware chain via a pointer stored in context, rather than plain
// context values. That distinction matters here specifically because of
// how the chain is structured: requestIDMiddleware (outermost) creates
// this record before the access-log middleware runs, but the tenant id
// is only known once auth.Middleware verifies the caller's JWT -
// deeper in the chain, and only on routes that require auth at all (health
// checks don't). A plain context.WithValue call from auth.Middleware
// would attach the tenant id to a *new* context that only its own
// downstream handler sees; the outer access-log middleware, which
// captured its own context reference before calling next.ServeHTTP,
// would never observe it. Sharing one mutable struct by pointer means
// auth.Middleware's update is visible to every other holder of the same
// pointer, including the outer logger, once the handler chain unwinds
// and the log line is written.
type RequestState struct {
	RequestID string
	TenantID  string
}

// NewLogger returns a JSON structured logger. JSON output (rather than
// text) is required from day one because logs are expected to be
// machine-parsed by the observability stack from Stage 1 onward - never
// grep-and-hope.
func NewLogger(env string) *slog.Logger {
	level := slog.LevelInfo
	if env == "development" {
		level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(handler)
}

// WithRequestState attaches the request's shared state record. Only
// requestIDMiddleware (internal/httpserver) should call this - it must
// run first in the chain so every other middleware and handler can find
// the same record via RequestStateFromContext.
func WithRequestState(ctx context.Context, rs *RequestState) context.Context {
	return context.WithValue(ctx, requestStateKey, rs)
}

// RequestStateFromContext returns the shared request state, or nil if
// none is attached (e.g. a unit test calling a handler directly without
// running the full middleware chain).
func RequestStateFromContext(ctx context.Context) *RequestState {
	rs, _ := ctx.Value(requestStateKey).(*RequestState)
	return rs
}

// RequestIDFromContext returns the request id stored on the context's
// RequestState, or "" if none is set.
func RequestIDFromContext(ctx context.Context) string {
	if rs := RequestStateFromContext(ctx); rs != nil {
		return rs.RequestID
	}
	return ""
}

// SetTenantIDForLogging records the resolved tenant id on the request's
// shared state so every log line for this request - including ones
// written by middleware that captured its context reference earlier in
// the chain, like the access logger - includes it once available. This
// is for log correlation ONLY - it is never the authorization mechanism;
// authoritative tenant context comes from internal/tenant, derived from
// the verified JWT.
func SetTenantIDForLogging(ctx context.Context, tenantID string) {
	if rs := RequestStateFromContext(ctx); rs != nil {
		rs.TenantID = tenantID
	}
}

// LoggerFromContext returns a logger enriched with whatever the request's
// shared state currently holds (request id, and tenant id once resolved),
// so every log line from a single request can be correlated without
// threading fields manually through every function call.
func LoggerFromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	rs := RequestStateFromContext(ctx)
	if rs == nil {
		return base
	}
	l := base
	if rs.RequestID != "" {
		l = l.With("request_id", rs.RequestID)
	}
	if rs.TenantID != "" {
		l = l.With("tenant_id", rs.TenantID)
	}
	return l
}
