// Package config loads platform-api configuration from environment
// variables. There is no config file support by design — secrets and
// per-environment values come from the environment (or, in later stages,
// a secrets manager that injects environment variables), never from a
// file checked into the repository.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
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
	// Six deliberate, reviewed exceptions exist (corrected at Stage 10.2
	// final review, K6/M5 - this list previously undercounted itself as
	// "three") - the sentence above is the DEFAULT rule, not an invariant,
	// and this list is exhaustive:
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
	//  6. cmd/platform-api/wiring.go's mockProviderWiring (Stage 10.2, ADR
	//     0091) derives its own testSupport bool from
	//     TestSupportRoutesEnabled() and uses it to decide whether the KYC
	//     mock/orchestrator and the payments/casino mock webhook resolvers
	//     are wired at all, including httpserver.Deps.KYCWebhookEnabled.
	//     Unlike (2)-(5) it is not itself a single Deps bool field, but it
	//     is gated by the same function and belongs on this list.
	//  7. MOCK-ADAPTER-PROD-1's synthetic-component startup guard
	//     (internal/providerkind.RefuseSyntheticInProduction, cmd/
	//     platform-api's own refuseSyntheticInProduction wrapper) and the
	//     secret-store backend allow-list (ValidateSecretBackendScheme,
	//     below) - Stage 10.3, ADR 0085's Stage 10.3 amendment, security
	//     condition C13/ruling R8. Both gate on GuardEnvironment(), below,
	//     rather than on Environment directly, specifically BECAUSE a
	//     missing APP_ENV must be treated as production for these two
	//     checks only (see GuardEnvironment's own doc comment) - the one
	//     deliberate departure, in this list, from "environment must never
	//     gate a security control", same as exception (1).
	//  8. db.VerifyRuntimeRoleInProduction's call site in cmd/platform-api/
	//     main.go now passes cfg.GuardEnvironment() rather than
	//     cfg.Environment directly (found by `architect`'s Stage 10.3 W0
	//     code check as the same class of gap as C13) - so a production
	//     task deployed with APP_ENV missing entirely is no longer silently
	//     exempted from the role-ownership check either. The function
	//     itself (db/production_safety.go) is UNCHANGED: it still gates on
	//     the literal string "production" and has no knowledge of
	//     GuardEnvironment - only the value main.go passes in changed.
	//
	// (1) fails CLOSED on a mis-set value (a typo just re-enables a check
	// production should pass anyway). (2)-(6) used to (or, for (5) and (6),
	// would have) fail OPEN on a typo or an unset variable, per two
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
	//     of the flags derived from TestSupportRoutesEnabled() actually
	//     registers a route or wires a mock provider.
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

	// EnvironmentExplicit records whether APP_ENV was genuinely present in
	// the process environment (os.LookupEnv's ok == true) at Load() time,
	// as distinct from Environment's own "development" default for a
	// genuinely absent variable. It exists solely so GuardEnvironment
	// (below) can tell "an operator explicitly chose development" apart
	// from "APP_ENV was never set at all" - Environment's own value is
	// identical ("development") in both cases, which is exactly the
	// ambiguity ADR 0085's Consequences section disclosed and Stage 10.3
	// (security condition C13, ruling R8) requires closing for the
	// synthetic-component guard and the secret-store backend allow-list.
	// Nothing outside GuardEnvironment/ValidateSecretBackendScheme should
	// read this field - every other consumer of Environment keeps treating
	// an absent APP_ENV as "development", unchanged.
	EnvironmentExplicit bool

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

	// WebhookAdmission is ADR 0097's transport-layer admission/rate-limit
	// configuration for the three provider-facing webhook routes
	// (PAYWH-RL-1). Loaded from WEBHOOK_ADMISSION_ENABLED,
	// WEBHOOK_RL_PER_IP_RPS/BURST and WEBHOOK_ADMISSION_OVERRIDES on top
	// of ADR 0097 §9.1's technical defaults (computed from
	// DatabaseMaxConns); validated by WebhookAdmissionConfig.Validate
	// (§9.3) below, including "production cannot disable".
	WebhookAdmission WebhookAdmissionConfig

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

	// ProviderCredentialFingerprintKey is the platform fingerprint HMAC
	// key (ADR 0093 §2 and its Stage 10.3 W2a amendment A3; security review
	// 07-w2a-design-review-security.md §3), from
	// PROVIDER_CREDENTIAL_FINGERPRINT_KEY. Every stored provider-credential
	// fingerprint is fp1:hex(HMAC-SHA256(key, label||0x00||secret)).
	//
	// Absent means the real provider-credential subsystem is simply NOT
	// constructed (the real resolver is nil, the request/approve/apply
	// routes are not mounted, non-synthetic outbound calls fail closed);
	// startup still succeeds. Present, Load refuses a key shorter than 32
	// bytes, equal to either JWT secret, or - outside an explicit
	// development environment - equal to the published .env.example/CI
	// placeholder. The key must be stable per environment: changing it
	// makes every stored fp1: fingerprint mismatch (every resolve then
	// fails closed with credential_integrity). It is a SecretValue, so it
	// never renders through fmt, slog or encoding/json.
	ProviderCredentialFingerprintKey SecretValue

	// SecretStoreBackends lists the secret-store backend schemes this
	// process constructs (SECRETSTORE_BACKENDS, comma-separated, e.g.
	// "devfile"). Every entry must pass ValidateSecretBackendScheme, so a
	// configured backend the environment does not permit refuses startup.
	// Empty (the default) constructs no backend, so the real credential
	// subsystem is not built (fail closed).
	SecretStoreBackends []string

	// SecretStoreDevFileRoot is the devfile:// backend's root directory
	// (SECRETSTORE_DEVFILE_ROOT, default ./.secrets/dev, which is listed in
	// both .gitignore and .dockerignore). Only read when "devfile" is in
	// SecretStoreBackends, which itself is development-only.
	SecretStoreDevFileRoot string

	// SecretStoreAWSRegion is the AWS Secrets Manager region the awssm://
	// backend uses (Stage 10.3 W3b wiring; ADR 0093 §6 "the region comes
	// explicitly from configuration"). Read from AWS_SECRETSMANAGER_REGION,
	// falling back to AWS_REGION. Only read, and only validated
	// (ValidateSecretStoreAWSRegion), when "awssm" is in
	// SecretStoreBackends; otherwise it is ignored. Not secret.
	SecretStoreAWSRegion string
}

// SecretValue is a configuration string that must never be printed: its
// String, GoString, Format, LogValue and MarshalJSON all redact. Reveal
// returns the value for the one consumer that needs it.
type SecretValue struct{ v string }

// NewSecretValue wraps v. For tests and for Load only.
func NewSecretValue(v string) SecretValue { return SecretValue{v: v} }

// Reveal returns the raw value.
func (s SecretValue) Reveal() string { return s.v }

// IsSet reports whether a non-empty value is present.
func (s SecretValue) IsSet() bool { return s.v != "" }

const redactedSecretValue = "[REDACTED]"

// String implements fmt.Stringer.
func (s SecretValue) String() string {
	if s.v == "" {
		return ""
	}
	return redactedSecretValue
}

// GoString implements fmt.GoStringer (so %#v also redacts).
func (s SecretValue) GoString() string { return s.String() }

// Format implements fmt.Formatter so every verb (%s, %q, %x, %v ...)
// renders the redacted form, never the value.
func (s SecretValue) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// LogValue implements slog.LogValuer.
func (s SecretValue) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalJSON implements json.Marshaler.
func (s SecretValue) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// Published placeholder fingerprint keys. They are NOT secrets: they are
// the literal values in .env.example and .github/workflows/ci.yml, and
// exist here only so Load can refuse them outside explicit development.
const (
	DevProviderCredentialFingerprintKeyPlaceholder = "dev-only-provider-credential-fingerprint-key-not-a-secret"
	CIProviderCredentialFingerprintKeyPlaceholder  = "ci-only-provider-credential-fingerprint-key-not-a-secret"
)

// MinProviderCredentialFingerprintKeyBytes is the shortest accepted
// fingerprint key (security review §3).
const MinProviderCredentialFingerprintKeyBytes = 32

// DefaultDatabaseMaxConns is the production default pool size
// (DATABASE_MAX_CONNS unset). ADR 0094's pool-size tests pin it: a change
// forces that ADR's resource-allocation rationale to be revisited.
const DefaultDatabaseMaxConns int32 = 10

// validateProviderCredentialFingerprintKey applies security review §3's
// rules. An absent key is valid (the subsystem is then not constructed).
func (c Config) validateProviderCredentialFingerprintKey() error {
	k := c.ProviderCredentialFingerprintKey.Reveal()
	if k == "" {
		return nil
	}
	if len(k) < MinProviderCredentialFingerprintKeyBytes {
		return fmt.Errorf("config: PROVIDER_CREDENTIAL_FINGERPRINT_KEY must be at least %d bytes", MinProviderCredentialFingerprintKeyBytes)
	}
	if k == c.JWTSigningSecret || (c.JWTPreviousSecret != "" && k == c.JWTPreviousSecret) {
		return fmt.Errorf("config: PROVIDER_CREDENTIAL_FINGERPRINT_KEY must differ from JWT_SIGNING_SECRET and JWT_PREVIOUS_SECRET")
	}
	if c.GuardEnvironment() != "development" &&
		(k == DevProviderCredentialFingerprintKeyPlaceholder || k == CIProviderCredentialFingerprintKeyPlaceholder) {
		return fmt.Errorf("config: PROVIDER_CREDENTIAL_FINGERPRINT_KEY is a published dev/CI placeholder and is refused unless APP_ENV is explicitly \"development\"")
	}
	return nil
}

// Load reads configuration from the process environment. It returns an
// error rather than panicking so callers (including tests) can handle a
// misconfigured environment explicitly.
func Load() (Config, error) {
	_, appEnvPresent := os.LookupEnv("APP_ENV")
	cfg := Config{
		Environment:                      resolveAppEnv(),
		EnvironmentExplicit:              appEnvPresent,
		HTTPAddr:                         getEnvDefault("HTTP_ADDR", ":8080"),
		DatabaseURL:                      os.Getenv("DATABASE_URL"),
		DatabaseMaxConns:                 DefaultDatabaseMaxConns,
		DatabaseConnTimeout:              5 * time.Second,
		JWTActiveKID:                     getEnvDefault("JWT_ACTIVE_KID", "k1"),
		JWTSigningSecret:                 os.Getenv("JWT_SIGNING_SECRET"),
		JWTPreviousKID:                   getEnvDefault("JWT_PREVIOUS_KID", "k0"),
		JWTPreviousSecret:                os.Getenv("JWT_PREVIOUS_SECRET"),
		JWTIssuer:                        getEnvDefault("JWT_ISSUER", "igaming-platform"),
		JWTAudience:                      getEnvDefault("JWT_AUDIENCE", "platform-api"),
		AccessTokenTTL:                   15 * time.Minute,
		RefreshTokenTTL:                  30 * 24 * time.Hour,
		OTelServiceName:                  getEnvDefault("OTEL_SERVICE_NAME", "platform-api"),
		OTelExporter:                     getEnvDefault("OTEL_EXPORTER", "stdout"),
		ReconciliationInterval:           time.Hour,
		RGEnumerationSweepInterval:       15 * time.Minute,
		RGEnumerationStalledThreshold:    15 * time.Minute,
		BonusDepositSweepInterval:        5 * time.Minute,
		BonusCashbackSweepInterval:       time.Hour,
		BonusExpirySweepInterval:         time.Hour,
		AuthRateLimitPerMinute:           0,
		TrustedProxyCount:                0,
		TestSupportEndpointsEnabled:      false,
		ProviderCredentialFingerprintKey: NewSecretValue(os.Getenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY")),
		SecretStoreDevFileRoot:           getEnvDefault("SECRETSTORE_DEVFILE_ROOT", "./.secrets/dev"),
		SecretStoreAWSRegion:             getEnvDefault("AWS_SECRETSMANAGER_REGION", os.Getenv("AWS_REGION")),
	}
	if v := os.Getenv("SECRETSTORE_BACKENDS"); v != "" {
		seen := map[string]bool{}
		for _, scheme := range strings.Split(v, ",") {
			scheme = strings.TrimSpace(scheme)
			if scheme == "" || seen[scheme] {
				continue
			}
			seen[scheme] = true
			cfg.SecretStoreBackends = append(cfg.SecretStoreBackends, scheme)
		}
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

	// Stage 10.3 W2a (ADR 0093 A3; security review §3/§4.1).
	if err := cfg.validateProviderCredentialFingerprintKey(); err != nil {
		return Config{}, err
	}

	// ADR 0097 PRH-I4: webhook admission/rate limiting. Loaded after
	// DatabaseMaxConns is resolved, since its technical defaults are
	// computed from the pool size (§9.1); validated with GuardEnvironment()
	// so a missing APP_ENV is treated as production here too, exactly like
	// the synthetic-adapter guard (§9.3 "production cannot disable").
	webhookAdmission, err := loadWebhookAdmissionConfig(cfg.DatabaseMaxConns)
	if err != nil {
		return Config{}, err
	}
	cfg.WebhookAdmission = webhookAdmission
	if err := cfg.WebhookAdmission.Validate(cfg.GuardEnvironment() == "production", cfg.DatabaseMaxConns); err != nil {
		return Config{}, err
	}
	for _, scheme := range cfg.SecretStoreBackends {
		if err := cfg.ValidateSecretBackendScheme(scheme); err != nil {
			return Config{}, fmt.Errorf("config: SECRETSTORE_BACKENDS: %w", err)
		}
		if scheme == SecretBackendAWSSecretsManager {
			if err := cfg.ValidateSecretStoreAWSRegion(); err != nil {
				return Config{}, err
			}
		}
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
// explicitly opted into. cmd/platform-api/main.go computes all four of
// those Deps flags directly from this one method rather than duplicating
// the composition inline four times. Stage 10.2 (ADR 0091) adds a fifth
// caller that is not itself a Deps flag: cmd/platform-api/wiring.go's
// mockProviderWiring(cfg) also calls this method (once, as testSupport) to
// decide whether to wire the KYC mock/orchestrator and the payments/casino
// mock webhook resolvers, including Deps.KYCWebhookEnabled. Either way,
// this is the one place the logic can be gotten wrong (see Config.
// Environment's doc comment for the full, current, exhaustive list of
// exceptions this method backs).
func (c Config) TestSupportRoutesEnabled() bool {
	return c.Environment != "production" && c.TestSupportEndpointsEnabled
}

// GuardEnvironment is the environment value MOCK-ADAPTER-PROD-1's
// synthetic-component startup guard and the secret-store backend
// allow-list (ValidateSecretBackendScheme, below) must use INSTEAD of
// Environment directly (Stage 10.3, ADR 0085's Stage 10.3 amendment;
// security condition C13, ruling R8 - "a missing APP_ENV is treated as
// production by the synthetic guard and the secret-backend allow-list,
// even though resolveAppEnv defaults to development elsewhere").
//
// It returns "production" when EITHER:
//   - Environment is already the literal string "production", or
//   - APP_ENV was never explicitly present in the process environment at
//     all (EnvironmentExplicit is false) - resolveAppEnv's own
//     "development" default for this case is INTENDED for every other
//     consumer of Environment (existing tests and local dev flows rely on
//     never having to set it), but is exactly the ambiguity these two
//     specific fail-closed checks must not inherit: a real production
//     task that is missing APP_ENV entirely must never be treated as
//     "deliberately development".
//
// In every other case (APP_ENV explicitly set to "development" or
// "staging") it returns Environment unchanged - Layer 1 (Load(), above)
// has already rejected any other explicit value, so by the time a Config
// exists at all, Environment is one of the three closed-set values.
//
// This is a NEW, additional, reviewed exception to "Environment must
// never gate a security control" (see Environment's own doc comment,
// exceptions 7-8) - it does not change resolveAppEnv, Load()'s own
// validation, or TestSupportRoutesEnabled's behavior in any way.
func (c Config) GuardEnvironment() string {
	if c.Environment == "production" || !c.EnvironmentExplicit {
		return "production"
	}
	return c.Environment
}

// Secret-store backend schemes (ADR 0093 §6; security condition C13/
// ruling R8's "small function that W2/W3 will consume"). This is the
// allow-list placeholder those later waves wire the real credential
// resolver's backend selection through - W1b adds only the rule itself,
// no store implementation.
const (
	// SecretBackendAWSSecretsManager is permitted ONLY when APP_ENV is
	// EXPLICITLY "staging" or "production" (ADR 0093 §6 table: "awssm:
	// staging, production"; W3b wires the actual AWS SDK client behind
	// it). It is refused in development and when APP_ENV is missing, so a
	// task with no APP_ENV can select no backend at all and fails closed.
	// W3b unit-tests the backend against an SDK fake without going
	// through this config gate.
	SecretBackendAWSSecretsManager = "awssm"
	// SecretBackendDevFile is a local-file-backed backend permitted ONLY
	// when GuardEnvironment() == "development" - i.e. APP_ENV must be
	// EXPLICITLY present and equal to "development"; a missing APP_ENV is
	// refused exactly like production is, per C13/R8.
	SecretBackendDevFile = "devfile"
	// SecretBackendMemory backs the in-process fake used by tests. It is
	// deliberately NOT part of this allow-list at all, in any
	// environment - ADR 0093 §6 requires it be constructible only from
	// test code (Go's own `_test.go` compilation boundary), never
	// selectable through configuration. ValidateSecretBackendScheme
	// always refuses it for exactly that reason.
	SecretBackendMemory = "memory"
)

// ValidateSecretBackendScheme refuses a secret-store backend scheme that
// cfg's environment does not permit (ADR 0093 §6; C13/R8). It is pure and
// has no side effects - the real store construction (W2a/W3b) is expected
// to call this before constructing any backend client.
func (c Config) ValidateSecretBackendScheme(scheme string) error {
	switch scheme {
	case SecretBackendAWSSecretsManager:
		// Explicit staging/production only (ADR 0093 §6). GuardEnvironment
		// alone would read a missing APP_ENV as "production" and permit
		// awssm; the stricter rule refuses it, so an unset APP_ENV permits
		// NO backend (fail closed).
		if !c.EnvironmentExplicit || (c.Environment != "staging" && c.Environment != "production") {
			return fmt.Errorf(
				"config: secret backend %q is permitted only when APP_ENV is explicitly \"staging\" or \"production\" "+
					"(ADR 0093 §6; refused in development and when APP_ENV is missing)", scheme)
		}
		return nil
	case SecretBackendDevFile:
		if c.GuardEnvironment() != "development" {
			return fmt.Errorf(
				"config: secret backend %q is development-only - APP_ENV must be explicitly \"development\" "+
					"(a missing APP_ENV is treated as production for this check, per C13/R8)", scheme)
		}
		return nil
	case SecretBackendMemory:
		return fmt.Errorf("config: secret backend %q is test-only and must never be selected via configuration", scheme)
	default:
		return fmt.Errorf("config: unknown secret backend scheme %q", scheme)
	}
}

// awsRegionPattern is the shape of an AWS region name (e.g. "eu-west-1",
// "us-gov-west-1"). It is a format check only; whether the region exists
// is AWS's answer, not ours.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)

// ValidateSecretStoreAWSRegion refuses a missing or malformed
// SecretStoreAWSRegion. Load calls it only when "awssm" is configured, and
// cmd/platform-api calls it again before constructing the awssm backend;
// with awssm not selected the region is never read.
func (c Config) ValidateSecretStoreAWSRegion() error {
	if c.SecretStoreAWSRegion == "" {
		return fmt.Errorf("config: secret backend %q needs a region: set AWS_SECRETSMANAGER_REGION (or AWS_REGION)", SecretBackendAWSSecretsManager)
	}
	if !awsRegionPattern.MatchString(c.SecretStoreAWSRegion) {
		return fmt.Errorf("config: AWS_SECRETSMANAGER_REGION/AWS_REGION %q is not an AWS region name", c.SecretStoreAWSRegion)
	}
	return nil
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
