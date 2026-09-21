// Stage 9.2 Workstream B Wave 1 (ADR 0083 §6.1.1): the DB-free half of
// the coverage for operationCumulativeSpecs' new OperationSportsbookBet
// entry - the spec's literal shape.
//
// This file carries NO `integration` build tag, deliberately and as a
// small improvement on the pattern it otherwise mirrors
// (TestCasinoBetCumulativeSpecIsUnchangedAndComplete in
// cumulative_leg_integration_test.go asserts an equally DB-free fact but
// sits behind the tag, so plain `go test ./...` never runs it). Pinning a
// production map literal needs no PostgreSQL, and a pin that only runs in
// the tagged suite is a weaker pin. The DB-backed half - measuring a real
// two-player-owned-leg posting - lives in
// cumulative_sportsbook_integration_test.go.
package risk

import "testing"

// TestSportsbookBetCumulativeSpecShapeIsExactlyAsSpecified pins every
// field of ADR 0083 §6.1.1's map literal, including the two fields whose
// EMPTINESS is a deliberate, reasoned decision rather than an omission:
//
//   - ReversalTypes is empty because `sportsbook_void` does not exist as
//     a ledger_transactions.transaction_type (migration 0078 admits
//     seventeen values, `sportsbook_bet` being the seventeenth; no later
//     migration widens it with a sportsbook reversal, and internal/ledger
//     declares no TxSportsbookVoid). ADR 0083 §6.1.2 + INV-SB-CUM-1: the
//     change that adds such a type must update the spec in the same
//     change, and this assertion is what makes that omission loud.
//   - player_locked_bonus is absent from BOTH MeasuredAccountTypes and
//     IgnoredAccountTypes, so an (impossible today) bonus-funded
//     sportsbook stake trips ErrUnrecognizedCumulativeLeg instead of
//     being silently ignored and under-counted.
func TestSportsbookBetCumulativeSpecShapeIsExactlyAsSpecified(t *testing.T) {
	spec, ok := operationCumulativeSpecs[OperationSportsbookBet]
	if !ok {
		t.Fatal("OperationSportsbookBet must have a production cumulative spec (ADR 0083 §6.1.1) - without it every cumulative_amount rule authored for sportsbook_bet fails closed with ErrUnsupportedCumulativeOperation on every bet")
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("sportsbook_bet spec is incomplete: %v", err)
	}
	assertStringSet(t, "TransactionTypes", spec.TransactionTypes, []string{"sportsbook_bet"})
	if len(spec.ReversalTypes) != 0 {
		t.Fatalf("ReversalTypes must stay EMPTY until a sportsbook reversal transaction_type actually exists (ADR 0083 §6.1.2 / INV-SB-CUM-1); got %v - if a widening migration landed, this spec must be updated in the SAME change and this assertion rewritten, not deleted", spec.ReversalTypes)
	}
	assertStringSet(t, "MeasuredAccountTypes", spec.MeasuredAccountTypes, []string{"player_cash"})
	assertStringSet(t, "IgnoredAccountTypes", spec.IgnoredAccountTypes, []string{"player_locked_cash"})
	if spec.ConsumingDirection != directionDebit {
		t.Fatalf("a stake consumes capacity on debit, got %q", spec.ConsumingDirection)
	}
	for _, undeclared := range []string{"player_locked_bonus", "player_bonus"} {
		if containsAccountType(spec.MeasuredAccountTypes, undeclared) || containsAccountType(spec.IgnoredAccountTypes, undeclared) {
			t.Fatalf("%s must NOT be declared while bonus-funded sportsbook wagering is blocked (ADR 0038 §9): leaving it undeclared is what makes the first bonus-funded stake raise ErrUnrecognizedCumulativeLeg rather than under-count the cap", undeclared)
		}
	}
}

// TestEverySportsbookRelatedOperationEitherHasACompleteSpecOrNone guards
// the map as a whole: every entry must validate (so a future author
// cannot half-add one), and sportsbook_bet must be a KNOWN operation, or
// Evaluate rejects it with ErrUnknownOperation long before the spec is
// ever consulted.
func TestSportsbookBetIsAKnownOperationWithACompleteSpec(t *testing.T) {
	if !IsKnownOperation(OperationSportsbookBet) {
		t.Fatal("sportsbook_bet must be a known Operation for its cumulative spec to be reachable at all")
	}
	for op, s := range operationCumulativeSpecs {
		if err := s.validate(); err != nil {
			t.Fatalf("cumulative spec for %s is incomplete: %v", op, err)
		}
	}
}

func assertStringSet(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: expected exactly %v, got %v", field, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: expected exactly %v, got %v", field, want, got)
		}
	}
}
