//go:build integration

package payments

import (
	"context"
	"testing"
)

// FP-1 (code review): the deposit decision row records the EVALUATION, not
// the claim. A deposit that passes KYC and is then declined for no routable
// provider leaves exactly one allow decision row and one audit row, and moves
// no money. This pins the documented behaviour (ADR 0096 §24.2).
func TestDepositGate_AllowThenNoRoutableProvider_LeavesOneRowAndMovesNoMoney(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool) // no capability registered: routing fails
	orch := NewOrchestrator(map[string]PaymentProvider{}, MultiWebhookCredentialResolver{})
	ledgerBefore := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1`, f.tenantID)

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, KYCEnforcementDepositGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "fpay-dep-noroute",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected the intent declined (no routable provider), got %s", res.Intent.Status)
	}
	if n := fpCount(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND player_account_id=$2 AND operation='deposit' AND allowed`,
		f.tenantID, f.playerAccountID); n != 1 {
		t.Fatalf("expected exactly 1 allow decision row (the evaluation), got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID,
		`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='kyc.enforcement_allowed' AND target_id=$2`,
		f.tenantID, f.playerAccountID.String()); n != 1 {
		t.Fatalf("expected exactly 1 decision audit row, got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM payment_attempts WHERE tenant_id=$1`, f.tenantID); n != 0 {
		t.Fatalf("expected no attempt row, got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1`, f.tenantID); n != ledgerBefore {
		t.Fatalf("expected no ledger posting, %d -> %d", ledgerBefore, n)
	}
}
