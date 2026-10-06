package config

import (
	"os"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"APP_ENV", "HTTP_ADDR", "DATABASE_URL", "DATABASE_MAX_CONNS", "JWT_SIGNING_SECRET", "OTEL_SERVICE_NAME", "OTEL_EXPORTER", "CORS_ALLOWED_ORIGINS", "TEST_SUPPORT_ENDPOINTS_ENABLED",
		// Stage 10.3 W2a: CI's job env carries the published CI fingerprint-key
		// placeholder, which Load refuses outside explicit development.
		"PROVIDER_CREDENTIAL_FINGERPRINT_KEY", "SECRETSTORE_BACKENDS", "SECRETSTORE_DEVFILE_ROOT", "JWT_PREVIOUS_SECRET",
		// Stage 10.3 W3b wiring: the awssm region sources.
		"AWS_SECRETSMANAGER_REGION", "AWS_REGION",
		// PRH-2 R5 (ADR 0110): the signed-actor-proof key configuration.
		"ACTOR_PROOF_KEYS", "ACTOR_PROOF_ACTIVE_KID"} {
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

// --- Stage 9.4: APP_ENV closed-set validation, TEST_SUPPORT_ENDPOINTS_ENABLED,
// and the contradictory-configuration fail-closed case. ---

// Unset APP_ENV must still default to "development" and Load() must
// still succeed - the existing local/CI convenience this stage's
// directive explicitly required not be broken. TestLoad_DefaultsAndOverrides
// above already covers this for the base case; this test names the
// property explicitly for this stage's own record.
func TestLoad_UnsetAppEnvStillDefaultsToDevelopment(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error with APP_ENV unset: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("expected default environment 'development', got %q", cfg.Environment)
	}
	if cfg.TestSupportEndpointsEnabled {
		t.Error("expected TestSupportEndpointsEnabled to default to false")
	}
	if cfg.TestSupportRoutesEnabled() {
		t.Error("expected TestSupportRoutesEnabled() to be false when TestSupportEndpointsEnabled was never set, even though Environment != production")
	}
}

// --- Stage 10.3 W1b, MOCK-ADAPTER-PROD-1: EnvironmentExplicit and
// GuardEnvironment (security condition C13, ruling R8; ADR 0085's Stage
// 10.3 amendment). ---

func TestLoad_EnvironmentExplicit_UnsetIsFalse(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EnvironmentExplicit {
		t.Error("expected EnvironmentExplicit to be false when APP_ENV was never set")
	}
	if cfg.Environment != "development" {
		t.Errorf("expected Environment to still default to development, got %q", cfg.Environment)
	}
}

func TestLoad_EnvironmentExplicit_TrueWhenSet(t *testing.T) {
	for _, env := range []string{"development", "staging", "production"} {
		t.Run(env, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
			t.Setenv("APP_ENV", env)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !cfg.EnvironmentExplicit {
				t.Errorf("expected EnvironmentExplicit to be true when APP_ENV=%q was explicitly set", env)
			}
		})
	}
}

// TestGuardEnvironment_Matrix is the required W1b matrix: {production,
// missing, staging, development}. "missing" (EnvironmentExplicit == false)
// must resolve identically to "production" for this method ONLY - every
// other consumer of Environment (TestSupportRoutesEnabled, logging, etc.)
// is untouched and still sees "development" for that same Config value,
// which TestLoad_EnvironmentExplicit_UnsetIsFalse and
// TestLoad_UnsetAppEnvStillDefaultsToDevelopment above both confirm.
func TestGuardEnvironment_Matrix(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		explicit    bool
		want        string
	}{
		{"production_explicit", "production", true, "production"},
		{"missing_app_env", "development", false, "production"},
		{"staging_explicit", "staging", true, "staging"},
		{"development_explicit", "development", true, "development"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Environment: tc.environment, EnvironmentExplicit: tc.explicit}
			if got := cfg.GuardEnvironment(); got != tc.want {
				t.Errorf("GuardEnvironment() = %q, want %q (environment=%q explicit=%v)", got, tc.want, tc.environment, tc.explicit)
			}
		})
	}
}

func TestGuardEnvironment_DoesNotChangeEnvironmentOrTestSupportRoutesEnabled(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("TEST_SUPPORT_ENDPOINTS_ENABLED", "true")
	// APP_ENV left unset entirely.

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("Environment must remain 'development' for a missing APP_ENV - GuardEnvironment must not mutate Config, got %q", cfg.Environment)
	}
	if cfg.GuardEnvironment() != "production" {
		t.Errorf("expected GuardEnvironment() to treat a missing APP_ENV as production, got %q", cfg.GuardEnvironment())
	}
	if !cfg.TestSupportRoutesEnabled() {
		t.Error("TestSupportRoutesEnabled() must be unaffected by GuardEnvironment's stricter rule - it still uses Environment directly, per ADR 0085's own scope")
	}
}

// --- ValidateSecretBackendScheme (ADR 0093 §6 placeholder; C13/R8's "small
// function that W2/W3 will consume"). ---

func TestValidateSecretBackendScheme(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		explicit    bool
		scheme      string
		wantErr     bool
	}{
		// ADR 0093 §6: awssm only in EXPLICIT staging/production.
		{"awssm_allowed_in_production", "production", true, SecretBackendAWSSecretsManager, false},
		{"awssm_allowed_in_staging", "staging", true, SecretBackendAWSSecretsManager, false},
		{"awssm_refused_in_development", "development", true, SecretBackendAWSSecretsManager, true},
		{"awssm_refused_when_app_env_missing", "development", false, SecretBackendAWSSecretsManager, true},
		{"awssm_refused_when_app_env_missing_even_if_environment_reads_production", "production", false, SecretBackendAWSSecretsManager, true},
		{"devfile_allowed_in_explicit_development", "development", true, SecretBackendDevFile, false},
		{"devfile_refused_in_staging", "staging", true, SecretBackendDevFile, true},
		{"devfile_refused_in_production", "production", true, SecretBackendDevFile, true},
		{"devfile_refused_when_app_env_missing", "development", false, SecretBackendDevFile, true},
		{"memory_always_refused_in_development", "development", true, SecretBackendMemory, true},
		{"memory_always_refused_in_production", "production", true, SecretBackendMemory, true},
		{"memory_always_refused_in_staging", "staging", true, SecretBackendMemory, true},
		{"memory_always_refused_when_app_env_missing", "development", false, SecretBackendMemory, true},
		{"unknown_scheme_refused", "development", true, "s3", true},
	}
	// Fail-closed: with APP_ENV missing, NO backend is selectable.
	missing := Config{Environment: "development", EnvironmentExplicit: false}
	for _, scheme := range []string{SecretBackendAWSSecretsManager, SecretBackendDevFile, SecretBackendMemory} {
		if err := missing.ValidateSecretBackendScheme(scheme); err == nil {
			t.Errorf("APP_ENV missing: backend %q must be refused (no backend is selectable)", scheme)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Environment: tc.environment, EnvironmentExplicit: tc.explicit}
			err := cfg.ValidateSecretBackendScheme(tc.scheme)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for scheme %q (environment=%q explicit=%v), got nil", tc.scheme, tc.environment, tc.explicit)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error for scheme %q (environment=%q explicit=%v), got: %v", tc.scheme, tc.environment, tc.explicit, err)
			}
		})
	}
}

// A typo'd, wrongly-cased, or whitespace-padded APP_ENV value must FAIL
// Load() outright - the exact vector two independent Stage 9.3 security
// reviews flagged as failing OPEN (a mis-set value silently resolved to
// "not production").
func TestLoad_InvalidAppEnvRejected(t *testing.T) {
	for _, bad := range []string{"Production", "PRODUCTION", "prod", "production ", " production", "Staging", "dev", "test"} {
		t.Run(bad, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
			t.Setenv("APP_ENV", bad)

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for invalid APP_ENV %q, got nil", bad)
			}
		})
	}
}

// APP_ENV explicitly set to an empty string is NOT the same as APP_ENV
// being unset, and must be rejected exactly like any other invalid value -
// the one gap both the Stage 9.4 security and architecture reviews
// independently found: a naive getEnvDefault-style helper folds "unset"
// and "present but empty" into the same default, which would let a future
// deployment tool that emits `APP_ENV=` (rather than omitting the
// variable) silently resolve to "development" and slip past Layer 1
// undetected. resolveAppEnv() uses os.LookupEnv directly specifically to
// keep this case distinguishable from a genuinely unset variable.
func TestLoad_ExplicitEmptyAppEnvRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("APP_ENV", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for explicit APP_ENV=\"\", got nil - an explicit empty value must not silently resolve to \"development\"")
	}
}

// The three valid, exact values must all still succeed.
func TestLoad_ValidAppEnvValuesAccepted(t *testing.T) {
	for _, good := range []string{"development", "staging", "production"} {
		t.Run(good, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
			t.Setenv("APP_ENV", good)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error for valid APP_ENV %q: %v", good, err)
			}
			if cfg.Environment != good {
				t.Errorf("expected Environment %q, got %q", good, cfg.Environment)
			}
		})
	}
}

// TEST_SUPPORT_ENDPOINTS_ENABLED must default to false and be overridable
// to true in a non-production environment.
func TestLoad_TestSupportEndpointsEnabledOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("APP_ENV", "staging")
	t.Setenv("TEST_SUPPORT_ENDPOINTS_ENABLED", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.TestSupportEndpointsEnabled {
		t.Error("expected TestSupportEndpointsEnabled true after override")
	}
	if !cfg.TestSupportRoutesEnabled() {
		t.Error("expected TestSupportRoutesEnabled() true for staging + TestSupportEndpointsEnabled=true")
	}
}

func TestLoad_InvalidTestSupportEndpointsEnabled(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("TEST_SUPPORT_ENDPOINTS_ENABLED", "not-a-bool")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for non-boolean TEST_SUPPORT_ENDPOINTS_ENABLED, got nil")
	}
}

// The explicit, required contradictory-configuration case: APP_ENV=production
// AND TEST_SUPPORT_ENDPOINTS_ENABLED=true together must FAIL Load() with a
// clear, specific error - never silently ignored/corrected.
func TestLoad_ProductionWithTestSupportEndpointsEnabledFailsClosed(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("APP_ENV", "production")
	t.Setenv("TEST_SUPPORT_ENDPOINTS_ENABLED", "true")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for APP_ENV=production combined with TEST_SUPPORT_ENDPOINTS_ENABLED=true, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "TEST_SUPPORT_ENDPOINTS_ENABLED") || !strings.Contains(got, "production") {
		t.Errorf("expected a specific error naming both APP_ENV=production and TEST_SUPPORT_ENDPOINTS_ENABLED, got: %v", got)
	}
}

// Production with the flag left at its default (false) must still start
// cleanly - the contradiction check must not be a blanket rejection of
// production, only of the unsafe combination.
func TestLoad_ProductionWithTestSupportEndpointsDefaultSucceeds(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("APP_ENV", "production")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error for plain APP_ENV=production: %v", err)
	}
	if cfg.TestSupportRoutesEnabled() {
		t.Error("expected TestSupportRoutesEnabled() to be false in production")
	}
}

// Each of the two gates is independently insufficient: Environment !=
// "production" alone (with TestSupportEndpointsEnabled left false) must
// not enable the routes, and TestSupportEndpointsEnabled=true alone (with
// Environment left at its "development" default) must ALSO not be
// reachable in production - proven here by the fact that a "development"
// environment with the flag off computes false, exercising both operands
// of the AND independently.
func TestLoad_TestSupportRoutesEnabled_BothConditionsIndependentlyRequired(t *testing.T) {
	cases := []struct {
		name              string
		environment       string
		testSupportEnvVal string // "" means unset
		want              bool
	}{
		{"development_flag_unset", "development", "", false},
		{"development_flag_false", "development", "false", false},
		{"development_flag_true", "development", "true", true},
		{"staging_flag_true", "staging", "true", true},
		{"staging_flag_unset", "staging", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
			t.Setenv("APP_ENV", tc.environment)
			if tc.testSupportEnvVal != "" {
				t.Setenv("TEST_SUPPORT_ENDPOINTS_ENABLED", tc.testSupportEnvVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.TestSupportRoutesEnabled(); got != tc.want {
				t.Errorf("TestSupportRoutesEnabled() = %v, want %v (environment=%q test_support_env=%q)", got, tc.want, tc.environment, tc.testSupportEnvVal)
			}
		})
	}
}

// PRH-2 H: PAYMENTS_SWEEP_INTERVAL_SECONDS defaults to ADR 0095 §7.3's 15 s tick, must be a
// positive integer, and is validated like its sibling sweep intervals.
func TestLoad_PaymentsSweepInterval(t *testing.T) {
	base := func(t *testing.T) {
		clearEnv(t)
		t.Setenv("DATABASE_URL", "postgres://localhost/test")
		t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
		t.Setenv("PAYMENTS_SWEEP_INTERVAL_SECONDS", "")
		_ = os.Unsetenv("PAYMENTS_SWEEP_INTERVAL_SECONDS")
	}
	base(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PaymentsSweepInterval.Seconds() != 15 {
		t.Fatalf("default = %v, want 15s", cfg.PaymentsSweepInterval)
	}
	base(t)
	t.Setenv("PAYMENTS_SWEEP_INTERVAL_SECONDS", "7")
	if cfg, err = Load(); err != nil || cfg.PaymentsSweepInterval.Seconds() != 7 {
		t.Fatalf("override: %v %v", cfg.PaymentsSweepInterval, err)
	}
	for _, bad := range []string{"0", "-3", "abc"} {
		base(t)
		t.Setenv("PAYMENTS_SWEEP_INTERVAL_SECONDS", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("PAYMENTS_SWEEP_INTERVAL_SECONDS=%q must be refused", bad)
		}
	}
}

// PRH-2 E1 (ADR 0106 section 7.3): KYC_OUTBOX_INTERVAL_SECONDS defaults to the 5 s
// engineering RECOMMENDATION and must be a positive integer.
func TestLoad_KYCOutboxInterval(t *testing.T) {
	base := func(t *testing.T) {
		clearEnv(t)
		t.Setenv("DATABASE_URL", "postgres://localhost/test")
		t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
		_ = os.Unsetenv("KYC_OUTBOX_INTERVAL_SECONDS")
	}
	base(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KYCOutboxInterval.Seconds() != 5 {
		t.Fatalf("default = %v, want 5s", cfg.KYCOutboxInterval)
	}
	base(t)
	t.Setenv("KYC_OUTBOX_INTERVAL_SECONDS", "9")
	if cfg, err = Load(); err != nil || cfg.KYCOutboxInterval.Seconds() != 9 {
		t.Fatalf("override: %v %v", cfg.KYCOutboxInterval, err)
	}
	for _, bad := range []string{"0", "-3", "abc"} {
		base(t)
		t.Setenv("KYC_OUTBOX_INTERVAL_SECONDS", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("KYC_OUTBOX_INTERVAL_SECONDS=%q must be refused", bad)
		}
	}
}
