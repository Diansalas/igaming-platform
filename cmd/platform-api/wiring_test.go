package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// TestMockProviderWiring_Matrix is PAYWH-GATE-1 (ruling J9) / K11's
// payments leg: mock wiring is derived only from TestSupportRoutesEnabled()
// for {production, non-production} x {test-support flag on, off}.
func TestMockProviderWiring_Matrix(t *testing.T) {
	cases := []struct {
		env          string
		flag         bool
		wantPayments bool
	}{
		// config.Load refuses production+flag, but the wiring must fail
		// closed on its own too (defence in depth, ADR 0085 layer 2).
		{"production", true, false},
		{"production", false, false},
		{"staging", true, true},
		{"staging", false, false},
		{"development", true, true},
		{"development", false, false},
	}
	for _, c := range cases {
		cfg := config.Config{Environment: c.env, TestSupportEndpointsEnabled: c.flag}
		got := mockProviderWiring(cfg)
		if got.PaymentsWebhookResolver != c.wantPayments {
			t.Errorf("env=%s flag=%v: PaymentsWebhookResolver=%v, want %v", c.env, c.flag, got.PaymentsWebhookResolver, c.wantPayments)
		}
		if got.PaymentsWebhookResolver != cfg.TestSupportRoutesEnabled() {
			t.Errorf("env=%s flag=%v: wiring diverges from TestSupportRoutesEnabled()", c.env, c.flag)
		}
	}
}

// TestPaymentsWebhookResolver_FollowsWiring proves the resolver actually
// injected follows the wiring value: a TRUE nil interface when off (so the
// Orchestrator's nil-resolver branch rejects every callback as
// no_resolver -> uniform 401, covered end-to-end by
// internal/httpserver TestWebhook_AuthFailureLogging_AllowListOnly and
// internal/payments' nil-resolver tenant-binding test), and a working
// MOCK resolver bound to the mock provider when on.
func TestPaymentsWebhookResolver_FollowsWiring(t *testing.T) {
	mock := payments.NewMockProvider("mock-payments", "EUR")

	for _, cfg := range []config.Config{
		{Environment: "production", TestSupportEndpointsEnabled: true},
		{Environment: "production", TestSupportEndpointsEnabled: false},
		{Environment: "staging", TestSupportEndpointsEnabled: false},
	} {
		if r := paymentsWebhookResolver(mockProviderWiring(cfg), mock); r != nil {
			t.Fatalf("env=%s flag=%v: expected a nil resolver interface, got %T", cfg.Environment, cfg.TestSupportEndpointsEnabled, r)
		}
	}

	r := paymentsWebhookResolver(mockProviderWiring(config.Config{Environment: "staging", TestSupportEndpointsEnabled: true}), mock)
	if r == nil {
		t.Fatal("expected the MOCK resolver with test support on")
	}
	tenantID := uuid.New()
	cred, err := r.Resolve(context.Background(), tenantID, "mock-payments", "mock-v1")
	if err != nil || cred.TenantID != tenantID || cred.ProviderID != "mock-payments" {
		t.Fatalf("MOCK resolver must resolve its own provider, got %v / %v", cred, err)
	}
	if _, err := r.Resolve(context.Background(), tenantID, "other", "mock-v1"); !errors.Is(err, payments.ErrWebhookCredentialUnavailable) {
		t.Fatalf("MOCK resolver must fail closed for another provider, got %v", err)
	}
	// And a callback signed by the mock verifies under exactly this
	// resolver's credential.
	in := mock.CallbackPayload(tenantID, payments.CallbackEventDeposit, "ref", "", payments.OutcomeSucceeded, 100, "EUR", "", false)
	if _, err := mock.HandleCallback(context.Background(), in, cred); err != nil {
		t.Fatalf("mock callback must verify under the wired resolver, got %v", err)
	}
}
