//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// QA binding test plan (docs/plans/stage-10.1-planning/
// 16-pay-wh-review-qa-test-plan.md) T13, plus design §3/§6's
// "TestSimulationRoute_CannotNameOtherTenant": the simulate-callback
// route's tenant binding comes from tc.TenantID (the authenticated JWT's
// own resolved tenant) ONLY - no request field can change which tenant the
// callback is signed and verified for, and the response body never leaks
// the raw signed bytes/signature.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestWebhook_SimulationRoute_TenantFromJWTOnly is QA plan T13: an
// authenticated player simulating their OWN deposit gets 200, and the
// response body exposes no signature or raw callback bytes - only the
// (deposit_intent_id, status, tombstoned) triple newSimulateDepositCallbackHandler
// documents as its whole response shape.
func TestWebhook_SimulationRoute_TenantFromJWTOnly(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 4444, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)

	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != http.StatusOK {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, body.Message)
	}

	var raw map[string]any
	decodeBody(t, resp, &raw)
	allowed := map[string]bool{"deposit_intent_id": true, "status": true, "tombstoned": true}
	for k := range raw {
		if !allowed[k] {
			t.Errorf("simulate-callback response carries an unexpected field %q - only deposit_intent_id/status/tombstoned are permitted (no signature/raw callback bytes)", k)
		}
	}
	for _, forbidden := range []string{"signature", "body", "header", "raw", "secret", "key"} {
		if _, present := raw[forbidden]; present {
			t.Errorf("simulate-callback response must never expose a %q field", forbidden)
		}
	}

	// The credited ledger transaction's provider_tx_id must be the
	// intent's OWN provider_reference (assigned server-side by
	// InitiateDeposit/attemptDeposit) - proving the signer was actually
	// invoked with the tenant/reference the platform itself resolved, not
	// anything from the request.
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, providerRef); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction under the JWT-resolved tenant for provider ref %q, got %d", providerRef, got)
	}
}

// TestSimulationRoute_CannotNameOtherTenant is design §3/§6's own required
// case: a request body naming a DIFFERENT, real tenant must not cause the
// callback to be signed/verified for that other tenant - the signer only
// ever receives tc.TenantID. If tenant binding leaked through the body,
// this would either settle the caller's OWN intent under the FOREIGN
// tenant id (a cross-tenant financial write - S-6 reopened at this seam)
// or fail signature verification as a byproduct; either way, this test
// proves it does NOT actually redirect the effect to tenantB.
func TestSimulationRoute_CannotNameOtherTenant(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	mustRegisterCapability(t, pool, tenantA.ID, mockProvider)

	tenantB := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenantB)
	mustRegisterCapability(t, pool, tenantB.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brandA.Slug)
	mustActivatePlayer(t, pool, tenantA.ID, player.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 5555, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	ledgerBBefore, auditBBefore := countLedgerAndAuditForTenant(t, pool, tenantB.ID)

	// The request body names tenant B by every field a naive implementation
	// might read: tenant_id AND a nested tenant/brand slug-shaped field.
	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{
		"tenant_id":   tenantB.ID.String(),
		"tenant_slug": tenantB.Slug,
	})
	if resp.StatusCode != http.StatusOK {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200 (the caller's OWN intent, under tenant A, still settles), got %d (%s)", resp.StatusCode, body.Message)
	}
	resp.Body.Close()

	// Settled under tenant A - the JWT-resolved tenant - not tenant B.
	if got := ledgerTransactionCountForProviderRef(t, pool, tenantA.ID, providerRef); got != 1 {
		t.Fatalf("expected the deposit to settle under tenant A (the JWT tenant), got %d ledger rows there", got)
	}

	// Tenant B is completely untouched: no ledger rows, no audit rows, and
	// (since the provider_reference was only ever minted under tenant A)
	// certainly no deposit_intents row of its own for this reference.
	ledgerBAfter, auditBAfter := countLedgerAndAuditForTenant(t, pool, tenantB.ID)
	if ledgerBAfter != ledgerBBefore {
		t.Fatalf("expected tenant B's ledger to be untouched, had %d before and %d after", ledgerBBefore, ledgerBAfter)
	}
	if auditBAfter != auditBBefore {
		t.Fatalf("expected tenant B's audit_log to be untouched, had %d before and %d after", auditBBefore, auditBAfter)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenantB.ID, providerRef); got != 0 {
		t.Fatalf("expected zero ledger rows under tenant B for the reference, got %d", got)
	}
}

// countLedgerAndAuditForTenant mirrors internal/payments'
// countLedgerAndAudit, local to this package.
func countLedgerAndAuditForTenant(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (ledgerCount, auditCount int) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&ledgerCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("countLedgerAndAuditForTenant: %v", err)
	}
	return
}
