package config

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// B13 (ADR 0111 2.2): PAYOUT_INSTRUMENT_* validation. Keys are generated at
// runtime (never literals).

func setPayoutKeys(t *testing.T, master, mkid, fp, fkid string) {
	t.Helper()
	t.Setenv("PAYOUT_INSTRUMENT_KEYS", master)
	t.Setenv("PAYOUT_INSTRUMENT_ACTIVE_KID", mkid)
	t.Setenv("PAYOUT_INSTRUMENT_FP_KEYS", fp)
	t.Setenv("PAYOUT_INSTRUMENT_FP_ACTIVE_KID", fkid)
}

func TestConfig_PayoutInstrumentKeys_AbsentIsValid(t *testing.T) {
	baseEnv(t, "production")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("absent keys must not fail Load (the requirement is main's startup gate): %v", err)
	}
	if cfg.PayoutInstrumentKeys.IsSet() || cfg.PayoutInstrumentFPKeys.IsSet() {
		t.Fatal("absent keys must be unset")
	}
}

func TestConfig_PayoutInstrumentKeys_ValidAndRotation(t *testing.T) {
	baseEnv(t, "production")
	m1, m2, f1 := b64Key(t, 32), b64Key(t, 40), b64Key(t, 32)
	setPayoutKeys(t, "m1:"+m1+",m2:"+m2, "m2", "f1:"+f1, "f1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("valid two-family config must load: %v", err)
	}
	if cfg.PayoutInstrumentActiveKID != "m2" || cfg.PayoutInstrumentFPActiveKID != "f1" {
		t.Fatalf("active kids = %q %q", cfg.PayoutInstrumentActiveKID, cfg.PayoutInstrumentFPActiveKID)
	}
}

func TestConfig_PayoutInstrumentKeys_Refusals(t *testing.T) {
	m, f := b64Key(t, 32), b64Key(t, 32)
	short := b64Key(t, 16)
	cases := []struct {
		name                 string
		master, mkid, fp, fk string
	}{
		{"short master key", "m1:" + short, "m1", "f1:" + f, "f1"},
		{"short fp key", "m1:" + m, "m1", "f1:" + short, "f1"},
		{"unknown master active kid", "m1:" + m, "m9", "f1:" + f, "f1"},
		{"unknown fp active kid", "m1:" + m, "m1", "f1:" + f, "f9"},
		{"only master family", "m1:" + m, "m1", "", ""},
		{"only fp family", "", "", "f1:" + f, "f1"},
		{"same key in both families", "m1:" + m, "m1", "f1:" + m, "f1"},
		{"not base64", "m1:not base64!", "m1", "f1:" + f, "f1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, "production")
			setPayoutKeys(t, tc.master, tc.mkid, tc.fp, tc.fk)
			if _, err := Load(); err == nil {
				t.Fatal("expected Load to refuse")
			}
		})
	}
}

func TestConfig_PayoutInstrumentKeys_RefusesOtherSecrets(t *testing.T) {
	baseEnv(t, "production")
	f := b64Key(t, 32)
	jwt := "a-secret-that-is-at-least-32-characters-long"
	t.Setenv("JWT_SIGNING_SECRET", jwt)
	setPayoutKeys(t, "m1:"+encodeRaw(jwt), "m1", "f1:"+f, "f1")
	if _, err := Load(); err == nil {
		t.Fatal("a master key equal to the JWT secret must be refused")
	}

	baseEnv(t, "production")
	ap := b64Key(t, 32)
	t.Setenv("ACTOR_PROOF_KEYS", "k1:"+ap)
	t.Setenv("ACTOR_PROOF_ACTIVE_KID", "k1")
	setPayoutKeys(t, "m1:"+b64Key(t, 32), "m1", "f1:"+ap, "f1")
	if _, err := Load(); err == nil {
		t.Fatal("a fingerprint key equal to an actor-proof key must be refused")
	}
}

func TestConfig_PayoutInstrumentKeys_Redacted(t *testing.T) {
	baseEnv(t, "production")
	m, f := b64Key(t, 32), b64Key(t, 32)
	setPayoutKeys(t, "m1:"+m, "m1", "f1:"+f, "f1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if containsAny(rendered, m, f) {
			t.Fatal("config rendering leaked a payout instrument key")
		}
	}
}

func encodeRaw(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
