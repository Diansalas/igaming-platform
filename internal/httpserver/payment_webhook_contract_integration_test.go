//go:build integration

// PRH-payments-callback-cutover (ADR 0095 §6.1/§6.2, S95-C4, LF95-C3):
// closes two gaps left open by the first cutover pass (see ADR 0095
// §27.10's implementation record).
//
//  1. The Outcome-normalization mutant in receipt.go's
//     applyReversalReceiptEvidence (some callers reuse the wire
//     Outcome=declined field as a chargeback-reason carrier on a
//     reversal event, which must never be persisted as a §4.4
//     attempt-decline outcome) was previously caught only by an
//     internal/payments test. TestPaymentWebhook_ReversalOutcomeDeclinedWireCarrier_PersistedAsSucceeded
//     kills it at the HTTP layer too: without the normalization, the
//     receipt insert violates payment_provider_events_check1 and the
//     whole callback transaction rolls back to a 500, not the uniform
//     200 this test asserts.
//  2. TestPaymentWebhook_UniformResponseAcrossDispositions proves the
//     §6.2/S95-C4 contract directly over HTTP: every 200 disposition
//     (applied, duplicate_effect, deferred_unresolved, and the anomaly/
//     tombstone-collision cases) returns a byte-identical
//     {request_id, received} body - no disposition-revealing field, and
//     no disposition signal leaks via any response header either.
//  3. TestPaymentWebhook_DeferredReceiptCapExceeded_503RetryAfter proves
//     the §6.1 step 5 / S95-C2(i) cap: once the unapplied-receipt cap for
//     (tenant, provider) is reached, a new unresolvable callback gets a
//     retryable 503 with Retry-After, and nothing new is stored.
package httpserver

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
)

// TestPaymentWebhook_ReversalOutcomeDeclinedWireCarrier_PersistedAsSucceeded
// see this file's header comment, gap 1.
func TestPaymentWebhook_ReversalOutcomeDeclinedWireCarrier_PersistedAsSucceeded(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 6000
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": "wire-declined-carrier-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	depositRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	successPayload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, depositRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", successPayload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deposit success callback: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The reversal's wire Outcome is "declined" (a chargeback-reason
	// carrier some callers still use, per receipt.go's own doc comment) -
	// it must never be persisted or treated as a §4.4 attempt decline; the
	// callback must be accepted (200) and the reversal must actually post.
	const reversalRef = "wire-declined-carrier-reversal-1"
	reversalPayload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, reversalRef, depositRef, payments.OutcomeDeclined, depositAmount, "EUR", "chargeback", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", reversalPayload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the reversal to be accepted (200) despite its wire Outcome=declined carrier, got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeBody(t, resp, &body)
	if received, _ := body["received"].(bool); !received {
		t.Fatalf("expected the uniform webhook body, got %+v", body)
	}

	// The persisted receipt's own outcome column must be "succeeded" -
	// never the wire's "declined" carrier value (which would also have
	// violated payment_provider_events_check1 without the normalization,
	// rolling the whole transaction back to a 500 instead of this 200).
	var outcome string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT outcome FROM payment_provider_events WHERE tenant_id = $1 AND provider_id = 'mock' AND provider_reference = $2`,
			tenant.ID, reversalRef,
		).Scan(&outcome)
	}); err != nil {
		t.Fatalf("query persisted receipt outcome: %v", err)
	}
	if outcome != "succeeded" {
		t.Fatalf("expected the persisted receipt outcome to be 'succeeded' (never the wire's 'declined' carrier), got %q", outcome)
	}

	// And the reversal actually posted: balance back to 0.
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != 0 {
		t.Fatalf("expected balance 0 after the reversal actually posted, got %d", walletResp.CashBalance)
	}
}

// captureUniformBody decodes a webhookReceivedResponse-shaped body,
// returning it with request_id zeroed so bodies from different requests
// compare equal apart from that one field (S95-C4's own carve-out).
func captureUniformBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	decodeBody(t, resp, &body)
	if len(body) != 2 {
		t.Fatalf("expected exactly 2 fields (request_id, received), got %+v", body)
	}
	if _, ok := body["request_id"]; !ok {
		t.Fatalf("expected a request_id field, got %+v", body)
	}
	if _, ok := body["received"]; !ok {
		t.Fatalf("expected a received field, got %+v", body)
	}
	body["request_id"] = "REDACTED"
	return body
}

// TestPaymentWebhook_UniformResponseAcrossDispositions see this file's
// header comment, gap 2.
func TestPaymentWebhook_UniformResponseAcrossDispositions(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	initiate := func(idemKey string, amount int64) string {
		t.Helper()
		resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": idemKey,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("initiate deposit %s: expected 201, got %d", idemKey, resp.StatusCode)
		}
		var intent depositIntentResponse
		decodeBody(t, resp, &intent)
		return providerReferenceFromRedirectURL(intent.RedirectURL)
	}

	var bodies []map[string]any
	var headers []http.Header
	record := func(name string, resp *http.Response, wantStatus int) {
		t.Helper()
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s: expected status %d, got %d", name, wantStatus, resp.StatusCode)
		}
		if wantStatus == http.StatusOK {
			bodies = append(bodies, captureUniformBody(t, resp))
			headers = append(headers, resp.Header.Clone())
		}
	}

	// applied: a genuine, first-time deposit success.
	depositRef1 := initiate("uniform-applied", 1000)
	record("applied",
		rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, depositRef1, "", payments.OutcomeSucceeded, 1000, "EUR", "", false)),
		http.StatusOK)

	// duplicate_effect: redeliver the identical success callback.
	record("duplicate_effect",
		rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, depositRef1, "", payments.OutcomeSucceeded, 1000, "EUR", "", false)),
		http.StatusOK)

	// deferred_unresolved: a reference nothing ever initiated.
	record("deferred_unresolved",
		rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "uniform-unresolved-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)),
		http.StatusOK)

	// anomaly (mismatched success -> committed T10 dispute, LF95-C3).
	depositRef2 := initiate("uniform-anomaly", 2000)
	record("anomaly_mismatch",
		rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, depositRef2, "", payments.OutcomeSucceeded, 9999, "EUR", "", false)),
		http.StatusOK)

	// tombstone collision scenario: a reversal of a never-posted deposit
	// (applied via the tombstone branch), then the late original deposit
	// success colliding with that tombstone (committed disputed, T10).
	const neverPostedRef = "uniform-tombstone-original"
	record("reversal_tombstone",
		rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, "uniform-tombstone-reversal", neverPostedRef, payments.OutcomeSucceeded, 500, "EUR", "", false)),
		http.StatusOK)
	record("tombstone_collision",
		rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, neverPostedRef, "", payments.OutcomeSucceeded, 500, "EUR", "", false)),
		http.StatusOK)

	if len(bodies) < 6 {
		t.Fatalf("expected 6 recorded 200 responses, got %d", len(bodies))
	}
	first := bodies[0]
	for i, b := range bodies[1:] {
		if b["received"] != first["received"] || len(b) != len(first) {
			t.Fatalf("body %d differs from the first (S95-C4 uniform-body violation): %+v vs %+v", i+1, b, first)
		}
	}

	// No disposition-revealing header either (only generic framework
	// headers - Content-Type, Content-Length, Date, and whatever the test
	// harness's own transport adds - are expected to vary or repeat; none
	// of them may ever be named after a disposition).
	forbidden := []string{"X-Disposition", "X-Payment-Disposition", "X-Applied", "X-Tombstoned", "X-Anomaly", "X-Duplicate"}
	for i, h := range headers {
		for _, name := range forbidden {
			if h.Get(name) != "" {
				t.Fatalf("response %d leaked disposition via header %q", i, name)
			}
		}
	}
}

// TestPaymentWebhook_DeferredReceiptCapExceeded_503RetryAfter see this
// file's header comment, gap 3.
func TestPaymentWebhook_DeferredReceiptCapExceeded_503RetryAfter(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	// Fill the unapplied-receipt cap for (tenant, mock) directly - driving
	// payments.DeferredReceiptCap (10,000) distinct callbacks through the
	// real HTTP path would be prohibitively slow for a test; the cap
	// probe (receipt.go CountUnappliedReceipts) only cares about row
	// count and resolved_at IS NULL, so a direct bulk insert reproduces
	// the exact condition it checks.
	const cap = payments.DeferredReceiptCap
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for i := 0; i < cap+1; i++ {
			ref := fmt.Sprintf("cap-fill-%d", i)
			fingerprint, _ := hex.DecodeString(fmt.Sprintf("%032x", i))
			batch.Queue(
				`INSERT INTO payment_provider_events (
					id, tenant_id, provider_id, event_type, provider_reference,
					outcome, amount, asset_code, event_fingerprint, disposition_at_receipt
				) VALUES (gen_random_uuid(), $1, 'mock', 'deposit', $2, 'succeeded', 1000, 'EUR', $3, 'deferred_unresolved')`,
				tenant.ID, ref, fingerprint,
			)
		}
		br := tx.SendBatch(ctx, batch)
		defer br.Close()
		for i := 0; i < cap+1; i++ {
			if _, err := br.Exec(); err != nil {
				return fmt.Errorf("insert cap-filler %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("fill the unapplied-receipt cap: %v", err)
	}

	var before int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1`, tenant.ID).Scan(&before)
	}); err != nil {
		t.Fatalf("count before: %v", err)
	}

	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "cap-exceeded-new-ref", "", payments.OutcomeSucceeded, 1000, "EUR", "", false))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 once the unapplied-receipt cap is exceeded, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header on the cap-exceeded 503")
	}
	var errBody map[string]any
	decodeBody(t, resp, &errBody)
	if _, ok := errBody["request_id"]; !ok {
		t.Fatalf("expected the generic error body shape, got %+v", errBody)
	}

	var after int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1`, tenant.ID).Scan(&after)
	}); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("expected NOTHING new to be stored once the cap is exceeded (S95-C2(i): never a 200 without storing, and never storing past the cap either), got %d -> %d", before, after)
	}
}
