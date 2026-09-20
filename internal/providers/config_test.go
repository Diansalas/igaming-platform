// Unit tests for LoadProviderConfig/ProviderConfig.ResolveAPIKey - no
// database, network, or real provider dependency. Per
// docs/decisions/0080-provider-integration-readiness-without-external-
// contracts.md Decision 4, this proves only the generic env-var loading
// contract; it does not test against any real vendor's configuration.
package providers

import (
	"testing"
	"time"
)

func TestLoadProviderConfig_Defaults(t *testing.T) {
	cfg, err := LoadProviderConfig("TESTPROV_DEFAULTS")
	if err != nil {
		t.Fatalf("LoadProviderConfig returned unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled = true, want false (default)")
	}
	if cfg.BaseURL != "" {
		t.Fatalf("BaseURL = %q, want empty", cfg.BaseURL)
	}
	if cfg.APIKeyEnvVar != "" {
		t.Fatalf("APIKeyEnvVar = %q, want empty", cfg.APIKeyEnvVar)
	}
	if cfg.Timeout != 10*time.Second {
		t.Fatalf("Timeout = %v, want 10s", cfg.Timeout)
	}
	if cfg.MaxRetries != 2 {
		t.Fatalf("MaxRetries = %d, want 2", cfg.MaxRetries)
	}
}

func TestLoadProviderConfig_AllSet(t *testing.T) {
	const prefix = "TESTPROV_ALLSET"
	t.Setenv(prefix+"_ENABLED", "true")
	t.Setenv(prefix+"_BASE_URL", "https://example.invalid/api")
	t.Setenv(prefix+"_API_KEY_ENV_VAR", "TESTPROV_ALLSET_SECRET")
	t.Setenv(prefix+"_TIMEOUT_SECONDS", "30")
	t.Setenv(prefix+"_MAX_RETRIES", "5")

	cfg, err := LoadProviderConfig(prefix)
	if err != nil {
		t.Fatalf("LoadProviderConfig returned unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if cfg.BaseURL != "https://example.invalid/api" {
		t.Fatalf("BaseURL = %q, want %q", cfg.BaseURL, "https://example.invalid/api")
	}
	if cfg.APIKeyEnvVar != "TESTPROV_ALLSET_SECRET" {
		t.Fatalf("APIKeyEnvVar = %q, want %q", cfg.APIKeyEnvVar, "TESTPROV_ALLSET_SECRET")
	}
	if cfg.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v, want 30s", cfg.Timeout)
	}
	if cfg.MaxRetries != 5 {
		t.Fatalf("MaxRetries = %d, want 5", cfg.MaxRetries)
	}
}

func TestLoadProviderConfig_DisabledShortCircuit(t *testing.T) {
	const prefix = "TESTPROV_DISABLED"
	t.Setenv(prefix+"_ENABLED", "false")
	// Deliberately no BASE_URL/API_KEY_ENV_VAR set - must not error.

	cfg, err := LoadProviderConfig(prefix)
	if err != nil {
		t.Fatalf("LoadProviderConfig returned unexpected error for a disabled provider with no URL configured: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled = true, want false")
	}
	if cfg.BaseURL != "" {
		t.Fatalf("BaseURL = %q, want empty", cfg.BaseURL)
	}
}

func TestLoadProviderConfig_MalformedEnabled(t *testing.T) {
	const prefix = "TESTPROV_BADENABLED"
	t.Setenv(prefix+"_ENABLED", "not-a-bool")

	if _, err := LoadProviderConfig(prefix); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for malformed _ENABLED, want error")
	}
}

func TestLoadProviderConfig_MalformedTimeout(t *testing.T) {
	const prefix = "TESTPROV_BADTIMEOUT"
	t.Setenv(prefix+"_TIMEOUT_SECONDS", "not-an-int")

	if _, err := LoadProviderConfig(prefix); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for malformed _TIMEOUT_SECONDS, want error")
	}
}

func TestLoadProviderConfig_NonPositiveTimeout(t *testing.T) {
	const prefix = "TESTPROV_ZEROTIMEOUT"
	t.Setenv(prefix+"_TIMEOUT_SECONDS", "0")

	if _, err := LoadProviderConfig(prefix); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for zero _TIMEOUT_SECONDS, want error")
	}
}

func TestLoadProviderConfig_MalformedMaxRetries(t *testing.T) {
	const prefix = "TESTPROV_BADRETRIES"
	t.Setenv(prefix+"_MAX_RETRIES", "not-an-int")

	if _, err := LoadProviderConfig(prefix); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for malformed _MAX_RETRIES, want error")
	}
}

func TestLoadProviderConfig_NegativeMaxRetries(t *testing.T) {
	const prefix = "TESTPROV_NEGRETRIES"
	t.Setenv(prefix+"_MAX_RETRIES", "-1")

	if _, err := LoadProviderConfig(prefix); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for negative _MAX_RETRIES, want error")
	}
}

func TestResolveAPIKey_NotConfigured(t *testing.T) {
	cfg := ProviderConfig{}
	v, err := cfg.ResolveAPIKey()
	if err != nil {
		t.Fatalf("ResolveAPIKey returned unexpected error: %v", err)
	}
	if v != "" {
		t.Fatalf("ResolveAPIKey = %q, want empty", v)
	}
}

func TestResolveAPIKey_Success(t *testing.T) {
	t.Setenv("TESTPROV_RESOLVE_SECRET", "the-actual-secret")
	cfg := ProviderConfig{APIKeyEnvVar: "TESTPROV_RESOLVE_SECRET"}

	v, err := cfg.ResolveAPIKey()
	if err != nil {
		t.Fatalf("ResolveAPIKey returned unexpected error: %v", err)
	}
	if v != "the-actual-secret" {
		t.Fatalf("ResolveAPIKey = %q, want %q", v, "the-actual-secret")
	}
}

func TestResolveAPIKey_MissingReferencedVar(t *testing.T) {
	cfg := ProviderConfig{APIKeyEnvVar: "TESTPROV_DOES_NOT_EXIST_ANYWHERE"}

	if _, err := cfg.ResolveAPIKey(); err == nil {
		t.Fatal("ResolveAPIKey returned nil error for a missing referenced env var, want error")
	}
}
