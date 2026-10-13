package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// ADR 0119 S3 security review C2: ValidateStaffMFAEnv (cmd/seed-admin) applies the same staff MFA rules as Load.

func TestValidateStaffMFAEnv_PublishedPlaceholders(t *testing.T) {
	envExample := readEnvExample(t)
	for _, env := range []string{"production", "staging", ""} {
		t.Run("APP_ENV="+env, func(t *testing.T) {
			baseEnv(t, env)
			t.Setenv("STAFF_MFA_FACTOR_KEYS", envExample["STAFF_MFA_FACTOR_KEYS"])
			t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", envExample["STAFF_MFA_FACTOR_ACTIVE_KID"])
			t.Setenv("STAFF_MFA_MAC_KEYS", envExample["STAFF_MFA_MAC_KEYS"])
			t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", envExample["STAFF_MFA_MAC_ACTIVE_KID"])
			if err := ValidateStaffMFAEnv(); err == nil || !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("the .env.example keys must be refused outside explicit development, got %v", err)
			}
		})
	}
	baseEnv(t, "development")
	t.Setenv("STAFF_MFA_FACTOR_KEYS", envExample["STAFF_MFA_FACTOR_KEYS"])
	t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", envExample["STAFF_MFA_FACTOR_ACTIVE_KID"])
	t.Setenv("STAFF_MFA_MAC_KEYS", envExample["STAFF_MFA_MAC_KEYS"])
	t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", envExample["STAFF_MFA_MAC_ACTIVE_KID"])
	if err := ValidateStaffMFAEnv(); err != nil {
		t.Fatalf("explicit development accepts the placeholders: %v", err)
	}
}

func TestValidateStaffMFAEnv_ProductionRequiresKeys_AndBounds(t *testing.T) {
	baseEnv(t, "production")
	unsetStaffMFAKeys(t)
	if err := ValidateStaffMFAEnv(); err == nil || !strings.Contains(err.Error(), "STAFF_MFA_MAC_KEYS") {
		t.Fatalf("production without keys must be refused, got %v", err)
	}
	baseEnv(t, "production")
	setStaffMFAKeys(t)
	if err := ValidateStaffMFAEnv(); err != nil {
		t.Fatalf("fresh keys must pass: %v", err)
	}
	t.Setenv("STAFF_SESSION_ABSOLUTE_TTL_SECONDS", "43201")
	if err := ValidateStaffMFAEnv(); err == nil || !strings.Contains(err.Error(), "STAFF_SESSION_ABSOLUTE_TTL_SECONDS") {
		t.Fatalf("out-of-bounds session TTL must be refused, got %v", err)
	}
}

// Every other registry family that Load reads is read here too: a staff MFA key shared with ANY of them is refused
// (this pins readPlatformKeyFamilies against the registry, so a family added to the registry and to Load but not
// here fails).
func TestValidateStaffMFAEnv_ReadsTheSameFamiliesAsLoad(t *testing.T) {
	setters := registrySetters()
	checked := 0
	for name, s := range setters {
		if strings.HasPrefix(name, "STAFF_MFA_") {
			continue
		}
		for _, staffFamily := range []string{"STAFF_MFA_FACTOR_KEYS", "STAFF_MFA_MAC_KEYS"} {
			t.Run(name+"+"+staffFamily, func(t *testing.T) {
				baseEnv(t, "development")
				setStaffMFAKeys(t)
				t.Setenv("STAFF_MFA_FACTOR_ACTIVE_KID", "sf1")
				t.Setenv("STAFF_MFA_MAC_ACTIVE_KID", "sm1")
				shared := randomKey(t, 40)
				s.env(t, shared)
				setters[staffFamily].env(t, shared)
				err := ValidateStaffMFAEnv()
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("%s sharing a key with %s must be refused naming it, got %v", staffFamily, name, err)
				}
				if strings.Contains(err.Error(), shared) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(shared))) {
					t.Fatal("error echoes key material")
				}
			})
			checked++
		}
	}
	if checked != 12 {
		t.Fatalf("expected 6 non-staff families x 2, got %d", checked)
	}
}
