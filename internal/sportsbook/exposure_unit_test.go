// Pure-function coverage for exposure.go that needs no database - runs
// under the default (non-integration) build, mirroring
// internal/risk/cumulative_sportsbook_test.go's identical split between
// DB-free unit coverage and `-tags integration` DB coverage.
package sportsbook

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestNumericToBigInt_RefusesFractionalExponent is part of ADR 0083
// §12.2 item 32 ("a NUMERIC outside the scannable range... aborts the
// bet"): a negative NUMERIC exponent (a fractional count of minor units)
// must be refused, never truncated or rounded (CLAUDE.md's no-floating-
// point-for-money rule, extended here to a NUMERIC that itself cannot
// represent a whole minor-unit count).
func TestNumericToBigInt_RefusesFractionalExponent(t *testing.T) {
	n := pgtype.Numeric{Valid: true, Int: big.NewInt(12345), Exp: -2}
	if _, err := numericToBigInt(n); err == nil {
		t.Fatal("expected numericToBigInt to refuse a negative-exponent NUMERIC")
	}
}

// TestNumericToBigInt_ScalesAPositiveExponentRatherThanTruncating proves
// the companion, non-obvious case §6.2.3's own comment calls out: a
// positive Exp is NORMAL (PostgreSQL may return an exact integer total
// normalized to a non-zero scale) and must be handled by scaling UP,
// never rejected outright - the pre-Stage-4H-B0-R6 version of risk's own
// identical helper rejected this and was itself a defect (see
// internal/risk/cumulative.go's numericToBigInt doc comment).
func TestNumericToBigInt_ScalesAPositiveExponentRatherThanTruncating(t *testing.T) {
	n := pgtype.Numeric{Valid: true, Int: big.NewInt(1), Exp: 3}
	got, err := numericToBigInt(n)
	if err != nil {
		t.Fatalf("numericToBigInt: %v", err)
	}
	if got.Int64() != 1000 {
		t.Fatalf("expected 1 * 10^3 = 1000, got %s", got.String())
	}
}

func TestNumericToBigInt_InvalidIsZero(t *testing.T) {
	got, err := numericToBigInt(pgtype.Numeric{Valid: false})
	if err != nil {
		t.Fatalf("numericToBigInt: %v", err)
	}
	if got.Sign() != 0 {
		t.Fatalf("expected zero for an invalid/NULL NUMERIC, got %s", got.String())
	}
}
