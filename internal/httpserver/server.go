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
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// defaultWithdrawalApprovalThreshold is the four-eyes threshold (EUR-
// equivalent minor units, applied uniformly per asset for Stage 3B)
// above which a withdrawal requires two distinct human approvals rather
// than one (withdrawal-state-machine.md §5). Per-tenant/per-asset
// configurable thresholds are an explicit OPEN DECISION in that document,
// not resolved here - this is a safe, conservative default, not a final
// business policy.
const defaultWithdrawalApprovalThreshold = 100000 // e.g. EUR 1,000.00 at a 2-decimal exponent

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
	// WithdrawalApprovalThreshold overrides defaultWithdrawalApprovalThreshold
	// when non-zero.
	WithdrawalApprovalThreshold int64
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

	instrumented := otelhttp.NewHandler(mux, deps.ServiceName)

	return chain(instrumented,
		requestIDMiddleware,
		loggingMiddleware(deps.Logger),
		recoverMiddleware(deps.Logger),
	)
}
