package db

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgUniqueViolationCode is the Postgres SQLSTATE for unique_violation.
const pgUniqueViolationCode = "23505"

// pgForeignKeyViolationCode is the Postgres SQLSTATE for
// foreign_key_violation - used by Stage 3B handlers to turn "the caller
// supplied an asset_code the registry doesn't have" into a 400 rather
// than a generic 500, without duplicating asset-registry validation logic
// in every handler.
const pgForeignKeyViolationCode = "23503"

// IsForeignKeyViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503).
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolationCode
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505) - the race-free way to detect "this email
// is already taken" and similar cases, since it reflects a constraint
// the database itself enforced under concurrency, unlike a
// check-then-insert in application code (see CLAUDE.md's idempotency
// rules, which apply the same principle to financial writes).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolationCode
}
