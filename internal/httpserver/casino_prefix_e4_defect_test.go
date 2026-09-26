//go:build integration

// Stage 10.2 CAS-WH-TENANT-1 fix verification (design §H, C1: "E4
// re-run"). This file used to reproduce, against pre-fix code, the
// structural finding that "the URL alone binds the tenant" (see the
// retired pre-fix evidence, captured once at commit d76bdd3 and stored
// under docs/plans/stage-10.2-planning/evidence/E4-casino-cross-tenant-
// tombstone.txt - that evidence file is UNCHANGED by this inversion).
//
// TestCasinoWebhook_CrossTenantRollback_Rejected proves the fix: a
// rollback of a provider_tx_id NEVER SEEN by either tenant, signed for
// tenant A's own per-tenant derived key, delivered to tenant B's webhook
// URL, is now rejected with 401 BEFORE any tenant-scoped read - no
// tombstone is written in EITHER tenant's ledger, and the full no-effect
// checklist holds for both.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
)

func TestCasinoWebhook_CrossTenantRollback_Rejected(t *testing.T) {
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
	// registration), both capabilities are backed by the same per-process
	// master secret - but Stage 10.2 derives a DISTINCT per-(tenant,
	// provider) signing key from it (webhookauth.DeriveMockKey), so a
	// signature minted for tenant A no longer verifies for tenant B.
	mustEnableCasinoCapability(t, srv, pool, tenantA)
	mustEnableCasinoCapability(t, srv, pool, tenantB)

	// A rollback for an original provider_tx_id NEITHER tenant has ever
	// seen (no bet/win was ever posted for it anywhere) - the exact "F-7
	// tombstone" shape design §C4/§H expects if it were ever accepted.
	// Signed for tenant A.
	rollbackProviderTxID := "cas-e4-rollback-" + uuid.NewString()
	originalProviderTxID := "cas-e4-unseen-original-" + uuid.NewString()
	payload := mock.CallbackPayload(tenantA.ID, casino.CallbackEventRollback,
		rollbackProviderTxID, originalProviderTxID, "", "", 0, "EUR",
		casino.OutcomeSucceeded, "", uuid.New(), uuid.Nil)

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenantA.ID, tenantB.ID})

	// Posted to tenant B's slug - never tenant A's. The signing input
	// includes tenant A's own tenant_id (deriveKey/SigningInput), so this
	// can never verify for tenant B, regardless of the URL.
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenantB.Slug+"/mock-casino", payload)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the cross-tenant rollback callback to be rejected with 401 (design §C1/C4/C5), got %d", resp.StatusCode)
	}
	errBody := decodeAPIError(t, resp)
	if errBody.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", errBody.Message)
	}

	// No-effect checklist (design §H): neither tenant has a tombstone, a
	// ledger_transactions row, or an audit_log row for this callback.
	for _, tn := range []struct {
		name string
		id   uuid.UUID
	}{{"A", tenantA.ID}, {"B", tenantB.ID}} {
		var ledgerCount int
		err := pool.WithTenant(context.Background(), tn.id, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id IN ($1, $2)`,
				rollbackProviderTxID, originalProviderTxID).Scan(&ledgerCount)
		})
		if err != nil {
			t.Fatalf("query tenant %s's ledger_transactions: %v", tn.name, err)
		}
		if ledgerCount != 0 {
			t.Fatalf("expected zero ledger_transactions rows (including tombstones) under tenant %s, got %d", tn.name, ledgerCount)
		}

		var auditCount int
		err = pool.WithTenant(context.Background(), tn.id, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND metadata::text LIKE '%'||$2||'%'`,
				tn.id, rollbackProviderTxID).Scan(&auditCount)
		})
		if err != nil {
			t.Fatalf("query tenant %s's audit_log: %v", tn.name, err)
		}
		if auditCount != 0 {
			t.Fatalf("expected zero audit_log rows naming this callback under tenant %s, got %d", tn.name, auditCount)
		}
	}

	// Full six-point checklist (both tenants), on top of the targeted
	// ledger/audit checks above.
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenantA.ID, tenantB.ID}, before)
}
