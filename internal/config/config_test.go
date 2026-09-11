package config

import (
	"os"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"APP_ENV", "HTTP_ADDR", "DATABASE_URL", "DATABASE_MAX_CONNS", "JWT_SIGNING_SECRET", "OTEL_SERVICE_NAME", "OTEL_EXPORTER"} {
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
