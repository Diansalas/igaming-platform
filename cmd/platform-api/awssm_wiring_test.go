package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/awssm"
)

// withCredentialsForTest is withCredentialSubsystem with the awssm SDK-fake
// constructor: it runs every awssm.New refusal but never builds a real AWS
// client, so no test here can dial the network (awssm's own
// TestAWSSM_NewNeverCalledFromTestsOutsideThisPackage forbids awssm.New in
// tests outside that package).
func withCredentialsForTest(cfg config.Config, b providerBundle) (providerBundle, error) {
	return withCredentialSubsystemUsing(context.Background(), cfg, b, awssm.NewWithSDKFake)
}

// awssmPermittedEnv clears every AWS-related variable awssm refuses, and
// sets the ECS container-credential relative URI it requires, for the
// duration of the test.
func awssmPermittedEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
		"AWS_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_DEFAULT_PROFILE",
		"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE",
		"AWS_ENDPOINT_URL", "AWS_CA_BUNDLE", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_EC2_METADATA_SERVICE_ENDPOINT",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
	} {
		t.Setenv(k, "")
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AWS_ENDPOINT_URL_") {
			t.Setenv(k, "")
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "/v2/credentials/00000000-test-task")
}

func awssmStores(stores []secretstore.Store) []*awssm.Store {
	var out []*awssm.Store
	for _, s := range stores {
		if a, ok := s.(*awssm.Store); ok {
			out = append(out, a)
		}
	}
	return out
}

// TestWiring_AWSSM_EnvironmentMatrix (Stage 10.3 W3b wiring): awssm is
// constructed ONLY when configured AND APP_ENV is explicitly staging or
// production; configured anywhere else, startup is refused; not
// configured, it is never constructed.
func TestWiring_AWSSM_EnvironmentMatrix(t *testing.T) {
	key := credentialTestConfig(t).ProviderCredentialFingerprintKey
	envs := []struct {
		name        string
		environment string
		explicit    bool
		permitted   bool
	}{
		{"development", "development", true, false},
		{"staging", "staging", true, true},
		{"production", "production", true, true},
		// config.Load's shape for a missing APP_ENV.
		{"missing APP_ENV", "development", false, false},
		// Defence in depth: "production" without an explicit APP_ENV is
		// still refused (ValidateSecretBackendScheme wants EXPLICIT).
		{"missing APP_ENV reading production", "production", false, false},
	}
	for _, e := range envs {
		for _, configured := range []bool{true, false} {
			name := e.name + "/awssm not configured"
			if configured {
				name = e.name + "/awssm configured"
			}
			t.Run(name, func(t *testing.T) {
				awssmPermittedEnv(t)
				cfg := baseConfig(t, e.environment, e.explicit)
				cfg.ProviderCredentialFingerprintKey = key
				cfg.SecretStoreAWSRegion = "eu-west-1"
				if configured {
					cfg.SecretStoreBackends = []string{config.SecretBackendAWSSecretsManager}
				}
				b, err := withCredentialsForTest(cfg, buildProviderBundle(mockWiring{}))
				switch {
				case configured && e.permitted:
					if err != nil {
						t.Fatalf("awssm configured in %s must start: %v", e.name, err)
					}
					if n := len(awssmStores(b.SecretBackends)); n != 1 || len(b.SecretBackends) != 1 {
						t.Fatalf("want exactly one awssm backend, got %d of %d", n, len(b.SecretBackends))
					}
					if b.Credentials == nil {
						t.Fatal("with a fingerprint key and awssm the real subsystem must be constructed")
					}
				case configured:
					if err == nil {
						t.Fatalf("awssm configured in %s must refuse startup", e.name)
					}
				default:
					if err != nil {
						t.Fatalf("awssm not configured must start: %v", err)
					}
					if n := len(awssmStores(b.SecretBackends)); n != 0 {
						t.Fatalf("awssm must not be constructed when not configured, got %d", n)
					}
				}
			})
		}
	}
}

// TestWiring_AWSSM_RefusalsPropagate: an explicitly configured awssm that
// cannot be built refuses startup - static credentials (including an SDK
// alias), a missing container-credential endpoint, a missing or malformed
// region.
func TestWiring_AWSSM_RefusalsPropagate(t *testing.T) {
	key := credentialTestConfig(t).ProviderCredentialFingerprintKey
	prod := func() config.Config {
		cfg := baseConfig(t, "production", true)
		cfg.ProviderCredentialFingerprintKey = key
		cfg.SecretStoreBackends = []string{config.SecretBackendAWSSecretsManager}
		cfg.SecretStoreAWSRegion = "eu-west-1"
		return cfg
	}
	for name, tc := range map[string]struct {
		env    map[string]string
		region string
		want   string
	}{
		"static AWS_ACCESS_KEY_ID":       {env: map[string]string{"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE"}, region: "eu-west-1", want: "static AWS credential"},
		"static alias AWS_ACCESS_KEY":    {env: map[string]string{"AWS_ACCESS_KEY": "AKIDEXAMPLE", "AWS_SECRET_KEY": "not-real"}, region: "eu-west-1", want: "static AWS credential"},
		"no container credential source": {env: map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": ""}, region: "eu-west-1", want: "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"},
		"missing region":                 {region: "", want: "region"},
		"malformed region":               {region: "EU-WEST-1", want: "not an AWS region"},
	} {
		t.Run(name, func(t *testing.T) {
			awssmPermittedEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := prod()
			cfg.SecretStoreAWSRegion = tc.region
			_, err := withCredentialsForTest(cfg, buildProviderBundle(mockWiring{}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a startup refusal containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestWiring_AWSSM_RegisteredAsProductionEligible: the awssm backend goes
// through buildRegistrations as a production-eligible (never synthetic)
// component, and the synthetic guard accepts it in production: over the
// credential registrations alone the guard passes, and over the whole
// (today all-MOCK) binary the guard's refusal never names awssm.
func TestWiring_AWSSM_RegisteredAsProductionEligible(t *testing.T) {
	awssmPermittedEnv(t)
	cfg := baseConfig(t, "production", true)
	cfg.ProviderCredentialFingerprintKey = credentialTestConfig(t).ProviderCredentialFingerprintKey
	cfg.SecretStoreBackends = []string{config.SecretBackendAWSSecretsManager}
	cfg.SecretStoreAWSRegion = "eu-west-1"
	b, err := withCredentialsForTest(cfg, buildProviderBundle(mockProviderWiring(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	regs := buildRegistrations(cfg, b)
	var credRegs []providerkind.Registration
	var saw bool
	for _, r := range regs {
		if r.Domain != "provider_credentials" {
			continue
		}
		credRegs = append(credRegs, r)
		if r.Name == "secret_backend:awssm" {
			saw = true
			if _, ok := r.Component.(providerkind.ProductionEligible); !ok {
				t.Fatal("the awssm backend must be registered as ProductionEligible")
			}
			if _, ok := r.Component.(providerkind.Synthetic); ok {
				t.Fatal("the awssm backend must never be synthetic")
			}
		}
	}
	if !saw || len(credRegs) != 2 {
		t.Fatalf("want the subsystem and secret_backend:awssm registered, got %v", credRegs)
	}
	if err := refuseSyntheticInProduction(cfg, credRegs); err != nil {
		t.Fatalf("the synthetic guard must accept awssm and the real subsystem in production: %v", err)
	}
	err = refuseSyntheticInProduction(cfg, regs)
	if err == nil {
		t.Fatal("today's binary wires MOCK adapters, so production must still refuse")
	}
	if strings.Contains(err.Error(), "awssm") || strings.Contains(err.Error(), "provider_credentials") {
		t.Fatalf("the guard must not refuse the awssm backend or the subsystem: %v", err)
	}
}
