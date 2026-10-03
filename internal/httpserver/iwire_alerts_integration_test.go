//go:build integration

// ADR 0102 I-wire at the HTTP layer (ALERT-DELIVERY-1).
//
//   - The deposit webhook owns its evidence transaction through
//     alerting.InTx and flushes after the response. An injected raise failure
//     inside T10 must leave the dispute committed and the response the SAME
//     uniform 200 (S95-C4), and the post-commit detached raise must persist
//     the P1.
//   - The kill-switch engage alert is post-commit only (LF F10, LF test 10):
//     no failure of any class in the alert path can change an engage.
//   - The simulation route's payload-mismatch alert is Kind-tagged
//     simulation (p3) and is never delivered.
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// iwWebhookMismatch drives a real deposit to pending, then delivers a verified
// success callback whose amount contradicts the attempt (T10 from the
// callback). It returns the response, the tenant and the intent's own
// provider reference.
func iwWebhookMismatch(t *testing.T, inject bool, mode alertinject.Mode, code string) (resp *http.Response, tenantID string, attemptState string, ledgerTxCount int, alerts []alertinject.Row) {
	t.Helper()
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 6000
	r := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": "iw-wh-" + tenant.ID.String(),
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("initiate deposit: %d", r.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, r, &intent)
	ref := providerReferenceFromRedirectURL(intent.RedirectURL)

	if inject {
		alertinject.Install(t, pool, tenant.ID, mode, code)
	}
	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, depositAmount-1, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)

	var state string
	var n int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM payment_attempts WHERE provider_reference = $1`, ref).Scan(&state); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, tenant.ID, ref).Scan(&n)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	return resp, tenant.ID.String(), state, n, alertinject.ForSubject(t, pool, tenant.ID)
}

func TestIWire_Webhook_T10_RaisesP1_UniformResponse(t *testing.T) {
	resp, _, state, txs, alerts := iwWebhookMismatch(t, false, alertinject.InTxOnly, "P0001")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d, want the uniform 200", resp.StatusCode)
	}
	body := captureUniformBody(t, resp)
	if state != "disputed" || txs != 0 {
		t.Fatalf("state=%s postings=%d: want a disputed attempt and no posting", state, txs)
	}
	var found bool
	for _, a := range alerts {
		if a.Kind == string(alerting.KindPaymentWebhookIntegrity) && strings.HasSuffix(a.Discriminator, ":reason:"+payments.TerminalReasonCallbackAmountAssetMismatch) && a.Severity == "p1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the callback_amount_asset_mismatch P1, got %+v (body %v)", alerts, body)
	}
}

// Security addendum 1 (e): an injected Raise failure inside T10 commits the
// dispute, returns the SAME uniform 200, and fires the post-commit detached
// Raise.
func TestIWire_Webhook_T10_InjectedRaiseFailure_UniformResponse_DetachedP1Fires(t *testing.T) {
	respClean, _, _, _, _ := iwWebhookMismatch(t, false, alertinject.InTxOnly, "P0001")
	cleanBody := captureUniformBody(t, respClean)

	resp, _, state, txs, alerts := iwWebhookMismatch(t, true, alertinject.InTxOnly, "P0001")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an alert failure must never change the response: %d", resp.StatusCode)
	}
	body := captureUniformBody(t, resp)
	if len(body) != len(cleanBody) || body["received"] != cleanBody["received"] {
		t.Fatalf("response body must be identical with and without the alert failure: %v vs %v", body, cleanBody)
	}
	if state != "disputed" || txs != 0 {
		t.Fatalf("the dispute must commit despite the alert failure: state=%s postings=%d", state, txs)
	}
	if len(alertinject.Find(alerts, string(alerting.KindPaymentWebhookIntegrity))) != 1 {
		t.Fatalf("the post-commit detached raise must persist the P1: %+v", alerts)
	}
}

func TestIWire_Webhook_T10_PersistentRaiseFailure_UniformResponse_DisputeCommits(t *testing.T) {
	for _, code := range []string{"P0001", "23514"} {
		t.Run(code, func(t *testing.T) {
			resp, _, state, txs, alerts := iwWebhookMismatch(t, true, alertinject.Persistent, code)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want the uniform 200", resp.StatusCode)
			}
			captureUniformBody(t, resp)
			if state != "disputed" || txs != 0 {
				t.Fatalf("state=%s postings=%d", state, txs)
			}
			if len(alerts) != 0 {
				t.Fatalf("no tenant-visible alert can persist under a persistent failure: %+v", alerts)
			}
		})
	}
}
