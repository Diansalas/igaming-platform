package httpserver

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
)

// maxRequestIDLen bounds a caller-supplied X-Request-Id so an
// unreasonably long or malformed value from an untrusted client can't
// bloat every log line for the request - if the inbound value looks
// wrong, we mint our own rather than trust it.
const maxRequestIDLen = 128

// requestIDMiddleware must run first in the chain (outermost): it
// creates the shared observability.RequestState that every later
// middleware and handler - including auth.Middleware, deeper in the
// chain - attaches information to (see observability.RequestState for
// why a shared pointer, not a plain context value, is required here). It
// reuses an inbound X-Request-Id if the caller supplied one (e.g. from an
// upstream load balancer) and it looks like a reasonable token, so a
// request can be correlated across services sharing that convention.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > maxRequestIDLen || !isPrintableASCII(id) {
			id = uuid.NewString()
		}
		rs := &observability.RequestState{RequestID: id}
		ctx := observability.WithRequestState(r.Context(), rs)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isPrintableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// statusRecorder captures the status code a handler wrote, so the
// logging middleware can report it after the handler returns (net/http's
// ResponseWriter doesn't expose what was written).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Unwrap lets http.NewResponseController reach the underlying writer (Flush for
// flushResponse, and SetReadDeadline for the ADR 0097 A5 body-read deadline,
// armBodyReadDeadline); without it the access-log wrapper hides both and they
// silently return ErrNotSupported.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// logPathFor returns the value the access log (and the panic-recovery
// line below) should record for r's path (RL-F4, ADR 0097 §8/§17 devops
// condition 3). admitPreAuth (webhook_admission.go) sets
// RequestState.LogPath to the MATCHED ROUTE PATTERN (r.Pattern, e.g.
// "POST /v1/webhooks/payments/{tenantSlug}/{providerID}") for every
// webhook request - never a path-prefix string match, which a crafted
// path could bypass - because the attacker-chosen {tenantSlug}/
// {providerID} segments are bounded only by MaxHeaderBytes. Every other
// route keeps logging the real r.URL.Path unchanged.
func logPathFor(r *http.Request) string {
	if rs := observability.RequestStateFromContext(r.Context()); rs != nil && rs.LogPath != "" {
		return rs.LogPath
	}
	return r.URL.Path
}

// loggingMiddleware emits one structured log line per request. This is
// intentionally simple (method, path, status, duration, request/tenant
// id) - richer request/response tracing comes from OpenTelemetry spans,
// not from log volume.
func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			observability.LoggerFromContext(r.Context(), logger).Info("http_request",
				"method", r.Method,
				"path", logPathFor(r),
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// recoverMiddleware converts a panic in any downstream handler into a
// clean 500 response instead of crashing the process or leaking a stack
// trace to the caller. The panic detail is logged server-side only.
func recoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					requestID := observability.RequestIDFromContext(r.Context())
					observability.LoggerFromContext(r.Context(), logger).Error("panic_recovered",
						"panic", rec,
						"path", logPathFor(r),
					)
					apierror.Write(w, requestID, apierror.CodeInternal, "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// chain applies middlewares in the order given, so chain(a, b, c)(h)
// executes a, then b, then c, then h - matching the order they read in
// source.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
