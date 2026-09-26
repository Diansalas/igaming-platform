package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// Stage 10.3 W2a (ADR 0093 A3; security review 07-w2a-design-review-
// security.md §3): PROVIDER_CREDENTIAL_FINGERPRINT_KEY validation and
// redaction, and SECRETSTORE_BACKENDS validation. Keys are generated at
// runtime; the only literals are the PUBLISHED placeholders Load refuses.

func randomKey(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)[:n]
}

func baseEnv(t *testing.T, appEnv string) {
	t.Helper()
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", randomKey(t, 40))
	if appEnv != "" {
		t.Setenv("APP_ENV", appEnv)
	}
}

func TestConfig_FingerprintKey_AbsentIsValid(t *testing.T) {
	baseEnv(t, "production")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("an absent key must not refuse startup: %v", err)
	}
	if cfg.ProviderCredentialFingerprintKey.IsSet() {
		t.Fatal("absent key must be unset")
	}
}

func TestConfig_FingerprintKey_TooShortRefused(t *testing.T) {
	baseEnv(t, "development")
	t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", randomKey(t, 31))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("a 31-byte key must be refused, got %v", err)
	}
	t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", randomKey(t, 32))
	if _, err := Load(); err != nil {
		t.Fatalf("a 32-byte key must be accepted: %v", err)
	}
}

func TestConfig_FingerprintKey_EqualsJWTSecretRefused(t *testing.T) {
	baseEnv(t, "development")
	jwt := randomKey(t, 40)
	t.Setenv("JWT_SIGNING_SECRET", jwt)
	t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", jwt)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("a key equal to JWT_SIGNING_SECRET must be refused, got %v", err)
	}
	prev := randomKey(t, 40)
	t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", prev)
	t.Setenv("JWT_PREVIOUS_SECRET", prev)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("a key equal to JWT_PREVIOUS_SECRET must be refused, got %v", err)
	}
}

func TestConfig_FingerprintKey_DevLiteralRefusedOutsideDevelopment(t *testing.T) {
	for _, literal := range []string{DevProviderCredentialFingerprintKeyPlaceholder, CIProviderCredentialFingerprintKeyPlaceholder} {
		for _, env := range []string{"staging", "production", ""} {
			baseEnv(t, env)
			t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", literal)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("APP_ENV=%q: the published placeholder must be refused, got %v", env, err)
			}
		}
		baseEnv(t, "development")
		t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", literal)
		if _, err := Load(); err != nil {
			t.Fatalf("explicit development must accept the placeholder: %v", err)
		}
	}
}

func TestConfig_FingerprintKeyRedacted(t *testing.T) {
	baseEnv(t, "development")
	key := randomKey(t, 48)
	t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", key)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderCredentialFingerprintKey.Reveal() != key {
		t.Fatal("Reveal must return the key")
	}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		outs = append(outs, fmt.Sprintf(verb, cfg), fmt.Sprintf(verb, cfg.ProviderCredentialFingerprintKey))
	}
	var sb strings.Builder
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("cfg", "config", cfg, "key", cfg.ProviderCredentialFingerprintKey)
	slog.New(slog.NewTextHandler(&sb, nil)).Info("cfg", "config", cfg, "key", cfg.ProviderCredentialFingerprintKey)
	outs = append(outs, sb.String())
	j, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(j))
	for _, out := range outs {
		if strings.Contains(out, key) || strings.Contains(out, hex.EncodeToString([]byte(key))) {
			t.Fatalf("the fingerprint key rendered: %q", out)
		}
	}
}

func TestConfig_SecretStoreBackendsValidated(t *testing.T) {
	baseEnv(t, "development")
	t.Setenv("SECRETSTORE_BACKENDS", "devfile")
	cfg, err := Load()
	if err != nil || len(cfg.SecretStoreBackends) != 1 || cfg.SecretStoreBackends[0] != "devfile" {
		t.Fatalf("devfile in explicit development: %v %v", cfg.SecretStoreBackends, err)
	}
	if cfg.SecretStoreDevFileRoot != "./.secrets/dev" {
		t.Fatalf("default devfile root = %q", cfg.SecretStoreDevFileRoot)
	}
	for _, tc := range []struct{ env, backends string }{
		{"production", "devfile"}, {"staging", "devfile"}, {"", "devfile"},
		{"development", "memory"}, {"production", "memory"}, {"development", "file"},
	} {
		baseEnv(t, tc.env)
		t.Setenv("SECRETSTORE_BACKENDS", tc.backends)
		if _, err := Load(); err == nil {
			t.Fatalf("APP_ENV=%q SECRETSTORE_BACKENDS=%q must refuse startup", tc.env, tc.backends)
		}
	}
}
