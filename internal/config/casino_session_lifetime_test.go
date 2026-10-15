package config

import (
	"testing"
	"time"
)

func loadWithLifetime(t *testing.T, v string) (Config, error) {
	t.Helper()
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("CASINO_SESSION_MAX_LIFETIME", v)
	return Load()
}

func TestCasinoSessionMaxLifetime_DefaultsToFourHours(t *testing.T) {
	cfg, err := loadWithLifetime(t, "")
	if err != nil || cfg.CasinoSessionMaxLifetime != 4*time.Hour {
		t.Fatalf("default: %v err=%v", cfg.CasinoSessionMaxLifetime, err)
	}
}

func TestCasinoSessionMaxLifetime_AcceptsABoundedDuration(t *testing.T) {
	for v, want := range map[string]time.Duration{"1m": time.Minute, "90m": 90 * time.Minute, "72h": 72 * time.Hour} {
		cfg, err := loadWithLifetime(t, v)
		if err != nil || cfg.CasinoSessionMaxLifetime != want {
			t.Fatalf("%s: %v err=%v", v, cfg.CasinoSessionMaxLifetime, err)
		}
	}
}

// A zero, negative, tiny or huge value would silently disable or break the control: refused at startup.
func TestCasinoSessionMaxLifetime_RejectsUnboundedOrMalformed(t *testing.T) {
	for _, v := range []string{"0", "0s", "-1h", "59s", "73h", "8760h", "forever", "4"} {
		if _, err := loadWithLifetime(t, v); err == nil {
			t.Fatalf("%q must be rejected", v)
		}
	}
}
