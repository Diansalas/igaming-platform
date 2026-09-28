// Package httpserver assembles platform-api's HTTP server: the
// middleware chain (request id, logging, panic recovery, OpenTelemetry
// instrumentation), health/readiness endpoints, and the route table. It
// deliberately uses only the standard library's net/http (Go 1.22+
// method+pattern ServeMux) rather than a router dependency, per the
// human's explicit "prefer the simplest architecture that can credibly
// scale" instruction for Stage 1.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// Deps are the dependencies routes need. Kept as one small struct so
// New's signature doesn't grow a parameter per handler as endpoints are
// added.
type Deps struct {
	Logger      *slog.Logger
	DB          *db.Pool
	AuthIssuer  *auth.Issuer
	ServiceName string

	// AuditPresentationResolver (ADR 0104 §5.3) resolves the tenant
	// platform-actions audit projection's presentation rules. nil means
	// "use audit.ResolvePresentation" (the production compiled-in
	// default) - production wiring never sets this field. Tests may
	// inject a resolver, including one that errors, to exercise the
	// fail-closed path (audit.ResolvePresentationOrRestrictive) without
	// any production code change.
	AuditPresentationResolver audit.PresentationResolver

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

	// PaymentsOutboundCredentials is InitiateDepositAttempt's phase B
	// credential resolver (ADR 0095 §9.1/§11, PROV-OUTBOUND-CRED-1,
	// PRH-I1 deposit cutover) - CasinoOutboundCredentials' payments twin.
	// Nil fails every deposit initiation closed (InitiateDepositAttempt's
	// own gate.go check), never falling back to an unauthenticated or
	// cached credential.
	PaymentsOutboundCredentials payments.OutboundCredentialResolver

	// PaymentsMockSettlementEnabled gates the Stage 9.3 mock-provider
	// deposit-settlement-simulation route (POST /v1/me/deposits/{id}/
	// simulate-callback - payment_deposit_simulation_handlers.go). This
	// exists only because there is no real, hosted PSP to deliver a
	// genuine webhook against a live, HTTP-only staging process: the mock
	// adapter's HMAC signing secret is generated in-process and never
	// exposed via any API (internal/payments/mock.go's own doc comment -
	// that unrecoverability is deliberate and must not change), so the
	// only way to complete a mock deposit through the real HTTP surface
	// alone is to ask the tenant's own registered *payments.MockProvider
	// to mint a correctly-signed callback payload on the AUTHENTICATED
	// PLAYER's own behalf and feed it through the real
	// Orchestrator.ReceiveCallback pipeline - mirrors
	// CasinoPlaySimulationEnabled's identical rationale below byte for
	// byte, including its trust-boundary caveat: the payload's signature
	// authenticates nothing about which deposit/amount/outcome is being
	// named (the platform signs it for the player), unlike a real
	// provider webhook's signature, so every identifying/effect-bearing
	// field the handler builds the payload from must come from the
	// caller's OWN, already-created, still-pending deposit intent row -
	// never from this request's body. Defaults to the zero value (false,
	// disabled) exactly like CasinoPlaySimulationEnabled;
	// cmd/platform-api/main.go sets it only outside production.
	PaymentsMockSettlementEnabled bool

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

	// CasinoOutboundCredentials is the casino domain's outbound-credential
	// resolver (ADR 0095 §15.1/§9.1, PROV-OUTBOUND-CRED-1): a MOCK synthetic
	// credential behind a synthetic/MOCK provider adapter, or the real
	// provider-credential subsystem's Outbound("casino") once a real
	// adapter exists. Nil fails every real launch closed, mirroring every
	// other nil-resolver convention in this struct - never a silent
	// fallback to an unauthenticated or cached credential.
	CasinoOutboundCredentials casino.OutboundCredentialResolver

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

	// SportsbookSettlementSimulationEnabled gates the Stage 10 W1
	// (docs/decisions/0088 §9) non-production test-support staff route
	// (POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event -
	// sportsbook_settlement_handlers.go). This is IN-HOUSE MOCK MODE only:
	// there is no real sportsbook settlement provider or webhook, so this
	// route is the only way to drive a bet through settle/void/rollback
	// end to end, simulating what a real provider would eventually tell
	// the platform. It is registered only when this AND SportsbookEnabled
	// are both true (registerSportsbookRoutes) - otherwise the pattern is
	// never added to the mux at all, mirroring CasinoPlaySimulationEnabled/
	// PaymentsMockSettlementEnabled/AccountActivationTestSupportEnabled's
	// identical Stage 9.4 two-layer, fail-closed gate exactly. Defaults to
	// the zero value (false, disabled); cmd/platform-api/main.go sets it
	// only outside production, from cfg.TestSupportRoutesEnabled(). Never
	// a player capability (auth.RequireStaffPrincipal), never reachable
	// without auth.PermSportsbookSettlementSimulate (sole grantee:
	// RoleRiskManager). Removal condition (ADR 0088 §9.1): once a real
	// sportsbook settlement provider is registered for any tenant, this
	// route is removed or permanently disabled in the same change.
	SportsbookSettlementSimulationEnabled bool

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

	// KYCOutboundCredentials is the KYC domain's outbound-credential
	// resolver (ADR 0095 §15.2/§15.3/§9.1, PROV-OUTBOUND-CRED-1) - a MOCK
	// synthetic credential behind a synthetic/MOCK provider adapter, or the
	// real provider-credential subsystem's Outbound("kyc") once a real
	// adapter exists. Nil fails every CreateVerification/SubmitVerification
	// call closed, mirroring CasinoOutboundCredentials' identical
	// nil-resolver convention - never a silent fallback to an
	// unauthenticated or cached credential.
	KYCOutboundCredentials kyc.OutboundCredentialResolver

	// KYCWebhookEnabled gates registration of the KYC provider-callback
	// route (POST /v1/webhooks/kyc/{tenantSlug}/{providerID}) - Stage
	// 10.2, ADR 0091, architect ruling R5/J7: KYC-WH-1 found this route
	// registered unconditionally regardless of KYCOrchestrator's origin.
	// registerKYCRoutes registers the route only when this AND
	// KYCOrchestrator != nil both hold (mirrors
	// SportsbookSettlementSimulationEnabled's identical two-layer,
	// fail-closed gate exactly) - false means the mux never has this
	// pattern at all, so an unauthenticated caller gets a genuine 404,
	// not a 503 from inside a registered handler. cmd/platform-api/main.go
	// sets this to the SAME cfg.TestSupportRoutesEnabled() value that
	// decided whether KYCOrchestrator itself got a MOCK provider/resolver
	// in the first place (cmd/platform-api/wiring.go's mockProviderWiring)
	// - one function decides both, so they cannot diverge (K11).
	KYCWebhookEnabled bool

	// ProviderCredentials is the real provider-credential subsystem (Stage
	// 10.3 W2a, ADR 0093). Nil when it is not constructed (no fingerprint
	// key or no permitted secret-store backend): the request, decision and
	// apply routes are then NOT mounted, while listing and single-actor
	// transitions (revocation) stay available.
	ProviderCredentials *providercred.Subsystem

	// EmailProvider is Stage 4F's email-delivery boundary (email
	// verification, password reset). Nil-means-disabled for the
	// player-facing request endpoints (they return 503) - never silently
	// skipped, since a player who thinks a verification/reset email was
	// sent when it wasn't is a worse outcome than an explicit, honest
	// "temporarily unavailable".
	EmailProvider email.Provider

	// AccountActivationTestSupportEnabled gates Stage 9.3/9.4's
	// account-activation test-support seam - the identity twin of
	// CasinoPlaySimulationEnabled/PaymentsMockSettlementEnabled above,
	// closing the identical shape of gap for identity instead of
	// casino/payments: email.MockProvider deliberately never exposes a
	// sent message's raw token via any API (its own doc comment - there is
	// no real inbox to deliver a mock email to), so a player registered
	// through the real HTTP API alone can never learn their own
	// email-verification token and can therefore never reach 'active'
	// status, which every financial/gambling gate
	// (internal/rg.EvaluateEligibility) requires.
	//
	// Stage 9.4 (replacing Stage 9.3's separate GET /v1/me/
	// email-verification/dev-token route and its in-memory,
	// per-process devVerificationTokenStore, which a Stage 9.3 security
	// review diagnosed as not safe under a multi-replica deployment - a
	// token recorded in one replica's memory is invisible to a request
	// that lands on a different replica): when this flag is true, the
	// EXISTING POST /v1/me/email-verification/request endpoint
	// (newRequestEmailVerificationHandler, credential_handlers.go) itself
	// returns the raw token in its own response body
	// (200 {"token": "..."} instead of 204) at the exact moment it
	// already holds the raw value in memory, immediately before handing
	// it to EmailProvider.Send. This is stateless and trivially
	// replica-safe: the client receives the token in the SAME HTTP
	// response that requested it, from whichever replica handled that
	// one request - there is no second, follow-up request to a
	// potentially different replica at all. The token is then fed into
	// the completely UNMODIFIED POST /v1/auth/email-verification/confirm
	// endpoint exactly as before (token hashing, expiry, and single-use
	// consumption are all untouched, and already correctly replica-safe
	// via the shared Postgres player_credential_tokens table). Defaults
	// to the zero value (false, disabled) exactly like the two
	// precedents above; cmd/platform-api/main.go sets it only when BOTH
	// `Environment != "production"` AND TestSupportEndpointsEnabled are
	// true (internal/config.Config's own doc comments have the full
	// two-layer rationale).
	//
	// SECURITY-SENSITIVE: this changes WHERE a credential-adjacent raw
	// token is exposed. Per CLAUDE.md, it requires explicit `security`
	// specialist review before being marked complete.
	AccountActivationTestSupportEnabled bool

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

	// CORSAllowedOrigins is the exact-match allowlist of browser Origins
	// permitted to make cross-origin requests to this API - see cors.go's
	// corsMiddleware for the full model. Empty (the default) disables CORS
	// entirely (a complete no-op), which is correct for every existing
	// dev/CI/test setup and for a same-origin production deployment.
	// Plumbed from the CORS_ALLOWED_ORIGINS environment variable via
	// internal/config.Config and cmd/platform-api/main.go.
	CORSAllowedOrigins []string

	// authLimiter is built by New from AuthRateLimitPerMinute/
	// TrustedProxyCount and shared by every rate-limited route. Unexported
	// deliberately: a caller configures the POLICY (above), never hands in
	// its own limiter instance.
	authLimiter *fixedWindowLimiter

	// WebhookAdmission is ADR 0097's (PAYWH-RL-1) transport-layer webhook
	// admission/rate-limiting configuration for the three provider-facing
	// webhook routes. The zero value (Enabled: false) is a complete no-op -
	// every existing test/deployment that doesn't set this field keeps
	// today's behaviour exactly. cmd/platform-api/main.go maps
	// config.WebhookAdmissionConfig into this field by field (internal/
	// httpserver never imports internal/config, per architect review AC1
	// "internal/config produces plain values; httpserver maps them to
	// admission types").
	WebhookAdmission WebhookAdmissionSettings

	// webhookAdmission is the constructed runtime (limiters, bulkheads,
	// tenant directory) New builds from WebhookAdmission. Unexported: a
	// caller configures policy, never hands in its own runtime. Exposed to
	// cmd/platform-api/main.go (for the synchronous initial directory load,
	// the background refresher, and /readyz gating) via WebhookAdmissionRuntime,
	// below.
	webhookAdmission *webhookAdmissionRuntime
}

// WebhookAdmissionRuntime is the subset of the constructed ADR 0097
// runtime cmd/platform-api/main.go needs: the synchronous initial
// directory load (§7), the background refresher (wired to the same
// shutdown context as http.Server.Shutdown - devops condition 2), and
// readiness gating. New returns this alongside the handler so main.go
// never constructs its own, second copy of the admission state.
type WebhookAdmissionRuntime struct {
	rt *webhookAdmissionRuntime
}

// LoadDirectory performs the synchronous initial tenant-directory load.
// No-op if admission is disabled.
func (r WebhookAdmissionRuntime) LoadDirectory(ctx context.Context) error {
	return r.rt.LoadDirectory(ctx)
}

// RunDirectoryRefresh runs the background refresher until ctx is
// cancelled. Call this in its own goroutine. No-op if admission is
// disabled.
func (r WebhookAdmissionRuntime) RunDirectoryRefresh(ctx context.Context) {
	r.rt.RunDirectoryRefresh(ctx)
}

// DirectoryReady reports whether the tenant directory has completed at
// least one successful load (used by /readyz). Always true if admission
// is disabled (nothing to gate on).
func (r WebhookAdmissionRuntime) DirectoryReady() bool {
	d := r.rt.Directory()
	if d == nil {
		return r.rt == nil // disabled -> nothing to wait for; enabled-but-no-pool -> not ready
	}
	return d.Loaded()
}

// New builds the fully-wired http.Handler for platform-api: global
// middleware (request id -> logging -> panic recovery -> OTel span) then
// the route table. Callers that need the ADR 0097 webhook-admission
// runtime (the synchronous initial directory load, its background
// refresher, and /readyz gating - cmd/platform-api/main.go) use
// NewWithAdmission instead; New itself is unchanged so every existing
// caller/test keeps compiling and behaving identically.
func New(deps Deps) http.Handler {
	h, _ := NewWithAdmission(deps)
	return h
}

// NewWithAdmission is New, additionally returning the constructed webhook-
// admission runtime so the caller can perform the synchronous initial
// tenant-directory load, start its background refresher (wired to the
// caller's own shutdown context - devops condition 2), and gate /readyz
// on it.
func NewWithAdmission(deps Deps) (http.Handler, WebhookAdmissionRuntime) {
	mux := http.NewServeMux()

	// Stage 9 §21: one limiter shared by every rate-limited route, built
	// here so no caller can forget to wire it and no test server silently
	// runs without it. See ratelimit.go.
	deps.authLimiter = newFixedWindowLimiter(rateLimitWindow, deps.AuthRateLimitPerMinute, deps.TrustedProxyCount)

	// ADR 0097 PRH-I4: built once, shared by every webhook route. nil
	// (WebhookAdmission.Enabled == false, the zero value) makes every
	// admission call in this package a no-op, preserving pre-PRH-I4
	// behaviour exactly.
	deps.webhookAdmission = newWebhookAdmission(deps.WebhookAdmission, deps.DB, deps.Logger)
	admissionRuntime := WebhookAdmissionRuntime{rt: deps.webhookAdmission}

	mux.HandleFunc("GET /healthz", livezHandler)
	mux.HandleFunc("GET /readyz", readyzHandler(deps.DB, admissionRuntime))

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
	registerProviderCredentialRoutes(mux, deps)
	registerPaymentsKillSwitchRoutes(mux, deps)

	instrumented := otelhttp.NewHandler(mux, deps.ServiceName)

	return chain(instrumented,
		corsMiddleware(deps.CORSAllowedOrigins),
		requestIDMiddleware,
		loggingMiddleware(deps.Logger),
		recoverMiddleware(deps.Logger),
	), admissionRuntime
}
