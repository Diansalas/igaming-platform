//go:build integration

// ADR 0119 S3 entry gate 21.7 (c) (closes S1-R1): the real compiled binary refuses to start without the staff MFA
// key families in EVERY environment - APP_ENV unset included - with the staff MFA gate's own named error, before the
// synthetic-component guard and before any database connection.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestStaffMFAStartupGate_RefusesWithoutKeys_Subprocess(t *testing.T) {
	bin := buildPlatformAPIBinary(t)
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"JWT_SIGNING_SECRET=a-secret-that-is-at-least-32-characters-long",
		"OTEL_EXPORTER=none",
		// Unreachable on purpose: a refusal must come before any connection attempt.
		"DATABASE_URL=postgres://user:pass@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=1",
	}
	keys := staffMFAKeyEnv(t)
	for name, env := range map[string][]string{
		"APP_ENV unset, no keys":             base,
		"APP_ENV=development, no keys":       append(append([]string{}, base...), "APP_ENV=development"),
		"APP_ENV=staging, no keys":           append(append([]string{}, base...), "APP_ENV=staging"),
		"APP_ENV unset, FACTOR family only":  append(append([]string{}, base...), keys[0], keys[1]),
		"APP_ENV=development, MAC kid wrong": append(append([]string{}, base...), "APP_ENV=development", keys[0], keys[1], keys[2], "STAFF_MFA_MAC_ACTIVE_KID=zz"),
	} {
		t.Run(name, func(t *testing.T) {
			_, stderr, code := runPlatformAPIOnce(t, bin, env)
			if code == 0 {
				t.Fatalf("the binary started without usable staff MFA keys. stderr=%s", stderr)
			}
			if !strings.Contains(stderr, "STAFF_MFA") && !strings.Contains(stderr, "staffmfa: startup gate") {
				t.Errorf("expected the staff MFA refusal on stderr, got: %s", stderr)
			}
			if strings.Contains(stderr, "providerkind: refusing to start") {
				t.Errorf("the staff MFA gate must run before the synthetic guard: %s", stderr)
			}
			if strings.Contains(stderr, "connect database") || strings.Contains(stderr, "connection refused") {
				t.Errorf("the staff MFA gate must refuse before any database connection: %s", stderr)
			}
		})
	}
	// Control: with fresh keys and APP_ENV unset the SAME binary gets past the staff MFA gate (and stops at the
	// synthetic guard instead), so the refusals above are the gate's, not an environment artefact.
	_, stderr, code := runPlatformAPIOnce(t, bin, append(append([]string{}, base...), keys...))
	if code == 0 || !strings.Contains(stderr, "providerkind: refusing to start") || strings.Contains(stderr, "staffmfa: startup gate") {
		t.Fatalf("with keys the binary must pass the staff MFA gate and stop at the synthetic guard: %s", stderr)
	}
}
