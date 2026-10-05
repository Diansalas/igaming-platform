package kyc

// PRH-2 E1 (ADR 0106 section 2.8, security M37): only a DETERMINISTIC apply
// failure (SQLSTATE class 22 data exception or 23 integrity constraint
// violation) ends a row failed_terminal / apply_conflict. Class 40
// (serialization failure, deadlock) is transient and must stay retryable, or a
// deadlock between two workers would strand a valid submission.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsApplyConflictError_OnlyClass22And23_M37(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unique violation 23505", pg("23505"), true},
		{"check violation 23514", pg("23514"), true},
		{"foreign key violation 23503", pg("23503"), true},
		{"numeric out of range 22003", pg("22003"), true},
		{"invalid text representation 22P02", pg("22P02"), true},
		{"wrapped class 23", fmt.Errorf("apply: %w", pg("23505")), true},
		{"serialization failure 40001", pg("40001"), false},
		{"deadlock detected 40P01", pg("40P01"), false},
		{"wrapped class 40", fmt.Errorf("apply: %w", pg("40P01")), false},
		{"connection failure 08006", pg("08006"), false},
		{"query canceled 57014", pg("57014"), false},
		{"insufficient privilege 42501", pg("42501"), false},
		{"a non-database error", errors.New("boom"), false},
		{"nil", nil, false},
		{"empty code", pg(""), false},
	}
	for _, c := range cases {
		if got := isApplyConflictError(c.err); got != c.want {
			t.Errorf("%s: isApplyConflictError = %v, want %v", c.name, got, c.want)
		}
	}
}
