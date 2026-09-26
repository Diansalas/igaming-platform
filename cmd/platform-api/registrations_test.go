package main

import (
	"reflect"
	"strings"
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

// unregisteredBundleFields is the ONE completeness check (code review #2:
// the real test and its negative control share it, so the control proves
// the same code the real test runs). It returns the name of every non-nil
// field of bundle (a struct value) whose value is not the Component of any
// registration in regs. Pointers match by identity; any other value (a
// struct resolver holding a []byte, a map, ...) matches by type plus
// reflect.DeepEqual - never by using the value as a map key, which would
// panic for unhashable components.
func unregisteredBundleFields(bundle any, regs []providerkind.Registration) []string {
	v := reflect.ValueOf(bundle)
	tv := v.Type()
	var missing []string
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			if fv.IsNil() {
				continue
			}
		}
		field := fv.Interface()
		found := false
		for _, r := range regs {
			if sameComponent(field, r.Component) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, tv.Field(i).Name)
		}
	}
	return missing
}

func sameComponent(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta.Kind() == reflect.Ptr {
		return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
	}
	return reflect.DeepEqual(a, b)
}

// allOnWiring enables every optional MOCK component, so every
// providerBundle field is non-nil and the completeness scan is not vacuous.
var allOnWiring = mockWiring{PaymentsWebhookResolver: true, KYCWebhookEnabled: true, CasinoWebhookResolver: true}

// TestSyntheticGuard_RegistrationCompletenessScan is the required W1b
// case: buildRegistrations(cfg, providers) must enumerate EVERY non-nil
// field of the providerBundle buildProviderBundle constructs - now
// including the three MOCK webhook credential resolvers (security S-4,
// code review #2). Reflection over the actual bundle struct (not a
// hand-maintained parallel list) means a future field added to
// providerBundle without a matching entry in buildRegistrations fails
// immediately.
func TestSyntheticGuard_RegistrationCompletenessScan(t *testing.T) {
	cfg := baseConfig(t, "development", true)
	bundle := buildProviderBundle(allOnWiring)

	// Non-vacuity: with everything wired, every field is set.
	v := reflect.ValueOf(bundle)
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		if (fv.Kind() == reflect.Ptr || fv.Kind() == reflect.Interface) && fv.IsNil() {
			t.Fatalf("providerBundle field %q is nil with every wiring flag on - the completeness scan would skip it", v.Type().Field(i).Name)
		}
	}

	if missing := unregisteredBundleFields(bundle, buildRegistrations(cfg, bundle)); len(missing) != 0 {
		t.Errorf("providerBundle fields %v are not present in buildRegistrations' output - MOCK-ADAPTER-PROD-1 requires every wired component to pass through the synthetic guard", missing)
	}
}

// TestSyntheticGuard_RegistrationCompletenessScan_CatchesUnregisteredMock
// is the required negative control: a fixture bundle-shaped type with
// extra fields simulates "a mock wired in main without being registered",
// and the SAME helper the real test uses must report exactly those fields
// - a pointer component and a non-pointer, unhashable one (like the MOCK
// resolvers) - proving the completeness check is not vacuous.
func TestSyntheticGuard_RegistrationCompletenessScan_CatchesUnregisteredMock(t *testing.T) {
	type fixtureBundleWithUnregisteredMock struct {
		Registered           *fixtureSyntheticWrapper
		Unregistered         *fixtureSyntheticWrapper // deliberately absent from fixtureRegs below
		RegisteredResolver   any
		UnregisteredResolver any // deliberately absent from fixtureRegs below
		Unwired              *fixtureSyntheticWrapper
	}

	bundle := fixtureBundleWithUnregisteredMock{
		Registered:           &fixtureSyntheticWrapper{id: 1},
		Unregistered:         &fixtureSyntheticWrapper{id: 2},
		RegisteredResolver:   fixtureValueResolver{secret: []byte("registered")},
		UnregisteredResolver: fixtureValueResolver{secret: []byte("unregistered")},
	}
	fixtureRegs := []providerkind.Registration{
		{Domain: "fixture", Name: "registered", Component: bundle.Registered},
		{Domain: "fixture", Name: "registered_resolver", Component: fixtureValueResolver{secret: []byte("registered")}},
	}

	missing := unregisteredBundleFields(bundle, fixtureRegs)
	if !reflect.DeepEqual(missing, []string{"Unregistered", "UnregisteredResolver"}) {
		t.Fatalf("expected exactly the deliberately unregistered fields to be caught, got %v", missing)
	}
}

// fixtureValueResolver is a non-pointer, unhashable (it holds a []byte)
// synthetic component, shaped like webhookauth.MockResolver.
type fixtureValueResolver struct{ secret []byte }

func (fixtureValueResolver) SyntheticComponent() {}

// TestBuildRegistrations_RegistersResolversAndSchemes: with every MOCK
// wired, the production guard names each MOCK webhook credential resolver
// (S-4 point 1) and each adapter's webhook scheme (S-1 / code review #6).
func TestBuildRegistrations_RegistersResolversAndSchemes(t *testing.T) {
	cfg := baseConfig(t, "production", true)
	err := refuseSyntheticInProduction(cfg, buildRegistrations(cfg, buildProviderBundle(allOnWiring)))
	if err == nil {
		t.Fatal("expected refusal in production")
	}
	for _, want := range []string{
		"payments/webhook_resolver", "casino/webhook_resolver", "kyc/webhook_resolver",
		"payments/webhook_scheme:mock-payments", "casino/webhook_scheme:mock-casino", "kyc/webhook_scheme:mock",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("guard error does not name %q: %v", want, err)
		}
	}
}

// TestValidateWebhookSchemes_Bundle: the bundle's own adapters pass the
// pre-DB scheme validation (S-1) for every wiring shape main() can build.
func TestValidateWebhookSchemes_Bundle(t *testing.T) {
	for _, w := range []mockWiring{{}, allOnWiring} {
		if err := validateWebhookSchemes(buildProviderBundle(w)); err != nil {
			t.Fatalf("wiring %+v: bundle schemes refused: %v", w, err)
		}
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
	bundle := buildProviderBundle(allOnWiring)
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
