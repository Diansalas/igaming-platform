package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// Published staff MFA placeholder keys (ADR 0119 section 5.1). They are NOT
// secrets: they are the raw bytes whose base64 appears, commented out, in
// .env.example. Load refuses them (and every other published placeholder)
// in either STAFF_MFA_* family unless APP_ENV is explicitly "development".
const (
	DevStaffMFAFactorKeyPlaceholder = "dev-only-staff-mfa-factor-key-not-a-secret-0001"
	DevStaffMFAMACKeyPlaceholder    = "dev-only-staff-mfa-mac-key-not-a-secret-00000001"
)

// publishedPlaceholderKeys is every key value this repository publishes. A
// staff MFA key equal to any of them is refused outside explicit
// development.
var publishedPlaceholderKeys = []string{
	DevStaffMFAFactorKeyPlaceholder, DevStaffMFAMACKeyPlaceholder,
	DevProviderCredentialFingerprintKeyPlaceholder, CIProviderCredentialFingerprintKeyPlaceholder,
}

// loadStaffMFA reads the ADR 0119 S1 settings into cfg. Validation is
// validateStaffMFA.
func loadStaffMFA(cfg *Config) error {
	cfg.StaffMFAFactorKeys = NewSecretValue(os.Getenv(staffmfa.EnvFactorKeys))
	cfg.StaffMFAFactorActiveKID = os.Getenv(staffmfa.EnvFactorActiveKID)
	cfg.StaffMFAMACKeys = NewSecretValue(os.Getenv(staffmfa.EnvMACKeys))
	cfg.StaffMFAMACActiveKID = os.Getenv(staffmfa.EnvMACActiveKID)
	cfg.StaffMFAIssuer = getEnvDefault("STAFF_MFA_ISSUER", staffmfa.DefaultIssuer)
	for _, d := range []struct {
		dst *time.Duration
		b   staffmfa.Bound
	}{
		{&cfg.StaffAccessTokenTTL, staffmfa.StaffAccessTokenTTLBound},
		{&cfg.StaffRefreshIdleTTL, staffmfa.StaffRefreshIdleTTLBound},
		{&cfg.StaffSessionAbsoluteTTL, staffmfa.StaffSessionAbsoluteTTLBound},
		{&cfg.StaffStepUpCriticalMaxAge, staffmfa.StepUpCriticalMaxAgeBound},
		{&cfg.StaffStepUpFinancialMaxAge, staffmfa.StepUpFinancialMaxAgeBound},
	} {
		*d.dst = d.b.Default
		v, ok := os.LookupEnv(d.b.Name)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: invalid %s: not an integer number of seconds", d.b.Name)
		}
		*d.dst = time.Duration(n) * time.Second
	}
	return nil
}

// validateStaffMFA applies the ADR 0119 section 5.1 and 7.4 rules:
//
//   - the two key families are required when APP_ENV is "production" (the
//     staffmfa.VerifyStartup gate, wired with the S3 flows, requires them in
//     every environment);
//   - a partial configuration (one family, or keys without an active kid) is
//     refused in every environment;
//   - malformed entries, keys under 32 decoded bytes, unknown active kids and
//     keys shared between kids or families are refused (staffmfa.NewKeys);
//   - a published placeholder is refused unless APP_ENV is explicitly
//     "development";
//   - the session and step-up durations must lie inside their section 7.4
//     bounds (configuration can only tighten), and the issuer must be a safe
//     otpauth label.
//
// Distinctness from every OTHER platform key family is the registry's job
// (validatePlatformKeyFamilies).
func (c Config) validateStaffMFA() error {
	for _, d := range []struct {
		v time.Duration
		b staffmfa.Bound
	}{
		{c.StaffAccessTokenTTL, staffmfa.StaffAccessTokenTTLBound},
		{c.StaffRefreshIdleTTL, staffmfa.StaffRefreshIdleTTLBound},
		{c.StaffSessionAbsoluteTTL, staffmfa.StaffSessionAbsoluteTTLBound},
		{c.StaffStepUpCriticalMaxAge, staffmfa.StepUpCriticalMaxAgeBound},
		{c.StaffStepUpFinancialMaxAge, staffmfa.StepUpFinancialMaxAgeBound},
	} {
		if err := d.b.Check(d.v); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	if err := staffmfa.ValidateIssuer(c.StaffMFAIssuer); err != nil {
		return fmt.Errorf("config: STAFF_MFA_ISSUER must be 1-64 bytes of UTF-8 without ':', control characters or surrounding whitespace")
	}

	keys, err := staffmfa.NewKeysFromConfig(c.StaffMFAFactorActiveKID, c.StaffMFAFactorKeys.Reveal(),
		c.StaffMFAMACActiveKID, c.StaffMFAMACKeys.Reveal())
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if keys == nil {
		if c.Environment == "production" {
			return fmt.Errorf("config: %s / %s and %s / %s are required when APP_ENV=production (ADR 0119 section 5.1); see docs/runbooks/staff-mfa-keys.md",
				staffmfa.EnvFactorKeys, staffmfa.EnvFactorActiveKID, staffmfa.EnvMACKeys, staffmfa.EnvMACActiveKID)
		}
		return nil
	}
	if c.GuardEnvironment() != "development" {
		for family, raw := range map[string]string{staffmfa.EnvFactorKeys: c.StaffMFAFactorKeys.Reveal(), staffmfa.EnvMACKeys: c.StaffMFAMACKeys.Reveal()} {
			set, err := staffmfa.ParseKeySet(family, raw)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			for kid, k := range set {
				for _, p := range publishedPlaceholderKeys {
					if string(k) == p {
						return fmt.Errorf("config: %s key %q is a published placeholder and is refused unless APP_ENV is explicitly \"development\"", family, kid)
					}
				}
			}
		}
	}
	return nil
}

// ---- the platform key-family registry (ADR 0119 section 5.1, review N7) ------

// platformKeyFamily is one entry of the single registry of platform key
// families. keys returns the family's key values by kid (empty when the
// family is not configured).
type platformKeyFamily struct {
	name string
	keys func(c Config) (map[string][]byte, error)
}

func singleKey(kid, v string) map[string][]byte {
	if v == "" {
		return nil
	}
	return map[string][]byte{kid: []byte(v)}
}

func kidListKeys(name string, raw string, parse func(string) (map[string][]byte, error)) (map[string][]byte, error) {
	if raw == "" {
		return nil, nil
	}
	m, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", name, err)
	}
	return m, nil
}

// platformKeyFamilies is THE registry: every platform secret key family the
// process holds. Every pair of families is checked for distinct key values,
// so a family added here cannot forget the check. A new platform key family
// MUST be added here (TestPlatformKeyFamilies_RegistryIsComplete pins the
// list against the Config fields).
var platformKeyFamilies = []platformKeyFamily{
	{"JWT_SIGNING_SECRET", func(c Config) (map[string][]byte, error) {
		return singleKey(c.JWTActiveKID, c.JWTSigningSecret), nil
	}},
	{"JWT_PREVIOUS_SECRET", func(c Config) (map[string][]byte, error) {
		return singleKey(c.JWTPreviousKID, c.JWTPreviousSecret), nil
	}},
	{"PROVIDER_CREDENTIAL_FINGERPRINT_KEY", func(c Config) (map[string][]byte, error) {
		return singleKey("-", c.ProviderCredentialFingerprintKey.Reveal()), nil
	}},
	{"ACTOR_PROOF_KEYS", func(c Config) (map[string][]byte, error) {
		return kidListKeys("ACTOR_PROOF_KEYS", c.ActorProofKeys.Reveal(), actorproof.ParseKeySet)
	}},
	{"PAYOUT_INSTRUMENT_KEYS", func(c Config) (map[string][]byte, error) {
		return kidListKeys("PAYOUT_INSTRUMENT_KEYS", c.PayoutInstrumentKeys.Reveal(), actorproof.ParseKeySet)
	}},
	{"PAYOUT_INSTRUMENT_FP_KEYS", func(c Config) (map[string][]byte, error) {
		return kidListKeys("PAYOUT_INSTRUMENT_FP_KEYS", c.PayoutInstrumentFPKeys.Reveal(), actorproof.ParseKeySet)
	}},
	{staffmfa.EnvFactorKeys, func(c Config) (map[string][]byte, error) {
		return kidListKeys(staffmfa.EnvFactorKeys, c.StaffMFAFactorKeys.Reveal(), func(raw string) (map[string][]byte, error) {
			return staffmfa.ParseKeySet(staffmfa.EnvFactorKeys, raw)
		})
	}},
	{staffmfa.EnvMACKeys, func(c Config) (map[string][]byte, error) {
		return kidListKeys(staffmfa.EnvMACKeys, c.StaffMFAMACKeys.Reveal(), func(raw string) (map[string][]byte, error) {
			return staffmfa.ParseKeySet(staffmfa.EnvMACKeys, raw)
		})
	}},
}

// validatePlatformKeyFamilies refuses any key value that appears in two
// DIFFERENT registry families (pairwise, every pair). Duplicates inside one
// family are each family's own rule. Errors name families and kids, never
// key material.
func (c Config) validatePlatformKeyFamilies() error {
	type owner struct{ family, kid string }
	seen := map[string]owner{}
	for _, f := range platformKeyFamilies {
		keys, err := f.keys(c)
		if err != nil {
			return err
		}
		mine := map[string]bool{}
		for _, kid := range sortedKeys(keys) {
			v := string(keys[kid])
			if prev, dup := seen[v]; dup && prev.family != f.name {
				return fmt.Errorf("config: %s key %q must differ from %s key %q (every platform key family must use independent secrets)", f.name, kid, prev.family, prev.kid)
			}
			if !mine[v] {
				mine[v] = true
				seen[v] = owner{f.name, kid}
			}
		}
	}
	return nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out) // deterministic error messages
	return out
}
