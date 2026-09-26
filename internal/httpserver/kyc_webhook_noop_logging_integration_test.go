//go:build integration

// KYC-WH-1 (Stage 10.2, ADR 0091). K4 (Stage 10.2 final review, M4):
// kyc.Orchestrator.ReceiveCallback reports applied vs no-op, and this
// route logs a single allow-listed "kyc_webhook_noop" info line - request_id,
// tenant_id, provider_id only - for a verified callback that changed
// nothing, and NEVER for a verified callback that actually applied a
// transition.
package httpserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// newKYCNoopTestServer mirrors newKYCTestServer (kyc_flow_integration_
// test.go) exactly, but takes an injected logger so this file's tests can
// capture structured log lines - kept as its own local helper rather than
// changing newKYCTestServerWithLogger's signature (kyc_webhook_auth_
// failure_logging_integration_test.go), which several existing tests
// already call without needing player registration/document deps.
func newKYCNoopTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, logger *slog.Logger) (*httptest.Server, *kyc.MockKYCProvider) {
	t.Helper()
	mockProvider := kyc.NewMockKYCProvider()
	srv := httptest.NewServer(New(Deps{
		Logger:            logger,
		DB:                pool,
		AuthIssuer:        issuer,
		ServiceName:       "platform-api-test",
		AccessTokenTTL:    5 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		PersonResolver:    identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:   kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mockProvider}, kyc.NewMockWebhookCredentials(mockProvider)),
		KYCWebhookEnabled: true,
		DocumentStorage:   kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:    kyc.NewMockMalwareScanner(),
		EmailProvider:     email.NewMockProvider(),
	}))
	t.Cleanup(srv.Close)
	return srv, mockProvider
}

func findKYCNoopLines(lines []capturedLogLine) []capturedLogLine {
	var matches []capturedLogLine
	for _, l := range lines {
		if l.msg == "kyc_webhook_noop" {
			matches = append(matches, l)
		}
	}
	return matches
}

var kycNoopAllowedLogKeys = map[string]bool{
	"request_id":  true,
	"tenant_id":   true,
	"provider_id": true,
}

// TestKYCWebhook_NoopLogging_ReplayLogsAllowListedLine_AppliedDoesNot is
// K4: the FIRST, genuinely applied delivery of a callback never logs
// kyc_webhook_noop; a REPLAY of that same, already-applied callback (a true
// no-op per B7/J11) logs exactly one kyc_webhook_noop line carrying only
// request_id/tenant_id/provider_id.
func TestKYCWebhook_NoopLogging_ReplayLogsAllowListedLine_AppliedDoesNot(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	srv, mockProvider := newKYCNoopTestServer(t, pool, issuer, logger)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)
	providerReference := mustGetKYCProviderReference(t, pool, tenant.ID, verificationID)

	in := mockProvider.CallbackPayload(tenant.ID, providerReference, kyc.ProviderApproved, "auto_approved")

	// First delivery: genuinely applies the approval - must NOT log
	// kyc_webhook_noop.
	resp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for the first, applied delivery, got %d", resp.StatusCode)
	}
	if lines := findKYCNoopLines(captured()); len(lines) != 0 {
		t.Fatalf("expected NO kyc_webhook_noop line for a callback that actually applied a transition, got %d: %+v", len(lines), lines)
	}

	// Second delivery: an exact replay of the already-applied callback - a
	// true no-op (B7/J11) - must log exactly one allow-listed
	// kyc_webhook_noop line.
	repeat := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", in)
	defer repeat.Body.Close()
	if repeat.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 replaying an already-applied callback, got %d", repeat.StatusCode)
	}
	lines := findKYCNoopLines(captured())
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 kyc_webhook_noop line for the replay, got %d: %+v", len(lines), lines)
	}
	line := lines[0]
	for k := range line.attrs {
		if !kycNoopAllowedLogKeys[k] {
			t.Errorf("kyc_webhook_noop carries a NON-allow-listed field %q (value %v)", k, line.attrs[k])
		}
	}
	for _, want := range []string{"request_id", "tenant_id", "provider_id"} {
		if _, present := line.attrs[want]; !present {
			t.Errorf("kyc_webhook_noop is missing the required field %q", want)
		}
	}
	if got, _ := line.attrs["tenant_id"].(string); got != tenant.ID.String() {
		t.Errorf("expected tenant_id=%s, got %q", tenant.ID, got)
	}
	if got, _ := line.attrs["provider_id"].(string); got != "mock" {
		t.Errorf("expected provider_id=%q, got %q", "mock", got)
	}
}
