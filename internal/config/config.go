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
	// One deliberate, reviewed exception:
	// db.VerifyRuntimeRoleInProduction (PLAT-ROLESPLIT-1,
	// docs/security/runtime-role-separation.md) gates a fail-closed
	// startup check on Environment == "production" specifically, because
	// development/CI legitimately and intentionally connect as the
	// database's table-owning role today (a large share of this repo's
	// integration suite requires owner/DDL privileges to run migration
	// mechanics at all), while production must never do so. See that
	// function's own doc comment for the full reasoning.
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
}

// Load reads configuration from the process environment. It returns an
// error rather than panicking so callers (including tests) can handle a
// misconfigured environment explicitly.
func Load() (Config, error) {
	cfg := Config{
		Environment:                   getEnvDefault("APP_ENV", "development"),
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

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
