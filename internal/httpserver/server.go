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
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
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

	// CasinoPlaySimulationEnabled gates the three Stage 7 mock-provider
	// play-simulation routes (POST /v1/me/casino/sessions/{id}/wager|win|
	// rollback - casino_play_handlers.go). These exist only because no
	// real, hosted casino game client exists yet: each one asks the
	// tenant's own registered *casino.MockCasinoProvider to mint a
	// self-signed callback payload on the AUTHENTICATED PLAYER's behalf
	// and feeds it through the real Orchestrator.ReceiveCallback pipeline
	// - which means the payload's signature authenticates nothing (the
	// platform signs it for the player), unlike a real provider webhook's
	// signature. Two independent specialist reviews (architect,
	// ledger-finance) both flagged this as a P0/architectural trust-
	// boundary inversion that must never reach a real deployment "because
	// we haven't wired it into anything yet" - the defense has to be in
	// the code, not an assumption about what gets deployed. Defaults to
	// the zero value (false, disabled) exactly like SportsbookEnabled;
	// cmd/platform-api/main.go sets it only outside production.
	CasinoPlaySimulationEnabled bool

	// SportsbookEnabled gates every Stage 6 sportsbook route (catalogue
	// browse, bet placement, bet history, admin visibility) - unlike
	// PaymentOrchestrator/CasinoOrchestrator (a provider-adapter registry),
	// internal/sportsbook.PlaceBet needs no adapter registry of its own
	// this stage (no external provider round-trip occurs at placement
	// time - see internal/sportsbook's own package doc comment), so this
	// is a plain bool rather than an orchestrator pointer. false disables
	// every sportsbook route (mirrors PaymentOrchestrator/
	// CasinoOrchestrator's identical nil-means-disabled convention).
	SportsbookEnabled bool

	// PersonResolver is Stage 4E's identity-resolution boundary, consulted
	// by newRegisterHandler BEFORE a Person is ever created - unlike
	// PaymentOrchestrator/CasinoOrchestrator, this is NOT "nil means the
	// feature is disabled": registration is the one route that must never
	// silently bypass identity resolution again (that was Stage 4D-RG's
	// own P0 finding). A nil PersonResolver makes the register handler
	// fail closed (503), never fall through to Stage 2's old
	// resolution-blind behavior. Every production and test Deps
	// construction is expected to set this - internal/identityresolution.
	// NewMockPersonResolver() until a real vendor is contracted (ADR 0027).
	PersonResolver identityresolution.PersonResolver

	// KYCOrchestrator/DocumentStorage/MalwareScanner are Stage 4F's
	// verification/document boundary. Nil disables every route that
	// calls them (mirrors PaymentOrchestrator/CasinoOrchestrator's
	// nil-means-disabled convention, NOT PersonResolver's fail-closed
	// one) - unlike identity resolution, KYC verification is a genuinely
	// optional/deferred flow no existing endpoint depends on, so a
	// deployment that hasn't wired one up simply doesn't expose these
	// routes' functionality, rather than failing registration/login.
	KYCOrchestrator *kyc.Orchestrator
	DocumentStorage kyc.DocumentStorageProvider
	MalwareScanner  kyc.MalwareScanner

	// EmailProvider is Stage 4F's email-delivery boundary (email
	// verification, password reset). Nil-means-disabled for the
	// player-facing request endpoints (they return 503) - never silently
	// skipped, since a player who thinks a verification/reset email was
	// sent when it wasn't is a worse outcome than an explicit, honest
	// "temporarily unavailable".
	EmailProvider email.Provider

	// AuthRateLimitPerMinute overrides the per-IP limit New applies to the
	// unauthenticated credential endpoints (see ratelimit.go for exactly
	// what is limited and why):
	//
	//	0  - use ratelimit.go's per-bucket defaults (the normal case).
	//	>0 - use this value for EVERY bucket instead.
	//	<0 - disable the limiter entirely.
	//
	// Plumbed end to end from the AUTH_RATE_LIMIT_PER_MINUTE environment
	// variable via internal/config.Config and cmd/platform-api/main.go
	// (closes S9.1-LAUNCH-2, docs/security/security-architecture.md) - an
	// operator can raise or disable these limits without a code change.
	AuthRateLimitPerMinute int

	// TrustedProxyCount is the number of the platform's OWN trusted
	// reverse-proxy hops sitting in front of this instance (a load
	// balancer, an ingress controller, etc - infrastructure this
	// deployment controls, never an arbitrary intermediary). It controls
	// how the rate limiter (ratelimit.go) determines caller identity:
	//
	//	0   - X-Forwarded-For is never read for rate-limiting purposes;
	//	      RemoteAddr is the caller's identity, unconditionally. This is
	//	      the safe default for an unconfigured deployment: an attacker
	//	      cannot spoof RemoteAddr across a completed TCP handshake, but
	//	      CAN set X-Forwarded-For to anything, so trusting it with no
	//	      known hop count to validate against would make the limiter
	//	      trivially bypassable (a fresh forged header = a fresh bucket).
	//	N>0 - this deployment's own proxy chain is N hops deep; the real
	//	      client's address is taken from the Nth-from-the-right entry
	//	      of X-Forwarded-For (trustedProxyClientIP's own doc comment
	//	      has the full reasoning for why counting from the right, not
	//	      the left, is what makes the header safe to use at all: the
	//	      trusted hops only ever APPEND to the right, so nothing a
	//	      client injects on the left can shift which entry is trusted).
	//
	// Plumbed from the TRUSTED_PROXY_COUNT environment variable via
	// internal/config.Config and cmd/platform-api/main.go (closes
	// S9.1-LAUNCH-1, docs/security/security-architecture.md). Set this to
	// EXACTLY the number of proxy hops this deployment controls - setting
	// it too high lets a client's own injected X-Forwarded-For entry be
	// mistaken for the trusted one; setting it too low (including leaving
	// it at the default 0 behind a real proxy) collapses every caller to
	// one shared bucket. This platform never builds a distributed/shared-
	// state rate limiter (Redis or otherwise) - each replica still
	// enforces its own independent in-memory window; see ratelimit.go's
	// own "WHAT THIS IS NOT" section and docs/architecture/
	// 38-deployment-architecture.md §4 for that accepted limitation.
	TrustedProxyCount int

	// authLimiter is built by New from AuthRateLimitPerMinute/
	// TrustedProxyCount and shared by every rate-limited route. Unexported
	// deliberately: a caller configures the POLICY (above), never hands in
	// its own limiter instance.
	authLimiter *fixedWindowLimiter
}

// New builds the fully-wired http.Handler for platform-api: global
// middleware (request id -> logging -> panic recovery -> OTel span) then
// the route table.
func New(deps Deps) http.Handler {
	mux := http.NewServeMux()

	// Stage 9 §21: one limiter shared by every rate-limited route, built
	// here so no caller can forget to wire it and no test server silently
	// runs without it. See ratelimit.go.
	deps.authLimiter = newFixedWindowLimiter(rateLimitWindow, deps.AuthRateLimitPerMinute, deps.TrustedProxyCount)

	mux.HandleFunc("GET /healthz", livezHandler)
	mux.HandleFunc("GET /readyz", readyzHandler(deps.DB))

	registerIdentityRoutes(mux, deps)
	registerFinancialRoutes(mux, deps)
	registerCasinoRoutes(mux, deps)
	registerSportsbookRoutes(mux, deps)
	registerRGRoutes(mux, deps)
	registerCredentialRoutes(mux, deps)
	registerKYCRoutes(mux, deps)
	registerRiskRoutes(mux, deps)
	registerAssetRegistryRoutes(mux, deps)
	registerBonusRoutes(mux, deps)
	registerJurisdictionRoutes(mux, deps)

	instrumented := otelhttp.NewHandler(mux, deps.ServiceName)

	return chain(instrumented,
		requestIDMiddleware,
		loggingMiddleware(deps.Logger),
		recoverMiddleware(deps.Logger),
	)
}
