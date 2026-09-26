package providerkind

import (
	"strings"
	"testing"
)

// fixtureSynthetic implements Synthetic only.
type fixtureSynthetic struct{}

func (fixtureSynthetic) SyntheticComponent() {}

// fixtureEligible implements ProductionEligible only.
type fixtureEligible struct{}

func (fixtureEligible) MarkProductionEligible() {}

// fixtureUnmarked implements neither marker - the default, safe-by-default
// state RefuseSyntheticInProduction must treat as disqualifying in
// production, exactly like an explicitly Synthetic component.
type fixtureUnmarked struct{}

// fixtureBoth implements both markers - a contradiction that must never
// happen in real code, but the guard must still refuse it (Synthetic wins)
// rather than silently preferring ProductionEligible.
type fixtureBoth struct{}

func (fixtureBoth) SyntheticComponent()     {}
func (fixtureBoth) MarkProductionEligible() {}

// TestSyntheticGuard_Matrix is the required W1b case: a table over
// {production, non-production} x {synthetic, eligible, unmarked, both}.
// environment here is already the CALLER's resolved GuardEnvironment()
// value - RefuseSyntheticInProduction itself takes no Config and has no
// other input, which is what makes "no flag can bypass it" true by
// construction (security condition C13's all-flags-on requirement is
// covered at the cmd/platform-api level, where the real Config exists -
// see refuse_synthetic_test.go's TestRefuseSyntheticInProduction_AllFlagsOn).
func TestSyntheticGuard_Matrix(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		component   any
		wantErr     bool
	}{
		{"production_synthetic_refused", "production", fixtureSynthetic{}, true},
		{"production_eligible_allowed", "production", fixtureEligible{}, false},
		{"production_unmarked_refused", "production", fixtureUnmarked{}, true},
		{"production_both_refused", "production", fixtureBoth{}, true},
		{"staging_synthetic_allowed", "staging", fixtureSynthetic{}, false},
		{"staging_unmarked_allowed", "staging", fixtureUnmarked{}, false},
		{"development_synthetic_allowed", "development", fixtureSynthetic{}, false},
		{"development_unmarked_allowed", "development", fixtureUnmarked{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			regs := []Registration{{Domain: "test", Name: "component", Component: tc.component}}
			err := RefuseSyntheticInProduction(tc.environment, regs)
			if tc.wantErr && err == nil {
				t.Fatalf("expected refusal for %s, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no refusal for %s, got: %v", tc.name, err)
			}
		})
	}
}

func TestSyntheticGuard_NilComponentSkipped(t *testing.T) {
	regs := []Registration{{Domain: "kyc", Name: "provider", Component: nil}}
	if err := RefuseSyntheticInProduction("production", regs); err != nil {
		t.Fatalf("expected nil Component to be skipped, got: %v", err)
	}
}

func TestSyntheticGuard_NamesEveryOffendingComponent(t *testing.T) {
	regs := []Registration{
		{Domain: "payments", Name: "provider", Component: fixtureSynthetic{}},
		{Domain: "casino", Name: "provider", Component: fixtureUnmarked{}},
		{Domain: "kyc", Name: "provider", Component: fixtureEligible{}},
	}
	err := RefuseSyntheticInProduction("production", regs)
	if err == nil {
		t.Fatal("expected refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "payments/provider") {
		t.Errorf("expected message to name payments/provider, got: %s", msg)
	}
	if !strings.Contains(msg, "casino/provider") {
		t.Errorf("expected message to name casino/provider, got: %s", msg)
	}
	if strings.Contains(msg, "kyc/provider") {
		t.Errorf("did not expect eligible kyc/provider to be named, got: %s", msg)
	}
}

func TestSyntheticGuard_NonProductionNeverRefuses(t *testing.T) {
	for _, env := range []string{"development", "staging", "", "anything-else"} {
		regs := []Registration{{Domain: "x", Name: "y", Component: fixtureUnmarked{}}}
		if err := RefuseSyntheticInProduction(env, regs); err != nil {
			t.Errorf("environment %q: expected no refusal, got: %v", env, err)
		}
	}
}
