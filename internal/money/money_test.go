package money

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/google/uuid"
)

func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("could not parse %q as a big.Rat", s)
	}
	return r
}

func mustBigInt(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("could not parse %q as a big.Int", s)
	}
	return n
}

// TestRoundToMinorUnits_TableDriven is ledger-accounting-model.md §7.15's
// rounding-width test set, generalized into a table across exponents 0,
// 2, 6, 8 and 18 (the property-testing discipline §6.5.8 item 9/§7.15
// item 27 establish elsewhere in this codebase for exponent-independence
// claims: proved by execution, not asserted). Every expected value is a
// *big.Int literal parsed from its decimal string - never derived from an
// int64 expression - so the test cannot silently reintroduce the width
// defect it exists to prevent (finding LF-16).
func TestRoundToMinorUnits_TableDriven(t *testing.T) {
	ruleID := uuid.New()

	cases := []struct {
		name            string
		exact           string
		decimalExponent int32
		want            string
	}{
		// Exponent 0: no minor units at all - the whole-unit value IS the
		// minor-unit value, and rounding still applies to any fractional
		// input.
		{"exp0_exact_whole", "7", 0, "7"},
		{"exp0_tie_positive_rounds_away_from_zero", "7.5", 0, "8"},
		{"exp0_tie_negative_rounds_away_from_zero", "-7.5", 0, "-8"},
		{"exp0_below_half_rounds_down_in_magnitude", "7.49", 0, "7"},
		{"exp0_above_half_rounds_up_in_magnitude", "7.51", 0, "8"},
		{"exp0_zero", "0", 0, "0"},

		// Exponent 2: the ordinary fiat case (EUR/USD cents).
		{"exp2_exact_cents", "10.50", 2, "1050"},
		{"exp2_tie_at_third_decimal_rounds_away_from_zero", "10.505", 2, "1051"},
		{"exp2_negative_tie", "-10.505", 2, "-1051"},
		{"exp2_below_half", "10.504", 2, "1050"},
		{"exp2_above_half", "10.506", 2, "1051"},

		// Exponent 6 (e.g. a 6-decimal stablecoin representation).
		{"exp6_exact", "1.000001", 6, "1000001"},
		{"exp6_tie_rounds_away_from_zero", "1.0000005", 6, "1000001"},
		{"exp6_negative_tie", "-1.0000005", 6, "-1000001"},

		// Exponent 8 (BTC).
		{"exp8_exact", "0.00000001", 8, "1"},
		{"exp8_tie_rounds_away_from_zero", "0.000000015", 8, "2"},
		{"exp8_negative_tie", "-0.000000015", 8, "-2"},
		{"exp8_zero", "0", 8, "0"},

		// Exponent 18 - the widest crypto case, and the one finding LF-16
		// exists because of. "10 whole units" is §7.15 test 29's own
		// example: the exact value handed in is 10 units, decimalExponent
		// 18, and the result must be the EXACT big.Int for 10*10^18, with
		// no truncation - a case int64 cannot represent (int64's maximum
		// is ~9.2 whole units at this exponent).
		{"exp18_ten_whole_units_exact_no_truncation", "10", 18, "10000000000000000000"},
		{"exp18_tie_rounds_away_from_zero", "0.0000000000000000015", 18, "2"},
		{"exp18_negative_tie", "-0.0000000000000000015", 18, "-2"},
		{"exp18_below_half", "0.0000000000000000014", 18, "1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RoundToMinorUnits(mustRat(t, c.exact), c.decimalExponent, ruleID)
			if err != nil {
				t.Fatalf("RoundToMinorUnits(%s, %d): unexpected error: %v", c.exact, c.decimalExponent, err)
			}
			want := mustBigInt(t, c.want)
			if got.Cmp(want) != 0 {
				t.Fatalf("RoundToMinorUnits(%s, %d) = %s, want %s", c.exact, c.decimalExponent, got.String(), want.String())
			}
		})
	}
}

// TestRoundToMinorUnits_ExponentIndependenceOfShape proves the SAME exact
// fractional relationship (a value ending in exactly .5 of a minor unit)
// rounds away from zero identically at every exponent - the arithmetic
// SHAPE is exponent-agnostic even though the represented magnitude is
// not (ledger-accounting-model.md §7.9's own distinction, finding LF-16b).
func TestRoundToMinorUnits_ExponentIndependenceOfShape(t *testing.T) {
	ruleID := uuid.New()
	for _, exp := range []int32{0, 2, 6, 8, 18} {
		// scale = 10^exp; construct exact = 3 / (2*scale), i.e. exactly
		// "1.5 minor units" worth of whole-unit value at THIS exponent -
		// exact*scale = 1.5 for every exponent, so the tie-breaking
		// behavior being tested (rounds away from zero to 2, never down
		// to 1) is compared like-for-like across all five exponents
		// rather than drifting with scale the way an earlier version of
		// this test accidentally did.
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
		denom := new(big.Int).Mul(scale, big.NewInt(2))
		exact := new(big.Rat).SetFrac(big.NewInt(3), denom)

		got, err := RoundToMinorUnits(exact, exp, ruleID)
		if err != nil {
			t.Fatalf("exponent %d: unexpected error: %v", exp, err)
		}
		if got.Cmp(big.NewInt(2)) != 0 {
			t.Fatalf("exponent %d: expected the .5 tie to round away from zero to 2, got %s", exp, got.String())
		}

		negExact := new(big.Rat).Neg(exact)
		gotNeg, err := RoundToMinorUnits(negExact, exp, ruleID)
		if err != nil {
			t.Fatalf("exponent %d (negative): unexpected error: %v", exp, err)
		}
		if gotNeg.Cmp(big.NewInt(-2)) != 0 {
			t.Fatalf("exponent %d (negative): expected the .5 tie to round away from zero to -2, got %s", exp, gotNeg.String())
		}
	}
}

func TestRoundToMinorUnits_ValidationErrors(t *testing.T) {
	ruleID := uuid.New()

	if _, err := RoundToMinorUnits(nil, 2, ruleID); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("nil exact: expected ErrInvalidAmount, got %v", err)
	}
	if _, err := RoundToMinorUnits(mustRat(t, "1.00"), -1, ruleID); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative decimalExponent: expected ErrInvalidAmount, got %v", err)
	}
	if _, err := RoundToMinorUnits(mustRat(t, "1.00"), 2, uuid.Nil); !errors.Is(err, ErrRoundingRuleRequired) {
		t.Fatalf("nil rule id: expected ErrRoundingRuleRequired, got %v", err)
	}
}

// TestToInt64_RangeChecked is HR-22's own test (ledger-accounting-
// model.md §7.15 items 29-30): a value at exactly math.MaxInt64/
// math.MinInt64 narrows successfully; one unit beyond either boundary is
// an ERROR, never a silently wrapped/truncated value - the property a
// bare (*big.Int).Int64() call does not have (that method's own
// documented behavior is to return an undefined value on overflow, not
// an error).
func TestToInt64_RangeChecked(t *testing.T) {
	cases := []struct {
		name    string
		amount  *big.Int
		want    int64
		wantErr bool
	}{
		{"zero", big.NewInt(0), 0, false},
		{"ordinary_positive", big.NewInt(1_000_000), 1_000_000, false},
		{"ordinary_negative", big.NewInt(-42), -42, false},
		{"exactly_max_int64", big.NewInt(math.MaxInt64), math.MaxInt64, false},
		{"exactly_min_int64", big.NewInt(math.MinInt64), math.MinInt64, false},
		{"max_int64_plus_one_overflows", new(big.Int).Add(big.NewInt(math.MaxInt64), big.NewInt(1)), 0, true},
		{"min_int64_minus_one_underflows", new(big.Int).Sub(big.NewInt(math.MinInt64), big.NewInt(1)), 0, true},
		{
			"exponent_18_ten_whole_units_overflows",
			// The exact case §7.15 test 30 names: RoundToMinorUnits at
			// decimalExponent 18 on 10 whole units produces a *big.Int far
			// beyond int64's range, and narrowing it MUST fail rather than
			// silently wrap.
			mustBigIntNoT("10000000000000000000"),
			0,
			true,
		},
		{"nil_amount", nil, 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ToInt64(c.amount)
			if c.wantErr {
				if !errors.Is(err, ErrAmountOutOfRange) {
					t.Fatalf("expected ErrAmountOutOfRange, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

func mustBigIntNoT(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad literal: " + s)
	}
	return n
}

// TestFromInt64_RoundTripsThroughToInt64 proves the explicit int64 ->
// *big.Int -> int64 round trip is lossless across representative values,
// including both extremes of int64's range (HR-22's "always safe, but
// must be explicit" reverse-direction rule).
func TestFromInt64_RoundTripsThroughToInt64(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 1_000_000, -1_000_000, math.MaxInt64, math.MinInt64} {
		widened := FromInt64(v)
		narrowed, err := ToInt64(widened)
		if err != nil {
			t.Fatalf("value %d: unexpected error narrowing back: %v", v, err)
		}
		if narrowed != v {
			t.Fatalf("value %d: round trip produced %d", v, narrowed)
		}
	}
}
