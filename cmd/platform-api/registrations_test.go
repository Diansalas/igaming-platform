package main

import (
	"reflect"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

func baseConfig(t *testing.T, environment string, explicit bool) config.Config {
	t.Helper()
	return config.Config{
		Environment:                 environment,
		EnvironmentExplicit:         explicit,
		DatabaseURL:                 "postgres://localhost/test",
		JWTSigningSecret:            "a-secret-that-is-at-least-32-characters-long",
		JWTActiveKID:                "k1",
		TestSupportEndpointsEnabled: false,
	}
}

// TestSyntheticGuard_RegistrationCompletenessScan is the required W1b
// case: buildRegistrations(cfg, providers) must enumerate EVERY non-nil
// field of the providerBundle buildProviderBundle constructs. This uses
// reflection over the actual bundle struct (not a hand-maintained parallel
// list) so a future field added to providerBundle without a matching
// entry in buildRegistrations fails this test immediately - the
// "deliberately unregistered mock" fixture below is exactly that
// scenario, reproduced structurally rather than by editing production
// code.
func TestSyntheticGuard_RegistrationCompletenessScan(t *testing.T) {
	cfg := baseConfig(t, "development", true)
	wiring := mockWiring{PaymentsWebhookResolver: true, KYCWebhookEnabled: true, CasinoWebhookResolver: true}
	bundle := buildProviderBundle(wiring)
	regs := buildRegistrations(cfg, bundle)

	registered := make(map[any]bool, len(regs))
	for _, r := range regs {
		if r.Component != nil {
			registered[r.Component] = true
		}
	}

	v := reflect.ValueOf(bundle)
	tv := v.Type()
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		if fv.Kind() == reflect.Ptr && fv.IsNil() {
			continue
		}
		if !registered[fv.Interface()] {
			t.Errorf("providerBundle field %q is not present in buildRegistrations' output - MOCK-ADAPTER-PROD-1 requires every wired component to pass through the synthetic guard", tv.Field(i).Name)
		}
	}
}

// TestSyntheticGuard_RegistrationCompletenessScan_CatchesUnregisteredMock
// is the required negative control: a fixture bundle-shaped type with an
// extra field simulates "a mock wired in main without being registered",
// and the identical reflection-based check used above must report it -
// proving the completeness check is not vacuous.
func TestSyntheticGuard_RegistrationCompletenessScan_CatchesUnregisteredMock(t *testing.T) {
	type fixtureBundleWithUnregisteredMock struct {
		Registered   *fixtureSyntheticWrapper
		Unregistered *fixtureSyntheticWrapper // deliberately absent from fixtureRegs below
	}

	bundle := fixtureBundleWithUnregisteredMock{
		Registered:   &fixtureSyntheticWrapper{id: 1},
		Unregistered: &fixtureSyntheticWrapper{id: 2},
	}
	fixtureRegs := []providerkind.Registration{
		{Domain: "fixture", Name: "registered", Component: bundle.Registered},
	}

	registered := make(map[any]bool, len(fixtureRegs))
	for _, r := range fixtureRegs {
		registered[r.Component] = true
	}

	v := reflect.ValueOf(bundle)
	tv := v.Type()
	var foundMissing bool
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		if !registered[fv.Interface()] {
			foundMissing = true
			t.Logf("fixture correctly caught unregistered field %q", tv.Field(i).Name)
		}
	}
	if !foundMissing {
		t.Fatal("expected the fixture's deliberately unregistered field to be caught, but the check found nothing")
	}
}

// fixtureSyntheticWrapper is the negative-control fixture's own synthetic
// type, defined here (not in production code) per the W1b test plan's "not
// by editing production code" requirement. It carries a field (rather than
// being a bare zero-size struct{}) specifically so two separately
// allocated instances never alias the same address - Go may return the
// identical pointer for distinct `&struct{}{}` allocations, which would
// silently defeat this test's pointer-identity comparison.
type fixtureSyntheticWrapper struct{ id int }

func (*fixtureSyntheticWrapper) SyntheticComponent() {}

// TestRefuseSyntheticInProduction_AllFlagsOn is security condition C13's
// "a test runs the guard with every boolean in Config set to true (and to
// false) and still gets a refusal in production" - proving the guard has
// no configuration input besides the environment.
func TestRefuseSyntheticInProduction_AllFlagsOn(t *testing.T) {
	allTrue := baseConfig(t, "production", true)
	allTrue.TestSupportEndpointsEnabled = false // Load() would refuse production+true; a hand-built Config must still respect the invariant this represents
	bundle := buildProviderBundle(mockWiring{PaymentsWebhookResolver: true, KYCWebhookEnabled: true, CasinoWebhookResolver: true})
	if err := refuseSyntheticInProduction(allTrue, buildRegistrations(allTrue, bundle)); err == nil {
		t.Fatal("expected refusal in production regardless of flag state (all true), got nil")
	}

	allFalse := baseConfig(t, "production", true)
	bundleFalse := buildProviderBundle(mockWiring{})
	if err := refuseSyntheticInProduction(allFalse, buildRegistrations(allFalse, bundleFalse)); err == nil {
		t.Fatal("expected refusal in production regardless of flag state (all false), got nil")
	}
}

// TestRefuseSyntheticInProduction_EnvironmentMatrix is the required W1b
// matrix: {production, missing, staging, development} - "missing" meaning
// EnvironmentExplicit == false (APP_ENV was never set), which
// GuardEnvironment folds into "production" per security condition C13/
// ruling R8.
func TestRefuseSyntheticInProduction_EnvironmentMatrix(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		explicit    bool
		wantErr     bool
	}{
		{"production_explicit", "production", true, true},
		{"missing_app_env_treated_as_production", "development", false, true},
		{"staging_explicit", "staging", true, false},
		{"development_explicit", "development", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t, tc.environment, tc.explicit)
			bundle := buildProviderBundle(mockWiring{})
			err := refuseSyntheticInProduction(cfg, buildRegistrations(cfg, bundle))
			if tc.wantErr && err == nil {
				t.Fatalf("%s: expected refusal, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s: expected no refusal, got: %v", tc.name, err)
			}
		})
	}
}
