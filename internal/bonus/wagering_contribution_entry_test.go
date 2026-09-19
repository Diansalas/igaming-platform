package bonus

import (
	"errors"
	"math/big"
	"testing"
)

func TestResolveContributionWeightBP(t *testing.T) {
	cases := []struct {
		name           string
		table          []byte
		gameType       string
		providerGameID string
		want           int32
	}{
		{name: "nil table defaults to full contribution", table: nil, gameType: "slot", want: 10000},
		{name: "empty table defaults to full contribution", table: []byte(``), gameType: "slot", want: 10000},
		{name: "malformed json fails open to full contribution", table: []byte(`{not json`), gameType: "slot", want: 10000},
		{name: "explicit default used when no more specific match", table: []byte(`{"default":5000}`), gameType: "table", want: 5000},
		{name: "by_game_type match wins over default", table: []byte(`{"default":5000,"by_game_type":{"slot":10000}}`), gameType: "slot", want: 10000},
		{name: "by_provider_game_id wins over by_game_type", table: []byte(`{"by_game_type":{"slot":10000},"by_provider_game_id":{"g1":2500}}`), gameType: "slot", providerGameID: "g1", want: 2500},
		{name: "excluded_game_types wins over everything", table: []byte(`{"default":10000,"excluded_game_types":["slot"]}`), gameType: "slot", want: 0},
		{name: "excluded_provider_game_ids wins over everything else including by_provider_game_id", table: []byte(`{"by_provider_game_id":{"g1":10000},"excluded_provider_game_ids":["g1"]}`), providerGameID: "g1", want: 0},
		{name: "no match anywhere falls back to full contribution", table: []byte(`{"by_game_type":{"table":5000}}`), gameType: "slot", want: 10000},

		// DR-4HB1W3-ARCH-03: every TABLE-DERIVED weight is clamped into the
		// [0, 10000] domain migration 0058's own column CHECK defines.
		// Without the clamp each of these resolved to its raw value, which
		// CreateWageringProgress' INSERT then rejected with a raw
		// constraint error - rolling back the player's own cash bet inside
		// casino's postBet. See maxContributionWeightBP's doc comment.
		{name: "out-of-range default is clamped down, never credited", table: []byte(`{"default":100000}`), gameType: "slot", want: 10000},
		{name: "out-of-range by_game_type is clamped down", table: []byte(`{"by_game_type":{"slot":25000}}`), gameType: "slot", want: 10000},
		{name: "out-of-range by_provider_game_id is clamped down", table: []byte(`{"by_provider_game_id":{"g1":10001}}`), providerGameID: "g1", want: 10000},
		{name: "negative default is clamped to zero, not to full contribution", table: []byte(`{"default":-5000}`), gameType: "slot", want: 0},
		{name: "negative by_game_type is clamped to zero", table: []byte(`{"by_game_type":{"slot":-1}}`), gameType: "slot", want: 0},
		{name: "in-range boundary values are untouched", table: []byte(`{"by_game_type":{"slot":10000}}`), gameType: "slot", want: 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveContributionWeightBP(c.table, c.gameType, c.providerGameID)
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestComputeQualifyingScaled(t *testing.T) {
	cases := []struct {
		name   string
		staked *big.Int
		weight int32
		want   *big.Int
	}{
		{name: "full weight passes through unchanged", staked: big.NewInt(1000), weight: 10000, want: big.NewInt(1000)},
		{name: "half weight halves the contribution", staked: big.NewInt(1000), weight: 5000, want: big.NewInt(500)},
		{name: "zero weight contributes nothing", staked: big.NewInt(1000), weight: 0, want: big.NewInt(0)},
		{name: "nil staked amount contributes nothing", staked: nil, weight: 10000, want: big.NewInt(0)},
		{name: "negative staked amount contributes nothing (defensive)", staked: big.NewInt(-5), weight: 10000, want: big.NewInt(0)},
		{name: "floors rather than rounds up (conservative direction)", staked: big.NewInt(3), weight: 3333, want: big.NewInt(0)}, // 3*3333/10000 = 0.9999 -> floor 0
		{name: "multiplier above 100% is honored (a 'boost' weight, not just a discount)", staked: big.NewInt(1000), weight: 20000, want: big.NewInt(2000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeQualifyingScaled(c.staked, c.weight)
			if got.Cmp(c.want) != 0 {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestWageringTargetScaled(t *testing.T) {
	mult35x := int32(350000)
	t.Run("nil multiplier means no wagering axis - nil target", func(t *testing.T) {
		got, err := WageringTargetScaled(OfferVersion{}, big.NewInt(1000))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil target for a nil WageringMultiplierBP, got %s", got)
		}
	})
	// DR-4HB1W3-RISK-02 regression: before this fix the nil target this
	// case produced was read by CheckAndCompleteGrant/ConvertGrant as
	// "already satisfied", silently completing a Grant that had wagered
	// nothing. An unmeasurable target must fail closed, never resolve to
	// ALLOW.
	t.Run("configured multiplier with no granted amount fails closed", func(t *testing.T) {
		got, err := WageringTargetScaled(OfferVersion{WageringMultiplierBP: &mult35x}, nil)
		if !errors.Is(err, ErrWageringTargetUnmeasurable) {
			t.Fatalf("expected ErrWageringTargetUnmeasurable, got target=%v err=%v", got, err)
		}
		if got != nil {
			t.Errorf("expected no target alongside the error, got %s", got)
		}
	})
	t.Run("computes multiplier * granted amount / 10000", func(t *testing.T) {
		got, err := WageringTargetScaled(OfferVersion{WageringMultiplierBP: &mult35x}, big.NewInt(1000))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := big.NewInt(35000) // 1000 * 35x
		if got.Cmp(want) != 0 {
			t.Errorf("got %s, want %s", got, want)
		}
	})
}
