//go:build integration

// Security review C1 of PRH-I4 (HIGH). Finding: a DB-gate rejection
// DURING credential resolution (the REAL internal/providercred.Resolver's
// single handle read - casino/KYC's only pre-verification read, payments'
// SECOND one) was silently folded into webhookauth.ErrCredentialUnavailable,
// which maps to the uniform pre-verification 401 - indistinguishable from
// "no credential exists". A legitimate, correctly-configured tenant would
// then see 401s under load, and a caller cannot tell "this callback is
// unauthenticated" from "the platform is momentarily busy".
//
// These tests use the REAL providercred.Resolver (never MOCK, per
// security's explicit instruction) in all three domains, with the A4b
// gate's per-key cap forced to 0 so gatedReader.WithTenantReadOnly always
// refuses - the resolver's handle-read query is therefore NEVER reached
// (no registered credential handle is needed at all: the gate decision
// happens strictly before the pool is ever touched, so this is also a
// clean demonstration that the fix works independent of whether a
// credential would otherwise have resolved).
package httpserver

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
)

// c1RealSubsystem builds a real, working *providercred.Subsystem (no MOCK
// resolver) backed by an in-memory secret store - the store is never
// actually reached in these tests (the gate refuses before the pool
// query), but a real Subsystem still requires a real router.
func c1RealSubsystem(t *testing.T) *providercred.Subsystem {
	t.Helper()
	router, err := memstore.NewRouter(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sub, err := providercred.New(config.Config{ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(key))}, router)
	if err != nil || sub == nil {
		t.Fatalf("providercred.New: %v (nil=%v)", err, sub == nil)
	}
	return sub
}

// c1AlwaysDenyGateSettings forces the A4b gate's per-key AND unknown caps
// to 0, so gatedReader.WithTenantReadOnly NEVER admits, for ANY key -
// deterministic, no timing dependency (Acquire's tryAcquireLocked fails
// immediately against a 0 cap, so DBGateWait is irrelevant).
func c1AlwaysDenyGateSettings() WebhookAdmissionSettings {
	rb := func(rate float64, burst int) WebhookRateBurst { return WebhookRateBurst{Rate: rate, Burst: burst} }
	return WebhookAdmissionSettings{
		Enabled:            true,
		PreAuthRate:        map[string]WebhookRateBurst{"payments": rb(1000, 1000), "casino": rb(1000, 1000), "kyc": rb(1000, 1000)},
		PreAuthUnknownRate: map[string]WebhookRateBurst{"payments": rb(1000, 1000), "casino": rb(1000, 1000), "kyc": rb(1000, 1000)},
		VerifiedRate:       map[string]WebhookRateBurst{"payments": rb(1000, 1000), "casino": rb(1000, 1000), "kyc": rb(1000, 1000)},
		InFlightGlobal:     1000, InFlightPerKey: 1000, InFlightUnknown: 1000,
		DBGateGlobal: 1000, DBGatePerKey: 0, DBGateUnknown: 0, DBGateWait: 50 * time.Millisecond,
		DomainTxPerTenant: 1000, DomainWait: 50 * time.Millisecond,
		BodyReadTimeout:  10 * time.Second,
		DirectoryRefresh: time.Hour, DirectoryCap: 200000,
		IdleEvict: time.Hour, VerifiedMaxKeys: 1000, PerIPMaxKeys: 1000,
	}
}

// TestAdmission_C1_PaymentsCredentialResolutionGateRejection_Maps503 covers
// payments' SECOND read (credential resolution) - the case security's own
// review specifically called out as still broken before this fix (the
// FIRST read, ProviderAcceptsWebhook, already propagated the raw sentinel
// correctly).
func TestAdmission_C1_PaymentsCredentialResolutionGateRejection_Maps503(t *testing.T) {
	pool, issuer := testEnv(t)
	sub := c1RealSubsystem(t)
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock}, sub.Resolver("payments"))

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, c1AlwaysDenyGateSettings(), false)

	payload := mock.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "c1-ref", "", payments.OutcomeSucceeded, 100, "EUR", "", false)
	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a DB-gate rejection during credential resolution must NEVER surface as the uniform 401 (security review C1)")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header (C2)")
	}
}

// TestAdmission_C1_CasinoCredentialResolutionGateRejection_Maps503 covers
// casino's ONLY pre-verification read - every gate rejection inside
// VerifyCallback goes through this path for casino.
func TestAdmission_C1_CasinoCredentialResolutionGateRejection_Maps503(t *testing.T) {
	pool, issuer := testEnv(t)
	sub := c1RealSubsystem(t)
	mock := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	orchestrator := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, sub.Resolver("casino"))

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)

	srv := newAdmissionTestServer(t, pool, issuer, nil, orchestrator, c1AlwaysDenyGateSettings(), false)

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "c1-casino-ref", "", "round-c1", "game-c1", 100, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
	resp := rawPostCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payments.InboundCallback{Header: payload.Header, Body: payload.Body})
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a DB-gate rejection during casino credential resolution must NEVER surface as the uniform 401 (security review C1)")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header (C2)")
	}
}

// TestAdmission_C1_KYCCredentialResolutionGateRejection_Maps503 covers
// kyc's ONLY pre-verification read, mirroring casino's shape.
func TestAdmission_C1_KYCCredentialResolutionGateRejection_Maps503(t *testing.T) {
	pool, issuer := testEnv(t)
	sub := c1RealSubsystem(t)
	mock := kyc.NewMockKYCProvider()
	orchestrator := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock-kyc": mock}, sub.Resolver("kyc"))

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)

	settings := c1AlwaysDenyGateSettings()
	handler, rt := NewWithAdmission(Deps{
		DB: pool, AuthIssuer: issuer, ServiceName: "platform-api-test",
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		KYCOrchestrator: orchestrator, KYCWebhookEnabled: true,
		WebhookAdmission: settings,
	})
	if err := rt.LoadDirectory(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	payload := mock.CallbackPayload(tenant.ID, "c1-kyc-ref", kyc.ProviderApproved, "")
	resp := rawPostCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock-kyc", payments.InboundCallback{Header: payload.Header, Body: payload.Body})
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a DB-gate rejection during kyc credential resolution must NEVER surface as the uniform 401 (security review C1)")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header (C2)")
	}
}
