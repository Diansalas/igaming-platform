package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PRH-2 R2 (security C4): `down` is refused in deployed environments.
func TestCheckDownAllowed(t *testing.T) {
	for _, tc := range []struct {
		env   string
		set   bool
		allow bool
	}{
		{"development", true, true},
		{"staging", true, true},
		{"production", true, false},
		{"", false, false}, // unset counts as production (GuardEnvironment semantics)
		{"", true, false},  // explicitly empty
		{"Production", true, false},
		{"prod", true, false},
		{"anything-else", true, false},
	} {
		err := checkDownAllowed(tc.env, tc.set)
		if tc.allow && err != nil {
			t.Errorf("APP_ENV=%q set=%v: want allowed, got %v", tc.env, tc.set, err)
		}
		if !tc.allow {
			if err == nil {
				t.Errorf("APP_ENV=%q set=%v: want refused, got nil", tc.env, tc.set)
			} else if !strings.Contains(err.Error(), "refusing `down`") {
				t.Errorf("unexpected refusal text: %v", err)
			}
		}
	}
}

// End to end (QA G4): the BUILT binary refuses `down` when APP_ENV is unset or
// production, before it touches the database, so the guard cannot be unplugged
// from run() without this test failing. The DATABASE_URL is unreachable on
// purpose: a refused `down` must not even try to connect.
func TestBinary_DownRefusedWhenAppEnvUnsetOrProduction(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "migrate-under-test")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build cmd/migrate: %v\n%s", err, out)
	}
	run := func(env ...string) (string, error) {
		cmd := exec.Command(bin, "-steps=1", "down")
		// A minimal environment: no inherited APP_ENV.
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=postgres://nobody:nopass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, env := range [][]string{nil, {"APP_ENV=production"}, {"APP_ENV="}, {"APP_ENV=prod"}} {
		out, err := run(env...)
		if err == nil || !strings.Contains(out, "refusing `down`") {
			t.Errorf("env %v: want a refusal, got err=%v out=%q", env, err, out)
		}
		if strings.Contains(out, "connect database") {
			t.Errorf("env %v: the refusal must come BEFORE the database connection: %q", env, out)
		}
	}
	// Non-vacuity: with APP_ENV=development the guard lets it through (and then it
	// fails on the unreachable database, a different error).
	out, err := run("APP_ENV=development")
	if err == nil || strings.Contains(out, "refusing `down`") || !strings.Contains(out, "connect database") {
		t.Errorf("APP_ENV=development must pass the guard and fail on the connection, got err=%v out=%q", err, out)
	}
}
