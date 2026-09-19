package httpserver

// K3-6 (docs/governance/stage-4i-canonical-model.md §9.3/§6.3 - the
// HTTP-boundary oracle rule, generalized platform-wide by security/
// architect beyond casino): DenialCodeJurisdictionUnresolved and
// DenialCodeJurisdictionBlocked must produce a BYTE-IDENTICAL player-
// facing response. No player-side jurisdiction resolution exists yet
// anywhere in this codebase (HDR-J-3 unanswered), so a genuinely
// "blocked" LaunchGameResult is UNREACHABLE via a real HTTP request in
// Stage 4I - this test exercises writeCasinoLaunchDenial directly (no
// router, no live DB, exactly mirroring admin_routes_test.go's own
// no-DB-needed convention) with both codes and compares the recorded
// response byte-for-byte.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/casino"
)

func TestWriteCasinoLaunchDenial_JurisdictionUnresolvedAndBlockedAreByteIdentical(t *testing.T) {
	const requestID = "test-request-id-fixed"

	unresolvedRec := httptest.NewRecorder()
	writeCasinoLaunchDenial(unresolvedRec, requestID, casino.LaunchGameResult{
		Denied: true, DenialCode: casino.DenialCodeJurisdictionUnresolved,
	})

	blockedRec := httptest.NewRecorder()
	writeCasinoLaunchDenial(blockedRec, requestID, casino.LaunchGameResult{
		Denied: true, DenialCode: casino.DenialCodeJurisdictionBlocked,
	})

	if unresolvedRec.Code != blockedRec.Code {
		t.Fatalf("expected identical status codes, got unresolved=%d blocked=%d", unresolvedRec.Code, blockedRec.Code)
	}
	if unresolvedRec.Body.String() != blockedRec.Body.String() {
		t.Fatalf("expected byte-identical response bodies, got unresolved=%q blocked=%q", unresolvedRec.Body.String(), blockedRec.Body.String())
	}
	if unresolvedRec.Header().Get("Content-Type") != blockedRec.Header().Get("Content-Type") {
		t.Fatalf("expected identical Content-Type headers, got unresolved=%q blocked=%q",
			unresolvedRec.Header().Get("Content-Type"), blockedRec.Header().Get("Content-Type"))
	}

	// Sanity: the collapsed message must not leak either internal denial
	// code, and must actually be the player-facing jurisdiction message -
	// otherwise this test could pass by both sides being identically
	// wrong (e.g. both falling through to the generic RG/Risk branch).
	if !strings.Contains(unresolvedRec.Body.String(), "not available in your jurisdiction") {
		t.Fatalf("expected the jurisdiction-denial message, got %q", unresolvedRec.Body.String())
	}
	if strings.Contains(unresolvedRec.Body.String(), "jurisdiction_unresolved") || strings.Contains(unresolvedRec.Body.String(), "jurisdiction_blocked") {
		t.Fatalf("expected the internal denial code NOT to leak into the player-facing response, got %q", unresolvedRec.Body.String())
	}
}

// TestWriteCasinoLaunchDenial_NonJurisdictionDenialStillDisclosesItsCode
// is the anti-inertness control: proving writeCasinoLaunchDenial is not
// simply collapsing EVERY denial (which would make the test above pass
// for the wrong reason) - an RG/Risk denial legitimately discloses its
// own DenialCode to the player (Stage 4D-RG's own established
// discipline), and that must still hold after this K3-6 fix.
func TestWriteCasinoLaunchDenial_NonJurisdictionDenialStillDisclosesItsCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCasinoLaunchDenial(rec, "test-request-id", casino.LaunchGameResult{
		Denied: true, DenialCode: "self_excluded",
	})
	if !strings.Contains(rec.Body.String(), "self_excluded") {
		t.Fatalf("expected a non-jurisdiction denial to disclose its own code, got %q", rec.Body.String())
	}
}
