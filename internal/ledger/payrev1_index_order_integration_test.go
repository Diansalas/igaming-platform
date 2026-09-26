//go:build integration

// Stage 10.1 ledger-finance review, P2-A (docs/governance/
// stage-10.1-ledger-finance-signoff.md): ledger.Post used to classify a
// same-key deposit_reversal conflict by trusting
// pgconn.PgError.ConstraintName directly. That name is whichever unique
// index Postgres happens to check FIRST when an INSERT violates more than
// one at once, and Postgres checks in INDEX-OID order - not a fixed,
// semantically meaningful order. A same-key retry of a deposit_reversal
// violates BOTH the ordinary (tenant_id, idempotency_key) constraint AND
// migration 0092's (tenant_id, reverses_transaction_id) partial index
// simultaneously. As shipped, the idempotency-key index happens to be
// OLDER (lower OID), so it is reported first and the retry correctly takes
// the AlreadyPosted/payload-comparison replay path. An ordinary
// `REINDEX INDEX CONCURRENTLY` against bloat, or any migration that
// recreates the idempotency-key constraint, gives it a NEW, higher OID -
// which flips which index Postgres reports first, and used to make a
// perfectly legitimate retry return ErrReversalAlreadyExists (-> a false
// HTTP 409 to the PSP, a false integrity alert, and a false denial audit
// row, even though nothing double-posted).
//
// This test forces that flip directly, on a throwaway scratch database
// (never the shared TEST_DATABASE_URL database, which nothing here may
// schema-mutate): it drops and recreates the idempotency-key constraint so
// its backing index becomes strictly NEWER (higher OID) than both
// migration 0092's index and idx_ledger_transactions_tenant_provider_tx,
// then proves ledger.Post is now index-order-independent - Post always
// looks up the idempotency key FIRST, regardless of which constraint name
// Postgres reports, and only returns ErrReversalAlreadyExists when NO row
// exists for that key at all.
package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// flipIdempotencyKeyConstraintOrder drops and recreates
// ledger_transactions_tenant_idempotency_key_key, giving its backing
// unique index a brand-new (and therefore strictly higher) OID than every
// index already on the table, including migration 0092's
// ledger_transactions_one_deposit_reversal - reproducing exactly what an
// ordinary REINDEX INDEX CONCURRENTLY does in production (ledger-finance's
// own probe 2 in the Stage 10.1 review). Runs as the scratch database's
// unprivileged owner (the same NOBYPASSRLS role every migration already
// ran as to create the constraint in the first place) - no superuser, no
// BYPASSRLS, nothing this test does needs either.
func flipIdempotencyKeyConstraintOrder(t *testing.T, pool *db.Pool) {
	t.Helper()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_tenant_idempotency_key_key`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_tenant_idempotency_key_key UNIQUE (tenant_id, idempotency_key)`); err != nil {
			return err
		}
		// A same-key reversal retry ALSO violates the provider-tx index
		// (migration 0021), which would otherwise still be older than the
		// 0092 index and be reported first - masking the defect this test
		// exists to catch (ledger-finance re-verification, 2026-09-26).
		// Recreate it too, so the 0092 index is the OLDEST of the three.
		if _, err := tx.Exec(ctx, `DROP INDEX idx_ledger_transactions_tenant_provider_tx`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`CREATE UNIQUE INDEX idx_ledger_transactions_tenant_provider_tx
			     ON ledger_transactions (tenant_id, provider_id, provider_tx_id)
			  WHERE provider_id IS NOT NULL`)
		return err
	})
	if err != nil {
		t.Fatalf("flip idempotency-key constraint order: %v", err)
	}
}

// TestPost_ReversalRetry_IndexOrderIndependent is ledger-finance's required
// P2-A test: with the idempotency-key index deliberately made NEWER than
// the 0092 index (so Postgres now reports
// ledger_transactions_one_deposit_reversal FIRST for a same-key conflict -
// the exact flip that used to misclassify a legitimate retry), a same-key
// replay of a posted reversal must still be an idempotent AlreadyPosted,
// and a same-key/different-payload replay must still be
// ErrIdempotencyPayloadMismatch - NEVER ErrReversalAlreadyExists, which
// would falsely tell a caller "distinct, second reversal" for a request
// that is, in fact, a retry of the very same one.
func TestPost_ReversalRetry_IndexOrderIndependent(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)
	dir := migrationsDir(t)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up (full chain, including 0092): %v", err)
	}

	f := seedFixture(t, pool)
	dep := mustPost(t, pool, f, depositInput(f, "flip-dep", 7_000))
	first := mustPost(t, pool, f, depositReversalInput(f, "flip-rev-1", dep.TransactionID, 7_000))
	if first.AlreadyPosted {
		t.Fatal("first reversal must not be AlreadyPosted")
	}

	flipIdempotencyKeyConstraintOrder(t, pool)

	// Confirm the flip actually took effect: a bare raw-SQL duplicate
	// idempotency key (which violates ONLY the idempotency-key index, not
	// 0092's) must still report that constraint - proving the constraint
	// still functions as a uniqueness guard after being recreated, and
	// isolating this test from a silent no-op ALTER.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id)
			 VALUES ($1, $2, 'deposit', $3, $4)`,
			uuid.New(), f.tenantID, "flip-dep", uuid.New())
		return err
	})
	if !db.IsUniqueViolation(err) {
		t.Fatalf("expected the recreated idempotency-key constraint to still refuse a bare duplicate, got %v", err)
	}
	if name, ok := db.UniqueViolationConstraintName(err); !ok || name != "ledger_transactions_tenant_idempotency_key_key" {
		t.Fatalf("expected the bare duplicate to name the idempotency-key constraint, got %q (ok=%v)", name, ok)
	}

	// Confirm the order that matters: a raw row duplicating the posted
	// reversal on ALL THREE unique indexes (idempotency key, provider tx,
	// one-reversal-per-deposit) must now be reported as the 0092
	// constraint. Without this, the test could pass against the pre-fix
	// Post because another index is reported first.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions
			     (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id, reverses_transaction_id)
			 SELECT $1, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, $2, reverses_transaction_id
			   FROM ledger_transactions WHERE id = $3`,
			uuid.New(), uuid.New(), first.TransactionID)
		return err
	})
	if name, ok := db.UniqueViolationConstraintName(err); !ok || name != "ledger_transactions_one_deposit_reversal" {
		t.Fatalf("after the flip, a full duplicate of the reversal must be reported as the 0092 constraint first, got %q (ok=%v, err=%v)", name, ok, err)
	}

	// Same key, same payload: an idempotent retry of the already-posted
	// reversal. Before the P2-A fix, this same scenario (idempotency index
	// now newer than 0092's, so Postgres reports the 0092 constraint
	// first) returned ErrReversalAlreadyExists here instead.
	replay := mustPost(t, pool, f, depositReversalInput(f, "flip-rev-1", dep.TransactionID, 7_000))
	if !replay.AlreadyPosted {
		t.Fatal("same-key/same-payload replay must be AlreadyPosted")
	}
	if replay.TransactionID != first.TransactionID {
		t.Fatalf("replay must return the ORIGINAL transaction id %s, got %s", first.TransactionID, replay.TransactionID)
	}

	// Same key, DIFFERENT payload (a different amount): must still be
	// ErrIdempotencyPayloadMismatch, never ErrReversalAlreadyExists and
	// never a silent AlreadyPosted.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Post(ctx, tx, depositReversalInput(f, "flip-rev-1", dep.TransactionID, 1))
		return err
	})
	if !errors.Is(err, ErrIdempotencyPayloadMismatch) {
		t.Fatalf("want ErrIdempotencyPayloadMismatch for a same-key/different-amount replay, got %v", err)
	}
	if errors.Is(err, ErrReversalAlreadyExists) {
		t.Fatalf("must NOT also match ErrReversalAlreadyExists: %v", err)
	}

	// A genuinely DISTINCT reversal (its own idempotency key) naming the
	// same already-reversed original must still be refused as a real
	// second reversal - the index-order fix must not have swallowed the
	// backstop itself, only stopped it from misfiring on retries.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Post(ctx, tx, depositReversalInput(f, "flip-rev-2-distinct", dep.TransactionID, 7_000))
		return err
	})
	if !errors.Is(err, ErrReversalAlreadyExists) {
		t.Fatalf("want ErrReversalAlreadyExists for a genuinely distinct second reversal, got %v", err)
	}

	// Exactly one deposit_reversal ever actually posted for this original.
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
