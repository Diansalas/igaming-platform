//go:build integration

// Stage 10.1 PAY-REV-1 (ADR 0090) HTTP-layer tests #3 and #10 (docs/plans/
// stage-10.1-planning-gate-proposal.md §J): a distinct-reference reversal
// callback naming an already-reversed deposit gets a generic 409, an
// integrity alert whose fields are restricted to provider_id/tenant_id/
// request_id, and a `deposit.reversal_rejected` audit record that
// COMMITTED even though the operation's own transaction rolled back -
// mirroring TestSettlementSimulate_IntegrityBackstop_409AndSeparateAuditCommit's
// own "read the denial from a fresh transaction" proof exactly.
package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func TestPaymentWebhookHandler_DepositReversalAlreadyReversed_Maps409(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	logger, capturedLines := newCapturingLogger()
	srv := httptest.NewServer(New(Deps{
		Logger: logger, DB: pool, AuthIssuer: issuer, ServiceName: "platform-api-test",
		AccessTokenTTL: 5 * time.Minute, RefreshTokenTTL: time.Hour,
		PaymentOrchestrator: orchestrator, PaymentsOutboundCredentials: payments.MockCredentialResolver{}, PersonResolver: identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-payrev1-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-payrev1-pw-1")
	capResp := capabilityPutRequest(t, srv, "mock", tenantAdminTokens.AccessToken, validCapabilityBody())
	if capResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 configuring the mock provider's capability, got %d", capResp.StatusCode)
	}
	capResp.Body.Close()

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 9_000
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": "payrev1-http-dep",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := strings.TrimPrefix(intent.RedirectURL, "https://mock-psp.invalid/pay/")

	successPayload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", successPayload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 confirming the deposit, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// First reversal: genuine, must succeed.
	firstReversal := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal,
		"payrev1-http-rev-1", providerRef, payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", firstReversal)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the first (genuine) reversal, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Second reversal: a DISTINCT provider reference naming the SAME
	// already-reversed original. Must be rejected, not posted.
	secondReversal := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal,
		"payrev1-http-rev-2", providerRef, payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", secondReversal)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for the second, distinct-reference reversal, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	// Generic body: never echoes the provider reference, amount or any
	// account/ledger id.
	if body.Message != "callback rejected" {
		t.Fatalf("expected the generic body %q, got %q", "callback rejected", body.Message)
	}
	// The integrity alert (security re-verification P2-3): exactly one line,
	// keys limited to the allow-list, and no value carrying the rejected
	// reference, the deposit's provider reference or the amount. The typed
	// DepositAlreadyReversedError's own message contains the PSP reference,
	// so an "error" attribute added to this line would leak it - this
	// assertion is what catches that.
	allowedAlertKeys := map[string]bool{"provider_id": true, "tenant_id": true, "request_id": true}
	var alerts []capturedLogLine
	for _, line := range capturedLines() {
		if line.msg == "payment_webhook_integrity_alert_deposit_already_reversed" {
			alerts = append(alerts, line)
		}
	}
	if len(alerts) != 1 {
		t.Fatalf("expected exactly 1 deposit_already_reversed alert line, got %d", len(alerts))
	}
	for key, value := range alerts[0].attrs {
		if !allowedAlertKeys[key] {
			t.Fatalf("alert line carries non-allow-listed key %q", key)
		}
		rendered := fmt.Sprint(value)
		for _, leaked := range []string{providerRef, "payrev1-http-rev-1", "payrev1-http-rev-2", "9000"} {
			if strings.Contains(rendered, leaked) {
				t.Fatalf("alert attribute %q leaks %q", key, leaked)
			}
		}
	}
	for _, leaked := range []string{providerRef, "payrev1-http-rev-1", "payrev1-http-rev-2", "9000", "9_000"} {
		if strings.Contains(body.Message, leaked) {
			t.Fatalf("the denial body must never echo %q, got %q", leaked, body.Message)
		}
	}

	// ADR 0102 8 row 10 (failure-path P1, detached): the same condition is also a
	// durable payment.webhook_integrity alert, with only server-side ids in its
	// discriminator and attributes limited to provider_id/request_id.
	durable := alertinject.Find(alertinject.ForSubject(t, pool, tenant.ID), string(alerting.KindPaymentWebhookIntegrity))
	if len(durable) != 1 || durable[0].Discriminator != "provider:mock:reason:deposit_already_reversed" || durable[0].Severity != "p1" {
		t.Fatalf("expected one durable deposit_already_reversed P1, got %+v", durable)
	}
	for k, v := range durable[0].Attributes {
		if k != "provider_id" && k != "request_id" {
			t.Fatalf("durable alert carries non-allow-listed attribute %q", k)
		}
		for _, leaked := range []string{providerRef, "payrev1-http-rev-1", "payrev1-http-rev-2", "9000"} {
			if strings.Contains(fmt.Sprint(v), leaked) {
				t.Fatalf("durable alert attribute %q leaks %q", k, leaked)
			}
		}
	}

	// Nothing extra posted: exactly the one genuine reversal exists.
	var reversalCount int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit_reversal'`,
			tenant.ID).Scan(&reversalCount)
	}); err != nil {
		t.Fatalf("count deposit_reversal rows: %v", err)
	}
	if reversalCount != 1 {
		t.Fatalf("expected exactly 1 deposit_reversal transaction, got %d", reversalCount)
	}

	// The one genuine reversal's own ledger transaction id, read
	// independently of the audit row, so the assertions below prove the
	// audit's existing_reversal_ledger_transaction_id names the REAL row
	// rather than merely being present.
	var firstReversalTxID, depositTxID string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT id::text FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit_reversal'`,
			tenant.ID).Scan(&firstReversalTxID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT id::text FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`,
			tenant.ID).Scan(&depositTxID)
	}); err != nil {
		t.Fatalf("read the genuine reversal/deposit transaction ids: %v", err)
	}

	// The denial audit DID commit, in a transaction separate from the
	// failed one (which rolled back) - read from a genuinely fresh
	// transaction, exactly like the sportsbook settlement integrity
	// backstop's own audit test.
	//
	// ledger-finance P2-B / security P2-2 / code review F2 (Stage 10.1
	// post-implementation review): the audit record must now name the
	// ORIGINAL deposit_intent (never a bare provider_id) and carry enough
	// detail for PSP reconciliation - asserted here from THIS fresh
	// transaction, not merely constructed and discarded in-process.
	var auditCount int
	var outcome, targetType, targetID, ipAddress string
	var metadataProviderID, metadataDepositIntentID, metadataOriginalTxID, metadataRejectedRef, metadataExistingReversalTxID string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'deposit.reversal_rejected'`,
			tenant.ID).Scan(&auditCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT outcome, target_type, target_id, host(ip_address),
			        metadata->>'provider_id', metadata->>'deposit_intent_id',
			        metadata->>'original_ledger_transaction_id', metadata->>'rejected_reversal_reference',
			        metadata->>'existing_reversal_ledger_transaction_id'
			   FROM audit_log
			  WHERE tenant_id = $1 AND action = 'deposit.reversal_rejected'
			  ORDER BY created_at DESC LIMIT 1`,
			tenant.ID).Scan(&outcome, &targetType, &targetID, &ipAddress,
			&metadataProviderID, &metadataDepositIntentID, &metadataOriginalTxID, &metadataRejectedRef, &metadataExistingReversalTxID)
	}); err != nil {
		t.Fatalf("read denial audit row: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 committed deposit.reversal_rejected audit row, got %d", auditCount)
	}
	if outcome != "denied" {
		t.Fatalf("denial audit outcome = %q, want denied", outcome)
	}
	if metadataProviderID != "mock" {
		t.Fatalf("denial audit metadata.provider_id = %q, want mock", metadataProviderID)
	}
	if targetType != "deposit_intent" {
		t.Fatalf("denial audit target_type = %q, want deposit_intent", targetType)
	}
	if targetID != intent.ID {
		t.Fatalf("denial audit target_id = %q, want the original deposit_intent id %q", targetID, intent.ID)
	}
	if metadataDepositIntentID != intent.ID {
		t.Fatalf("denial audit metadata.deposit_intent_id = %q, want %q", metadataDepositIntentID, intent.ID)
	}
	if metadataOriginalTxID != depositTxID {
		t.Fatalf("denial audit metadata.original_ledger_transaction_id = %q, want the original deposit's own transaction id %q", metadataOriginalTxID, depositTxID)
	}
	if metadataRejectedRef != "payrev1-http-rev-2" {
		t.Fatalf("denial audit metadata.rejected_reversal_reference = %q, want the REJECTED reversal's own reference %q", metadataRejectedRef, "payrev1-http-rev-2")
	}
	if metadataExistingReversalTxID != firstReversalTxID {
		t.Fatalf("denial audit metadata.existing_reversal_ledger_transaction_id = %q, want the genuinely posted first reversal's id %q", metadataExistingReversalTxID, firstReversalTxID)
	}
	if ipAddress == "" {
		t.Fatal("denial audit ip_address must be populated (CLAUDE.md's mutating/denial audit rule)")
	}
}
