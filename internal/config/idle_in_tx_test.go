package config

import (
	"testing"
	"time"
)

// ADR 0112 slice-3 residual 14(f): the per-connection idle_in_transaction_session_timeout.
func TestLoad_DatabaseIdleInTxTimeout_DefaultOverrideAndRefusals(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SIGNING_SECRET", "a-secret-that-is-at-least-32-characters-long")
	t.Setenv("DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseIdleInTxTimeout != 30*time.Second || cfg.DatabaseRuntimeParams()["idle_in_transaction_session_timeout"] != "30000" {
		t.Fatalf("default: %v %v", cfg.DatabaseIdleInTxTimeout, cfg.DatabaseRuntimeParams())
	}

	t.Setenv("DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS", "45")
	cfg, err = Load()
	if err != nil || cfg.DatabaseIdleInTxTimeout != 45*time.Second || cfg.DatabaseRuntimeParams()["idle_in_transaction_session_timeout"] != "45000" {
		t.Fatalf("override: %v %v", cfg.DatabaseIdleInTxTimeout, err)
	}

	// The timeout can never be switched off or made unbounded.
	for _, bad := range []string{"0", "-1", "3601", "abc", "1.5"} {
		t.Setenv("DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS=%q must be refused", bad)
		}
	}
	t.Setenv("DATABASE_IDLE_IN_TRANSACTION_TIMEOUT_SECONDS", "3600")
	if _, err := Load(); err != nil {
		t.Errorf("the documented maximum must be accepted: %v", err)
	}
}

func TestDatabaseRuntimeParams_ZeroConfigStillHasTheDefault(t *testing.T) {
	if got := (Config{}).DatabaseRuntimeParams()["idle_in_transaction_session_timeout"]; got != "30000" {
		t.Fatalf("a Config built without Load must still carry the default, got %q", got)
	}
	if got := (Config{DatabaseIdleInTxTimeout: -5 * time.Second}).DatabaseRuntimeParams()["idle_in_transaction_session_timeout"]; got != "30000" {
		t.Fatalf("a non-positive value must never reach the session (0 means off): %q", got)
	}
}
