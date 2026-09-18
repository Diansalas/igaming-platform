package bonus

import (
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"
)

// numericToBigInt mirrors internal/risk/cumulative.go's own helper of
// the same name - this codebase's established, per-package convention
// for crossing the pgtype.Numeric -> *big.Int boundary. NUMERIC(38,0)
// columns are never scanned into int64/float64 (CLAUDE.md).
//
// A POSITIVE Exp is normal and must be handled, NOT rejected: PostgreSQL
// returns a NUMERIC value in whatever scale it likes, so an exact integer
// of 1000 can arrive as Int=1, Exp=3 - internal/risk/cumulative.go's own
// doc comment names this exactly, having once gotten it backwards the
// same way and turned it into a fail-closed availability defect on any
// round number. A NEGATIVE Exp IS refused: a fractional count of minor
// units cannot exist in a NUMERIC(38,0) schema, and truncating or
// rounding one would be exactly the silent money-mangling CLAUDE.md's
// no-floating-point rule exists to prevent.
func numericToBigInt(n pgtype.Numeric) (*big.Int, error) {
	if !n.Valid || n.Int == nil {
		return nil, nil
	}
	if n.NaN || n.InfinityModifier != pgtype.Finite {
		return nil, fmt.Errorf("bonus: NUMERIC value is not a finite number")
	}
	if n.Exp < 0 {
		return nil, fmt.Errorf("bonus: refusing a fractional NUMERIC minor-unit value (exponent %d)", n.Exp)
	}
	if n.Exp == 0 {
		return n.Int, nil
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)
	return new(big.Int).Mul(n.Int, scale), nil
}

func bigIntToNumeric(v *big.Int) pgtype.Numeric {
	if v == nil {
		return pgtype.Numeric{Valid: false}
	}
	return pgtype.Numeric{Int: new(big.Int).Set(v), Exp: 0, Valid: true}
}

func nonNilJSON(b []byte) []byte {
	if b == nil {
		return []byte("{}")
	}
	return b
}

// nonNilStrings coerces a nil []string to an empty, non-nil slice. A nil
// slice bound as a TEXT[] parameter is sent as SQL NULL (not the
// column's own '{}' DEFAULT, which only applies when the column is
// omitted from the INSERT's column list entirely) - every TEXT[] column
// in this package's schema is NOT NULL, so a caller-supplied nil Go
// slice must be normalized before binding, not left to the database
// default to catch.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
