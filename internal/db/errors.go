package db

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgUniqueViolationCode is the Postgres SQLSTATE for unique_violation.
const pgUniqueViolationCode = "23505"

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
