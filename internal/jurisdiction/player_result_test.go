package jurisdiction

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This file covers the PHASE-C fix round's non-forgeability regression
// guards: architect P1-3 / security SEC-4I-C-01 (defensive copies on every
// slice-returning accessor) and the %#v redaction bypass every reviewer
// independently found (security, architect, qa) before GoString() existed
// on these types.

func resolvedMarketAccessResultWithLocation(t *testing.T) PlayerJurisdictionResult {
	t.Helper()
	maxAge := 24 * time.Hour
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeMarketAccessControl,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()},
			DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "DE", CapturedAt: asOfFixture.Add(-2 * time.Hour), PlayerAccountID: uuid.New()},
			LocationSignal:    &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(-time.Minute), ProviderID: "p", ProviderReference: "r"},
		},
		Policy: EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
		AsOf:   asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Resolved {
		t.Fatalf("expected Resolved, got %s(%s)", res.Outcome(), res.Reason())
	}
	return res
}

// --- Slice mutation-resistance (architect P1-3 / security SEC-4I-C-01) ---

func TestPlayerJurisdictionResult_CandidatesReturnsADefensiveCopy(t *testing.T) {
	res := resolvedMarketAccessResultWithLocation(t)

	first, err := res.Candidates()
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(first))
	}
	// Mutate the returned slice: reorder it so the additional-restriction
	// candidate comes first, exactly the attack architect/security/qa each
	// independently demonstrated against the pre-fix code.
	first[0], first[1] = first[1], first[0]

	second, err := res.Candidates()
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if second[0].Role() != RolePrimaryDetermination || second[0].Basis() != BasisPlayerVerifiedResidence {
		t.Fatalf("mutating a previously-returned slice must not affect the internal state; got candidate 0 = %+v", second[0])
	}

	primary, err := res.PrimaryCandidate()
	if err != nil {
		t.Fatalf("PrimaryCandidate: %v", err)
	}
	if primary.Basis() != BasisPlayerVerifiedResidence {
		t.Fatalf("PrimaryCandidate must still report the real primary determination after external mutation, got basis=%s", primary.Basis())
	}
}

func TestPlayerJurisdictionResult_ConsideredEvidenceReturnsADefensiveCopy(t *testing.T) {
	res := resolvedMarketAccessResultWithLocation(t)

	first := res.ConsideredEvidence()
	if len(first) == 0 {
		t.Fatal("expected at least one considered-evidence entry")
	}
	original := first[0]
	// Mutate the returned slice's entry in place - ConsideredEvidence has
	// exported fields, so this is the more dangerous variant: a caller
	// could relabel an entry's Basis to one this engine never itself
	// emits (e.g. BasisTenantLicence) and then log/persist it as if the
	// engine had produced it.
	first[0].Basis = BasisTenantLicence
	first[0].Status = StatusSelected

	second := res.ConsideredEvidence()
	if second[0].Basis != original.Basis || second[0].Status != original.Status {
		t.Fatalf("mutating a previously-returned slice must not affect internal state; got %+v, want %+v", second[0], original)
	}
	for _, c := range second {
		if c.Basis == BasisTenantLicence {
			t.Fatal("BasisTenantLicence must never appear in a player-jurisdiction result's considered evidence")
		}
	}
}

func TestComposedRestriction_ContributorsReturnsADefensiveCopy(t *testing.T) {
	in := []AppliedRestriction{
		{Outcome: RestrictionBlocked, Basis: BasisGeoSignal, GateID: "g1"},
		{Outcome: RestrictionRestricted, Basis: BasisPlayerVerifiedResidence, GateID: "g2"},
	}
	// A tie is the sharpest test: force two entries at the same (max)
	// severity so a caller mutating the returned slice could otherwise
	// desync Outcome() from Contributors().
	in[1].Outcome = RestrictionBlocked

	composed, err := ComposeRestrictions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	first := composed.Contributors()
	if len(first) != 2 {
		t.Fatalf("expected 2 tied contributors, got %d", len(first))
	}
	first[0].Outcome = RestrictionAllowed
	first[1].GateID = "tampered"

	second := composed.Contributors()
	if second[0].Outcome != RestrictionBlocked {
		t.Fatalf("mutating a previously-returned slice must not affect internal state; got %+v", second[0])
	}
	if second[1].GateID == "tampered" {
		t.Fatal("mutating a previously-returned slice must not affect internal state")
	}
	if composed.Outcome() != RestrictionBlocked {
		t.Fatalf("ComposedRestriction.Outcome() must remain consistent with its (unmutated) Contributors(), got %s", composed.Outcome())
	}
}

// --- %#v redaction (independently found by architect, security, and qa) ---

func TestGoStringRedaction_NeverLeaksACountryCodeViaSharpV(t *testing.T) {
	res := resolvedMarketAccessResultWithLocation(t)
	cands, err := res.Candidates()
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	primary, err := res.PrimaryCandidate()
	if err != nil {
		t.Fatalf("PrimaryCandidate: %v", err)
	}

	in := []AppliedRestriction{{Outcome: RestrictionBlocked, Basis: BasisGeoSignal, Code: primary.Code(), GateID: "g1"}}
	composed, err := ComposeRestrictions(in)
	if err != nil {
		t.Fatalf("ComposeRestrictions: %v", err)
	}

	check := func(label string, v any) {
		t.Helper()
		s := fmt.Sprintf("%#v", v)
		if strings.Contains(s, "MT") {
			t.Fatalf("%s leaked the country code via %%#v: %q", label, s)
		}
	}

	check("PlayerJurisdictionResult", res)
	check("Candidate(primary)", primary)
	for i, c := range cands {
		check(fmt.Sprintf("Candidate(%d)", i), c)
	}
	check("PlayerJurisdictionCode", primary.Code())
	check("ComposedRestriction", composed)
	for i, c := range composed.Contributors() {
		check(fmt.Sprintf("AppliedRestriction(%d)", i), c)
	}
	check("[]Candidate", cands)
	check("[]AppliedRestriction", composed.Contributors())
}

func TestPlayerJurisdictionCode_GoStringNeverLeaksTheCode(t *testing.T) {
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeIdentityDetermination,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()},
		},
		AsOf: asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	primary, err := res.PrimaryCandidate()
	if err != nil {
		t.Fatalf("PrimaryCandidate: %v", err)
	}
	code := primary.Code()
	if !code.IsSet() {
		t.Fatal("expected a resolved candidate's code to report IsSet()==true")
	}
	var zero PlayerJurisdictionCode
	if zero.IsSet() {
		t.Fatal("the zero-value PlayerJurisdictionCode must report IsSet()==false")
	}
	if strings.Contains(fmt.Sprintf("%#v", code), "MT") {
		t.Fatalf("PlayerJurisdictionCode.GoString leaked the code: %q", fmt.Sprintf("%#v", code))
	}
}
