//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// ADR 0102 8 row 10, payload_mismatch: a reversal reference already used to
// reverse deposit D1 is redelivered naming a DIFFERENT deposit D2. The domain
// transaction rolls back (nothing posts), the response is the generic 409, and
// the failure-path P1 is raised detached.
func TestIWire_Webhook_PayloadMismatch_RaisesDetachedP1(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const amount int64 = 4000
	deposit := func(key string) string {
		resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": key,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("deposit %s: %d", key, resp.StatusCode)
		}
		var intent depositIntentResponse
		decodeBody(t, resp, &intent)
		ref := providerReferenceFromRedirectURL(intent.RedirectURL)
		cb := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount, "EUR", "", false))
		if cb.StatusCode != http.StatusOK {
			t.Fatalf("success callback %s: %d", key, cb.StatusCode)
		}
		cb.Body.Close()
		return ref
	}
	d1, d2 := deposit("iw-pm-1"), deposit("iw-pm-2")

	post := func(origin string) int {
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, "iw-pm-rev", origin, payments.OutcomeSucceeded, amount, "EUR", "", false))
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := post(d1); s != http.StatusOK {
		t.Fatalf("first reversal: %d", s)
	}
	if s := post(d2); s != http.StatusConflict {
		t.Fatalf("a reversal reference re-used for a different deposit must be 409, got %d", s)
	}
	rows := alertinject.Find(alertinject.ForSubject(t, pool, tenant.ID), string(alerting.KindPaymentWebhookIntegrity))
	if len(rows) != 1 || rows[0].Discriminator != "provider:mock:reason:payload_mismatch" || rows[0].Severity != "p1" {
		t.Fatalf("expected the payload_mismatch P1, got %+v", rows)
	}
}
