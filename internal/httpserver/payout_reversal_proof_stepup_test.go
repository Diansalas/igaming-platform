package httpserver

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/stepup"
)

// ADR 0110 T8 (migration 0165): the payout-reversal route signs withdrawal_payout_reversal:decide. Its golden
// step-up class (critical) must equal the signer backstop's class for that operation: a route weaker than
// the registry would 500 at the backstop, a registry entry missing would let a stale session be signed for.
func TestPayoutReversalRoute_StepUpClassMatchesSignerBackstop(t *testing.T) {
	const pat = "POST /v1/admin/withdrawals/{id}/payout-reversal"
	if got := staffRouteGolden[pat]; got != "critical" {
		t.Fatalf("%s: golden class %q, want critical", pat, got)
	}
	if c, ok := actorproof.StepUpOperations[actorproof.OpPayoutReversalDecide]; !ok || c != stepup.Critical {
		t.Fatalf("%s: signer backstop class %q (registered %v), want critical", actorproof.OpPayoutReversalDecide, c, ok)
	}
}
