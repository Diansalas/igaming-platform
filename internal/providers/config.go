// Package providers holds scaffolding shared by future real vendor
// provider adapters (casino, sportsbook, payments, identity-compliance)
// once one is built against an actual documented contract - see
// docs/decisions/0080-provider-integration-readiness-without-external-
// contracts.md ("Provider Integration Readiness Without External
// Contracts", Decision 4). No specific real-world provider is named or
// wired in anywhere in this package; ProviderConfig is a generic shape
// keyed by whatever setting prefix a caller chooses per provider (e.g.
// "CASINO_PROVIDER_DUMMY", "SPORTSBOOK_PROVIDER_DUMMY") once one exists.
//
// Stage 10.3 W2a (PROV-OUTBOUND-CRED-1, ADR 0093 §5): this package carries
// NO credential and no credential reference any more. The former
// ProviderConfig.APIKeyEnvVar / ResolveAPIKey (one static, process-wide key
// read from an environment variable) is removed: an outbound credential is
// resolved PER CALL, per tenant, from the tenant's outbound_api handle
// (internal/providercred.OutboundResolver) and applied through a per-call
// httpclient.Authenticator. TestOutbound_APIKeyEnvVarRemoved enforces that
// no code references APIKeyEnvVar and that this package reads no process
// environment itself (the caller supplies the lookup function).
package providers

import (
	"fmt"
	"strconv"
	"time"
)

// Sane, non-security-relevant defaults applied when the corresponding
// setting is unset. Mirrors internal/config.Config's own convention of only
// defaulting values that are safe to default (never a base URL, which has
// no default and is simply left empty when unset).
const (
	defaultProviderTimeoutSeconds = 10
	defaultProviderMaxRetries     = 2
)

// ProviderConfig is the generic, minimum, credential-free configuration
// shape a future real vendor adapter needs: whether it is enabled, its
// base URL, and the per-call timeout/retry budget
// internal/providers/httpclient.ClientConfig expects.
//
// Enabled defaults to false. When false, every other field may be
// empty/zero - LoadProviderConfig never errors solely because a disabled
// provider has no URL configured. Fail-closed enforcement is deliberately
// NOT this package's job: whoever composes a ProviderConfig into a real
// httpclient.Client-backed adapter must check Enabled before attempting
// anything - this package only loads configuration, it never gates
// behavior.
type ProviderConfig struct {
	Enabled    bool
	BaseURL    string
	Timeout    time.Duration
	MaxRetries int
}

// LoadProviderConfig reads {prefix}_ENABLED, {prefix}_BASE_URL,
// {prefix}_TIMEOUT_SECONDS and {prefix}_MAX_RETRIES through lookup (the
// caller passes os.Getenv or a test map; this package never reads the
// process environment itself).
//
// It returns an error on a malformed (non-boolean/non-integer, or
// out-of-range) value for any of these that were actually set - a typo'd
// setting is a configuration bug worth surfacing immediately, regardless of
// whether the provider ends up enabled. It never errors merely because
// Enabled is false (or unset) and the remaining fields are consequently
// left at their zero value/default.
func LoadProviderConfig(prefix string, lookup func(string) string) (ProviderConfig, error) {
	cfg := ProviderConfig{
		Timeout:    defaultProviderTimeoutSeconds * time.Second,
		MaxRetries: defaultProviderMaxRetries,
	}
	if lookup == nil {
		return cfg, nil
	}

	if v := lookup(prefix + "_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return ProviderConfig{}, fmt.Errorf("providers: invalid %s_ENABLED: %w", prefix, err)
		}
		cfg.Enabled = b
	}

	cfg.BaseURL = lookup(prefix + "_BASE_URL")

	if v := lookup(prefix + "_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return ProviderConfig{}, fmt.Errorf("providers: invalid %s_TIMEOUT_SECONDS: %w", prefix, err)
		}
		if n <= 0 {
			return ProviderConfig{}, fmt.Errorf("providers: %s_TIMEOUT_SECONDS must be positive", prefix)
		}
		cfg.Timeout = time.Duration(n) * time.Second
	}

	if v := lookup(prefix + "_MAX_RETRIES"); v != "" {
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
