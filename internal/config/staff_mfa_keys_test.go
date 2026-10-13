package config

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// ADR 0119 S1: STAFF_MFA_* validation, the session bounds, and the single
// platform key-family registry. Keys are generated at runtime, never literals
// (the published placeholders are the one deliberate exception, and they are
// the refusal under test).

// setStaffMFAKeys sets two fresh, valid staff MFA key families.
func setStaffMFAKeys(t *testing.T) (factor, mac string) {
	t.Helper()
	factor, mac = b64Key(t, 32), b64Key(t, 32)
	t.Setenv("STAFF_MFA_FACTOR_KEYS", "sf1:"+factor)
	t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", "sf1")
	t.Setenv("STAFF_MFA_MAC_KEYS", "sm1:"+mac)
	t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", "sm1")
	return factor, mac
}

func unsetStaffMFAKeys(t *testing.T) {
	t.Helper()
	for _, k := range []string{"STAFF_MFA_FACTOR_KEYS", "STAFF_MFA_FACTOR_ACTIVE_KID", "STAFF_MFA_MAC_KEYS", "STAFF_MFA_MAC_ACTIVE_KID"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestStaffMFA_AbsentIsValidOutsideProduction(t *testing.T) {
	for _, env := range []string{"development", "staging", ""} {
		t.Run("APP_ENV="+env, func(t *testing.T) {
			baseEnv(t, env)
			unsetStaffMFAKeys(t)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("absent staff MFA keys must load outside production (VerifyStartup is the every-environment gate, wired in S3): %v", err)
			}
			if cfg.StaffMFAFactorKeys.IsSet() || cfg.StaffMFAMACKeys.IsSet() {
				t.Fatal("absent keys must be unset")
			}
		})
	}
}

func TestStaffMFA_RequiredInProduction(t *testing.T) {
	baseEnv(t, "production")
	unsetStaffMFAKeys(t)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "STAFF_MFA_FACTOR_KEYS") || !strings.Contains(err.Error(), "STAFF_MFA_MAC_KEYS") {
		t.Fatalf("production without staff MFA keys must refuse, got %v", err)
	}
	setStaffMFAKeys(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("production with valid keys must load: %v", err)
	}
	if cfg.StaffMFAFactorActiveKID != "sf1" || cfg.StaffMFAMACActiveKID != "sm1" {
		t.Fatalf("active kids = %q %q", cfg.StaffMFAFactorActiveKID, cfg.StaffMFAMACActiveKID)
	}
}

func TestStaffMFA_PartialConfigRefusedEverywhere(t *testing.T) {
	for _, env := range []string{"development", "production"} {
		for _, missing := range []string{"STAFF_MFA_FACTOR_KEYS", "STAFF_MFA_FACTOR_ACTIVE_KID", "STAFF_MFA_MAC_KEYS", "STAFF_MFA_MAC_ACTIVE_KID"} {
			t.Run(env+"/"+missing, func(t *testing.T) {
				baseEnv(t, env)
				setStaffMFAKeys(t)
				t.Setenv(missing, "")
				if _, err := Load(); err == nil {
					t.Fatal("a partial staff MFA configuration must be refused")
				}
			})
		}
	}
}

func TestStaffMFA_Refusals_WithoutEchoingKeys(t *testing.T) {
	good, other := b64Key(t, 32), b64Key(t, 32)
	short := b64Key(t, 31)
	cases := []struct {
		name                    string
		factor, fkid, mac, mkid string
	}{
		{"short factor key", "sf1:" + short, "sf1", "sm1:" + other, "sm1"},
		{"short mac key", "sf1:" + good, "sf1", "sm1:" + short, "sm1"},
		{"unknown factor active kid", "sf1:" + good, "sf9", "sm1:" + other, "sm1"},
		{"unknown mac active kid", "sf1:" + good, "sf1", "sm1:" + other, "sm9"},
		{"malformed entry", "sf1=" + good, "sf1", "sm1:" + other, "sm1"},
		{"not base64", "sf1:not base64!", "sf1", "sm1:" + other, "sm1"},
		{"empty entry", "sf1:" + good + ",", "sf1", "sm1:" + other, "sm1"},
		{"same key across the two families", "sf1:" + good, "sf1", "sm1:" + good, "sm1"},
		{"same key across kids", "sf1:" + good + ",sf2:" + good, "sf1", "sm1:" + other, "sm1"},
		{"duplicate kid", "sf1:" + good + ",sf1:" + b64Key(t, 32), "sf1", "sm1:" + other, "sm1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, "production")
			t.Setenv("STAFF_MFA_FACTOR_KEYS", tc.factor)
			t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", tc.fkid)
			t.Setenv("STAFF_MFA_MAC_KEYS", tc.mac)
			t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", tc.mkid)
			_, err := Load()
			if err == nil {
				t.Fatal("expected Load to refuse")
			}
			if containsAny(err.Error(), good, other, short) {
				t.Fatalf("error echoes key material: %v", err)
			}
		})
	}
}

func TestStaffMFA_RotationTwoKidsLoads(t *testing.T) {
	baseEnv(t, "production")
	t.Setenv("STAFF_MFA_FACTOR_KEYS", "sf1:"+b64Key(t, 32)+",sf2:"+b64Key(t, 40))
	t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", "sf2")
	t.Setenv("STAFF_MFA_MAC_KEYS", "sm1:"+b64Key(t, 32)+",sm2:"+b64Key(t, 32))
	t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", "sm1")
	if _, err := Load(); err != nil {
		t.Fatalf("a rotation configuration must load: %v", err)
	}
}

// The published placeholders are refused outside an EXPLICIT development
// environment (a missing APP_ENV counts as production), and the values pinned
// here are exactly the ones .env.example publishes.
func TestStaffMFA_PublishedPlaceholders(t *testing.T) {
	envExample := readEnvExample(t)
	for name, want := range map[string]string{
		"STAFF_MFA_FACTOR_KEYS": DevStaffMFAFactorKeyPlaceholder,
		"STAFF_MFA_MAC_KEYS":    DevStaffMFAMACKeyPlaceholder,
	} {
		v, ok := envExample[name]
		if !ok {
			t.Fatalf(".env.example does not publish %s", name)
		}
		kid, b, found := strings.Cut(v, ":")
		dec, err := base64.StdEncoding.DecodeString(b)
		if !found || kid != "dev1" || err != nil || string(dec) != want {
			t.Fatalf(".env.example %s does not decode to the pinned placeholder", name)
		}
		if len(want) < 32 {
			t.Fatalf("placeholder %s is shorter than 32 bytes", name)
		}
	}
	placeholderFactor := "dev1:" + base64.StdEncoding.EncodeToString([]byte(DevStaffMFAFactorKeyPlaceholder))
	placeholderMAC := "dev1:" + base64.StdEncoding.EncodeToString([]byte(DevStaffMFAMACKeyPlaceholder))
	ciFingerprint := "dev1:" + base64.StdEncoding.EncodeToString([]byte(CIProviderCredentialFingerprintKeyPlaceholder))
	cases := []struct {
		name        string
		factor, mac string
	}{
		{"factor placeholder", placeholderFactor, "sm1:" + b64Key(t, 32)},
		{"mac placeholder", "sf1:" + b64Key(t, 32), placeholderMAC},
		{"fingerprint CI placeholder reused", ciFingerprint, "sm1:" + b64Key(t, 32)},
	}
	for _, tc := range cases {
		for _, env := range []string{"production", "staging", ""} {
			t.Run(tc.name+"/APP_ENV="+env, func(t *testing.T) {
				baseEnv(t, env)
				t.Setenv("STAFF_MFA_FACTOR_KEYS", tc.factor)
				t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", strings.SplitN(tc.factor, ":", 2)[0])
				t.Setenv("STAFF_MFA_MAC_KEYS", tc.mac)
				t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", strings.SplitN(tc.mac, ":", 2)[0])
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), "placeholder") {
					t.Fatalf("a published placeholder must be refused, got %v", err)
				}
			})
		}
	}
	baseEnv(t, "development")
	t.Setenv("STAFF_MFA_FACTOR_KEYS", placeholderFactor)
	t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", "dev1")
	t.Setenv("STAFF_MFA_MAC_KEYS", placeholderMAC)
	t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", "dev1")
	if _, err := Load(); err != nil {
		t.Fatalf("the placeholders are accepted in explicit development: %v", err)
	}
}

// readEnvExample returns the KEY=value pairs of the repository's .env.example,
// including commented-out "# KEY=value" lines.
func readEnvExample(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "#"))
		if k, v, ok := strings.Cut(line, "="); ok && !strings.ContainsAny(k, " \t") {
			out[k] = v
		}
	}
	return out
}

func TestStaffMFA_Redacted(t *testing.T) {
	baseEnv(t, "production")
	factor, mac := setStaffMFAKeys(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	rawF, _ := base64.StdEncoding.DecodeString(factor)
	rawM, _ := base64.StdEncoding.DecodeString(mac)
	j, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%s", cfg.StaffMFAFactorKeys), string(j)} {
		if containsAny(rendered, factor, mac, string(rawF), string(rawM)) {
			t.Fatal("config rendering leaked a staff MFA key")
		}
	}
}

func TestStaffMFA_SessionBounds(t *testing.T) {
	baseEnv(t, "development")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StaffAccessTokenTTL != 15*time.Minute || cfg.StaffRefreshIdleTTL != 30*time.Minute || cfg.StaffSessionAbsoluteTTL != 12*time.Hour ||
		cfg.StaffStepUpCriticalMaxAge != 5*time.Minute || cfg.StaffStepUpFinancialMaxAge != 10*time.Minute {
		t.Fatalf("defaults changed: %v %v %v %v %v", cfg.StaffAccessTokenTTL, cfg.StaffRefreshIdleTTL, cfg.StaffSessionAbsoluteTTL, cfg.StaffStepUpCriticalMaxAge, cfg.StaffStepUpFinancialMaxAge)
	}
	cases := []struct {
		env    string
		lo, hi int
		field  func(Config) time.Duration
	}{
		{"STAFF_ACCESS_TOKEN_TTL_SECONDS", 60, 900, func(c Config) time.Duration { return c.StaffAccessTokenTTL }},
		{"STAFF_REFRESH_IDLE_TTL_SECONDS", 300, 3600, func(c Config) time.Duration { return c.StaffRefreshIdleTTL }},
		{"STAFF_SESSION_ABSOLUTE_TTL_SECONDS", 3600, 43200, func(c Config) time.Duration { return c.StaffSessionAbsoluteTTL }},
		{"STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS", 60, 300, func(c Config) time.Duration { return c.StaffStepUpCriticalMaxAge }},
		{"STAFF_STEP_UP_FINANCIAL_MAX_AGE_SECONDS", 60, 600, func(c Config) time.Duration { return c.StaffStepUpFinancialMaxAge }},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			for _, ok := range []int{tc.lo, tc.hi} {
				baseEnv(t, "development")
				t.Setenv(tc.env, fmt.Sprint(ok))
				cfg, err := Load()
				if err != nil || tc.field(cfg) != time.Duration(ok)*time.Second {
					t.Fatalf("%d must load: %v", ok, err)
				}
			}
			for _, bad := range []string{fmt.Sprint(tc.lo - 1), fmt.Sprint(tc.hi + 1), "0", "-60", "abc", "", "1.5"} {
				baseEnv(t, "development")
				t.Setenv(tc.env, bad)
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.env) {
					t.Fatalf("%q must be refused, got %v", bad, err)
				}
			}
		})
	}
}

func TestStaffMFA_Issuer(t *testing.T) {
	baseEnv(t, "development")
	cfg, err := Load()
	if err != nil || cfg.StaffMFAIssuer != "iGaming Platform Back Office" {
		t.Fatalf("default issuer: %q, %v", cfg.StaffMFAIssuer, err)
	}
	t.Setenv("STAFF_MFA_ISSUER", "Platform Ops")
	if cfg, err := Load(); err != nil || cfg.StaffMFAIssuer != "Platform Ops" {
		t.Fatalf("custom issuer: %v", err)
	}
	for _, bad := range []string{"Brand:Casino", " lead", "trail ", "line\nbreak", strings.Repeat("x", 65)} {
		t.Setenv("STAFF_MFA_ISSUER", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "STAFF_MFA_ISSUER") {
			t.Fatalf("issuer %q must be refused, got %v", bad, err)
		}
	}
}

// ---- the platform key-family registry ------------------------------------------

// secretFieldFamilies maps every secret-bearing Config field to its registry
// family. TestPlatformKeyFamilies_RegistryIsComplete fails when a new
// SecretValue (or *Secret string) field is added without a registry entry.
var secretFieldFamilies = map[string]string{
	"JWTSigningSecret":                 "JWT_SIGNING_SECRET",
	"JWTPreviousSecret":                "JWT_PREVIOUS_SECRET",
	"ProviderCredentialFingerprintKey": "PROVIDER_CREDENTIAL_FINGERPRINT_KEY",
	"ActorProofKeys":                   "ACTOR_PROOF_KEYS",
	"PayoutInstrumentKeys":             "PAYOUT_INSTRUMENT_KEYS",
	"PayoutInstrumentFPKeys":           "PAYOUT_INSTRUMENT_FP_KEYS",
	"StaffMFAFactorKeys":               "STAFF_MFA_FACTOR_KEYS",
	"StaffMFAMACKeys":                  "STAFF_MFA_MAC_KEYS",
}

func TestPlatformKeyFamilies_RegistryIsComplete(t *testing.T) {
	var names []string
	for _, f := range platformKeyFamilies {
		names = append(names, f.name)
	}
	var want []string
	for _, n := range secretFieldFamilies {
		want = append(want, n)
	}
	sort.Strings(names)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("registry = %v, want %v", names, want)
	}
	typ := reflect.TypeOf(Config{})
	secretType := reflect.TypeOf(SecretValue{})
	found := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type == secretType || (f.Type.Kind() == reflect.String && strings.HasSuffix(f.Name, "Secret")) {
			if _, ok := secretFieldFamilies[f.Name]; !ok {
				t.Fatalf("Config.%s holds a secret but is not in the platform key-family registry", f.Name)
			}
			found++
		}
	}
	if found != len(secretFieldFamilies) {
		t.Fatalf("found %d secret fields, registry maps %d", found, len(secretFieldFamilies))
	}
}

// familySetter puts the raw secret v into one family of cfg (directly, for the
// unit matrix) and returns the environment form for the Load matrix.
type familySetter struct {
	direct func(c *Config, v string)
	env    func(t *testing.T, v string)
}

func registrySetters() map[string]familySetter {
	enc := func(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }
	return map[string]familySetter{
		"JWT_SIGNING_SECRET": {func(c *Config, v string) { c.JWTSigningSecret = v },
			func(t *testing.T, v string) { t.Setenv("JWT_SIGNING_SECRET", v) }},
		"JWT_PREVIOUS_SECRET": {func(c *Config, v string) { c.JWTPreviousSecret = v },
			func(t *testing.T, v string) { t.Setenv("JWT_PREVIOUS_SECRET", v) }},
		"PROVIDER_CREDENTIAL_FINGERPRINT_KEY": {func(c *Config, v string) { c.ProviderCredentialFingerprintKey = NewSecretValue(v) },
			func(t *testing.T, v string) { t.Setenv("PROVIDER_CREDENTIAL_FINGERPRINT_KEY", v) }},
		"ACTOR_PROOF_KEYS": {func(c *Config, v string) { c.ActorProofKeys = NewSecretValue("a1:" + enc(v)) },
			func(t *testing.T, v string) {
				t.Setenv("ACTOR_PROOF_KEYS", "a1:"+enc(v))
				t.Setenv("ACTOR_PROOF_ACTIVE_KID", "a1")
			}},
		"PAYOUT_INSTRUMENT_KEYS": {func(c *Config, v string) { c.PayoutInstrumentKeys = NewSecretValue("p1:" + enc(v)) },
			func(t *testing.T, v string) { t.Setenv("PAYOUT_INSTRUMENT_KEYS", "p1:"+enc(v)) }},
		"PAYOUT_INSTRUMENT_FP_KEYS": {func(c *Config, v string) { c.PayoutInstrumentFPKeys = NewSecretValue("q1:" + enc(v)) },
			func(t *testing.T, v string) { t.Setenv("PAYOUT_INSTRUMENT_FP_KEYS", "q1:"+enc(v)) }},
		"STAFF_MFA_FACTOR_KEYS": {func(c *Config, v string) { c.StaffMFAFactorKeys = NewSecretValue("sf1:" + enc(v)) },
			func(t *testing.T, v string) { t.Setenv("STAFF_MFA_FACTOR_KEYS", "sf1:"+enc(v)) }},
		"STAFF_MFA_MAC_KEYS": {func(c *Config, v string) { c.StaffMFAMACKeys = NewSecretValue("sm1:" + enc(v)) },
			func(t *testing.T, v string) { t.Setenv("STAFF_MFA_MAC_KEYS", "sm1:"+enc(v)) }},
	}
}

// fullConfig has every registry family set to an independent random key.
func fullConfig(t *testing.T) Config {
	c := Config{Environment: "development", EnvironmentExplicit: true, JWTActiveKID: "k1", JWTPreviousKID: "k0"}
	for _, s := range registrySetters() {
		s.direct(&c, randomKey(t, 40))
	}
	return c
}

// Every PAIR of registry families is refused when it shares a key value; the
// error names both families and no key material.
func TestPlatformKeyFamilies_PairwiseDistinct_Direct(t *testing.T) {
	if err := fullConfig(t).validatePlatformKeyFamilies(); err != nil {
		t.Fatalf("independent keys must pass: %v", err)
	}
	setters := registrySetters()
	pairs := 0
	for a := range setters {
		for b := range setters {
			if a >= b {
				continue
			}
			pairs++
			c := fullConfig(t)
			shared := randomKey(t, 40)
			setters[a].direct(&c, shared)
			setters[b].direct(&c, shared)
			err := c.validatePlatformKeyFamilies()
			if err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
				t.Fatalf("%s and %s sharing a key must be refused naming both, got %v", a, b, err)
			}
			if strings.Contains(err.Error(), shared) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(shared))) {
				t.Fatal("error echoes key material")
			}
		}
	}
	if pairs != 28 {
		t.Fatalf("expected 28 pairs over 8 families, got %d", pairs)
	}
}

// The same matrix through Load (every refusal path, including the older
// per-family checks that may fire first).
func TestPlatformKeyFamilies_PairwiseDistinct_Load(t *testing.T) {
	setters := registrySetters()
	baseline := func(t *testing.T) {
		baseEnv(t, "development")
		t.Setenv("JWT_PREVIOUS_KID", "k0")
		for _, s := range setters {
			s.env(t, randomKey(t, 40))
		}
		t.Setenv("PAYOUT_INSTRUMENT_ACTIVE_KID", "p1")
		t.Setenv("PAYOUT_INSTRUMENT_FP_ACTIVE_KID", "q1")
		t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", "sf1")
		t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", "sm1")
	}
	t.Run("baseline", func(t *testing.T) {
		baseline(t)
		if _, err := Load(); err != nil {
			t.Fatalf("independent keys must load: %v", err)
		}
	})
	for a := range setters {
		for b := range setters {
			if a >= b {
				continue
			}
			t.Run(a+"+"+b, func(t *testing.T) {
				baseline(t)
				shared := randomKey(t, 40)
				setters[a].env(t, shared)
				setters[b].env(t, shared)
				_, err := Load()
				if err == nil {
					t.Fatal("expected Load to refuse a shared key")
				}
				if strings.Contains(err.Error(), shared) {
					t.Fatal("error echoes key material")
				}
			})
		}
	}
}

// Behaviour change recorded in the S1 implementation record: the two JWT
// secrets are two registry families, so JWT_PREVIOUS_SECRET equal to
// JWT_SIGNING_SECRET (a "rotation" that rotates nothing) is now refused.
func TestPlatformKeyFamilies_JWTPreviousEqualToActiveIsRefused(t *testing.T) {
	baseEnv(t, "development")
	s := randomKey(t, 40)
	t.Setenv("JWT_SIGNING_SECRET", s)
	t.Setenv("JWT_PREVIOUS_SECRET", s)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_PREVIOUS_SECRET") {
		t.Fatalf("expected refusal, got %v", err)
	}
}
