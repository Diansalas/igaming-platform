//go:build integration

// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091). C6/C12-equivalent (security
// review SC-3, 2026-09-26): for each webhookauth.Reason the casino public
// webhook route can produce, the single captured "casino_webhook_auth_
// failed" log line carries ONLY allow-listed keys, and the response body
// is BYTE-IDENTICAL (the uniform 401 "callback rejected") regardless of
// reason. Modeled on internal/httpserver/kyc_webhook_auth_failure_logging_
// integration_test.go's own pattern exactly (same allow-list per design
// §A).
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func newCasinoTestServerWithLogger(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *casino.Orchestrator, logger *slog.Logger) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:                      logger,
		DB:                          pool,
		AuthIssuer:                  issuer,
		ServiceName:                 "platform-api-test",
		AccessTokenTTL:              5 * time.Minute,
		RefreshTokenTTL:             time.Hour,
		CasinoOrchestrator:          orchestrator,
		CasinoPlaySimulationEnabled: true,
		PersonResolver:              identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

var casinoWebhookAllowedLogKeys = map[string]bool{
	"request_id":             true,
	"reason":                 true,
	"tenant_id":              true,
	"provider_id":            true,
	"key_id":                 true,
	"credential_fingerprint": true,
	"client_ip":              true,
	"body_len":               true,
}

func findCasinoAuthFailureLine(t *testing.T, lines []capturedLogLine) capturedLogLine {
	t.Helper()
	var matches []capturedLogLine
	for _, l := range lines {
		if l.msg == "casino_webhook_auth_failed" {
			matches = append(matches, l)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 casino_webhook_auth_failed log line, got %d (%+v)", len(matches), matches)
	}
	return matches[0]
}

// runCasinoAuthFailureLogCase posts one webhook request expected to fail
// pre-verification authentication and asserts BOTH the uniform response
// and the allow-listed log line together.
func runCasinoAuthFailureLogCase(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orch *casino.Orchestrator,
	path string, in webhookauth.Inbound, reason webhookauth.Reason) {
	t.Helper()
	logger, captured := newCapturingLogger()
	srv := newCasinoTestServerWithLogger(t, pool, issuer, orch, logger)

	resp := rawPostCasinoCallback(t, srv, path, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message for reason=%s, got %q", reason, apiErr.Message)
	}

	line := findCasinoAuthFailureLine(t, captured())
	for k := range line.attrs {
		if !casinoWebhookAllowedLogKeys[k] {
			t.Errorf("casino_webhook_auth_failed carries a NON-allow-listed field %q (value %v)", k, line.attrs[k])
		}
	}
	for _, forbiddenKey := range []string{"body", "header", "signature", "slug", "err", "error", "raw"} {
		if _, present := line.attrs[forbiddenKey]; present {
			t.Errorf("casino_webhook_auth_failed must never carry a %q field", forbiddenKey)
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

func TestCasinoWebhook_AuthFailureLogging_AllowListOnly(t *testing.T) {
	pool, issuer := testEnv(t)

	activeTenant := mustCreateTenant(t, pool)
	suspendedTenant := mustCreateTenant(t, pool)
	if err := pool.WithPlatformAdmin(context.Background(), activeTenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, suspendedTenant.ID)
		return err
	}); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	t.Run("tenant_unknown", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, casino.CallbackEventBet, "cas-log-1", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/does-not-exist-slug/mock-casino", genuine, webhookauth.ReasonTenantUnknown)
	})

	t.Run("tenant_inactive", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(suspendedTenant.ID, casino.CallbackEventBet, "cas-log-2", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/"+suspendedTenant.Slug+"/mock-casino", genuine, webhookauth.ReasonTenantInactive)
	})

	t.Run("provider_unregistered", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, casino.CallbackEventBet, "cas-log-3", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/"+activeTenant.Slug+"/not-a-real-provider", genuine, webhookauth.ReasonProviderUnregistered)
	})

	t.Run("no_resolver", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, nil)
		genuine := mock.CallbackPayload(activeTenant.ID, casino.CallbackEventBet, "cas-log-4", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/"+activeTenant.Slug+"/mock-casino", genuine, webhookauth.ReasonNoResolver)
	})

	t.Run("signature_invalid", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, casino.CallbackEventBet, "cas-log-5", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		genuine.Header.Set(webhookauth.CasinoSignatureHeader, "v1="+"0000000000000000000000000000000000000000000000000000000000000000"[:64])
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/"+activeTenant.Slug+"/mock-casino", genuine, webhookauth.ReasonSignatureInvalid)
	})

	t.Run("provider_invalid", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, casino.CallbackEventBet, "cas-log-6", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/"+activeTenant.Slug+"/BAD_ID!", genuine, webhookauth.ReasonProviderInvalid)
	})

	t.Run("body_too_large", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		oversized := webhookauth.Inbound{Header: map[string][]string{}, Body: make([]byte, maxCasinoWebhookBodyBytes+1)}
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/"+activeTenant.Slug+"/mock-casino", oversized, webhookauth.ReasonBodyTooLarge)
	})

	t.Run("signature_missing", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR")
		orch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		genuine := mock.CallbackPayload(activeTenant.ID, casino.CallbackEventBet, "cas-log-7", "", "round-1", "game-1", 1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		noHeaders := webhookauth.Inbound{Header: map[string][]string{}, Body: genuine.Body}
		runCasinoAuthFailureLogCase(t, pool, issuer, orch,
			"/v1/webhooks/casino/"+activeTenant.Slug+"/mock-casino", noHeaders, webhookauth.ReasonSignatureMissing)
	})
}
