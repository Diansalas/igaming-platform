//go:build integration

// PAY-H-FOLLOWUPS-1 (12) (A11; re-rated "money-path resilience" by ADR 0095 41.5 N2): the FIRST attempt's phase C in
// InitiateDepositAttempt used to run on the caller's request context. If the player's client disconnected (or the
// request deadline passed) while the provider call was in flight, phase B's result - possibly a capture, and through
// the T6 drain possibly a posting - was not recorded: the attempt stayed `submitting` with no reference until the
// sweeper or a callback recovered it. Like driveCreatedAttempt's phase C since PRH-2 H (LF F1), it now runs on a
// detached, bounded context.
package payments

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestH12_FirstAttemptPhaseC_RecordsThePhaseBResultAfterTheRequestContextIsCancelled(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-h12")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spy.onDeposit = func(int) { cancel() } // the client goes away while the provider call is in flight

	// The returned error is irrelevant (the request is gone and post-commit reads on the dead context may fail);
	// what matters is what committed.
	_, _ = orch.InitiateDepositAttempt(ctx, pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{MarketGuard: testUngatedGuard,
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "h12-first-attempt",
	})

	var attemptID [16]byte
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE tenant_id = $1 ORDER BY created_at LIMIT 1`, f.tenantID).Scan(&attemptID)
	}); err != nil {
		t.Fatalf("the attempt row must exist: %v", err)
	}
	got := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if got.State != AttemptPending || got.ProviderReference == nil {
		t.Fatalf("phase B's result must be recorded even though the request context was cancelled during the call: state=%s ref=%v", got.State, got.ProviderReference)
	}
	if d, _, _ := spy.counts(); d != 1 {
		t.Fatalf("expected exactly one Deposit, got %d", d)
	}
}
