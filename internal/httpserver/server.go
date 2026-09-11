// Package httpserver assembles platform-api's HTTP server: the
// middleware chain (request id, logging, panic recovery, OpenTelemetry
// instrumentation), health/readiness endpoints, and the route table. It
// deliberately uses only the standard library's net/http (Go 1.22+
// method+pattern ServeMux) rather than a router dependency, per the
// human's explicit "prefer the simplest architecture that can credibly
// scale" instruction for Stage 1.
package httpserver

import (
	"log/slog"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// Deps are the dependencies routes need. Kept as one small struct so
// New's signature doesn't grow a parameter per handler as endpoints are
// added.
type Deps struct {
	Logger      *slog.Logger
	DB          *db.Pool
	AuthIssuer  *auth.Issuer
	ServiceName string
}

// New builds the fully-wired http.Handler for platform-api: global
// middleware (request id -> logging -> panic recovery -> OTel span) then
// the route table.
func New(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", livezHandler)
	mux.HandleFunc("GET /readyz", readyzHandler(deps.DB))

	// /v1/tenant-config is the Stage 1 foundation demonstration endpoint:
	// it proves the full chain - JWT verification, tenant-context
	// extraction, and RLS-enforced, tenant-scoped database access - works
	// end to end. It is not a Stage 4+ backoffice feature; see
	// docs/architecture/03-database-architecture.md.
	tenantConfigHandler := newTenantConfigHandler(deps.DB, deps.Logger)
	mux.Handle("GET /v1/tenant-config", auth.Middleware(deps.AuthIssuer)(tenantConfigHandler))

	instrumented := otelhttp.NewHandler(mux, deps.ServiceName)

	return chain(instrumented,
		requestIDMiddleware,
		loggingMiddleware(deps.Logger),
		recoverMiddleware(deps.Logger),
	)
}
