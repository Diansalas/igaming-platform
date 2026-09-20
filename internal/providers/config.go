// Package providers holds scaffolding shared by future real vendor
// provider adapters (casino, sportsbook, payments, identity-compliance)
// once one is built against an actual documented contract - see
// docs/decisions/0080-provider-integration-readiness-without-external-
// contracts.md ("Provider Integration Readiness Without External
// Contracts", Decision 4). No specific real-world provider is named or
// wired in anywhere in this package; ProviderConfig is a generic shape
// keyed by whatever env var prefix a caller chooses per provider (e.g.
// "CASINO_PROVIDER_DUMMY", "SPORTSBOOK_PROVIDER_DUMMY") once one exists.
package providers

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Sane, non-security-relevant defaults applied when the corresponding env
// var is unset. Mirrors internal/config.Config's own convention of only
// defaulting values that are safe to default (never a credential or base
// URL, which have no default and are simply left empty when unset).
const (
	defaultProviderTimeoutSeconds = 10
	defaultProviderMaxRetries     = 2
)

// ProviderConfig is the generic, minimum configuration shape a future
// real vendor adapter needs: whether it is enabled, its base URL, a
// *reference* to the environment variable holding its API key/secret
// (never the credential value itself), and the per-call timeout/retry
// budget internal/providers/httpclient.ClientConfig expects.
//
// Enabled defaults to false. When false, every other field may be
// empty/zero - LoadProviderConfig never errors solely because a disabled
// provider has no URL or credential reference configured. Fail-closed
// enforcement is deliberately NOT this package's job: whoever composes a
// ProviderConfig into a real httpclient.Client-backed adapter must check
// Enabled before attempting anything (constructing a Client, resolving a
// credential, or making a call) - this package only loads configuration,
// it never gates behavior.
type ProviderConfig struct {
	Enabled      bool
	BaseURL      string
	APIKeyEnvVar string
	Timeout      time.Duration
	MaxRetries   int
}

// LoadProviderConfig reads {prefix}_ENABLED, {prefix}_BASE_URL,
// {prefix}_API_KEY_ENV_VAR, {prefix}_TIMEOUT_SECONDS, and
// {prefix}_MAX_RETRIES from the process environment.
//
// It returns an error on a malformed (non-boolean/non-integer, or
// out-of-range) value for any of these that were actually set in the
// environment - a typo'd env var is a configuration bug worth surfacing
// immediately, regardless of whether the provider ends up enabled. It
// never errors merely because Enabled is false (or unset) and the
// remaining fields are consequently left at their zero value/default -
// see ProviderConfig's own doc comment for why that fail-closed check
// belongs to the caller wiring this into a real adapter, not to this
// loader.
func LoadProviderConfig(prefix string) (ProviderConfig, error) {
	cfg := ProviderConfig{
		Timeout:    defaultProviderTimeoutSeconds * time.Second,
		MaxRetries: defaultProviderMaxRetries,
	}

	if v := os.Getenv(prefix + "_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return ProviderConfig{}, fmt.Errorf("providers: invalid %s_ENABLED: %w", prefix, err)
		}
		cfg.Enabled = b
	}

	cfg.BaseURL = os.Getenv(prefix + "_BASE_URL")
	cfg.APIKeyEnvVar = os.Getenv(prefix + "_API_KEY_ENV_VAR")

	if v := os.Getenv(prefix + "_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return ProviderConfig{}, fmt.Errorf("providers: invalid %s_TIMEOUT_SECONDS: %w", prefix, err)
		}
		if n <= 0 {
			return ProviderConfig{}, fmt.Errorf("providers: %s_TIMEOUT_SECONDS must be positive", prefix)
		}
		cfg.Timeout = time.Duration(n) * time.Second
	}

	if v := os.Getenv(prefix + "_MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return ProviderConfig{}, fmt.Errorf("providers: invalid %s_MAX_RETRIES: %w", prefix, err)
		}
		if n < 0 {
			return ProviderConfig{}, fmt.Errorf("providers: %s_MAX_RETRIES must not be negative", prefix)
		}
		cfg.MaxRetries = n
	}

	return cfg, nil
}

// ResolveAPIKey reads the environment variable named by c.APIKeyEnvVar
// and returns its value.
//
// It returns ("", nil) if APIKeyEnvVar itself is unset - nothing was
// configured to resolve (e.g. a disabled provider with no credential
// reference at all). It returns an error if APIKeyEnvVar is set but the
// environment variable it names is empty/unset: a dangling reference is
// a configuration bug, not a valid "no credential" state, and must not
// be silently treated as one.
func (c ProviderConfig) ResolveAPIKey() (string, error) {
	if c.APIKeyEnvVar == "" {
		return "", nil
	}
	v := os.Getenv(c.APIKeyEnvVar)
	if v == "" {
		return "", fmt.Errorf("providers: environment variable %s referenced by APIKeyEnvVar is not set", c.APIKeyEnvVar)
	}
	return v, nil
}
