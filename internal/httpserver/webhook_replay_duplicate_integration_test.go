//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// Dedicated HTTP-layer T8 coverage requested alongside the QA binding test
// plan (docs/plans/stage-10.1-planning/16-pay-wh-review-qa-test-plan.md):
// the SAME signed callback delivered twice sequentially through the real
// route produces exactly one financial effect; delivered concurrently, N
// times, still exactly one; and a genuinely tenant-A-signed callback
// replayed against tenant B's own webhook URL is rejected 401 with zero
// financial effect anywhere. internal/payments' own webhook_replay_
// duplicate_integration_test.go covers the identical properties at the
// Orchestrator level directly; this file exercises the full HTTP handler
// (tenant-slug resolution, header parsing, WithTenant wiring) end to end.
package httpserver

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// TestWebhook_HTTP_SequentialReplay_OneFinancialEffect: the same signed
// success callback POSTed twice, one after another, must credit the
// player's wallet exactly once.
func TestWebhook_HTTP_SequentialReplay_OneFinancialEffect(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 8899
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)

	for i := 0; i < 2; i++ {
		r := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("delivery %d: expected 200, got %d", i+1, r.StatusCode)
		}
		r.Body.Close()
	}

	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, providerRef); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction after 2 sequential identical deliveries, got %d", got)
	}
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != depositAmount {
		t.Fatalf("expected exactly one credit of %d, got %d", depositAmount, walletResp.CashBalance)
	}
}

// TestWebhook_HTTP_ConcurrentDuplicate_OneFinancialEffect: N goroutines
// deliver the IDENTICAL signed success callback simultaneously through the
// real HTTP route (a start barrier, never a sleep, maximizes actual
// overlap). Exactly one ledger transaction/credit must result.
func TestWebhook_HTTP_ConcurrentDuplicate_OneFinancialEffect(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 7788
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)

	const n = 8
	start := make(chan struct{})
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
			statuses[i] = r.StatusCode
			r.Body.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, code := range statuses {
		if code != http.StatusOK {
			t.Errorf("goroutine %d: expected 200 for an identical, idempotently-absorbed concurrent delivery, got %d", i, code)
		}
	}

	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, providerRef); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction after %d concurrent identical deliveries, got %d", n, got)
	}
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != depositAmount {
		t.Fatalf("expected exactly one deposit's worth (%d) after %d concurrent deliveries, got %d", depositAmount, n, walletResp.CashBalance)
	}
}

// TestWebhook_HTTP_CrossTenantReplay_401NoEffect: a genuinely tenant-A-
// signed callback replayed against tenant B's own webhook path is
// rejected 401, with zero financial effect in EITHER tenant.
func TestWebhook_HTTP_CrossTenantReplay_401NoEffect(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	mustRegisterCapability(t, pool, tenantA.ID, mockProvider)

	tenantB := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenantB)
	mustRegisterCapability(t, pool, tenantB.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brandA.Slug)
	mustActivatePlayer(t, pool, tenantA.ID, player.ID)

	const depositAmount int64 = 6677
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 initiating tenant A's deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	// A-signed payload (its InboundCallback.TenantID is set to tenantA.ID
	// inside the mock's own CallbackPayload/signingInput) - delivered to
	// TENANT B's own webhook path. The route resolves tenant B from the
	// slug and rebuilds signing_input with tenant B's id, so this must fail
	// verification even though the bytes are byte-for-byte a genuine,
	// correctly-signed (for A) callback.
	payload := mockProvider.CallbackPayload(tenantA.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)

	ledgerBBefore, auditBBefore := auditAndLedgerCount(t, pool, tenantB.ID)

	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenantB.Slug+"/mock", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a tenant-A-signed callback replayed at tenant B's path, got %d", resp.StatusCode)
	}

	ledgerBAfter, auditBAfter := auditAndLedgerCount(t, pool, tenantB.ID)
	if ledgerBAfter != ledgerBBefore {
		t.Fatalf("expected tenant B's ledger to be untouched, had %d before and %d after", ledgerBBefore, ledgerBAfter)
	}
	if auditBAfter != auditBBefore {
		t.Fatalf("expected tenant B's audit_log to be untouched, had %d before and %d after", auditBBefore, auditBAfter)
	}

	// Tenant A's own intent (the one legitimately named by this reference)
	// must ALSO remain untouched - the cross-tenant delivery must never
	// even reach A's own row, since verification ran against tenant B's
	// credential/binding, not A's.
	resp2 := getJSON(t, srv, "/v1/me/deposits/"+intent.ID, player.Tokens.AccessToken)
	var reread depositIntentResponse
	decodeBody(t, resp2, &reread)
	if reread.Status != "pending" {
		t.Fatalf("expected tenant A's own deposit to remain 'pending', got %q", reread.Status)
	}
}

func auditAndLedgerCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (ledgerCount, auditCount int) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&ledgerCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("auditAndLedgerCount: %v", err)
	}
	return
}
