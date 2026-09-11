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
// JWT signing secret and database URL have no default and fail startup
// if unset).
type Config struct {
	// Environment is "development", "staging", or "production". It is
	// informational (used in logs/traces) and must never gate a security
	// control - environments are otherwise identically configured.
	Environment string

	HTTPAddr string

	DatabaseURL         string
	DatabaseMaxConns    int32
	DatabaseConnTimeout time.Duration

	// JWTSigningSecret is a Stage 1 foundation mechanism (HMAC shared
	// secret). It is explicitly PROVIDER DEPENDENT / STUB: production
	// authentication is expected to move to asymmetric signing backed by
	// a real identity provider or KMS-managed key before go-live. See
	// docs/architecture/04-api-architecture.md.
	JWTSigningSecret string
	// JWTIssuer is a dedicated identity for token issuance/verification,
	// intentionally independent of OTelServiceName - reusing a telemetry
	// setting here would mean renaming OTEL_SERVICE_NAME silently
	// invalidates every outstanding token.
	JWTIssuer string

	OTelServiceName string
	// OTelExporter selects the telemetry exporter. "stdout" (default) is
	// the Stage 1 foundation choice - human-readable, no external
	// collector required. "none" disables export entirely (useful for
	// tests). A real OTLP exporter is introduced when there's a concrete
	// observability backend to send to.
	OTelExporter string
}

// Load reads configuration from the process environment. It returns an
// error rather than panicking so callers (including tests) can handle a
// misconfigured environment explicitly.
func Load() (Config, error) {
	cfg := Config{
		Environment:         getEnvDefault("APP_ENV", "development"),
		HTTPAddr:            getEnvDefault("HTTP_ADDR", ":8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		DatabaseMaxConns:    10,
		DatabaseConnTimeout: 5 * time.Second,
		JWTSigningSecret:    os.Getenv("JWT_SIGNING_SECRET"),
		JWTIssuer:           getEnvDefault("JWT_ISSUER", "igaming-platform"),
		OTelServiceName:     getEnvDefault("OTEL_SERVICE_NAME", "platform-api"),
		OTelExporter:        getEnvDefault("OTEL_EXPORTER", "stdout"),
	}

	if v := os.Getenv("DATABASE_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid DATABASE_MAX_CONNS: %w", err)
		}
		cfg.DatabaseMaxConns = int32(n)
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

	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
