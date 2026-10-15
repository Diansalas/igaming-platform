package config

import (
	"strings"
	"testing"
)

// --- A8: MOCK_GEOLOCATION_COUNTRY ---

func loadWithMockCountry(t *testing.T, env, country string) (Config, error) {
	t.Helper()
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	if env != "" {
		t.Setenv("APP_ENV", env)
	}
	t.Setenv("MOCK_GEOLOCATION_COUNTRY", country)
	return Load()
}

func TestMockGeolocationCountry_DefaultsToUnknown(t *testing.T) {
	cfg, err := loadWithMockCountry(t, "development", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MockGeolocationCountry != "" {
		t.Fatalf("default must be empty (unknown evidence), got %q", cfg.MockGeolocationCountry)
	}
}

func TestMockGeolocationCountry_AcceptedOutsideProduction(t *testing.T) {
	for _, env := range []string{"development", "staging"} {
		cfg, err := loadWithMockCountry(t, env, "MT")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", env, err)
		}
		if cfg.MockGeolocationCountry != "MT" {
			t.Fatalf("%s: got %q", env, cfg.MockGeolocationCountry)
		}
	}
}

func TestMockGeolocationCountry_RefusedInProduction(t *testing.T) {
	_, err := loadWithMockCountry(t, "production", "MT")
	if err == nil || !strings.Contains(err.Error(), "MOCK_GEOLOCATION_COUNTRY") {
		t.Fatalf("a static country under APP_ENV=production must fail startup, got %v", err)
	}
}

func TestMockGeolocationCountry_RejectsMalformedValues(t *testing.T) {
	for _, bad := range []string{"mt", "MLT", "ZZ", " MT", "M"} {
		if _, err := loadWithMockCountry(t, "development", bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
