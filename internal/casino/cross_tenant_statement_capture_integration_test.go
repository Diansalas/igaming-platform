//go:build integration

package casino

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TestCasinoWebhook_CrossTenantSignature_NoStatementBeforeVerification
// completes C7 for the cross-tenant shape security's SC-1 names (Stage
// 10.2 final review, 09-review-security-final.md): a callback genuinely
// signed by the mock for tenant A, delivered on tenant B's route and
// under B's RLS context, must be rejected as signature_invalid with ZERO
// statements run on B's transaction - B's capability row, rounds and
// ledger are never read. Both tenants have the provider enabled, so the
// only thing that can reject it is the tenant bound into the MAC.
func TestCasinoWebhook_CrossTenantSignature_NoStatementBeforeVerification(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, fA, provider, 100)
	registerCasinoCapability(t, pool, fB, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	signedForA := provider.CallbackPayload(fA.tenantID, CallbackEventBet, "cas-c7-cross-tenant", "", "round-c7-xt", "game-1",
		1000, "EUR", OutcomeSucceeded, "", fA.playerAccountID, uuid.New())

	var captured *recordingTx
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		captured = newRecordingTx(tx)
		_, err := orch.receiveCallbackInTx(ctx, captured, fB.tenantID, "mock-casino", signedForA)
		return err
	})
	assertZeroStatementsBeforeVerification(t, captured, err, webhookauth.ReasonSignatureInvalid)
}
