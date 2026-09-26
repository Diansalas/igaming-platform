// Unit tests for LoadProviderConfig - no database, network, or real
// provider dependency. Stage 10.3 W2a removed ProviderConfig.APIKeyEnvVar/
// ResolveAPIKey (PROV-OUTBOUND-CRED-1): their tests are replaced by
// TestLoadProviderConfig_HasNoCredentialSetting below and by
// TestOutbound_APIKeyEnvVarRemoved (internal/providercred). Per
// docs/decisions/0080-provider-integration-readiness-without-external-
// contracts.md Decision 4, this proves only the generic env-var loading
// contract; it does not test against any real vendor's configuration.
package providers

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadProviderConfig_Defaults(t *testing.T) {
	cfg, err := LoadProviderConfig("TESTPROV_DEFAULTS", os.Getenv)
	if err != nil {
		t.Fatalf("LoadProviderConfig returned unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled = true, want false (default)")
	}
	if cfg.BaseURL != "" {
		t.Fatalf("BaseURL = %q, want empty", cfg.BaseURL)
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
	t.Setenv(prefix+"_TIMEOUT_SECONDS", "30")
	t.Setenv(prefix+"_MAX_RETRIES", "5")

	cfg, err := LoadProviderConfig(prefix, os.Getenv)
	if err != nil {
		t.Fatalf("LoadProviderConfig returned unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if cfg.BaseURL != "https://example.invalid/api" {
		t.Fatalf("BaseURL = %q, want %q", cfg.BaseURL, "https://example.invalid/api")
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
	// Deliberately no BASE_URL set - must not error.

	cfg, err := LoadProviderConfig(prefix, os.Getenv)
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

	if _, err := LoadProviderConfig(prefix, os.Getenv); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for malformed _ENABLED, want error")
	}
}

func TestLoadProviderConfig_MalformedTimeout(t *testing.T) {
	const prefix = "TESTPROV_BADTIMEOUT"
	t.Setenv(prefix+"_TIMEOUT_SECONDS", "not-an-int")

	if _, err := LoadProviderConfig(prefix, os.Getenv); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for malformed _TIMEOUT_SECONDS, want error")
	}
}

func TestLoadProviderConfig_NonPositiveTimeout(t *testing.T) {
	const prefix = "TESTPROV_ZEROTIMEOUT"
	t.Setenv(prefix+"_TIMEOUT_SECONDS", "0")

	if _, err := LoadProviderConfig(prefix, os.Getenv); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for zero _TIMEOUT_SECONDS, want error")
	}
}

func TestLoadProviderConfig_MalformedMaxRetries(t *testing.T) {
	const prefix = "TESTPROV_BADRETRIES"
	t.Setenv(prefix+"_MAX_RETRIES", "not-an-int")

	if _, err := LoadProviderConfig(prefix, os.Getenv); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for malformed _MAX_RETRIES, want error")
	}
}

func TestLoadProviderConfig_NegativeMaxRetries(t *testing.T) {
	const prefix = "TESTPROV_NEGRETRIES"
	t.Setenv(prefix+"_MAX_RETRIES", "-1")

	if _, err := LoadProviderConfig(prefix, os.Getenv); err == nil {
		t.Fatal("LoadProviderConfig returned nil error for negative _MAX_RETRIES, want error")
	}
}

// TestLoadProviderConfig_HasNoCredentialSetting: the legacy
// {prefix}_API_KEY_ENV_VAR setting is no longer read, and ProviderConfig
// has no field that could carry a credential or a reference to one
// (PROV-OUTBOUND-CRED-1: outbound credentials are resolved per call).
func TestLoadProviderConfig_HasNoCredentialSetting(t *testing.T) {
	seen := map[string]bool{}
	lookup := func(k string) string { seen[k] = true; return "" }
	if _, err := LoadProviderConfig("TESTPROV_NOCRED", lookup); err != nil {
		t.Fatal(err)
	}
	for k := range seen {
		if strings.Contains(k, "KEY") || strings.Contains(k, "SECRET") || strings.Contains(k, "TOKEN") {
			t.Fatalf("LoadProviderConfig read credential-like setting %q", k)
		}
	}
	typ := reflect.TypeOf(ProviderConfig{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "key") || strings.Contains(name, "secret") || strings.Contains(name, "token") || strings.Contains(name, "credential") {
			t.Fatalf("ProviderConfig has credential-like field %s", typ.Field(i).Name)
		}
	}
}
