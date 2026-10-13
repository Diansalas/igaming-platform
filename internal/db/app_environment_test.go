package db

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeAnchor struct {
	env string
	err error
}

func (f fakeAnchor) ReadEnvironmentAnchor(context.Context) (string, error) { return f.env, f.err }

// ADR 0115 S1 (security LOW + launch gate): the four anchor states x the production / non-production
// guard, plus fail-closed on a read error. Production refuses development / test AND an unrecorded
// anchor; recorded production passes; every non-production guard checks nothing.
func TestVerifyEnvironmentAnchor(t *testing.T) {
	ctx := context.Background()
	readErr := errors.New("boom")
	cases := []struct {
		name    string
		anchor  string
		wantErr error // nil = start allowed
	}{
		{"unrecorded", "", ErrEnvironmentAnchorUnrecorded},
		{"production", "production", nil},
		{"development", "development", ErrEnvironmentAnchorMismatch},
		{"test", "test", ErrEnvironmentAnchorMismatch},
		{"staging (not synthetic-admitting; unchanged)", "staging", nil},
	}
	for _, c := range cases {
		got, err := VerifyEnvironmentAnchor(ctx, "production", fakeAnchor{env: c.anchor})
		if c.wantErr == nil {
			if err != nil || got != c.anchor {
				t.Fatalf("production guard, anchor %s: %q %v", c.name, got, err)
			}
		} else if !errors.Is(err, c.wantErr) {
			t.Fatalf("production guard, anchor %s: want %v, got %v", c.name, c.wantErr, err)
		}
		// Non-production guards never read or refuse, whatever the anchor (and even a read error).
		for _, g := range []string{"development", "staging", "test", ""} {
			if _, err := VerifyEnvironmentAnchor(ctx, g, fakeAnchor{env: c.anchor, err: readErr}); err != nil {
				t.Fatalf("guard %q anchor %s: non-production must check nothing: %v", g, c.name, err)
			}
		}
	}
	if _, err := VerifyEnvironmentAnchor(ctx, "production", fakeAnchor{err: readErr}); !errors.Is(err, readErr) {
		t.Fatalf("an unreadable anchor must fail closed in production, got %v", err)
	}
	// A read error must not be mistaken for (or masked as) an unrecorded anchor.
	if _, err := VerifyEnvironmentAnchor(ctx, "production", fakeAnchor{env: "production", err: readErr}); err == nil {
		t.Fatal("a read error must refuse even when a value accompanies it")
	}
	// The refusal names the provisioning step (what an operator must do).
	_, err := VerifyEnvironmentAnchor(ctx, "production", fakeAnchor{})
	if err == nil || !strings.Contains(err.Error(), "INSERT INTO database_environment") || !strings.Contains(err.Error(), "production-configuration-checklist") {
		t.Fatalf("the unrecorded-anchor error must name the provisioning step: %v", err)
	}
}

// ADR 0115 S1: a connection string can never supply app.environment (any spelling), and an options
// parameter naming it is refused before any connection.
func TestDropURLAppEnvironment(t *testing.T) {
	rp := map[string]string{"app.environment": "test", "APP.Environment": "development", "application_name": "x"}
	if err := dropURLAppEnvironment(rp); err != nil {
		t.Fatal(err)
	}
	if len(rp) != 1 || rp["application_name"] != "x" {
		t.Fatalf("every spelling of app.environment must be dropped: %v", rp)
	}
	for _, opt := range []string{"-c app.environment=test", "-c APP.ENVIRONMENT=development"} {
		if err := dropURLAppEnvironment(map[string]string{"options": opt}); err == nil {
			t.Fatalf("options %q must be refused", opt)
		}
	}
	if err := dropURLAppEnvironment(map[string]string{"options": "-c statement_timeout=5s"}); err != nil {
		t.Fatalf("an unrelated options value is untouched: %v", err)
	}
}
