package jurisdiction_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

// This is a black-box test (package jurisdiction_test) specifically to
// prove PlayerJurisdictionCode is obtainable only through the real public
// API - never via a bare conversion, which package-external code cannot
// even attempt to compile (see PlayerJurisdictionCode's own doc comment in
// player_result.go for why).
func TestPlayerJurisdictionCode_IsObtainableOnlyFromAResolvedCandidate(t *testing.T) {
	asOf := time.Now().UTC()
	res, err := jurisdiction.DeterminePlayerJurisdiction(jurisdiction.DetermineParams{
		Purpose: jurisdiction.PurposeIdentityDetermination,
		Evidence: jurisdiction.EvidenceSet{
			VerifiedResidence: &jurisdiction.VerifiedResidenceEvidence{
				CountryCode:    "MT",
				SetAt:          asOf.Add(-time.Hour),
				VerificationID: uuid.New(),
			},
		},
		AsOf: asOf,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != jurisdiction.Resolved {
		t.Fatalf("expected Resolved, got %s", res.Outcome())
	}

	candidate, err := res.PrimaryCandidate()
	if err != nil {
		t.Fatalf("PrimaryCandidate: %v", err)
	}

	code := candidate.Code()
	if code.String() != "MT" {
		t.Fatalf("expected a real, usable PlayerJurisdictionCode(\"MT\"), got %q", code.String())
	}

	// The following does NOT compile, by design: this repo has no
	// compile-fail test harness, so this is documented rather than run:
	//
	//   _ = jurisdiction.PlayerJurisdictionCode{} // fine, zero value
	//   bad := jurisdiction.PlayerJurisdictionCode("MT")
	//   // ^ compile error: cannot convert "MT" (untyped string constant)
	//   // to type jurisdiction.PlayerJurisdictionCode - it is a struct
	//   // with an unexported field, not a named string type, so a bare
	//   // conversion from a string is structurally impossible from
	//   // outside package jurisdiction.
}
