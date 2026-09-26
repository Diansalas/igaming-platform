//go:build integration

// Stage 10.1 PAY-REV-1 (ADR 0090) test #6 (docs/plans/
// stage-10.1-planning-gate-proposal.md §J): the migration 0092 unique
// index is a BACKSTOP against a writer that skips internal/payments' own
// L2 lock, never the primary control. This file proves the backstop
// fires correctly through TWO different callers - ledger.Post itself
// (typed ErrReversalAlreadyExists) and a raw SQL INSERT that bypasses
// Post entirely - since a caller that skipped the lock could just as
// easily be a future poster that also doesn't go through Post's own
// prepareEntries/validation path.
package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func depositReversalInput(f fixture, idempotencyKey string, reverses uuid.UUID, amount int64) TransactionInput {
	provider, ref := "mockpsp", idempotencyKey
	return TransactionInput{
		TenantID: f.tenantID, TransactionType: TxDepositReversal, IdempotencyKey: idempotencyKey,
		ProviderID: &provider, ProviderTxID: &ref, CorrelationID: uuid.NewSHA1(f.tenantID, []byte(idempotencyKey)),
		ReversesTransactionID: &reverses,
		Entries: []EntryInput{
			{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: amount},
			{LedgerAccountID: f.clearingID, Direction: Credit, Amount: amount},
		},
	}
}

// TestPayRev1_UniqueIndex_BackstopsBypassOfLock is test #6: a second,
// DISTINCT-idempotency-key reversal naming an already-reversed original
// gets ErrReversalAlreadyExists from Post - not the misleading "conflict
// but idempotency-key lookup found no row" a bare IsUniqueViolation
// treatment would produce (ledger-finance review P2-1) - and a raw SQL
// INSERT making the identical mistake is refused by the database itself
// with the same named constraint, independent of any Go code at all.
func TestPayRev1_UniqueIndex_BackstopsBypassOfLock(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	dep := mustPost(t, pool, f, depositInput(f, "payrev1-backstop-dep", 5_000))

	first := mustPost(t, pool, f, depositReversalInput(f, "payrev1-backstop-rev-1", dep.TransactionID, 5_000))
	if first.AlreadyPosted {
		t.Fatal("first reversal must not be AlreadyPosted")
	}

	// Through Post: a DISTINCT idempotency key (so this is not an ordinary
	// replay at all - it looks, to Post's own idempotency-key index, like
	// a brand new transaction) naming the SAME already-reversed original.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Post(ctx, tx, depositReversalInput(f, "payrev1-backstop-rev-2", dep.TransactionID, 5_000))
		return err
	})
	if !errors.Is(err, ErrReversalAlreadyExists) {
		t.Fatalf("want ErrReversalAlreadyExists through Post, got %v", err)
	}
	if errors.Is(err, ErrIdempotencyKeyReused) || errors.Is(err, ErrIdempotencyPayloadMismatch) {
		t.Fatalf("must not also match either idempotency-key sentinel: %v", err)
	}

	// Raw SQL, entirely bypassing Post/prepareEntries/the L3 pre-lock -
	// modelling a hypothetical future writer that skips both the
	// application lock AND Post itself. The database's own constraint is
	// the only thing that can still catch this.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, reverses_transaction_id)
			 VALUES ($1, $2, 'deposit_reversal', $3, $4, $5)`,
			uuid.New(), f.tenantID, "payrev1-backstop-raw-sql", uuid.New(), dep.TransactionID)
		return execErr
	})
	if !db.IsUniqueViolation(err) {
		t.Fatalf("expected a unique violation for the raw-SQL second reversal, got %v", err)
	}
	name, ok := db.UniqueViolationConstraintName(err)
	if !ok || name != "ledger_transactions_one_deposit_reversal" {
		t.Fatalf("expected the violation to name ledger_transactions_one_deposit_reversal, got %q (ok=%v)", name, ok)
	}

	// Nothing extra posted by either rejected attempt.
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND reverses_transaction_id = $2 AND transaction_type = 'deposit_reversal'`,
			f.tenantID, dep.TransactionID).Scan(&n)
	}); err != nil {
		t.Fatalf("count reversals: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 posted deposit_reversal for this original, got %d", n)
	}
}
