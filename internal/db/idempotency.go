package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// IdempotentInsert runs insert inside a SAVEPOINT nested within tx, then
// reports whether insert failed with a unique-constraint violation
// (SQLSTATE 23505) - the expected outcome of a retried request or a
// concurrent duplicate racing against another insert of the same
// idempotency key/provider reference (see docs/decisions/0020).
//
// Postgres implementation note (the reason this function exists at all):
// once ANY statement in a transaction errors - including a unique
// violation from an idempotent INSERT - the whole transaction is aborted
// and every subsequent statement is rejected until ROLLBACK. "Catch the
// error and look up the original result in the same outer transaction"
// is therefore not achievable without first isolating the risky INSERT
// in its own SAVEPOINT. pgx.Tx.Begin, called on an already-open pgx.Tx,
// issues exactly that SAVEPOINT and gives back a nested pgx.Tx whose
// Rollback issues "ROLLBACK TO SAVEPOINT" (undoing only the failed
// insert, not the outer transaction's already-held locks/GUC settings)
// and whose Commit issues "RELEASE SAVEPOINT".
//
// On conflict==true, insert's own effects are fully undone and the outer
// transaction is otherwise untouched - the caller is expected to look up
// and return the original, already-committed result using tx (the OUTER
// transaction, not the now-rolled-back savepoint). On conflict==false
// and err==nil, insert's effects are committed into the outer
// transaction as normal.
func IdempotentInsert(ctx context.Context, tx pgx.Tx, insert func(pgx.Tx) error) (conflict bool, err error) {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("db: open savepoint: %w", err)
	}

	if insertErr := insert(savepoint); insertErr != nil {
		_ = savepoint.Rollback(ctx)
		if IsUniqueViolation(insertErr) {
			return true, nil
		}
		return false, insertErr
	}

	if err := savepoint.Commit(ctx); err != nil {
		return false, fmt.Errorf("db: release savepoint: %w", err)
	}
	return false, nil
}
