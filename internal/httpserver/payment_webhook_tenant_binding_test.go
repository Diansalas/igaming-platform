//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// QA binding test plan (docs/plans/stage-10.1-planning/
// 16-pay-wh-review-qa-test-plan.md) T1, T4, T9, T13 at the HTTP-handler
// level.
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// TestWebhook_SameTenant_AcceptedAndCredited is QA plan T1/T4: A-signed
// deposit delivered to A (and, symmetrically, B-signed to B) is accepted
// and credited exactly once.
func TestWebhook_SameTenant_AcceptedAndCredited(t *testing.T) {
	for _, tenantLabel := range []string{"A", "B"} {
		t.Run(tenantLabel, func(t *testing.T) {
			pool, issuer := testEnv(t)
			orchestrator, mockProvider := newMockOrchestrator()
			srv := newFinancialTestServer(t, pool, issuer, orchestrator)

			tenant := mustCreateTenant(t, pool)
			brand := mustCreateBrand(t, pool, tenant)
			mustRegisterCapability(t, pool, tenant.ID, mockProvider)

			player := mustRegisterPlayer(t, srv, brand.Slug)
			mustActivatePlayer(t, pool, tenant.ID, player.ID)

			const depositAmount int64 = 12_300
			resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
				"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": "t1-" + tenantLabel,
			})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
			}
			var intent depositIntentResponse
			decodeBody(t, resp, &intent)
			providerRef := strings.TrimPrefix(intent.RedirectURL, "https://mock-psp.invalid/pay/")

			payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
			resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			resp.Body.Close()

			resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
			var walletResp walletSummaryResponse
			decodeBody(t, resp, &walletResp)
			if walletResp.CashBalance != depositAmount {
				t.Fatalf("expected exactly one credit of %d, got cash_balance=%d", depositAmount, walletResp.CashBalance)
			}

			// Replayed: no second credit.
			resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
			resp.Body.Close()
			resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
			decodeBody(t, resp, &walletResp)
			if walletResp.CashBalance != depositAmount {
				t.Fatalf("expected the SAME single credit after a replay, got cash_balance=%d", walletResp.CashBalance)
			}
		})
	}
}

// TestWebhook_EnumerationOracle_IndistinguishableResponses is QA plan T9:
// every pre-verification rejection reason must produce a byte-identical
// 401 response (status and body, modulo request_id).
func TestWebhook_EnumerationOracle_IndistinguishableResponses(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	activeTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, activeTenant)
	mustRegisterCapability(t, pool, activeTenant.ID, mockProvider)

	suspendedTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, suspendedTenant)
	if err := pool.WithPlatformAdmin(context.Background(), activeTenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, suspendedTenant.ID)
		return err
	}); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	unconfiguredTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, unconfiguredTenant)
	// No capability row at all for "mock" under this tenant.

	pollingOnlyTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, pollingOnlyTenant)
	if err := pool.WithTenant(context.Background(), pollingOnlyTenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		declared := mockProvider.Capabilities()
		_, err := payments.WriteCapability(ctx, tx, mockProvider, pollingOnlyTenant.ID, nil, payments.CapabilityConfig{
			SupportedFiatCurrencies: declared.SupportedFiatCurrencies, SupportedPaymentMethods: declared.SupportedPaymentMethods,
			SupportsDeposit: true, SupportsWithdrawal: true, AmountLimits: declared.AmountLimits, Status: payments.CapabilityActive,
		})
		return err
	}); err != nil {
		t.Fatalf("register polling-only capability: %v", err)
	}
	// mustRegisterCapability/WriteCapability always takes callback
	// capability from the adapter's own declared value (webhook-only for
	// the mock), so directly downgrade the row to polling_only to model a
	// tenant that configured this provider for polling only.
	if err := pool.WithTenant(context.Background(), pollingOnlyTenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_capabilities SET callback_capabilities = 'polling_only' WHERE tenant_id = $1 AND provider_id = 'mock'`, pollingOnlyTenant.ID)
		return err
	}); err != nil {
		t.Fatalf("downgrade to polling_only: %v", err)
	}

	genuineBody := []byte(`{"event_type":"deposit","provider_reference":"enum-ref","outcome":"succeeded","amount":1000,"asset_code":"EUR"}`)
	keyMaterialBody := []byte(`{"event_type":"deposit","provider_reference":"enum-ref","outcome":"succeeded","amount":1000,"asset_code":"EUR","private_key":"deadbeef"}`)
	genuineSigned := mockProvider.CallbackPayload(activeTenant.ID, payments.CallbackEventDeposit, "enum-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)

	// keyMaterialBody's header below is genuineSigned's - signed over
	// genuineBody, NOT keyMaterialBody - so this probe's signature does
	// not verify at all (Stage 10.1 security review P2-1/code review
	// F1/architect PW-1: signature verification now runs BEFORE any body
	// parsing, including the key-material scan, so this probe is rejected
	// for reason signature_invalid, not key_material). It still belongs in
	// this table: the point of T9 is that the RESPONSE is identical either
	// way, regardless of which specific reason produced it - a genuinely-
	// signed key-material rejection is covered separately, at the reason-
	// specific level, by T12
	// (TestWebhook_AuthFailureLogging_AllowListOnly/key_material).
	badSigHeader := genuineSigned.Header.Clone()
	sig := badSigHeader.Get("X-Payments-Signature")
	flipped := strings.Replace(sig, "0", "f", 1)
	if flipped == sig {
		flipped = strings.Replace(sig, "1", "e", 1)
	}
	badSigHeader.Set("X-Payments-Signature", flipped)

	// Security review P2-1 / code review F1 / architect PW-1 (Stage 10.1
	// post-implementation review): a non-JSON body used to reach
	// HandleCallback's generic json.Unmarshal BEFORE signature
	// verification, returning a distinguishable 500 (with an error-level
	// log carrying a body fragment) instead of the SAME 401 every other
	// pre-verification failure got here - to an ACTIVE, CONFIGURED tenant
	// specifically, which is exactly the tenant/provider enumeration this
	// whole contract exists to prevent. The header is well-formed
	// (genuineSigned's own, syntactically valid v1=<64 hex>/key id), but
	// does not need to numerically verify against this body - the pre-fix
	// bug reached json.Unmarshal regardless of whether the signature would
	// ultimately have matched.
	nonJSONBody := []byte("this is deliberately not valid JSON at all {{{")

	// The same enumeration gap existed for an oversized body being
	// checked AFTER the tenant lookup (ruling 5 violation): an active,
	// resolvable tenant slug got a distinguishable 400 "request body too
	// large" while an unknown/suspended slug with the identical oversized
	// body got the uniform 401. Body size alone (never its content)
	// matters here, so genuineSigned's header is reused as-is.
	oversizedBody := make([]byte, maxWebhookBodyBytes+1)

	type probe struct {
		name   string
		path   string
		header http.Header
		body   []byte
	}
	probes := []probe{
		{"unknown_slug", "/v1/webhooks/payments/does-not-exist-" + activeTenant.Slug + "/mock", genuineSigned.Header, genuineBody},
		{"suspended_tenant", "/v1/webhooks/payments/" + suspendedTenant.Slug + "/mock", genuineSigned.Header, genuineBody},
		{"unregistered_provider", "/v1/webhooks/payments/" + activeTenant.Slug + "/not-a-real-provider", genuineSigned.Header, genuineBody},
		{"provider_not_configured", "/v1/webhooks/payments/" + unconfiguredTenant.Slug + "/mock", genuineSigned.Header, genuineBody},
		{"polling_only", "/v1/webhooks/payments/" + pollingOnlyTenant.Slug + "/mock", genuineSigned.Header, genuineBody},
		{"bad_signature", "/v1/webhooks/payments/" + activeTenant.Slug + "/mock", badSigHeader, genuineBody},
		{"key_material", "/v1/webhooks/payments/" + activeTenant.Slug + "/mock", genuineSigned.Header, keyMaterialBody},
		{"non_json_body_active_tenant", "/v1/webhooks/payments/" + activeTenant.Slug + "/mock", genuineSigned.Header, nonJSONBody},
		{"oversized_body_active_tenant", "/v1/webhooks/payments/" + activeTenant.Slug + "/mock", genuineSigned.Header, oversizedBody},
	}

	var referenceBody string
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			resp := rawPostCallback(t, srv, p.path, payments.InboundCallback{Header: p.header, Body: p.body})
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
	// since it uses a syntactically different path segment.
	t.Run("bad_provider_id_charset", func(t *testing.T) {
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+activeTenant.Slug+"/BAD_ID!", payments.InboundCallback{Header: genuineSigned.Header, Body: genuineBody})
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
