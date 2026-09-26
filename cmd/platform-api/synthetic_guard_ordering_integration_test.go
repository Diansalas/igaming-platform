//go:build integration

// Stage 10.3 W1b, MOCK-ADAPTER-PROD-1: the required subprocess/ordering
// test. It proves refusal happens BEFORE any database side effect, not
// merely that the guard's logic is correct in isolation - by running the
// real compiled binary with APP_ENV=production and an unreachable
// DATABASE_URL and asserting the failure is the guard's own named error,
// never a database-connection error. Tagged `//go:build integration`
// per the W1b test plan (it spawns a real process and needs no database
// itself, but follows this codebase's existing convention of keeping
// process-spawning tests out of the plain `go test -race ./...` unit
// step).
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildPlatformAPIBinary compiles cmd/platform-api once into a temp dir and
// returns the binary path, so every subtest below reuses the same build.
func buildPlatformAPIBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "platform-api")

	buildCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(buildCtx, "go", "build", "-o", bin, ".")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build platform-api: %v\n%s", err, out)
	}
	return bin
}

func runPlatformAPIOnce(t *testing.T, bin string, env []string) (stdout, stderr string, exitCode int) {
	t.Helper()

	runCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin)
	cmd.Env = env

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run platform-api (not a normal exit): %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// TestSyntheticGuard_RunsBeforeDBConnect_Subprocess is the required W1b
// case.
func TestSyntheticGuard_RunsBeforeDBConnect_Subprocess(t *testing.T) {
	bin := buildPlatformAPIBinary(t)

	env := append(os.Environ(),
		"APP_ENV=production",
		"JWT_SIGNING_SECRET=a-secret-that-is-at-least-32-characters-long",
		"OTEL_EXPORTER=none",
		// Deliberately unreachable: port 1 refuses connections immediately
		// on essentially any host, so if the guard did NOT run first, this
		// would fail fast with a distinct "connect: connection refused"
		// database error instead of the guard's own named error.
		"DATABASE_URL=postgres://user:pass@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=1",
	)

	_, stderr, exitCode := runPlatformAPIOnce(t, bin, env)

	if exitCode == 0 {
		t.Fatalf("expected non-zero exit code when APP_ENV=production with synthetic components wired, got 0. stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "providerkind: refusing to start") {
		t.Errorf("expected the synthetic guard's own named error on stderr, got: %s", stderr)
	}
	if strings.Contains(stderr, "connect database") || strings.Contains(stderr, "connection refused") {
		t.Errorf("expected the guard to refuse BEFORE any database connection attempt, but stderr mentions a database error: %s", stderr)
	}
}

// TestSyntheticGuard_RunsBeforeDBConnect_Subprocess_MissingAppEnv is the
// same proof for the "APP_ENV missing" case (security condition C13,
// ruling R8) - a production-shaped deployment that simply forgot to set
// APP_ENV must be refused identically, before any database side effect.
func TestSyntheticGuard_RunsBeforeDBConnect_Subprocess_MissingAppEnv(t *testing.T) {
	bin := buildPlatformAPIBinary(t)

	// Build the environment from scratch (not os.Environ()) to guarantee
	// APP_ENV is genuinely absent, regardless of the test runner's own
	// environment.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"JWT_SIGNING_SECRET=a-secret-that-is-at-least-32-characters-long",
		"OTEL_EXPORTER=none",
		"DATABASE_URL=postgres://user:pass@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=1",
	}

	_, stderr, exitCode := runPlatformAPIOnce(t, bin, env)

	if exitCode == 0 {
		t.Fatalf("expected non-zero exit code with APP_ENV missing, got 0. stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "providerkind: refusing to start") {
		t.Errorf("expected the synthetic guard's own named error on stderr for a missing APP_ENV, got: %s", stderr)
	}
	if strings.Contains(stderr, "connect database") || strings.Contains(stderr, "connection refused") {
		t.Errorf("expected the guard to refuse BEFORE any database connection attempt, but stderr mentions a database error: %s", stderr)
	}
}
