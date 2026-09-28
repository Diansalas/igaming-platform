package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RunReadOnlyInSavepoint runs fn (expected to issue reads only - no
// writes) inside a SAVEPOINT nested within tx (the same pgx.Tx.Begin-on-
// an-open-Tx mechanism IdempotentInsert uses, see idempotency.go's own
// doc comment for the underlying Postgres reasoning), and UNCONDITIONALLY
// rolls the savepoint back afterwards - regardless of whether fn
// succeeded, returned a business-level "could not determine" result, or
// hit a genuine Postgres error.
//
// This exists so a caller whose read logic swallows a genuine DB error
// into a typed "unavailable" result (as internal/kyc.EvaluateEnforcement
// does, by design, per ADR 0096 §2.6(c): "the query failed" must never be
// confused with "no rows found") does not leave the OUTER transaction
// aborted as a side effect. Once any statement inside a Postgres
// transaction errors, the whole transaction (or, if nested in a
// SAVEPOINT, only that savepoint) is aborted and rejects every further
// statement until a ROLLBACK - so a query failure three calls deep inside
// fn would otherwise poison every later statement the OUTER caller tries
// to run in the same transaction (ADR 0096 KYC-ENF-OUTAGE-1 / code review
// finding N1: this reproduced as a 25P02 "current transaction is aborted"
// when the withdrawal handler then tried to record the decision it had
// just computed, in the very transaction the failed read had just
// poisoned).
//
// Because fn is documented to be read-only, rolling back its savepoint
// unconditionally is always safe: on success there is nothing to lose by
// discarding it (no writes occurred), and on failure it is the ONLY way
// to recover the connection to a usable state without discarding
// anything the OUTER transaction itself already holds (locks, GUC
// settings, prior writes). The outer transaction is therefore always left
// exactly as healthy as it was before RunReadOnlyInSavepoint was called,
// whatever fn did.
//
// The error fn returns (if any) is passed through unchanged after the
// rollback completes, so callers keep their existing error-handling
// contract; RunReadOnlyInSavepoint's own housekeeping errors (failing to
// open or roll back the savepoint itself - both exceedingly rare, and
// themselves a sign the outer transaction/connection is already unusable)
// are folded into that returned error rather than silently dropped.
func RunReadOnlyInSavepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	savepoint, openErr := tx.Begin(ctx)
	if openErr != nil {
		return fmt.Errorf("db: open read-only savepoint: %w", openErr)
	}

	fnErr := fn(savepoint)

	if rollbackErr := savepoint.Rollback(ctx); rollbackErr != nil {
		if fnErr != nil {
			return fmt.Errorf("db: read-only savepoint rollback failed (%v) after fn error: %w", rollbackErr, fnErr)
		}
		return fmt.Errorf("db: roll back read-only savepoint: %w", rollbackErr)
	}

	return fnErr
}
