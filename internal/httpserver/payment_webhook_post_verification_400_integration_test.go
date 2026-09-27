//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// N3 (docs/governance/stage-10.1-code-review.md, "Re-verification
// (2026-09-26, 3f67ac5)"): the post-verification 400 mappings introduced
// by F1's fix (a VERIFIED callback that is either structurally malformed
// or whose own declared facts contradict the deposit it names) had no
// HTTP-level test - only the pre-verification uniform-401 family (T9) was
// covered. These two cases are deliberately NOT part of the uniform-401
// enumeration-oracle contract: the caller has already proven knowledge of
// the shared signing secret, so a distinguishable 400 here leaks nothing
// an unauthenticated caller could exploit (mapReceiveCallbackError's own
// doc comment; ADR 0022 §3 amendment point 7).
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// TestWebhook_MalformedBodyAfterVerification_Maps400 is N3's first case: a
// body that verifies against its own signature but fails to parse as
// mockCallbackBody. It also pins the "payment_webhook_malformed_body_
// after_verification" WARN line to its documented allow-list (provider_id,
// tenant_id, request_id only - never err or the body text).
func TestWebhook_MalformedBodyAfterVerification_Maps400(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	logger, captured := newCapturingLogger()
	srv := newFinancialTestServerWithLogger(t, pool, issuer, orchestrator, logger)

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	// A well-formed, genuinely verifying signature (SignRawBody signs
	// EXACTLY these bytes), but the bytes themselves are not valid JSON at
	// all - unlike T9's non_json_body_active_tenant probe, whose header is
	// only SYNTACTICALLY valid and does NOT verify against that body. This
	// probe's signature DOES verify, so HandleCallback must reach its
	// post-verification json.Unmarshal and fail there.
	nonJSONBody := []byte("this is deliberately not valid JSON at all {{{")
	signed := mockProvider.SignRawBody(tenant.ID, nonJSONBody)

	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", signed)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeValidation || body.Message != "callback rejected" {
		t.Fatalf("expected generic validation_error/\"callback rejected\", got %+v", body)
	}

	var line *capturedLogLine
	for i, l := range captured() {
		if l.msg == "payment_webhook_malformed_body_after_verification" {
			if line != nil {
				t.Fatalf("expected exactly one payment_webhook_malformed_body_after_verification line, got a second one")
			}
			l := l
			line = &l
			_ = i
		}
	}
	if line == nil {
		t.Fatalf("expected a payment_webhook_malformed_body_after_verification WARN line")
	}
	allowed := map[string]bool{"provider_id": true, "tenant_id": true, "request_id": true}
	for k, v := range line.attrs {
		if !allowed[k] {
			t.Errorf("payment_webhook_malformed_body_after_verification carries a non-allow-listed field %q=%v", k, v)
		}
	}
	for _, forbidden := range []string{"err", "error", "body", "raw"} {
		if _, present := line.attrs[forbidden]; present {
			t.Errorf("payment_webhook_malformed_body_after_verification must never carry a %q field", forbidden)
		}
	}
	if got, _ := line.attrs["tenant_id"].(string); got != tenant.ID.String() {
		t.Errorf("expected tenant_id=%q, got %q", tenant.ID.String(), got)
	}
}

// TestWebhook_ProviderMismatchAfterVerification_IsDisputedNotRejected is
// N3's second case, adapted for the PRH-payments-callback-cutover (ADR 0095
// §6.2, LF95-C3): a GENUINELY verified callback (correct signature,
// well-formed JSON) whose own declared amount contradicts the payment
// attempt its provider_reference resolves to.
//
// Old->new (ADR 0095 §6.2 "anomaly" row, superseding the pre-cutover
// rollback-and-409 for ErrCallbackProviderMismatch): before the cutover,
// postDepositSuccess's amount/asset cross-check returned a Go error that
// rolled back the whole transaction and mapped to 400 "callback rejected".
// After the cutover, a mismatched success is §4.4's T10 cell
// ("succeeded (mismatch)"): the attempt moves to `disputed`
// (terminal_reason "callback_amount_asset_mismatch") and that state change
// is COMMITTED together with its receipt - never rolled back to produce an
// error code (LF95-C3) - so the HTTP response is the SAME uniform 200 as
// every other disposition (S95-C4), never a 400. The security property
// this test originally pinned (no amount/asset/reference ever echoed to
// the caller) still holds, and is now trivially true: the uniform body
// carries no disposition-specific content at all.
func TestWebhook_ProviderMismatchAfterVerification_IsDisputedNotRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	logger, captured := newCapturingLogger()
	srv := newFinancialTestServerWithLogger(t, pool, issuer, orchestrator, logger)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 5000
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": "n3-mismatch-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := strings.TrimPrefix(intent.RedirectURL, "https://mock-psp.invalid/pay/")

	// Genuinely signed for THIS tenant/provider_reference, but the
	// declared amount (9999) does not match the attempt's own amount
	// (5000) - the signature verifies fine; the CONTENT is wrong.
	mismatched := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, 9999, "EUR", "", false)

	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", mismatched)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (uniform body; the mismatch is committed as a disputed attempt, not rolled back to an error), got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeBody(t, resp, &body)
	if _, ok := body["received"]; !ok {
		t.Fatalf("expected the uniform webhook body (received/request_id), got %+v", body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "9999") || strings.Contains(string(raw), "5000") || strings.Contains(string(raw), providerRef) {
		t.Fatalf("response body must never echo the mismatched amounts or the provider reference, got %+v", body)
	}

	// The state change is durable and correctly classified: the attempt is
	// `disputed` with the exact terminal_reason applyResolvedReceiptEvidence
	// assigns for this cell (receipt.go), never silently dropped and never
	// posted.
	var state, terminalReason string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT state, terminal_reason FROM payment_attempts WHERE provider_id = 'mock' AND provider_reference = $1`,
			providerRef,
		).Scan(&state, &terminalReason)
	}); err != nil {
		t.Fatalf("query attempt state: %v", err)
	}
	if state != "disputed" {
		t.Fatalf("expected attempt state 'disputed' after a mismatched success, got %q", state)
	}
	if terminalReason != "callback_amount_asset_mismatch" {
		t.Fatalf("expected terminal_reason 'callback_amount_asset_mismatch', got %q", terminalReason)
	}

	// No ledger effect: the mismatch must not have posted anything.
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != 0 {
		t.Fatalf("expected NO credit from a mismatched callback, got cash_balance=%d", walletResp.CashBalance)
	}
	_ = captured
}
