package casino

import (
	"errors"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/risk"
)

// TestClassifyRiskOutcome_FourDistinctOutcomes is the unit-level proof of
// ADR 0031 §34's four-outcome contract at casino's enforcement points:
// ALLOW proceeds, DENY and REVIEW both block as BUSINESS decisions (a
// decline, never a Go error), and anything else is refused as an ERROR -
// "the platform could not decide" is never reported as "the player's
// limits declined this", and never falls through to ALLOW.
//
// This also pins the REVIEW behavior deliberately: REVIEW blocking here
// is the disclosed SIMPLIFICATION ADR 0031 §6/§8/§11 records (no
// compliance-review queue exists yet), not a resolved product decision.
// If a future stage lets REVIEW proceed provisionally, THIS test is what
// has to change, which is exactly the visibility the ADR asks for.
func TestClassifyRiskOutcome_FourDistinctOutcomes(t *testing.T) {
	proceed, err := classifyRiskOutcome(risk.OutcomeAllow)
	if err != nil || !proceed {
		t.Fatalf("ALLOW must proceed, got proceed=%v err=%v", proceed, err)
	}

	for _, blocking := range []risk.Outcome{risk.OutcomeDeny, risk.OutcomeReview} {
		proceed, err := classifyRiskOutcome(blocking)
		if err != nil {
			t.Fatalf("%q is a business decision and must not surface as an error: %v", blocking, err)
		}
		if proceed {
			t.Fatalf("%q must block at this enforcement point", blocking)
		}
	}

	for _, unknown := range []risk.Outcome{risk.Outcome(""), risk.Outcome("allowed"), risk.Outcome("pending_review"), risk.Outcome("ALLOW")} {
		proceed, err := classifyRiskOutcome(unknown)
		if proceed {
			t.Fatalf("%q must never proceed", unknown)
		}
		if !errors.Is(err, ErrRiskOutcomeUnrecognized) {
			t.Fatalf("%q must fail closed as an error, got %v", unknown, err)
		}
	}
}
