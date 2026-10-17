package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
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

// publishedJWTSigningSecrets are the JWT_SIGNING_SECRET values this repository publishes (.env.example and the CI
// workflows). Load refuses them (as signing or previous secret) unless APP_ENV is explicitly "development": a
// deployment that copied them would let anyone forge player and staff tokens.
var publishedJWTSigningSecrets = []string{
	"dev-only-secret-change-me-32-chars-min",
	"ci-signing-secret-at-least-32-characters-long",
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
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("config: invalid %s: not an integer number of seconds", d.b.Name)
		}
		// Range-check the seconds BEFORE multiplying: n * time.Second can wrap
		// int64 (18446744134 s wraps to about 60 s) and land inside a bound.
		if n < int64(d.b.Min/time.Second) || n > int64(d.b.Max/time.Second) {
			return fmt.Errorf("config: %s must be between %d and %d seconds", d.b.Name, int64(d.b.Min/time.Second), int64(d.b.Max/time.Second))
		}
		*d.dst = time.Duration(n) * time.Second
	}
	return nil
}

// validateStaffMFA applies the ADR 0119 section 5.1 and 7.4 rules:
//
//   - the two key families are required when APP_ENV is "production" (the
//     staffmfa.LoadKeys / VerifyStartup gate, called by cmd/platform-api and
//     cmd/seed-admin since S3, requires them in every environment);
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

	configured, err := staffmfa.CheckKeyConfig(c.StaffMFAFactorActiveKID, c.StaffMFAFactorKeys.Reveal(),
		c.StaffMFAMACActiveKID, c.StaffMFAMACKeys.Reveal())
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if !configured {
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

// ValidateStaffMFAEnv applies, to the process environment, exactly the staff
// MFA rules Load applies (loadStaffMFA, validateStaffMFA and the pairwise
// platform key-family registry): the published-placeholder refusal outside
// explicit development, the duration bounds, the issuer, and distinctness
// from every other platform key family that is set. Binaries that need the
// staff MFA keys without the whole platform-api configuration (cmd/seed-admin)
// call it before staffmfa.LoadKeys (ADR 0119 S3 security review C2), so no
// binary can mint staff MFA material under keys platform-api would refuse.
func ValidateStaffMFAEnv() error {
	_, appEnvPresent := os.LookupEnv("APP_ENV")
	cfg := Config{
		Environment:         resolveAppEnv(),
		EnvironmentExplicit: appEnvPresent,
	}
	readPlatformKeyFamilies(&cfg)
	if err := loadStaffMFA(&cfg); err != nil {
		return err
	}
	if err := cfg.validateStaffMFA(); err != nil {
		return err
	}
	return cfg.validatePlatformKeyFamilies()
}

// readPlatformKeyFamilies reads every NON-staff-MFA platform key family of the
// registry from the environment, the same variables and defaults as Load
// (TestValidateStaffMFAEnv_ReadsTheSameFamiliesAsLoad pins the agreement).
func readPlatformKeyFamilies(cfg *Config) {
	cfg.JWTActiveKID = getEnvDefault("JWT_ACTIVE_KID", "k1")
	cfg.JWTSigningSecret = os.Getenv("JWT_SIGNING_SECRET")
	cfg.JWTPreviousKID = getEnvDefault("JWT_PREVIOUS_KID", "k0")
	cfg.JWTPreviousSecret = os.Getenv("JWT_PREVIOUS_SECRET")
	cfg.ProviderCredentialFingerprintKey = NewSecretValue(os.Getenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY"))
	cfg.ActorProofKeys = NewSecretValue(os.Getenv("ACTOR_PROOF_KEYS"))
	cfg.PayoutInstrumentKeys = NewSecretValue(os.Getenv("PAYOUT_INSTRUMENT_KEYS"))
	cfg.PayoutInstrumentFPKeys = NewSecretValue(os.Getenv("PAYOUT_INSTRUMENT_FP_KEYS"))
}

// ---- the platform key-family registry (ADR 0119 section 5.1, review N7) ------

// platformKeyFamily is one entry of the single registry of platform key
// families. keys returns the family's key values by kid (empty when the
// family is not configured); raw returns the configured text; list reports a
// "kid:base64" list family (otherwise the configured text is the key itself).
type platformKeyFamily struct {
	name string
	keys func(c Config) (map[string][]byte, error)
	raw  func(c Config) string
	list bool
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

// decodeAnyBase64 decodes s under standard or URL-safe base64, padded or raw
// (strict), the forms the kid-list parsers accept.
func decodeAnyBase64(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.Strict().DecodeString(s); err == nil && len(b) > 0 {
			return b, true
		}
	}
	return nil, false
}

// keyForm is one comparable form of one key of a family.
type keyForm struct {
	kid, form string
	v         []byte
}

// forms returns every form under which a key of the family could be reused
// by another family (security review C3): the raw text of a text key AND its
// base64 decoding when it decodes; the decoded bytes of a kid-list entry AND
// its encoded text. So JWT_SIGNING_SECRET=X and STAFF_MFA_MAC_KEYS=m1:X
// collide, whichever way round.
func (f platformKeyFamily) forms(c Config) ([]keyForm, error) {
	keys, err := f.keys(c)
	if err != nil {
		return nil, err
	}
	var out []keyForm
	for _, kid := range sortedKeys(keys) {
		v := keys[kid]
		if f.list {
			out = append(out, keyForm{kid, "decoded", v})
			continue
		}
		out = append(out, keyForm{kid, "text", v})
		if b, ok := decodeAnyBase64(string(v)); ok {
			out = append(out, keyForm{kid, "base64-decoded", b})
		}
	}
	if f.list && len(keys) > 0 {
		for _, part := range strings.Split(f.raw(c), ",") {
			kid, text, ok := strings.Cut(strings.TrimSpace(part), ":")
			if ok && text != "" {
				out = append(out, keyForm{kid, "encoded text", []byte(text)})
			}
		}
	}
	return out, nil
}

// platformKeyFamilies is THE registry: every platform secret key family the
// process holds. Every pair of families is checked for distinct key values in
// every form (forms), so a family added here cannot forget the check. A new
// platform key family MUST be added here (TestPlatformKeyFamilies_RegistryIsComplete
// pins the list against the Config fields).
var platformKeyFamilies = []platformKeyFamily{
	{name: "JWT_SIGNING_SECRET", keys: func(c Config) (map[string][]byte, error) {
		return singleKey(c.JWTActiveKID, c.JWTSigningSecret), nil
	}},
	{name: "JWT_PREVIOUS_SECRET", keys: func(c Config) (map[string][]byte, error) {
		return singleKey(c.JWTPreviousKID, c.JWTPreviousSecret), nil
	}},
	{name: "PROVIDER_CREDENTIAL_FINGERPRINT_KEY", keys: func(c Config) (map[string][]byte, error) {
		return singleKey("-", c.ProviderCredentialFingerprintKey.Reveal()), nil
	}},
	{name: "ACTOR_PROOF_KEYS", list: true, raw: func(c Config) string { return c.ActorProofKeys.Reveal() },
		keys: func(c Config) (map[string][]byte, error) {
			return kidListKeys("ACTOR_PROOF_KEYS", c.ActorProofKeys.Reveal(), actorproof.ParseKeySet)
		}},
	{name: "PAYOUT_INSTRUMENT_KEYS", list: true, raw: func(c Config) string { return c.PayoutInstrumentKeys.Reveal() },
		keys: func(c Config) (map[string][]byte, error) {
			return kidListKeys("PAYOUT_INSTRUMENT_KEYS", c.PayoutInstrumentKeys.Reveal(), actorproof.ParseKeySet)
		}},
	{name: "PAYOUT_INSTRUMENT_FP_KEYS", list: true, raw: func(c Config) string { return c.PayoutInstrumentFPKeys.Reveal() },
		keys: func(c Config) (map[string][]byte, error) {
			return kidListKeys("PAYOUT_INSTRUMENT_FP_KEYS", c.PayoutInstrumentFPKeys.Reveal(), actorproof.ParseKeySet)
		}},
	{name: staffmfa.EnvFactorKeys, list: true, raw: func(c Config) string { return c.StaffMFAFactorKeys.Reveal() },
		keys: func(c Config) (map[string][]byte, error) {
			return kidListKeys(staffmfa.EnvFactorKeys, c.StaffMFAFactorKeys.Reveal(), func(raw string) (map[string][]byte, error) {
				return staffmfa.ParseKeySet(staffmfa.EnvFactorKeys, raw)
			})
		}},
	{name: staffmfa.EnvMACKeys, list: true, raw: func(c Config) string { return c.StaffMFAMACKeys.Reveal() },
		keys: func(c Config) (map[string][]byte, error) {
			return kidListKeys(staffmfa.EnvMACKeys, c.StaffMFAMACKeys.Reveal(), func(raw string) (map[string][]byte, error) {
				return staffmfa.ParseKeySet(staffmfa.EnvMACKeys, raw)
			})
		}},
}

// validatePlatformKeyFamilies refuses any key value that appears, in any form,
// in two DIFFERENT registry families (pairwise, every pair). Duplicates inside
// one family are each family's own rule. Errors name families, kids and forms,
// never key material.
func (c Config) validatePlatformKeyFamilies() error {
	type owner struct{ family, kid, form string }
	seen := map[string]owner{}
	for _, f := range platformKeyFamilies {
		forms, err := f.forms(c)
		if err != nil {
			return err
		}
		for _, k := range forms {
			v := string(k.v)
			prev, dup := seen[v]
			if dup && prev.family != f.name {
				return fmt.Errorf("config: %s key %q (%s) must differ from %s key %q (%s): every platform key family must use independent secrets, compared as text and as decoded bytes",
					f.name, k.kid, k.form, prev.family, prev.kid, prev.form)
			}
			if !dup {
				seen[v] = owner{f.name, k.kid, k.form}
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
