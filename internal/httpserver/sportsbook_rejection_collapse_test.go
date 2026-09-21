// Stage 9.2 fix round: toPlaceBetRejectionResponse (sportsbook_handlers.go)
// had NO direct test for either collapse it performs - K3-6 (jurisdiction)
// or the new SEC-S92-6 fix (exposure). A pure function of
// sportsbook.PlaceBetResult -> placeBetResponse, so this is a plain unit
// test - no build tag, no database.
package httpserver

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// TestToPlaceBetRejectionResponse_JurisdictionCollapse is K3-6: both
// internal jurisdiction denial codes must produce a BYTE-IDENTICAL
// player-facing response - the distinction between "unresolved" and
// "blocked" is exactly the signal an attacker could use to tell whether a
// manipulation attempt registered.
func TestToPlaceBetRejectionResponse_JurisdictionCollapse(t *testing.T) {
	unresolved := sportsbook.PlaceBetResult{
		Accepted: false, RejectionCategory: sportsbook.RejectionJurisdictionDenied,
		RejectionCode: "jurisdiction_unresolved", RejectionMessage: "this bet is not available in your jurisdiction",
	}
	blocked := sportsbook.PlaceBetResult{
		Accepted: false, RejectionCategory: sportsbook.RejectionJurisdictionDenied,
		RejectionCode: "jurisdiction_blocked", RejectionMessage: "this bet is not available in your jurisdiction",
	}

	gotUnresolved := toPlaceBetRejectionResponse(unresolved)
	gotBlocked := toPlaceBetRejectionResponse(blocked)

	if gotUnresolved != gotBlocked {
		t.Fatalf("expected the two internally-distinguishable jurisdiction denial codes to collapse to a BYTE-IDENTICAL response, got %+v vs %+v", gotUnresolved, gotBlocked)
	}
	want := placeBetResponse{
		Accepted: false, RejectionCategory: sportsbook.RejectionJurisdictionDenied,
		RejectionCode: "jurisdiction_unavailable", RejectionMessage: "this bet is not available in your jurisdiction",
	}
	if gotUnresolved != want {
		t.Fatalf("expected the collapsed jurisdiction response to be %+v, got %+v", want, gotUnresolved)
	}
}

// TestToPlaceBetRejectionResponse_ExposureLimitCollapse is the SEC-S92-6
// fix: sportsbook.RejectionExposureLimit must no longer reach the player
// as the distinct, never-otherwise-used literal "exposure_limit" -
// instead it must come back in EXACTLY the shape a genuine
// risk.CodeLimitBreach decline already uses.
func TestToPlaceBetRejectionResponse_ExposureLimitCollapse(t *testing.T) {
	// INV-SB-EXP-2 is already enforced upstream (orchestrator.go never
	// populates RejectionCode/a specific RejectionMessage for this
	// category) - this test exercises the HTTP-boundary collapse
	// regardless of what upstream happens to leave in those fields, since
	// the fix must not depend on that.
	exposureResult := sportsbook.PlaceBetResult{
		Accepted: false, RejectionCategory: sportsbook.RejectionExposureLimit,
		RejectionCode: "", RejectionMessage: "this bet cannot be accepted at this time",
	}
	got := toPlaceBetRejectionResponse(exposureResult)

	if got.RejectionCategory == sportsbook.RejectionExposureLimit {
		t.Fatalf("SEC-S92-6: expected the player-facing RejectionCategory to never be the distinct literal %q, got %+v", sportsbook.RejectionExposureLimit, got)
	}
	want := placeBetResponse{
		Accepted: false, RejectionCategory: sportsbook.RejectionRiskDenied,
		RejectionCode:    risk.CodeLimitBreach,
		RejectionMessage: "this bet was declined by platform risk policy: " + risk.CodeLimitBreach,
	}
	if got != want {
		t.Fatalf("expected the collapsed exposure-limit response to be %+v, got %+v", want, got)
	}
}

// TestToPlaceBetRejectionResponse_ExposureLimitIndistinguishableFromGenuineRiskDecline
// is the actual security property SEC-S92-6 fixes: a client must not be
// able to tell "the book's cross-player exposure ceiling fired" apart
// from "my own applicable risk/limit policy declined this bet" - this
// proves it directly by constructing what a REAL risk.CodeLimitBreach
// decline's own response looks like (via the untouched passthrough path)
// and confirming it is byte-for-byte identical to the collapsed exposure
// response above.
func TestToPlaceBetRejectionResponse_ExposureLimitIndistinguishableFromGenuineRiskDecline(t *testing.T) {
	genuineRiskDecline := sportsbook.PlaceBetResult{
		Accepted: false, RejectionCategory: sportsbook.RejectionRiskDenied,
		RejectionCode:    risk.CodeLimitBreach,
		RejectionMessage: "this bet was declined by platform risk policy: " + risk.CodeLimitBreach,
	}
	exposureResult := sportsbook.PlaceBetResult{
		Accepted: false, RejectionCategory: sportsbook.RejectionExposureLimit,
	}

	gotGenuine := toPlaceBetRejectionResponse(genuineRiskDecline)
	gotExposure := toPlaceBetRejectionResponse(exposureResult)

	if gotGenuine != gotExposure {
		t.Fatalf("SEC-S92-6: a genuine risk.CodeLimitBreach decline and an exposure-limit decline must produce an IDENTICAL player-facing response - got %+v (genuine) vs %+v (exposure)", gotGenuine, gotExposure)
	}
}

// TestToPlaceBetRejectionResponse_OtherCategoriesPassThroughUnchanged
// proves the collapse is scoped to exactly jurisdiction and exposure -
// every other rejection category still discloses its own specific
// RejectionCode/RejectionMessage unchanged (this file's own doc comment:
// "a player declined for insufficient funds or a stale price is
// legitimately owed that specific reason").
func TestToPlaceBetRejectionResponse_OtherCategoriesPassThroughUnchanged(t *testing.T) {
	cases := []sportsbook.PlaceBetResult{
		{Accepted: false, RejectionCategory: sportsbook.RejectionOddsChanged, RejectionMessage: "the odds for this selection have changed since you last viewed them"},
		{Accepted: false, RejectionCategory: sportsbook.RejectionEventNotOpen, RejectionCode: "finished", RejectionMessage: "this event is no longer open for betting"},
		{Accepted: false, RejectionCategory: sportsbook.RejectionInsufficientFunds, RejectionMessage: "insufficient available balance for this stake"},
		{Accepted: false, RejectionCategory: sportsbook.RejectionRGDenied, RejectionCode: "self_exclusion", RejectionMessage: "gambling is currently restricted for this account: self_exclusion"},
		{Accepted: false, RejectionCategory: sportsbook.RejectionRiskDenied, RejectionCode: "hard_limit_breached", RejectionMessage: "this bet was declined by platform risk policy: hard_limit_breached"},
	}
	for _, c := range cases {
		t.Run(c.RejectionCategory, func(t *testing.T) {
			got := toPlaceBetRejectionResponse(c)
			want := placeBetResponse{Accepted: false, RejectionCategory: c.RejectionCategory, RejectionCode: c.RejectionCode, RejectionMessage: c.RejectionMessage}
			if got != want {
				t.Fatalf("expected category %q to pass through unchanged: want %+v, got %+v", c.RejectionCategory, want, got)
			}
		})
	}
}
