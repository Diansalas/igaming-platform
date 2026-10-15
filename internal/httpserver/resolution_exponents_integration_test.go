//go:build integration

package httpserver

import (
	"context"
	"testing"
)

// F2 (A10b): the lookup reads the real asset registry (ADR 0007) and fails closed on an unknown asset.
func TestAssetExponents_RealRegistry_AndUnknownFailsClosed(t *testing.T) {
	w := newFRWorld(t, false)
	deps := Deps{DB: w.a.pool}
	got, err := assetExponents(context.Background(), deps, "USD", "BTC", "USDT", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if got["USD"] != 2 || got["BTC"] != 8 || got["USDT"] != 6 || len(got) != 3 {
		t.Fatalf("exponents = %v, want USD 2, BTC 8, USDT 6", got)
	}
	if _, err := assetExponents(context.Background(), deps, "USD", "NOPE"); err == nil {
		t.Fatal("an unknown asset must be an error, not a guessed exponent")
	}
	if ref := exponentRef(got, "NOPE"); ref != nil {
		t.Fatal("exponentRef of a missing code must be nil")
	}
}
