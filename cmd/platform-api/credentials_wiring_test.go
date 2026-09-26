package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TestWiring_NoFingerprintKey_RealCredentialSubsystemAbsent (security
// review §3): without a fingerprint key - or without a backend - the real
// subsystem is not constructed, startup still succeeds, and the MOCK
// wiring is unaffected.
func TestWiring_NoFingerprintKey_RealCredentialSubsystemAbsent(t *testing.T) {
	noKey := credentialTestConfig(t)
	noKey.ProviderCredentialFingerprintKey = config.NewSecretValue("")
	noBackend := credentialTestConfig(t)
	noBackend.SecretStoreBackends = nil
	for name, cfg := range map[string]config.Config{"no key": noKey, "no backend": noBackend} {
		b, err := withCredentialSubsystem(cfg, buildProviderBundle(allOnWiring))
		if err != nil {
			t.Fatalf("%s: startup must succeed, got %v", name, err)
		}
		if b.Credentials != nil {
			t.Fatalf("%s: the real subsystem must not be constructed", name)
		}
		if b.CasinoWebhookResolver == nil || b.casinoOrchestratorResolver() == nil {
			t.Fatalf("%s: the MOCK wiring must be unaffected", name)
		}
	}
	// Production, no key, no backend: startup succeeds with no resolver at
	// all (every real callback fails closed as no_resolver).
	prod := baseConfig(t, "production", true)
	b, err := withCredentialSubsystem(prod, buildProviderBundle(mockProviderWiring(prod)))
	if err != nil || b.Credentials != nil || b.casinoOrchestratorResolver() != nil || b.paymentsOrchestratorResolver() != nil {
		t.Fatalf("production without the subsystem: err=%v creds=%v", err, b.Credentials)
	}
}

// TestWiring_RealResolverServesNonSyntheticAdaptersOnly: with the subsystem
// constructed, the orchestrator resolver is the two-way split by adapter
// KIND - the bundle's adapters are all synthetic MOCKs, so every one of
// their callbacks still goes to the MOCK resolver (and with test support
// off, to no resolver at all: the real resolver never serves a synthetic
// adapter).
func TestWiring_RealResolverServesNonSyntheticAdaptersOnly(t *testing.T) {
	cfg := credentialTestConfig(t)
	on, err := withCredentialSubsystem(cfg, buildProviderBundle(allOnWiring))
	if err != nil || on.Credentials == nil {
		t.Fatalf("subsystem: %v", err)
	}
	tenant := uuid.New()
	if cred, err := resolveKey(on.casinoOrchestratorResolver(), tenant, "mock-casino", webhookauth.MockKeyID); err != nil || cred.TenantID != tenant {
		t.Fatalf("a synthetic adapter must be served by the MOCK resolver: %v", err)
	}
	off, err := withCredentialSubsystem(cfg, buildProviderBundle(mockWiring{}))
	if err != nil {
		t.Fatal(err)
	}
	r := off.casinoOrchestratorResolver()
	if r == nil {
		t.Fatal("with the subsystem constructed the split resolver exists")
	}
	if _, err := r.Resolve(context.Background(), nil, tenant, "mock-casino", webhookauth.MockKeyID, webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrNoResolver) {
		t.Fatalf("a synthetic adapter with MOCK wiring off must get no_resolver (never the real resolver), got %v", err)
	}
}

// TestWiring_SecretBackendsAndProductionGuard: devfile is development-only
// (and carries no production-eligibility marker, so the ADR 0085 guard
// refuses it in production); awssm is NOT IMPLEMENTED in this build and a
// configuration naming it refuses startup; the real subsystem itself is
// production-eligible.
func TestWiring_SecretBackendsAndProductionGuard(t *testing.T) {
	cfg := credentialTestConfig(t)
	b, err := withCredentialSubsystem(cfg, buildProviderBundle(mockWiring{}))
	if err != nil || len(b.SecretBackends) != 1 {
		t.Fatalf("devfile in development: %v", err)
	}
	regs := buildRegistrations(cfg, b)
	var sawSub, sawDevfile bool
	for _, r := range regs {
		switch r.Name {
		case "subsystem":
			sawSub = true
			if _, ok := r.Component.(providerkind.ProductionEligible); !ok {
				t.Fatal("the real subsystem must be production-eligible")
			}
		case "secret_backend:devfile":
			sawDevfile = true
		}
	}
	if !sawSub || !sawDevfile {
		t.Fatalf("the subsystem and its backend must be registered with the guard: %v", regs)
	}
	err = providerkind.RefuseSyntheticInProduction("production", regs)
	if err == nil || !strings.Contains(err.Error(), "secret_backend:devfile") {
		t.Fatalf("the guard must refuse a devfile backend in production, got %v", err)
	}

	prodDevfile := cfg
	prodDevfile.Environment = "production"
	if _, err := withCredentialSubsystem(prodDevfile, buildProviderBundle(mockWiring{})); err == nil {
		t.Fatal("devfile must refuse startup in production")
	}
	awssm := baseConfig(t, "production", true)
	awssm.ProviderCredentialFingerprintKey = cfg.ProviderCredentialFingerprintKey
	awssm.SecretStoreBackends = []string{config.SecretBackendAWSSecretsManager}
	if _, err := withCredentialSubsystem(awssm, buildProviderBundle(mockWiring{})); err == nil || !strings.Contains(err.Error(), "NOT IMPLEMENTED") {
		t.Fatalf("awssm must refuse startup until W3b, got %v", err)
	}
	memory := cfg
	memory.SecretStoreBackends = []string{"memory"}
	if _, err := withCredentialSubsystem(memory, buildProviderBundle(mockWiring{})); err == nil {
		t.Fatal("memory must never be configurable")
	}
}
