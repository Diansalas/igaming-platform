package db

import (
	"context"
	"errors"
	"testing"
)

type fakeAnchor struct {
	env string
	err error
}

func (f fakeAnchor) ReadEnvironmentAnchor(context.Context) (string, error) { return f.env, f.err }

// ADR 0115 S1 (security LOW): production refuses a development / test anchor, fails closed on a
// read error, and accepts production / staging / an unrecorded anchor; non-production checks nothing.
func TestVerifyEnvironmentAnchor(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{"development", "test"} {
		if _, err := VerifyEnvironmentAnchor(ctx, "production", fakeAnchor{env: bad}); !errors.Is(err, ErrEnvironmentAnchorMismatch) {
			t.Fatalf("anchor %s must refuse a production start, got %v", bad, err)
		}
	}
	readErr := errors.New("boom")
	if _, err := VerifyEnvironmentAnchor(ctx, "production", fakeAnchor{err: readErr}); !errors.Is(err, readErr) {
		t.Fatalf("an unreadable anchor must fail closed in production, got %v", err)
	}
	for _, ok := range []string{"production", "staging", ""} {
		if got, err := VerifyEnvironmentAnchor(ctx, "production", fakeAnchor{env: ok}); err != nil || got != ok {
			t.Fatalf("anchor %q: %q %v", ok, got, err)
		}
	}
	if _, err := VerifyEnvironmentAnchor(ctx, "development", fakeAnchor{env: "test", err: readErr}); err != nil {
		t.Fatalf("outside production nothing is checked: %v", err)
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
