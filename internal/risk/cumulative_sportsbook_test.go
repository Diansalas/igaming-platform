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
// field of ADR 0083 §6.1.1's map literal as amended by ADR 0088 §6.1,
// including the two fields whose contents are deliberate, reasoned
// decisions rather than omissions:
//
//   - ReversalTypes is exactly ["sportsbook_void"] (ADR 0088 §6.1):
//     migration 0091 admitted the sportsbook reversal type, and
//     INV-SB-CUM-1 required this spec to change in the same commit.
//     sportsbook_rollback and sportsbook_settlement must never appear in
//     it: a rollback is never netted (ADR 0038 §13) and a settlement
//     payout is not a stake reversal.
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
	assertStringSet(t, "ReversalTypes", spec.ReversalTypes, []string{"sportsbook_void"})
	for _, never := range []string{"sportsbook_rollback", "sportsbook_settlement"} {
		if containsAccountType(spec.ReversalTypes, never) || containsAccountType(spec.TransactionTypes, never) {
			t.Fatalf("%s must never be counted by the sportsbook cumulative measure (ADR 0038 §13, ADR 0088 §6.1 / INV-SB-CUM-1); got TransactionTypes=%v ReversalTypes=%v", never, spec.TransactionTypes, spec.ReversalTypes)
		}
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
