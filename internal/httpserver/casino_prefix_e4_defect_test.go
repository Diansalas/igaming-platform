//go:build integration

// Stage 10.2 pre-fix evidence; retired or inverted by the fix commit.
//
// CAS-WH-TENANT-1 (design doc §0, §H E4):
// TestCasWH_PreFix_CrossTenantCallbackSucceeds reproduces, against
// CURRENT, UNMODIFIED production code, the structural finding that "the
// URL alone binds the tenant": tenants A and B both have the mock-casino
// capability enabled, sharing the ONE per-process
// MockCasinoProvider/signingSecret instance (internal/casino/mock.go's
// own doc comment: "One per-process crypto/rand key serves all
// tenants" - never a real credential, and never derived from or
// tied to either tenant id). A rollback of a provider_tx_id NEVER SEEN
// by either tenant, validly signed by that one shared instance, is
// posted to tenant B's webhook URL and succeeds - producing a tombstone
// in B's own ledger for an "original" transaction B never had anything
// to do with. Nothing about the signed bytes themselves names, or is
// bound to, any tenant at all; only the URL path segment picks the
// tenant.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
)

func TestCasWH_PreFix_CrossTenantCallbackSucceeds(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	// Both tenants have mock-casino enabled, going through the exact same
	// admin HTTP path a real operator would use for each - and, because
	// newMockCasinoOrchestrator/newCasinoTestServer register exactly ONE
	// process-global mock.MockCasinoProvider instance for BOTH tenants
	// (mirroring cmd/platform-api/main.go's own single, shared adapter
	// registration - there is no per-tenant adapter instance anywhere in
	// this codebase today), both capabilities are backed by the identical
	// signingSecret.
	mustEnableCasinoCapability(t, srv, pool, tenantA)
	mustEnableCasinoCapability(t, srv, pool, tenantB)

	// A rollback for an original provider_tx_id NEITHER tenant has ever
	// seen (no bet/win was ever posted for it anywhere) - the exact "F-7
	// tombstone" shape design §C4/§H expects. Signed by the one shared
	// mock instance; nothing in the signed bytes names a tenant.
	rollbackProviderTxID := "cas-e4-rollback-" + uuid.NewString()
	originalProviderTxID := "cas-e4-unseen-original-" + uuid.NewString()
	payload := mock.CallbackPayload(casino.CallbackEventRollback,
		rollbackProviderTxID, originalProviderTxID, "", "", 0, "EUR",
		casino.OutcomeSucceeded, "", uuid.New(), uuid.Nil)

	// Posted to tenant B's slug - never tenant A's.
	resp := rawPostJSON(t, srv, "/v1/webhooks/casino/"+tenantB.Slug+"/mock-casino", payload)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PRE-FIX EVIDENCE: expected the cross-tenant rollback callback to succeed with 200 (design §H E4), got %d", resp.StatusCode)
	}
	var body struct {
		Outcome    string `json:"outcome"`
		Tombstoned bool   `json:"tombstoned"`
	}
	decodeBody(t, resp, &body)
	if !body.Tombstoned {
		t.Fatalf("PRE-FIX EVIDENCE: expected tombstoned:true in the response, got %+v", body)
	}
	t.Logf("PRE-FIX EVIDENCE: cross-tenant rollback callback (delivered to tenant B, %s) succeeded: %+v", tenantB.Slug, body)

	// The defect's key assertion: the tombstone was written under TENANT
	// B, not tenant A - proving the URL alone (not any content of the
	// signed callback) decided which tenant's ledger absorbed it.
	var tombstoneCount int
	err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions
			 WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2 AND transaction_type = 'tombstone'`,
			tenantB.ID, originalProviderTxID).Scan(&tombstoneCount)
	})
	if err != nil {
		t.Fatalf("failed to query tenant B's ledger_transactions: %v", err)
	}
	if tombstoneCount != 1 {
		t.Fatalf("PRE-FIX EVIDENCE: expected exactly one tombstone row in tenant B's own ledger for provider_tx_id %q, got %d",
			originalProviderTxID, tombstoneCount)
	}
	t.Logf("PRE-FIX EVIDENCE: tombstone confirmed present in tenant B's ledger (tenant_id=%s) for original_provider_tx_id=%s",
		tenantB.ID, originalProviderTxID)

	// Sanity: tenant A's own scope must show none of this (it was never
	// tenant A's callback to begin with - this just confirms the write
	// really landed under B, not merely "somewhere").
	var crossCount int
	err = pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = $1`,
			originalProviderTxID).Scan(&crossCount)
	})
	if err != nil {
		t.Fatalf("failed to query tenant A's ledger_transactions: %v", err)
	}
	if crossCount != 0 {
		t.Fatalf("expected zero rows visible under tenant A's own RLS scope for tenant B's tombstone, got %d", crossCount)
	}
}
