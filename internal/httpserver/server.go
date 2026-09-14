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
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// Deps are the dependencies routes need. Kept as one small struct so
// New's signature doesn't grow a parameter per handler as endpoints are
// added.
type Deps struct {
	Logger      *slog.Logger
	DB          *db.Pool
	AuthIssuer  *auth.Issuer
	ServiceName string

	// AccessTokenTTL/RefreshTokenTTL control session lifetime. Short
	// access tokens limit the blast radius of a leaked one (nothing to
	// revoke - it just expires); the longer refresh token is what
	// actually gets revoked on logout/reuse-detection (internal/auth/
	// session.go).
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration

	// PaymentOrchestrator resolves/routes deposits and dispatches provider
	// callbacks (Stage 3B). Nil is treated as "financial routes
	// disabled" by registerFinancialRoutes, so services that don't need
	// them (e.g. a future read-only reporting deployment) aren't forced
	// to wire one up.
	PaymentOrchestrator *payments.Orchestrator

	// CasinoOrchestrator resolves a game's provider, mints/resolves
	// game-launch sessions, and dispatches provider bet/win/rollback
	// callbacks (Stage 4A). Nil disables every route that actually calls
	// it (catalogue listing, launch, the webhook, tenant capability
	// config) - each such handler guards explicitly, mirroring
	// PaymentOrchestrator's identical nil-means-disabled convention. The
	// platform-wide catalogue-upsert and tenant-availability-toggle
	// routes call casino package functions directly (never the
	// orchestrator) and remain reachable regardless: managing WHICH
	// titles exist and WHICH ones a tenant has opted into is catalogue
	// administration, not adapter dispatch, so it is deliberately not
	// gated by whether any adapter is currently registered.
	CasinoOrchestrator *casino.Orchestrator
}

// New builds the fully-wired http.Handler for platform-api: global
// middleware (request id -> logging -> panic recovery -> OTel span) then
// the route table.
func New(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", livezHandler)
	mux.HandleFunc("GET /readyz", readyzHandler(deps.DB))

	registerIdentityRoutes(mux, deps)
	registerFinancialRoutes(mux, deps)
	registerCasinoRoutes(mux, deps)

	instrumented := otelhttp.NewHandler(mux, deps.ServiceName)

	return chain(instrumented,
		requestIDMiddleware,
		loggingMiddleware(deps.Logger),
		recoverMiddleware(deps.Logger),
	)
}
