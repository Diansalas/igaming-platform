// Package money implements ADR 0021's rounding contract and HR-22's
// *big.Int/int64 boundary discipline (docs/architecture/ledger-
// accounting-model.md §7.8, §7.14) - `ledger-finance`-owned, per that
// document's own naming of this package.
//
// This package is deliberately pure Go math with NO database dependency
// and NO import of internal/ledger or internal/assetregistry: it takes
// decimalExponent as a plain parameter (read by its caller from the Asset
// registry, invariant #8 - never a constant, never inferred from the
// asset code) and never opens a connection itself. Two consequences
// follow directly from that:
//
//   - "An unknown or inactive [rounding rule] id is an error" (§7.8) is
//     only PARTIALLY this package's job. RoundToMinorUnits rejects a nil
//     ruleID (a structural precondition this package CAN check without a
//     database), but the full check - does this id exist in the
//     rounding_rules reference table, and is it still the active version
//     for its composite rule - requires a table this dispatch does not
//     migrate (ledger-accounting-model.md §7.16: requested, not claimed;
//     left for the Orchestrator to sequence after this dispatch). A
//     caller with database access must perform that lookup itself before
//     calling RoundToMinorUnits, or trust a value it already validated.
//     This is disclosed here rather than silently narrowed.
//   - Never `float64`, at any point, anywhere in this package (invariant
//     #7, ADR 0032 §9) - every intermediate value is *big.Rat or *big.Int.
package money

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
)

// ErrInvalidAmount is returned for a structurally invalid input to
// RoundToMinorUnits (a nil exact value, a negative decimalExponent).
var ErrInvalidAmount = errors.New("money: invalid amount input")

// ErrRoundingRuleRequired is returned when ruleID is the zero UUID. See
// this package's own doc comment for what this package can and cannot
// validate about a rounding rule id.
var ErrRoundingRuleRequired = errors.New("money: rounding rule id is required")

// ErrAmountOutOfRange is HR-22's narrowing error (ledger-accounting-
// model.md §7.14): a *big.Int minor-unit amount does not fit in the
// target integer width. Returned by ToInt64 instead of ever calling
// (*big.Int).Int64() directly, which is defined to return an undefined
// value - not an error - when the receiver does not fit, and would post
// a plausible-looking wrong amount that satisfies every downstream
// constraint (finding LF-16).
var ErrAmountOutOfRange = errors.New("money: amount does not fit in the target integer width")

// RoundToMinorUnits applies ADR 0021's DS-1 (round-half-up, ties AWAY
// from zero) and DS-2 (round exactly ONCE, at the final monetary
// boundary) rule to exact, an exact pre-rounding value denominated in
// WHOLE units of the asset (e.g. 10.5 EUR, or a deposit-match
// percentage's exact result before any cap is applied), and returns the
// exact minor-unit integer result at decimalExponent's precision -
// e.g. exact=10.5, decimalExponent=2 -> 1050 (EUR minor units, i.e.
// cents); exact=10, decimalExponent=18 -> 10*10^18 (this is HR-22 test
// 29's own case, ledger-accounting-model.md §7.15).
//
// exact is *big.Rat, never float64, so the value carries no
// representation error at any point in the call chain - the codebase's
// established rule for a NUMERIC(38,0)-destined computation (see
// internal/risk/cumulative.go's numericToBigInt, which states the
// identical rule for the read side of the same boundary).
//
// The result is *big.Int, NOT int64 (finding LF-16, corrected from an
// earlier int64 signature this document itself once specified): the
// value it represents is a NUMERIC(38,0) minor-unit quantity, and at
// decimalExponent 18 an int64 saturates at ~9.2 whole units of the asset
// - far short of an ordinary grant, deposit or win. Callers that must
// hand this result to a still-int64 posting API (internal/ledger, at
// HEAD - finding LF-16b, disclosed and out of this package's scope) cross
// that width boundary with ToInt64, never with a bare (*big.Int).Int64().
//
// decimalExponent comes from the Asset registry (invariant #8) and must
// not be negative - a negative exponent has no meaning for a minor-unit
// scale and this function refuses to guess one.
//
// ruleID selects the composite rounding-rule version (the Q1
// direction and the Q2 rounding-point/residue treatment, encoded
// together per ADR 0021's "never two independently versioned axes" rule)
// and is required precisely because a historical recomputation must be
// able to re-round under the SAME rule a past transaction used, never
// today's default (see this package's own doc comment for what this
// function can and cannot validate about it).
func RoundToMinorUnits(exact *big.Rat, decimalExponent int32, ruleID uuid.UUID) (*big.Int, error) {
	if exact == nil {
		return nil, fmt.Errorf("%w: exact value must not be nil", ErrInvalidAmount)
	}
	if decimalExponent < 0 {
		return nil, fmt.Errorf("%w: decimalExponent must not be negative, got %d", ErrInvalidAmount, decimalExponent)
	}
	if ruleID == uuid.Nil {
		return nil, ErrRoundingRuleRequired
	}

	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimalExponent)), nil)
	scaled := new(big.Rat).Mul(exact, new(big.Rat).SetInt(scale))
	return roundHalfUpAwayFromZero(scaled), nil
}

// roundHalfUpAwayFromZero implements ADR 0021's DS-1 exactly: a tie (the
// fractional part is exactly 1/2) rounds AWAY from zero, so -2.5 -> -3,
// not -2 (the "round half up" a naive reading of the name might suggest,
// which is really "round half toward positive infinity" and would treat
// positive and negative values asymmetrically - not this platform's
// rule). Operates entirely in *big.Int/*big.Rat; no float64 anywhere.
func roundHalfUpAwayFromZero(r *big.Rat) *big.Int {
	num := new(big.Int).Set(r.Num())
	den := r.Denom() // big.Rat's own invariant: always > 0

	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}

	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(num, den, remainder)

	// remainder/den >= 1/2  <=>  2*remainder >= den. Rounds the tie away
	// from zero, per DS-1.
	doubledRemainder := new(big.Int).Lsh(remainder, 1)
	if doubledRemainder.Cmp(den) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}

	if neg {
		quotient.Neg(quotient)
	}
	return quotient
}

// ToInt64 is HR-22's sole, named, range-checked *big.Int -> int64
// narrowing helper (ledger-accounting-model.md §7.14): the ONE place a
// monetary *big.Int minor-unit amount may cross into internal/ledger's
// int64-based posting API. It fails closed - returning ErrAmountOutOfRange,
// never a silently wrong value - for any amount (*big.Int).Int64() would
// not represent exactly, using math/big's own IsInt64 predicate rather
// than comparing against math.MaxInt64/MinInt64 by hand.
//
// A direct call to (*big.Int).Int64() anywhere on a monetary value is a
// blocking review finding, not a style note (HR-22): that method is
// defined to return an undefined value - not an error - when the
// receiver does not fit, which would post a plausible-looking wrong
// amount that satisfies amount>0, balances per asset, and mirrors
// correctly under Rule B2 - invisible to every database constraint and to
// the B1 reconciliation sweep.
func ToInt64(amount *big.Int) (int64, error) {
	if amount == nil {
		return 0, fmt.Errorf("%w: amount is nil", ErrAmountOutOfRange)
	}
	if !amount.IsInt64() {
		return 0, fmt.Errorf("%w: %s does not fit in a signed 64-bit integer", ErrAmountOutOfRange, amount.String())
	}
	return amount.Int64(), nil
}

// FromInt64 widens an existing int64 minor-unit amount to *big.Int. This
// direction is always safe (int64's range is a strict subset of
// *big.Int's), but HR-22 requires it be an explicit, named conversion
// rather than one buried inside a larger expression - this is that single
// named conversion point.
func FromInt64(amount int64) *big.Int {
	return big.NewInt(amount)
}
