//go:build integration

// KYC-WH-1 (Stage 10.2, ADR 0091). K6/K12 (design §H): for each
// webhookauth.Reason the KYC public webhook route can produce, the single
// captured "kyc_webhook_auth_failed" log line carries ONLY allow-listed
// keys, and the response body is BYTE-IDENTICAL (the uniform 401 "callback
// rejected") regardless of reason. Modeled on
// payment_webhook_auth_failure_logging_integration_test.go's own pattern.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

var kycWebhookAllowedLogKeys = map[string]bool{
	"request_id":             true,
	"reason":                 true,
	"tenant_id":              true,
	"provider_id":            true,
	"key_id":                 true,
	"credential_fingerprint": true,
	"client_ip":              true,
	"body_len":               true,
}

func newKYCTestServerWithLogger(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *kyc.Orchestrator, logger *slog.Logger) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:            logger,
		DB:                pool,
		AuthIssuer:        issuer,
		ServiceName:       "platform-api-test",
		AccessTokenTTL:    5 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		PersonResolver:    identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:   orchestrator,
		KYCWebhookEnabled: true,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func findKYCAuthFailureLine(t *testing.T, lines []capturedLogLine) capturedLogLine {
	t.Helper()
	var matches []capturedLogLine
	for _, l := range lines {
		if l.msg == "kyc_webhook_auth_failed" {
			matches = append(matches, l)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 kyc_webhook_auth_failed log line, got %d (%+v)", len(matches), matches)
	}
	return matches[0]
}

// runKYCAuthFailureLogCase posts one webhook request expected to fail
// pre-verification authentication and asserts BOTH the uniform response
// (K6) and the allow-listed log line (K12) together.
func runKYCAuthFailureLogCase(t *testing.T, pool *db.Pool, issuer *auth.Issuer, tenantID string, orch *kyc.Orchestrator,
	path string, in webhookauth.Inbound, reason webhookauth.Reason) {
	t.Helper()
	logger, captured := newCapturingLogger()
	srv := newKYCTestServerWithLogger(t, pool, issuer, orch, logger)

	resp := rawPostKYCCallback(t, srv, path, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Message != "callback rejected" {
		t.Fatalf("K6: expected the uniform 'callback rejected' message for reason=%s, got %q", reason, apiErr.Message)
	}

	line := findKYCAuthFailureLine(t, captured())
	for k := range line.attrs {
		if !kycWebhookAllowedLogKeys[k] {
			t.Errorf("kyc_webhook_auth_failed carries a NON-allow-listed field %q (value %v)", k, line.attrs[k])
		}
	}
	for _, forbiddenKey := range []string{"body", "header", "signature", "slug", "err", "error", "raw"} {
		if _, present := line.attrs[forbiddenKey]; present {
			t.Errorf("kyc_webhook_auth_failed must never carry a %q field", forbiddenKey)
		}
	}
	if got, _ := line.attrs["reason"].(string); got != string(reason) {
		t.Errorf("expected reason=%q, got %q", reason, got)
	}
	if _, present := line.attrs["request_id"]; !present {
		t.Error("expected request_id to always be present")
	}
	if _, present := line.attrs["client_ip"]; !present {
		t.Error("expected client_ip to always be present")
	}
	if _, present := line.attrs["body_len"]; !present {
		t.Error("expected body_len to always be present")
	}
}

func TestKYCWebhook_AuthFailureLogging_AllowListOnly(t *testing.T) {
	pool, issuer := testEnv(t)

	activeTenant := mustCreateTenant(t, pool)
	suspendedTenant := mustCreateTenant(t, pool)
	if err := launchfix.TrySetTenantStatus(context.Background(), suspendedTenant.ID, "suspended"); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	t.Run("tenant_unknown", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, "k12-ref", kyc.ProviderApproved, "x")
		runKYCAuthFailureLogCase(t, pool, issuer, activeTenant.ID.String(), orch,
			"/v1/webhooks/kyc/does-not-exist-slug/mock", genuine, webhookauth.ReasonTenantUnknown)
	})

	t.Run("tenant_inactive", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(suspendedTenant.ID, "k12-ref", kyc.ProviderApproved, "x")
		runKYCAuthFailureLogCase(t, pool, issuer, suspendedTenant.ID.String(), orch,
			"/v1/webhooks/kyc/"+suspendedTenant.Slug+"/mock", genuine, webhookauth.ReasonTenantInactive)
	})

	t.Run("provider_unregistered", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, "k12-ref", kyc.ProviderApproved, "x")
		runKYCAuthFailureLogCase(t, pool, issuer, activeTenant.ID.String(), orch,
			"/v1/webhooks/kyc/"+activeTenant.Slug+"/not-a-real-provider", genuine, webhookauth.ReasonProviderUnregistered)
	})

	t.Run("no_resolver", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, nil)
		genuine := mock.CallbackPayload(activeTenant.ID, "k12-ref", kyc.ProviderApproved, "x")
		runKYCAuthFailureLogCase(t, pool, issuer, activeTenant.ID.String(), orch,
			"/v1/webhooks/kyc/"+activeTenant.Slug+"/mock", genuine, webhookauth.ReasonNoResolver)
	})

	t.Run("signature_invalid", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, "k12-ref", kyc.ProviderApproved, "x")
		genuine.Header.Set(webhookauth.KYCSignatureHeader, "v1="+"0000000000000000000000000000000000000000000000000000000000000000"[:64])
		runKYCAuthFailureLogCase(t, pool, issuer, activeTenant.ID.String(), orch,
			"/v1/webhooks/kyc/"+activeTenant.Slug+"/mock", genuine, webhookauth.ReasonSignatureInvalid)
	})

	// K5/K6/K12 full-list closure: the three preamble-only reasons
	// (provider_id charset, oversized body, missing headers entirely) that
	// were previously exercised only by K6's byte-identical-response
	// assertion, not paired with their own K12 allow-listed-log assertion.
	t.Run("provider_invalid", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, "k12-ref", kyc.ProviderApproved, "x")
		runKYCAuthFailureLogCase(t, pool, issuer, activeTenant.ID.String(), orch,
			"/v1/webhooks/kyc/"+activeTenant.Slug+"/BAD_ID!", genuine, webhookauth.ReasonProviderInvalid)
	})

	t.Run("body_too_large", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))
		oversized := webhookauth.Inbound{Header: map[string][]string{}, Body: make([]byte, maxKYCWebhookBodyBytes+1)}
		runKYCAuthFailureLogCase(t, pool, issuer, activeTenant.ID.String(), orch,
			"/v1/webhooks/kyc/"+activeTenant.Slug+"/mock", oversized, webhookauth.ReasonBodyTooLarge)
	})

	t.Run("signature_missing", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, "k12-ref", kyc.ProviderApproved, "x")
		noHeaders := webhookauth.Inbound{Header: map[string][]string{}, Body: genuine.Body}
		runKYCAuthFailureLogCase(t, pool, issuer, activeTenant.ID.String(), orch,
			"/v1/webhooks/kyc/"+activeTenant.Slug+"/mock", noHeaders, webhookauth.ReasonSignatureMissing)
	})
}
