// Package config loads platform-api configuration from environment
// variables. There is no config file support by design — secrets and
// per-environment values come from the environment (or, in later stages,
// a secrets manager that injects environment variables), never from a
// file checked into the repository.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full set of configuration platform-api needs to start.
// Every field is sourced from an environment variable; there are no
// hidden defaults for anything security- or correctness-relevant (the
// JWT signing keys and database URL have no default and fail startup if
// unset).
type Config struct {
	// Environment is "development", "staging", or "production". It is
	// informational (used in logs/traces) and must never gate a security
	// control - environments are otherwise identically configured.
	//
	// Three deliberate, reviewed exceptions exist - the sentence above is
	// the DEFAULT rule, not an invariant, and this list is exhaustive:
	//
	//  1. db.VerifyRuntimeRoleInProduction (PLAT-ROLESPLIT-1,
	//     docs/security/runtime-role-separation.md) gates a fail-closed
	//     startup check on Environment == "production" specifically,
	//     because development/CI legitimately and intentionally connect as
	//     the database's table-owning role today (a large share of this
	//     repo's integration suite requires owner/DDL privileges to run
	//     migration mechanics at all), while production must never do so.
	//     See that function's own doc comment for the full reasoning.
	//  2. httpserver.Deps.CasinoPlaySimulationEnabled (Stage 7).
	//  3. httpserver.Deps.PaymentsMockSettlementEnabled (Stage 9.3).
	//  4. httpserver.Deps.AccountActivationTestSupportEnabled (Stage 9.3).
	//  5. httpserver.Deps.SportsbookSettlementSimulationEnabled (Stage 10
	//     W1, ADR 0088 §9) - gates the non-production test-support
	//     `POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event`
	//     route the same way, added to the existing gate rather than a
	//     new one.
	//
	// (1) fails CLOSED on a mis-set value (a typo just re-enables a check
	// production should pass anyway). (2), (3), (4) and (5) used to (or, for
	// (5), would have) fail OPEN on a typo or an unset variable, per two
	// independent Stage 9.3 security reviews - Load() computed Environment
	// from an unvalidated, open string, and cmd/platform-api/main.go gated
	// all of these simulation flags purely on `cfg.Environment !=
	// "production"`, so "Production"/"prod"/"production " (trailing space)
	// or an unset APP_ENV all silently REGISTERED the test-support routes on
	// what the operator believed was a production deployment. Stage 9.4
	// closed this with a two-layer, fail-closed design (both layers are
	// independently required - a single mistake in either one can never
	// register a test-support route on a real production deployment):
	//
	//   - Layer 1 (this field): Load() now validates Environment against
	//     the exact closed set {"development", "staging", "production"}
	//     whenever APP_ENV is explicitly set to ANY value, including an
	//     explicit empty string (`APP_ENV=`) - only a variable that is
	//     genuinely absent from the environment (os.LookupEnv's `ok ==
	//     false`) resolves to the "development" default; a present-but-
	//     empty value is treated as an explicit, invalid setting and fails
	//     Load() outright, the same as a typo or stray whitespace would.
	//     This closes the one gap an earlier version of Layer 1 left open:
	//     folding "APP_ENV unset" and "APP_ENV explicitly empty" into the
	//     same default (as a naive getEnvDefault-style helper does) would
	//     let a future orchestration tool that ever emits an empty string
	//     for an unset variable (rather than omitting it) silently resolve
	//     to "development" and pass Layer 1 undetected. Leaving APP_ENV
	//     UNSET is unchanged and still defaults to "development" (existing,
	//     tested local/CI convenience - many of this repo's own tests and
	//     local dev flows rely on never having to set it) - but any REAL
	//     deployment (staging or production) MUST set APP_ENV explicitly;
	//     relying on the unset default in a real environment is an operator
	//     error this field cannot detect by itself, precisely because
	//     "unset" and "deliberately development" are indistinguishable once
	//     resolved.
	//   - Layer 2 (TestSupportEndpointsEnabled, below): a second,
	//     independent, explicit opt-in that must ALSO be true before any
	//     of the three flags actually register a route.
	//     cmd/platform-api/main.go now computes each flag as
	//     `cfg.Environment != "production" && cfg.TestSupportEndpointsEnabled`
	//     - a wrong/typoed Environment value alone can no longer register
	//     anything (Layer 1 already rejects it at startup), and a
	//     deployment that simply forgets to set TestSupportEndpointsEnabled
	//     gets it disabled by default, regardless of Environment.
	//   - Contradictory configuration (Environment == "production" AND
	//     TestSupportEndpointsEnabled explicitly true) is not silently
	//     corrected - Load() FAILS STARTUP with a clear, named error. An
	//     operator setting both is directly attempting something unsafe;
	//     the correct response is a loud, hard failure, not a silent
	//     override.
	//
	// See docs/runbooks/production-configuration-checklist.md's
	// `Environment`/`APP_ENV` and `TestSupportEndpointsEnabled`/
	// `TEST_SUPPORT_ENDPOINTS_ENABLED` rows for the operator-facing
	// version of this.
	Environment string

	HTTPAddr string

	DatabaseURL         string
	DatabaseMaxConns    int32
	DatabaseConnTimeout time.Duration

	// JWTActiveKID/JWTSigningSecret are the current signing key and its
	// id, used for every new token. JWTPreviousKID/JWTPreviousSecret are
	// optional: when set, tokens signed with the previous key still
	// verify (so already-issued tokens don't break) while every new token
	// is signed with the active key - this is what key rotation looks
	// like operationally: introduce a new active key, keep the old one
	// registered as "previous" until its longest-lived outstanding token
	// expires, then drop it. See internal/auth/keys.go.
	//
	// This is a Stage 2 foundation mechanism (HMAC shared secrets managed
	// by this process's own config). It is explicitly PROVIDER DEPENDENT:
	// production authentication is expected to move to asymmetric signing
	// backed by a KMS-managed key or a real identity provider before
	// go-live - see docs/security/security-architecture.md's production
	// authentication evaluation.
	JWTActiveKID      string
	JWTSigningSecret  string
	JWTPreviousKID    string
	JWTPreviousSecret string
	// JWTIssuer/JWTAudience are dedicated identities for token issuance/
	// verification, intentionally independent of OTelServiceName -
	// reusing a telemetry setting here would mean renaming
	// OTEL_SERVICE_NAME silently invalidates every outstanding token.
	JWTIssuer   string
	JWTAudience string

	// AccessTokenTTL/RefreshTokenTTL control session lifetime - short
	// access tokens limit a leak's blast radius (nothing to revoke, it
	// just expires); the longer refresh token is what actually gets
	// revoked on logout/reuse-detection (internal/auth/session.go).
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration

	OTelServiceName string
	// OTelExporter selects the telemetry exporter. "stdout" (default) is
	// the Stage 1 foundation choice - human-readable, no external
	// collector required. "none" disables export entirely (useful for
	// tests). A real OTLP exporter is introduced when there's a concrete
	// observability backend to send to.
	OTelExporter string

	// ReconciliationInterval is the cadence of the ledger-vs-projection
	// reconciliation sweep (internal/reconciliation.RunSchedulerLoop,
	// Stage 3C directive item 4). Defaults to the Blueprint's target
	// cadence, hourly - reconciliation-model.md names no other cadence.
	// Configurable only so tests and local development don't have to
	// wait an hour to observe a sweep; production is expected to run at
	// the default.
	ReconciliationInterval time.Duration

	// RGEnumerationSweepInterval is the cadence of the self-exclusion
	// enumeration-run reconciliation sweep
	// (internal/rg.RunEnumerationReconciliationSchedulerLoop, Stage
	// 4H-B0-R7 directive item 4). Deliberately tighter than
	// ReconciliationInterval's hourly default: a dropped or stalled
	// enumeration run represents a self-exclusion that may not have had
	// its open bets acted on at all, a compliance-enforcement gap, not a
	// financial-drift one - 15 minutes balances catching that quickly
	// against sweep load. Configurable for the identical reason
	// ReconciliationInterval is: tests and local development should not
	// have to wait to observe a sweep.
	RGEnumerationSweepInterval time.Duration
	// RGEnumerationStalledThreshold is how old a self_exclusion_
	// enumeration_runs row may be, without reaching 'completed', before
	// RunEnumerationReconciliationSweep reports it as stalled
	// (internal/rg.FindStalledEnumerationRuns' own caller-supplied
	// olderThan parameter - see that function's doc comment for why this
	// is an operational tuning value, not a fact this package asserts on
	// its own).
	RGEnumerationStalledThreshold time.Duration

	// Stage 4H-B1 Wave 3 Phase 3 (bonus-engine): the three scheduled
	// Bonus Engine jobs (internal/bonus.RunDepositSweepSchedulerLoop/
	// RunCashbackSchedulerLoop/RunExpirySweepSchedulerLoop), mirroring
	// ReconciliationInterval/RGEnumerationSweepInterval's own
	// configurable-cadence pattern exactly. Defaults chosen per each
	// job's own consequence of running late (ledger-accounting-model.md
	// §7.18's own text): a deposit-bonus grant delayed a few minutes is a
	// minor player-experience lag, not a compliance/financial-drift risk,
	// so BonusDepositSweepInterval defaults to a relatively tight 5
	// minutes; cashback settlement and expiry are inherently window/date
	// -grained operations where an hourly cadence (matching
	// ReconciliationInterval's own default) is more than sufficient.
	BonusDepositSweepInterval  time.Duration
	BonusCashbackSweepInterval time.Duration
	BonusExpirySweepInterval   time.Duration

	// AuthRateLimitPerMinute overrides the per-IP-per-minute limit applied
	// to the unauthenticated credential endpoints (internal/httpserver/
	// ratelimit.go's own doc comment names exactly which routes and why).
	// 0 (the default) uses ratelimit.go's own per-bucket defaults, which
	// is the sane production-appropriate starting point: those defaults
	// were sized to comfortably exceed any plausible legitimate single-IP
	// burst while still bounding Argon2id cost-amplification abuse.
	// Positive values replace EVERY bucket's limit; negative disables the
	// limiter entirely. Closes S9.1-LAUNCH-2 (docs/security/security-
	// architecture.md) - previously only reachable via a code change.
	AuthRateLimitPerMinute int

	// TrustedProxyCount is the number of this deployment's OWN trusted
	// reverse-proxy hops in front of platform-api (a load balancer,
	// ingress controller, etc - never an arbitrary intermediary). It
	// governs how the rate limiter determines caller identity from
	// X-Forwarded-For; see httpserver.Deps.TrustedProxyCount's own doc
	// comment (internal/httpserver/server.go) and trustedProxyClientIP
	// (internal/httpserver/ratelimit.go) for the exact trust model.
	// Defaults to 0: X-Forwarded-For is never read, RemoteAddr is used
	// unconditionally - the safe default for an unconfigured deployment,
	// since trusting a client-settable header with no known hop count to
	// validate against would make the limiter trivially bypassable.
	// Closes S9.1-LAUNCH-1 (docs/security/security-architecture.md).
	TrustedProxyCount int

	// CORSAllowedOrigins is the exact-match allowlist of browser Origins
	// permitted to make cross-origin requests (comma-separated in
	// CORS_ALLOWED_ORIGINS, e.g. "https://staging.example.com,https://
	// admin-staging.example.com"). Empty (the default) means no CORS
	// headers are emitted at all - the correct behavior for a same-origin
	// deployment (a reverse proxy serving the frontend and API from one
	// origin) and for every existing dev/CI/test setup, none of which
	// need it. Introduced for Stage 9.3 staging, where the B2C and Back
	// Office frontends are deliberately deployed on different subdomains
	// than the API (docs/architecture/38-deployment-architecture.md's own
	// "production hosting is a deployment-time decision" note, made
	// concrete). This is a browser-enforced convenience, never a security
	// boundary: the Origin header is caller-supplied and unauthenticated,
	// so every route's real authorization decision is unchanged and still
	// enforced server-side regardless of Origin.
	CORSAllowedOrigins []string

	// TestSupportEndpointsEnabled is Stage 9.4's Layer 2 gate (see
	// Environment's own doc comment above for the full two-layer design).
	// It must be explicitly set to "true" (TEST_SUPPORT_ENDPOINTS_ENABLED)
	// for any of the four non-production simulation flags -
	// CasinoPlaySimulationEnabled, PaymentsMockSettlementEnabled,
	// AccountActivationTestSupportEnabled, SportsbookSettlementSimulationEnabled
	// (ADR 0088 §9) - to actually register their routes; cmd/platform-api/main.go
	// requires this field AND
	// `Environment != "production"` together. Defaults to false: an
	// unconfigured deployment (including one that also gets Environment
	// wrong) never exposes any test-support route. Only a genuine
	// staging/test deployment that deliberately wants the mock-settlement/
	// self-signed-play/account-activation seams should set this to true -
	// see deploy/aws/environments/staging's Terraform, which sets it
	// explicitly for exactly that reason. Load() FAILS startup if this is
	// true at the same time Environment == "production" - see Environment's
	// own doc comment.
	TestSupportEndpointsEnabled bool
}

// Load reads configuration from the process environment. It returns an
// error rather than panicking so callers (including tests) can handle a
// misconfigured environment explicitly.
func Load() (Config, error) {
	cfg := Config{
		Environment:                   resolveAppEnv(),
		HTTPAddr:                      getEnvDefault("HTTP_ADDR", ":8080"),
		DatabaseURL:                   os.Getenv("DATABASE_URL"),
		DatabaseMaxConns:              10,
		DatabaseConnTimeout:           5 * time.Second,
		JWTActiveKID:                  getEnvDefault("JWT_ACTIVE_KID", "k1"),
		JWTSigningSecret:              os.Getenv("JWT_SIGNING_SECRET"),
		JWTPreviousKID:                getEnvDefault("JWT_PREVIOUS_KID", "k0"),
		JWTPreviousSecret:             os.Getenv("JWT_PREVIOUS_SECRET"),
		JWTIssuer:                     getEnvDefault("JWT_ISSUER", "igaming-platform"),
		JWTAudience:                   getEnvDefault("JWT_AUDIENCE", "platform-api"),
		AccessTokenTTL:                15 * time.Minute,
		RefreshTokenTTL:               30 * 24 * time.Hour,
		OTelServiceName:               getEnvDefault("OTEL_SERVICE_NAME", "platform-api"),
		OTelExporter:                  getEnvDefault("OTEL_EXPORTER", "stdout"),
		ReconciliationInterval:        time.Hour,
		RGEnumerationSweepInterval:    15 * time.Minute,
		RGEnumerationStalledThreshold: 15 * time.Minute,
		BonusDepositSweepInterval:     5 * time.Minute,
		BonusCashbackSweepInterval:    time.Hour,
		BonusExpirySweepInterval:      time.Hour,
		AuthRateLimitPerMinute:        0,
		TrustedProxyCount:             0,
		TestSupportEndpointsEnabled:   false,
	}

	if v := os.Getenv("DATABASE_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid DATABASE_MAX_CONNS: %w", err)
		}
		cfg.DatabaseMaxConns = int32(n)
	}
	if v := os.Getenv("ACCESS_TOKEN_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid ACCESS_TOKEN_TTL_SECONDS: %w", err)
		}
		cfg.AccessTokenTTL = time.Duration(n) * time.Second
	}
	if v := os.Getenv("REFRESH_TOKEN_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid REFRESH_TOKEN_TTL_SECONDS: %w", err)
		}
		cfg.RefreshTokenTTL = time.Duration(n) * time.Second
	}
	if v := os.Getenv("RECONCILIATION_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid RECONCILIATION_INTERVAL_SECONDS: %w", err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("config: RECONCILIATION_INTERVAL_SECONDS must be positive")
		}
		cfg.ReconciliationInterval = time.Duration(n) * time.Second
	}
	if v := os.Getenv("RG_ENUMERATION_SWEEP_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid RG_ENUMERATION_SWEEP_INTERVAL_SECONDS: %w", err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("config: RG_ENUMERATION_SWEEP_INTERVAL_SECONDS must be positive")
		}
		cfg.RGEnumerationSweepInterval = time.Duration(n) * time.Second
	}
	if v := os.Getenv("RG_ENUMERATION_STALLED_THRESHOLD_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid RG_ENUMERATION_STALLED_THRESHOLD_SECONDS: %w", err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("config: RG_ENUMERATION_STALLED_THRESHOLD_SECONDS must be positive")
		}
		cfg.RGEnumerationStalledThreshold = time.Duration(n) * time.Second
	}

	if v := os.Getenv("BONUS_DEPOSIT_SWEEP_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid BONUS_DEPOSIT_SWEEP_INTERVAL_SECONDS: %w", err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("config: BONUS_DEPOSIT_SWEEP_INTERVAL_SECONDS must be positive")
		}
		cfg.BonusDepositSweepInterval = time.Duration(n) * time.Second
	}
	if v := os.Getenv("BONUS_CASHBACK_SWEEP_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid BONUS_CASHBACK_SWEEP_INTERVAL_SECONDS: %w", err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("config: BONUS_CASHBACK_SWEEP_INTERVAL_SECONDS must be positive")
		}
		cfg.BonusCashbackSweepInterval = time.Duration(n) * time.Second
	}
	if v := os.Getenv("BONUS_EXPIRY_SWEEP_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid BONUS_EXPIRY_SWEEP_INTERVAL_SECONDS: %w", err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("config: BONUS_EXPIRY_SWEEP_INTERVAL_SECONDS must be positive")
		}
		cfg.BonusExpirySweepInterval = time.Duration(n) * time.Second
	}

	if v := os.Getenv("AUTH_RATE_LIMIT_PER_MINUTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid AUTH_RATE_LIMIT_PER_MINUTE: %w", err)
		}
		cfg.AuthRateLimitPerMinute = n
	}
	if v := os.Getenv("TRUSTED_PROXY_COUNT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid TRUSTED_PROXY_COUNT: %w", err)
		}
		if n < 0 {
			return Config{}, fmt.Errorf("config: TRUSTED_PROXY_COUNT must not be negative")
		}
		cfg.TrustedProxyCount = n
	}
	if v := os.Getenv("CORS_ALLOWED_ORIGINS"); v != "" {
		for _, origin := range strings.Split(v, ",") {
			origin = strings.TrimSpace(origin)
			if origin == "" {
				continue
			}
			if origin == "*" {
				return Config{}, fmt.Errorf("config: CORS_ALLOWED_ORIGINS must not contain a wildcard; list exact origins")
			}
			cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins, origin)
		}
	}

	if v := os.Getenv("TEST_SUPPORT_ENDPOINTS_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid TEST_SUPPORT_ENDPOINTS_ENABLED: %w", err)
		}
		cfg.TestSupportEndpointsEnabled = b
	}

	// Stage 9.4, Layer 1 (see Environment's own doc comment): validate
	// against the exact closed set this codebase's own Terraform
	// convention already uses (deploy/aws/modules/ecs/variables.tf's
	// app_environment). Leaving APP_ENV genuinely unset (resolveAppEnv's
	// os.LookupEnv sees it absent) resolves to the "development" default
	// above and always passes this check - but APP_ENV explicitly set to
	// ANY other value, including an explicit empty string, is rejected:
	// an unrecognized value (wrong case, a known alias like "prod", stray
	// whitespace) or an explicit empty string are all equally an operator
	// error, not a silent "development".
	switch cfg.Environment {
	case "development", "staging", "production":
	default:
		return Config{}, fmt.Errorf("config: invalid APP_ENV %q: must be exactly one of \"development\", \"staging\", or \"production\" (case-sensitive, no surrounding whitespace, not empty); leave APP_ENV unset entirely to default to \"development\"", cfg.Environment)
	}

	// Stage 9.4, contradictory-configuration fail-closed case (see
	// Environment's own doc comment): an operator explicitly requesting
	// BOTH "this is production" AND "enable test-support endpoints" is
	// directly attempting something unsafe. Fail loudly rather than
	// silently ignoring TestSupportEndpointsEnabled in that case.
	if cfg.Environment == "production" && cfg.TestSupportEndpointsEnabled {
		return Config{}, fmt.Errorf("config: TEST_SUPPORT_ENDPOINTS_ENABLED must not be true when APP_ENV=production - test-support endpoints (mock casino play/payment settlement/account-activation) are never permitted in production")
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.JWTSigningSecret == "" {
		return Config{}, fmt.Errorf("config: JWT_SIGNING_SECRET is required")
	}
	if len(cfg.JWTSigningSecret) < 32 {
		return Config{}, fmt.Errorf("config: JWT_SIGNING_SECRET must be at least 32 characters")
	}
	if cfg.JWTPreviousSecret != "" && len(cfg.JWTPreviousSecret) < 32 {
		return Config{}, fmt.Errorf("config: JWT_PREVIOUS_SECRET must be at least 32 characters")
	}
	if cfg.JWTPreviousSecret != "" && cfg.JWTPreviousKID == cfg.JWTActiveKID {
		return Config{}, fmt.Errorf("config: JWT_PREVIOUS_KID must differ from JWT_ACTIVE_KID")
	}

	return cfg, nil
}

// TestSupportRoutesEnabled reports whether the four Stage 7/9.3/10
// non-production simulation routes (CasinoPlaySimulationEnabled,
// PaymentsMockSettlementEnabled, AccountActivationTestSupportEnabled,
// SportsbookSettlementSimulationEnabled in internal/httpserver.Deps) should
// register - the single, shared
// Stage 9.4 two-layer gate: never true in production (Load() has
// already refused to start if TestSupportEndpointsEnabled were true
// there), and never true unless TestSupportEndpointsEnabled was also
// explicitly opted into. cmd/platform-api/main.go computes all three
// flags from this one method rather than duplicating the composition
// inline three times, so there is exactly one place this logic can be
// gotten wrong.
func (c Config) TestSupportRoutesEnabled() bool {
	return c.Environment != "production" && c.TestSupportEndpointsEnabled
}

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// resolveAppEnv reads APP_ENV directly via os.LookupEnv rather than
// getEnvDefault, deliberately NOT treating a present-but-empty value the
// same as an absent one. getEnvDefault's `ok && v != ""` folds both into
// the same default, which is exactly right for every other setting here
// (an empty override is never meaningfully different from "not set") but
// wrong for APP_ENV specifically once Layer 1 validation (below, in
// Load()) exists to catch operator mistakes: it must see the true raw
// value, including an explicit empty string, in order to reject it - see
// Environment's own doc comment for why this one gap mattered.
func resolveAppEnv() string {
	if v, ok := os.LookupEnv("APP_ENV"); ok {
		return v
	}
	return "development"
}
