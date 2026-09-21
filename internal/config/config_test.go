package config

import (
	"os"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"APP_ENV", "HTTP_ADDR", "DATABASE_URL", "DATABASE_MAX_CONNS", "JWT_SIGNING_SECRET", "OTEL_SERVICE_NAME", "OTEL_EXPORTER", "CORS_ALLOWED_ORIGINS"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when DATABASE_URL is unset, got nil")
	}
}

func TestLoad_MissingJWTSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when JWT_SIGNING_SECRET is unset, got nil")
	}
}

func TestLoad_ShortJWTSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "too-short")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when JWT_SIGNING_SECRET is too short, got nil")
	}
}

func TestLoad_DefaultsAndOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("expected default environment 'development', got %q", cfg.Environment)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("expected default HTTP addr ':8080', got %q", cfg.HTTPAddr)
	}
	if cfg.DatabaseMaxConns != 10 {
		t.Errorf("expected default max conns 10, got %d", cfg.DatabaseMaxConns)
	}

	t.Setenv("APP_ENV", "staging")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("DATABASE_MAX_CONNS", "25")

	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "staging" {
		t.Errorf("expected overridden environment 'staging', got %q", cfg.Environment)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("expected overridden HTTP addr ':9090', got %q", cfg.HTTPAddr)
	}
	if cfg.DatabaseMaxConns != 25 {
		t.Errorf("expected overridden max conns 25, got %d", cfg.DatabaseMaxConns)
	}
}

func TestLoad_InvalidMaxConns(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("DATABASE_MAX_CONNS", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for non-numeric DATABASE_MAX_CONNS, got nil")
	}
}

// S9.1-LAUNCH-1/S9.1-LAUNCH-2: AuthRateLimitPerMinute/TrustedProxyCount
// must default to 0 (per-bucket defaults / X-Forwarded-For never trusted)
// so an unconfigured deployment is safe, and must be overridable via their
// documented environment variables without any other side effect.

func TestLoad_AuthRateLimitAndTrustedProxyDefaultToZero(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthRateLimitPerMinute != 0 {
		t.Errorf("expected default AuthRateLimitPerMinute 0, got %d", cfg.AuthRateLimitPerMinute)
	}
	if cfg.TrustedProxyCount != 0 {
		t.Errorf("expected default TrustedProxyCount 0, got %d", cfg.TrustedProxyCount)
	}
}

func TestLoad_AuthRateLimitPerMinuteOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("AUTH_RATE_LIMIT_PER_MINUTE", "-1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthRateLimitPerMinute != -1 {
		t.Errorf("expected AuthRateLimitPerMinute -1 (disabled), got %d", cfg.AuthRateLimitPerMinute)
	}
}

func TestLoad_InvalidAuthRateLimitPerMinute(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("AUTH_RATE_LIMIT_PER_MINUTE", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for non-numeric AUTH_RATE_LIMIT_PER_MINUTE, got nil")
	}
}

func TestLoad_TrustedProxyCountOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("TRUSTED_PROXY_COUNT", "2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TrustedProxyCount != 2 {
		t.Errorf("expected TrustedProxyCount 2, got %d", cfg.TrustedProxyCount)
	}
}

func TestLoad_NegativeTrustedProxyCountRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("TRUSTED_PROXY_COUNT", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative TRUSTED_PROXY_COUNT, got nil")
	}
}

func TestLoad_InvalidTrustedProxyCount(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("TRUSTED_PROXY_COUNT", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for non-numeric TRUSTED_PROXY_COUNT, got nil")
	}
}

func TestLoad_CORSAllowedOriginsUnsetIsEmpty(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.CORSAllowedOrigins) != 0 {
		t.Errorf("expected no CORS origins by default, got %v", cfg.CORSAllowedOrigins)
	}
}

func TestLoad_CORSAllowedOriginsParsedAndTrimmed(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://staging.example.com, https://admin-staging.example.com ,")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"https://staging.example.com", "https://admin-staging.example.com"}
	if len(cfg.CORSAllowedOrigins) != len(want) {
		t.Fatalf("expected %v, got %v", want, cfg.CORSAllowedOrigins)
	}
	for i, origin := range want {
		if cfg.CORSAllowedOrigins[i] != origin {
			t.Errorf("expected origin %d to be %q, got %q", i, origin, cfg.CORSAllowedOrigins[i])
		}
	}
}

func TestLoad_CORSAllowedOriginsWildcardRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("CORS_ALLOWED_ORIGINS", "*")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for wildcard CORS_ALLOWED_ORIGINS, got nil")
	}
}
