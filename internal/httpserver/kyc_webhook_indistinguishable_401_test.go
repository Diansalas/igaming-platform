//go:build integration

// KYC-WH-1 (Stage 10.2, ADR 0091). K6 (design §H): every pre-verification
// rejection reason the KYC public webhook route can produce must give a
// BYTE-IDENTICAL 401 response (status and body, modulo request_id) -
// modelled directly on payment_webhook_tenant_binding_test.go's own
// TestWebhook_EnumerationOracle_IndistinguishableResponses, the pattern
// this file is required to follow ("Use payments' equivalent suites as the
// pattern").
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TestKYCWebhook_EnumerationOracle_IndistinguishableResponses is K6: an
// unauthenticated caller must never be able to distinguish "unknown slug"
// from "bad signature" from "oversized body", etc., by status code or
// response body.
func TestKYCWebhook_EnumerationOracle_IndistinguishableResponses(t *testing.T) {
	pool, issuer := testEnv(t)
	mockProvider := kyc.NewMockKYCProvider()
	orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mockProvider}, kyc.NewMockWebhookCredentials(mockProvider))
	srv := newKYCTestServerWithLogger(t, pool, issuer, orch, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	activeTenant := mustCreateTenant(t, pool)
	suspendedTenant := mustCreateTenant(t, pool)
	if err := launchfix.TrySetTenantStatus(context.Background(), suspendedTenant.ID, "suspended"); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	genuine := mockProvider.CallbackPayload(activeTenant.ID, "k6-ref", kyc.ProviderApproved, "x")

	// A validly-formatted (v1=<64 hex>) but wrong signature - same
	// technique as every other bad_signature probe in this suite.
	badSigHeader := genuine.Header.Clone()
	sig := badSigHeader.Get(webhookauth.KYCSignatureHeader)
	flipped := sig
	for i := len(sig) - 1; i >= 0; i-- {
		if sig[i] != '0' {
			flipped = sig[:i] + "0" + sig[i+1:]
			break
		}
	}
	badSigHeader.Set(webhookauth.KYCSignatureHeader, flipped)

	// A non-JSON body under genuine's OWN valid headers (signed for a
	// DIFFERENT, JSON body) - verify-before-parse (design §B6(c)/(d)) must
	// reject this as signature_invalid, the SAME uniform 401, never a
	// distinguishable parse-error response reachable only for a
	// resolvable active tenant.
	nonJSONBody := []byte("this is deliberately not valid JSON at all {{{")

	oversizedBody := make([]byte, maxKYCWebhookBodyBytes+1)

	type probe struct {
		name   string
		path   string
		header http.Header
		body   []byte
	}
	probes := []probe{
		{"unknown_slug", "/v1/webhooks/kyc/does-not-exist-" + activeTenant.Slug + "/mock", genuine.Header, genuine.Body},
		{"suspended_tenant", "/v1/webhooks/kyc/" + suspendedTenant.Slug + "/mock", genuine.Header, genuine.Body},
		{"unregistered_provider", "/v1/webhooks/kyc/" + activeTenant.Slug + "/not-a-real-provider", genuine.Header, genuine.Body},
		{"bad_signature", "/v1/webhooks/kyc/" + activeTenant.Slug + "/mock", badSigHeader, genuine.Body},
		{"no_headers", "/v1/webhooks/kyc/" + activeTenant.Slug + "/mock", http.Header{}, genuine.Body},
		{"non_json_body_active_tenant", "/v1/webhooks/kyc/" + activeTenant.Slug + "/mock", genuine.Header, nonJSONBody},
		{"oversized_body_active_tenant", "/v1/webhooks/kyc/" + activeTenant.Slug + "/mock", genuine.Header, oversizedBody},
	}

	var referenceBody string
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			resp := rawPostKYCCallback(t, srv, p.path, webhookauth.Inbound{Header: p.header, Body: p.body})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", resp.StatusCode)
			}
			var body apierror.Error
			decodeBody(t, resp, &body)
			body.RequestID = "" // excluded from the comparison, per the QA plan
			normalized := string(body.Code) + "|" + body.Message
			if referenceBody == "" {
				referenceBody = normalized
			} else if normalized != referenceBody {
				t.Fatalf("expected the SAME body (modulo request_id) as every other case, got %q, want %q", normalized, referenceBody)
			}
			if body.Code != apierror.CodeUnauthorized || body.Message != "callback rejected" {
				t.Fatalf("unexpected body shape: %+v", body)
			}
		})
	}

	// Bad provider_id charset gets the SAME 401 too, checked separately
	// since it uses a syntactically different path segment (mirrors
	// payments' own "bad_provider_id_charset" subtest).
	t.Run("bad_provider_id_charset", func(t *testing.T) {
		resp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+activeTenant.Slug+"/BAD_ID!", webhookauth.Inbound{Header: genuine.Header, Body: genuine.Body})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
		var body apierror.Error
		decodeBody(t, resp, &body)
		if body.Code != apierror.CodeUnauthorized || body.Message != "callback rejected" {
			t.Fatalf("unexpected body shape: %+v", body)
		}
	})
}
