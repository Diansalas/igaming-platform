//go:build integration

// Stage 10.1 PAY-REV-1 (ADR 0090) test #11 (docs/plans/
// stage-10.1-planning-gate-proposal.md §J): the S2 lock query's own
// tenant_id predicate, and RLS underneath it, mean a reversal callback can
// never lock or even OBSERVE another tenant's original deposit - not even
// when a corrupted deposit_intents row names a foreign tenant's
// ledger_transactions id (deposit_intents.ledger_transaction_id is a
// single-column FK with no tenant_id component, exactly like
// reverses_transaction_id - security review S-1's point, tested here for
// the S2 read side rather than the write side migration 0092 covers).
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPayRev1_TenantIsolation_CannotLockOrObserveAnotherTenantsOriginal is
// test #11.
func TestPayRev1_TenantIsolation_CannotLockOrObserveAnotherTenantsOriginal(t *testing.T) {
	pool := testPool(t)
	tenantA := seedOrchFixture(t, pool)
	tenantB := seedOrchFixture(t, pool)
	providerA := NewMockProvider("mock-psp", "EUR")
	providerB := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, tenantA, providerA, 100)
	registerCapability(t, pool, tenantB, providerB, 100)
	orchA := NewOrchestrator(map[string]PaymentProvider{"mock-psp": providerA}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(providerA)})
	orchB := NewOrchestrator(map[string]PaymentProvider{"mock-psp": providerB}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(providerB)})

	// Tenant B: a genuine, confirmed deposit - the target of the attempted
	// cross-tenant reach.
	const amountB = int64(3_000)
	_, ledgerTxIDB := pr1ConfirmedDeposit(t, pool, tenantB, orchB, providerB, amountB)

	// Tenant A: its own deposit intent (with a matching payment_attempts
	// row - PRH-payments-callback-cutover, ADR 0095 §5.4/LF95-C6(b): the
	// reversal's S2 lock now reads the ATTEMPT's own ledger_transaction_id,
	// never the intent's, so THIS is the column the corruption must target
	// to exercise the same tenant-isolation property against the new
	// resolution mechanism), then corrupted (directly, bypassing the
	// application entirely - modelling a data-integrity fault, not a
	// legitimate code path) to point at TENANT B's ledger transaction.
	const amountA = int64(1_000)
	var intent DepositIntent
	err := pool.WithTenant(context.Background(), tenantA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orchA, InitiateDepositParams{
			Scope:     DepositScope{TenantID: tenantA.tenantID, BrandID: tenantA.brandID, PlayerAccountID: tenantA.playerAccountID, WalletID: tenantA.walletID},
			AssetCode: "EUR", Amount: amountA, PaymentMethod: "card", IdempotencyKey: "payrev1-tenant-iso-dep",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit (tenant A): %v", err)
	}
	depositRefA := *intent.ProviderReference

	corruptErr := pool.WithTenant(context.Background(), tenantA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE deposit_intents SET status = 'succeeded', ledger_transaction_id = $1 WHERE id = $2 AND tenant_id = $3`,
			ledgerTxIDB, intent.ID, tenantA.tenantID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE payment_attempts SET ledger_transaction_id = $1 WHERE deposit_intent_id = $2 AND tenant_id = $3`,
			ledgerTxIDB, intent.ID, tenantA.tenantID)
		return err
	})
	if corruptErr != nil {
		t.Fatalf("corrupt tenant A's deposit intent/attempt to point at tenant B's ledger transaction: %v", corruptErr)
	}

	// Deliver, under TENANT A's own scope, a reversal callback naming
	// tenant A's own deposit reference - which now (only through the
	// corrupted row) resolves to tenant B's ledger transaction id.
	reversalPayload := providerA.CallbackPayload(tenantA.tenantID, CallbackEventDepositReversal,
		"payrev1-tenant-iso-rev", depositRefA, OutcomeSucceeded, amountA, "EUR", "", false)
	err = pool.WithTenant(context.Background(), tenantA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orchA.receiveCallbackInTx(ctx, tx, tenantA.tenantID, "mock-psp", reversalPayload)
		return err
	})

	if !errors.Is(err, ErrDepositReversalIntegrity) {
		t.Fatalf("expected ErrDepositReversalIntegrity (the S2 lock query, scoped to tenant A, must see ZERO rows for "+
			"tenant B's id - both via its own tenant_id predicate and via RLS), got %v", err)
	}

	// Tenant B's deposit is completely untouched: still exactly one
	// ledger transaction (the original deposit), no reversal, balance
	// unaffected.
	if got := cashBalance(t, pool, tenantB); got != amountB {
		t.Fatalf("tenant B's player_cash must be untouched by tenant A's corrupted reversal attempt: got %d, want %d", got, amountB)
	}
	var reversalCountB int
	if err := pool.WithTenant(context.Background(), tenantB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit_reversal'`,
			tenantB.tenantID).Scan(&reversalCountB)
	}); err != nil {
		t.Fatalf("count tenant B reversals: %v", err)
	}
	if reversalCountB != 0 {
		t.Fatalf("tenant B must have 0 deposit_reversal transactions, got %d", reversalCountB)
	}
}
