package stagingseed

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeAnchor struct {
	v   string
	err error
}

func (f fakeAnchor) ReadEnvironmentAnchor(context.Context) (string, error) { return f.v, f.err }

// SRP3-1: no anchor row means PRODUCTION to the database, so the seeding commands refuse it (reproduced probe).
func TestRequireSyntheticAnchor(t *testing.T) {
	for _, ok := range []string{"staging", "development", "test"} {
		if got, err := RequireSyntheticAnchor(context.Background(), fakeAnchor{v: ok}); err != nil || got != ok {
			t.Errorf("anchor %q must pass, got %q %v", ok, got, err)
		}
	}
	_, err := RequireSyntheticAnchor(context.Background(), fakeAnchor{v: ""})
	if err == nil || !strings.Contains(err.Error(), "no database_environment anchor") || !strings.Contains(err.Error(), "PRODUCTION") {
		t.Errorf("an absent anchor must be refused as production, got %v", err)
	}
	if _, err := RequireSyntheticAnchor(context.Background(), fakeAnchor{v: "production"}); err == nil {
		t.Error("a production anchor must be refused")
	}
	if _, err := RequireSyntheticAnchor(context.Background(), fakeAnchor{v: "Staging "}); err == nil {
		t.Error("a non-canonical spelling must be refused (fail closed)")
	}
	if _, err := RequireSyntheticAnchor(context.Background(), fakeAnchor{err: errors.New("boom")}); err == nil {
		t.Error("an unreadable anchor must be refused")
	}
}

func TestCheckAppEnv(t *testing.T) {
	for _, e := range []string{"development", "staging", " staging "} {
		if err := CheckAppEnv(e); err != nil {
			t.Errorf("%q must pass: %v", e, err)
		}
	}
	for _, e := range []string{"production", "Production", "", "test", "prod"} {
		if err := CheckAppEnv(e); err == nil {
			t.Errorf("%q must be refused", e)
		}
	}
}

func TestCheckField(t *testing.T) {
	if err := CheckField("operator", "ops-alice"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", " x", "x ", "a\nb", "a\u202eb", "a\u200bb", strings.Repeat("x", MaxField+1)} {
		if err := CheckField("operator", bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
