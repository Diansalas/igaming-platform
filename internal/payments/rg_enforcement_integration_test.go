//go:build integration

// Stage 9 §14 (identity-compliance): internal/payments.InitiateDeposit
// began consulting internal/rg.EvaluateEligibility this stage (see the
// "Stage 9 production-readiness fix" comment on InitiateDeposit itself) -
// before this fix, a platform-wide self-excluded player could fund a
// wallet indefinitely even though the SAME person could never launch a
// game or place a bet with the resulting balance (internal/casino has
// consulted this exact boundary since Stage 4D-RG). This file is this
// fix's own regression test, mirroring internal/casino's own
// rg_enforcement_integration_test.go fixture/assertion conventions
// exactly (denial code, NO ledger effect, an audit record) so the two
// enforcement points are held to the identical bar.
package payments

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

func depositAuditActionExists(t *testing.T, pool *db.Pool, f orchFixture, action string) bool {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`,
			f.tenantID, action,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query audit_log for action %q: %v", action, err)
	}
	return count > 0
}

func depositAuditReasonCode(t *testing.T, pool *db.Pool, f orchFixture, action string) string {
	t.Helper()
	var reasonCode string
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'reason_code' FROM audit_log
			  WHERE tenant_id = $1 AND action = $2
			  ORDER BY created_at DESC LIMIT 1`,
			f.tenantID, action,
		).Scan(&reasonCode)
	})
	if err != nil {
		t.Fatalf("read %s audit metadata: %v", action, err)
	}
	return reasonCode
}

// TestInitiateDeposit_DeniedWhenSelfExcludedPlatformWide_NoLedgerEffect
// proves the fail-closed property this regression test exists for: a
// platform-wide self-excluded player's InitiateDeposit call is DECLINED
// with rg.CodeSelfExcluded, never reaches RouteProvider/provider.Deposit
// (asserted indirectly: no provider_id/provider_reference is ever
// attempted), posts NOTHING to the ledger (cash balance stays exactly 0,
// zero ledger_transactions rows for the tenant), and both the underlying
// deposit request and the RG denial itself are independently audited -
// the same "enforcement and the log are built together" bar CLAUDE.md
// requires for every compliance-relevant action.
func TestInitiateDeposit_DeniedWhenSelfExcludedPlatformWide_NoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
		return err
	})
	if err != nil {
		t.Fatalf("self-exclude: %v", err)
	}

	var intent DepositIntent
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "dep-rg-self-excluded",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}

	// --- Fail-closed: declined, never routed to a provider ---
	if intent.Status != DepositIntentDeclined {
		t.Fatalf("expected DepositIntentDeclined, got %v", intent.Status)
	}
	if intent.ProviderID != nil || intent.ProviderReference != nil {
		t.Fatalf("expected the self-excluded deposit to NEVER reach RouteProvider/provider.Deposit, got provider_id=%v provider_reference=%v", intent.ProviderID, intent.ProviderReference)
	}
	if intent.LedgerTransactionID != nil {
		t.Fatalf("expected no ledger transaction to be attached to a declined intent, got %v", *intent.LedgerTransactionID)
	}

	// The persisted row must agree with the returned struct, not merely
	// the in-memory value - in case a future change short-circuits the
	// database write on this path.
	var persisted DepositIntent
	var persistedFound bool
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		persisted, persistedFound, err = loadDepositIntentByIdempotencyKey(ctx, tx, f.tenantID, f.playerAccountID, "dep-rg-self-excluded")
		return err
	})
	if err != nil {
		t.Fatalf("reload persisted intent: %v", err)
	}
	if !persistedFound {
		t.Fatal("expected the declined intent to still be persisted")
	}
	if persisted.Status != DepositIntentDeclined {
		t.Fatalf("expected the PERSISTED intent status to be declined, got %v", persisted.Status)
	}

	// --- No ledger effect at all ---
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("expected cash balance UNCHANGED at 0 (a denied RG check must post nothing), got %d", balance)
	}
	var ledgerCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&ledgerCount)
	})
	if err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	if ledgerCount != 0 {
		t.Fatalf("expected ZERO ledger_transactions rows for a self-excluded player's declined deposit, got %d", ledgerCount)
	}

	// --- Audited: both the underlying request and the RG denial itself ---
	if !depositAuditActionExists(t, pool, f, "deposit.requested") {
		t.Fatal("expected a deposit.requested audit record even though the deposit was ultimately denied")
	}
	if !depositAuditActionExists(t, pool, f, "payments.deposit_denied_by_rg") {
		t.Fatal("expected a payments.deposit_denied_by_rg audit record")
	}
	if !depositAuditActionExists(t, pool, f, "deposit.declined") {
		t.Fatal("expected a deposit.declined audit record (finalizeDeclined's own audit write)")
	}

	if reasonCode := depositAuditReasonCode(t, pool, f, "payments.deposit_denied_by_rg"); reasonCode != rg.CodeSelfExcluded {
		t.Fatalf("expected audited reason_code %q, got %q", rg.CodeSelfExcluded, reasonCode)
	}
}

// TestInitiateDeposit_DeniedWhenPlayerAccountSuspended_NoLedgerEffect
// covers the SAME EvaluateEligibility boundary's other pre-existing
// signal (player-account status), proving InitiateDeposit's new RG check
// is not narrowly wired to self-exclusion alone.
func TestInitiateDeposit_DeniedWhenPlayerAccountSuspended_NoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE player_accounts SET status = 'suspended' WHERE id = $1`, f.playerAccountID)
		return err
	})
	if err != nil {
		t.Fatalf("suspend account: %v", err)
	}

	var intent DepositIntent
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 2500, PaymentMethod: "card", IdempotencyKey: "dep-rg-suspended",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.Status != DepositIntentDeclined {
		t.Fatalf("expected DepositIntentDeclined, got %v", intent.Status)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("expected cash balance UNCHANGED at 0, got %d", balance)
	}
	if reasonCode := depositAuditReasonCode(t, pool, f, "payments.deposit_denied_by_rg"); reasonCode != rg.CodePlayerAccountNotActive {
		t.Fatalf("expected audited reason_code %q, got %q", rg.CodePlayerAccountNotActive, reasonCode)
	}
}
