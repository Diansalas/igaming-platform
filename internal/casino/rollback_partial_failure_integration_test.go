//go:build integration

// Stage 10.3 gate 10.3-W1 ledger-finance condition C5: "failure injected
// between tombstone and audit -> full rollback, then one tombstone on
// redelivery". postRollback's unseen-original path writes the tombstone
// (postRollbackTombstone, ledger.Post) and THEN writes its own audit row
// (audit.Record) as two separate statements inside the SAME caller-
// supplied transaction (see ReceiveCallback's own doc comment: this
// package never opens or commits a transaction of its own). Postgres
// therefore makes "half of this function's writes" structurally
// unreachable - it can only ever commit ALL of postRollback's writes for
// this delivery, or NONE of them - so a failure landing at ANY point
// inside that transaction, including the exact point between the
// tombstone write and the audit write, is provably equivalent to a
// failure landing right there. This mirrors
// TestFailureModeMatrix_A_PlatformAbortsAfterProviderAccepted_NoPartial-
// LedgerWrite's own established technique for the identical class of claim
// on the bet path.
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPostRollback_C5_FailureBetweenTombstoneAndAudit_FullRollbackThenOneTombstoneOnRedelivery(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const originalTxID = "cas-c5-partial-original"
	const rollbackTxID = "cas-c5-partial-rollback"
	payload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, rollbackTxID, originalTxID, "round-c5-partial", "game-1",
		0, "EUR", "", "", f.playerAccountID, uuid.Nil)

	// Simulate a platform failure landing SOMEWHERE inside this delivery's
	// transaction (the tombstone write has already happened by the time
	// ReceiveCallback returns - the audit write happens immediately after
	// it, inside the SAME transaction) - forcing the whole transaction to
	// roll back, exactly as if the failure had landed between those two
	// specific statements.
	errBoom := errors.New("simulated platform failure between the tombstone write and its audit record")
	var tombstoned bool
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, callErr := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		if callErr != nil {
			return callErr
		}
		tombstoned = result.Tombstoned
		if !tombstoned {
			return errors.New("fixture precondition failed: the rollback did not tombstone before the simulated crash")
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected the simulated platform failure to propagate, got %v", err)
	}

	// Full rollback: NOTHING from this delivery survived - not the
	// tombstone, not its audit row.
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", originalTxID); count != 0 {
		t.Fatalf("a rolled-back delivery must leave ZERO ledger_transactions rows for the original reference, got %d", count)
	}
	var auditCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino_rollback.tombstoned'
			   AND metadata->>'original_provider_tx_id' = $2`, f.tenantID, originalTxID).Scan(&auditCount)
	}); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("expected zero casino_rollback.tombstoned audit rows after the rollback, got %d", auditCount)
	}

	// Redelivery: the provider retries the identical rollback - it must
	// tombstone exactly once, cleanly, with its own audit row this time.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, callErr := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		if callErr != nil {
			return callErr
		}
		if !result.Tombstoned {
			return errors.New("expected the redelivery to tombstone")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("redelivery after the simulated partial failure: %v", err)
	}
	if count := fmCountLedgerTx(t, pool, f.tenantID, "mock-casino", originalTxID); count != 1 {
		t.Fatalf("expected exactly 1 tombstone after the redelivery, got %d", count)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino_rollback.tombstoned'
			   AND metadata->>'original_provider_tx_id' = $2`, f.tenantID, originalTxID).Scan(&auditCount)
	}); err != nil {
		t.Fatalf("query audit_log after redelivery: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 casino_rollback.tombstoned audit row after the redelivery, got %d", auditCount)
	}
}
