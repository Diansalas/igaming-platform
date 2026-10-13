package config

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// Security review C3: the registry compares keys across their text and
// decoded forms, so the same secret cannot be reused by pasting the base64
// text of one family's entry as another family's raw text key (or by encoding
// it differently).

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPlatformKeyFamilies_CrossForm_Direct(t *testing.T) {
	b := randomBytes(t, 32)
	x := base64.StdEncoding.EncodeToString(b)    // X: the base64 text
	xURL := base64.URLEncoding.EncodeToString(b) // the same bytes, URL-safe alphabet
	xx := base64.StdEncoding.EncodeToString([]byte(x))
	cases := []struct {
		name   string
		set    func(c *Config)
		a, b   string
		reason string
	}{
		{"JWT_SIGNING_SECRET=X vs STAFF_MFA_MAC_KEYS=m1:X", func(c *Config) {
			c.JWTSigningSecret = x
			c.StaffMFAMACKeys = NewSecretValue("sm1:" + x)
		}, "JWT_SIGNING_SECRET", "STAFF_MFA_MAC_KEYS", "text key decodes to the entry's bytes / equals the entry's text"},
		{"JWT_PREVIOUS_SECRET=X vs ACTOR_PROOF_KEYS=a1:X", func(c *Config) {
			c.JWTPreviousSecret = x
			c.ActorProofKeys = NewSecretValue("a1:" + x)
		}, "JWT_PREVIOUS_SECRET", "ACTOR_PROOF_KEYS", "same"},
		{"PROVIDER_CREDENTIAL_FINGERPRINT_KEY=X vs STAFF_MFA_FACTOR_KEYS=sf1:X", func(c *Config) {
			c.ProviderCredentialFingerprintKey = NewSecretValue(x)
			c.StaffMFAFactorKeys = NewSecretValue("sf1:" + x)
		}, "PROVIDER_CREDENTIAL_FINGERPRINT_KEY", "STAFF_MFA_FACTOR_KEYS", "same"},
		{"ACTOR_PROOF_KEYS=a1:X vs PAYOUT_INSTRUMENT_FP_KEYS=q1:X(url-safe)", func(c *Config) {
			c.ActorProofKeys = NewSecretValue("a1:" + x)
			c.PayoutInstrumentFPKeys = NewSecretValue("q1:" + xURL)
		}, "ACTOR_PROOF_KEYS", "PAYOUT_INSTRUMENT_FP_KEYS", "decoded bytes equal across encodings"},
		{"ACTOR_PROOF_KEYS=a1:X vs PAYOUT_INSTRUMENT_FP_KEYS=q1:base64(X)", func(c *Config) {
			c.ActorProofKeys = NewSecretValue("a1:" + x)
			c.PayoutInstrumentFPKeys = NewSecretValue("q1:" + xx)
		}, "ACTOR_PROOF_KEYS", "PAYOUT_INSTRUMENT_FP_KEYS", "one entry's decoded bytes equal the other's encoded text"},
		{"STAFF_MFA_MAC_KEYS=m1:base64(X) vs STAFF_MFA_FACTOR_KEYS=sf1:X", func(c *Config) {
			c.StaffMFAMACKeys = NewSecretValue("sm1:" + xx)
			c.StaffMFAFactorKeys = NewSecretValue("sf1:" + x)
		}, "STAFF_MFA_FACTOR_KEYS", "STAFF_MFA_MAC_KEYS", "same"},
		{"JWT_SIGNING_SECRET=base64(X) vs PAYOUT_INSTRUMENT_KEYS=p1:X", func(c *Config) {
			c.JWTSigningSecret = xx
			c.PayoutInstrumentKeys = NewSecretValue("p1:" + x)
		}, "JWT_SIGNING_SECRET", "PAYOUT_INSTRUMENT_KEYS", "decoded text key equals the entry's text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fullConfig(t)
			tc.set(&c)
			err := c.validatePlatformKeyFamilies()
			if err == nil || !strings.Contains(err.Error(), tc.a) || !strings.Contains(err.Error(), tc.b) {
				t.Fatalf("%s: want a refusal naming %s and %s, got %v", tc.reason, tc.a, tc.b, err)
			}
			for _, secret := range []string{x, xURL, xx, string(b)} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error echoes key material")
				}
			}
		})
	}
}

func TestPlatformKeyFamilies_CrossForm_ThroughLoad(t *testing.T) {
	x := base64.StdEncoding.EncodeToString(randomBytes(t, 32))
	baseEnv(t, "production")
	setStaffMFAKeys(t)
	t.Setenv("JWT_SIGNING_SECRET", x)
	t.Setenv("STAFF_MFA_MAC_KEYS", "sm1:"+x)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "JWT_SIGNING_SECRET") || !strings.Contains(err.Error(), "STAFF_MFA_MAC_KEYS") {
		t.Fatalf("JWT_SIGNING_SECRET=X with STAFF_MFA_MAC_KEYS=m1:X must be refused, got %v", err)
	}
	if strings.Contains(err.Error(), x) {
		t.Fatal("error echoes key material")
	}
}

// A text key that is not base64 contributes only its text form; independent
// keys in every family still load (no false positive from the extra forms).
func TestPlatformKeyFamilies_CrossForm_NoFalsePositive(t *testing.T) {
	for i := 0; i < 20; i++ {
		c := fullConfig(t)
		c.JWTSigningSecret = base64.StdEncoding.EncodeToString(randomBytes(t, 32))
		c.ProviderCredentialFingerprintKey = NewSecretValue(base64.RawURLEncoding.EncodeToString(randomBytes(t, 40)))
		if err := c.validatePlatformKeyFamilies(); err != nil {
			t.Fatalf("independent keys refused: %v", err)
		}
	}
}
