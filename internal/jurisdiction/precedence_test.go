package jurisdiction

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var asOfFixture = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func TestDetermine_NoEvidenceIsUnresolvedNoSignal(t *testing.T) {
	for _, purpose := range []Purpose{PurposeIdentityDetermination, PurposeMarketAccessControl} {
		policy := EvaluationPolicy{}
		res, err := DeterminePlayerJurisdiction(DetermineParams{Purpose: purpose, Policy: policy, AsOf: asOfFixture})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", purpose, err)
		}
		if res.Outcome() != Unresolved || res.Reason() != ReasonNoSignal {
			t.Fatalf("%s: expected unresolved(no_signal), got %s(%s)", purpose, res.Outcome(), res.Reason())
		}
	}
}

func TestDetermine_DeclaredResidenceAloneIsInsufficientForEveryLivePurpose(t *testing.T) {
	ev := EvidenceSet{DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "DE", CapturedAt: asOfFixture.Add(-time.Hour), PlayerAccountID: uuid.New()}}

	for _, purpose := range []Purpose{PurposeIdentityDetermination, PurposeMarketAccessControl} {
		res, err := DeterminePlayerJurisdiction(DetermineParams{Purpose: purpose, Evidence: ev, AsOf: asOfFixture})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", purpose, err)
		}
		if res.Outcome() != Unresolved || res.Reason() != ReasonInsufficientConfidence {
			t.Fatalf("%s: expected unresolved(insufficient_confidence), got %s(%s)", purpose, res.Outcome(), res.Reason())
		}
		found := false
		for _, c := range res.ConsideredEvidence() {
			if c.Basis == BasisPlayerDeclaredResidence && c.Status == StatusRejectedLowerPrecedence {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: expected a rejected_lower_precedence considered-evidence entry for declared residence", purpose)
		}
	}
}

func TestDetermine_VerifiedResidenceIsAuthoritativeForIdentityDetermination(t *testing.T) {
	verificationID := uuid.New()
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeIdentityDetermination,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: verificationID},
		},
		AsOf: asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Resolved || res.Reason() != ReasonDetermined {
		t.Fatalf("expected resolved(determined), got %s(%s)", res.Outcome(), res.Reason())
	}
	primary, err := res.PrimaryCandidate()
	if err != nil {
		t.Fatalf("PrimaryCandidate: %v", err)
	}
	if primary.Basis() != BasisPlayerVerifiedResidence {
		t.Fatalf("expected basis=player_verified_residence, got %s", primary.Basis())
	}
	if primary.Confidence() != ConfidenceVerified {
		t.Fatalf("expected confidence=verified, got %s", primary.Confidence())
	}
	if primary.Role() != RolePrimaryDetermination {
		t.Fatalf("expected role=primary_determination, got %s", primary.Role())
	}
	if primary.Code().String() != "MT" {
		t.Fatalf("expected code MT, got %s", primary.Code())
	}
	if primary.EvidenceRef().Kind != EvidenceRefKYCVerification || primary.EvidenceRef().ID != verificationID {
		t.Fatalf("unexpected evidence ref: %+v", primary.EvidenceRef())
	}
}

func TestDetermine_DeclaredNeverOverridesVerifiedAndDisagreementIsRecordedNotFatal(t *testing.T) {
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeIdentityDetermination,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()},
			DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "DE", CapturedAt: asOfFixture.Add(-2 * time.Hour), PlayerAccountID: uuid.New()},
		},
		AsOf: asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Resolved {
		t.Fatalf("expected Resolved, got %s", res.Outcome())
	}
	primary, err := res.PrimaryCandidate()
	if err != nil {
		t.Fatalf("PrimaryCandidate: %v", err)
	}
	if primary.Code().String() != "MT" {
		t.Fatalf("expected the verified code MT to win, got %s", primary.Code())
	}
	if !res.HasDisagreement() {
		t.Fatal("expected HasDisagreement() == true when declared and verified disagree")
	}
	found := false
	for _, c := range res.ConsideredEvidence() {
		if c.Basis == BasisPlayerDeclaredResidence && c.Status == StatusDisagreed {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a disagreed considered-evidence entry for declared residence")
	}
}

func TestDetermine_DeclaredAgreeingWithVerifiedIsRejectedLowerPrecedenceNotDisagreement(t *testing.T) {
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeIdentityDetermination,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()},
			DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "MT", CapturedAt: asOfFixture.Add(-2 * time.Hour), PlayerAccountID: uuid.New()},
		},
		AsOf: asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.HasDisagreement() {
		t.Fatal("agreeing declared/verified residence must not be reported as a disagreement")
	}
	found := false
	for _, c := range res.ConsideredEvidence() {
		if c.Basis == BasisPlayerDeclaredResidence && c.Status == StatusRejectedLowerPrecedence {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a rejected_lower_precedence considered-evidence entry for the agreeing declared residence")
	}
}

func TestDetermine_LocationSignalIsNeverASubstituteForVerifiedResidence(t *testing.T) {
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeIdentityDetermination,
		Evidence: EvidenceSet{
			LocationSignal: &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(-time.Minute), ProviderID: "p", ProviderReference: "r"},
		},
		AsOf: asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Unresolved || res.Reason() != ReasonNoApplicableEvidence {
		t.Fatalf("expected unresolved(no_applicable_evidence) - location alone must never resolve identity, got %s(%s)", res.Outcome(), res.Reason())
	}
	found := false
	for _, c := range res.ConsideredEvidence() {
		if c.Basis == BasisGeoSignal && c.Status == StatusInapplicable {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a location considered-evidence entry with status=inapplicable")
	}
	if res.LocationSignalState() != LocationObserved {
		t.Fatalf("expected locationState=observed, got %s", res.LocationSignalState())
	}
}

func TestDetermine_MarketAccessEmitsLocationAsAnAdditionalRestrictionCandidate(t *testing.T) {
	maxAge := 24 * time.Hour

	run := func(t *testing.T, locationCode string) {
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose: PurposeMarketAccessControl,
			Evidence: EvidenceSet{
				VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()},
				LocationSignal:    &LocationSignalEvidence{State: LocationObserved, CountryCode: locationCode, ObservedAt: asOfFixture.Add(-time.Minute), ProviderID: "p", ProviderReference: "r"},
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
		cands, err := res.Candidates()
		if err != nil {
			t.Fatalf("Candidates: %v", err)
		}
		if len(cands) != 2 {
			t.Fatalf("expected exactly 2 candidates, got %d", len(cands))
		}
		if cands[0].Role() != RolePrimaryDetermination || cands[0].Basis() != BasisPlayerVerifiedResidence {
			t.Fatalf("candidate 0 must be the primary residence determination, got %+v", cands[0])
		}
		if cands[1].Role() != RoleAdditionalRestriction || cands[1].Basis() != BasisGeoSignal {
			t.Fatalf("candidate 1 must be the additional location restriction, got %+v", cands[1])
		}
		if cands[1].Confidence() != ConfidenceCorroborated {
			t.Fatalf("expected location candidate confidence=corroborated, got %s", cands[1].Confidence())
		}
	}

	t.Run("agreeing", func(t *testing.T) { run(t, "MT") })
	t.Run("disagreeing", func(t *testing.T) { run(t, "DE") })
}

func TestDetermine_LocationRequiredAndUnusableIsUnresolvedLocationSignalUnusable(t *testing.T) {
	maxAge := 24 * time.Hour
	base := EvidenceSet{VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()}}

	cases := map[string]*LocationSignalEvidence{
		"not_consulted":  nil,
		"inconclusive":   {State: LocationInconclusive, ObservedAt: asOfFixture.Add(-time.Minute)},
		"unavailable":    {State: LocationUnavailable, ObservedAt: asOfFixture.Add(-time.Minute)},
		"provider_error": {State: LocationProviderError, ObservedAt: asOfFixture.Add(-time.Minute)},
		"stale":          {State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(-48 * time.Hour)},
	}

	for name, ls := range cases {
		t.Run(name, func(t *testing.T) {
			ev := base
			ev.LocationSignal = ls
			res, err := DeterminePlayerJurisdiction(DetermineParams{
				Purpose:  PurposeMarketAccessControl,
				Evidence: ev,
				Policy:   EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
				AsOf:     asOfFixture,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome() != Unresolved || res.Reason() != ReasonLocationSignalUnusable {
				t.Fatalf("expected unresolved(location_signal_unusable), got %s(%s)", res.Outcome(), res.Reason())
			}
			if _, err := res.Candidates(); err == nil {
				t.Fatal("an unresolved result must not expose Candidates()")
			}
		})
	}
}

func TestDetermine_LocationFreshnessBoundaryIsExactAndInclusive(t *testing.T) {
	maxAge := 24 * time.Hour
	verified := &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()}

	t.Run("exactly at boundary is fresh", func(t *testing.T) {
		ls := &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(-maxAge)}
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose:  PurposeMarketAccessControl,
			Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: ls},
			Policy:   EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
			AsOf:     asOfFixture,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Outcome() != Resolved {
			t.Fatalf("age == MaxLocationSignalAge must be treated as fresh, got %s(%s)", res.Outcome(), res.Reason())
		}
		cands, _ := res.Candidates()
		if len(cands) != 2 {
			t.Fatalf("expected the location candidate to be usable at the exact boundary, got %d candidates", len(cands))
		}
	})

	t.Run("one nanosecond past boundary is stale", func(t *testing.T) {
		ls := &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(-maxAge - time.Nanosecond)}
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose:  PurposeMarketAccessControl,
			Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: ls},
			Policy:   EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
			AsOf:     asOfFixture,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Outcome() != Unresolved || res.Reason() != ReasonLocationSignalUnusable {
			t.Fatalf("age == MaxLocationSignalAge+1ns must be stale/unusable under LocationRequired, got %s(%s)", res.Outcome(), res.Reason())
		}
	})
}

func TestDetermine_UnsetLocationPolicyValuesFailClosedWithErrPolicyUnset(t *testing.T) {
	verified := &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()}
	maxAge := 24 * time.Hour

	t.Run("LocationSignalRequirement unset", func(t *testing.T) {
		_, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose:  PurposeMarketAccessControl,
			Evidence: EvidenceSet{VerifiedResidence: verified},
			Policy:   EvaluationPolicy{MaxLocationSignalAge: &maxAge},
			AsOf:     asOfFixture,
		})
		if !errors.Is(err, ErrPolicyUnset) {
			t.Fatalf("expected ErrPolicyUnset, got %v", err)
		}
	})

	t.Run("MaxLocationSignalAge nil with an observed signal", func(t *testing.T) {
		_, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose: PurposeMarketAccessControl,
			Evidence: EvidenceSet{
				VerifiedResidence: verified,
				LocationSignal:    &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(-time.Minute)},
			},
			Policy: EvaluationPolicy{LocationSignalRequirement: LocationRequired},
			AsOf:   asOfFixture,
		})
		if !errors.Is(err, ErrPolicyUnset) {
			t.Fatalf("expected ErrPolicyUnset, got %v", err)
		}
	})

	t.Run("identity determination never requires these policy fields", func(t *testing.T) {
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose:  PurposeIdentityDetermination,
			Evidence: EvidenceSet{VerifiedResidence: verified},
			Policy:   EvaluationPolicy{},
			AsOf:     asOfFixture,
		})
		if err != nil {
			t.Fatalf("identity determination must never return an error for an unset policy: %v", err)
		}
		if res.Outcome() != Resolved {
			t.Fatalf("expected Resolved, got %s(%s)", res.Outcome(), res.Reason())
		}
	})
}

func TestDetermine_InvalidEvidenceIsNeverSilentlyDemotedToALowerBasis(t *testing.T) {
	for _, purpose := range []Purpose{PurposeIdentityDetermination, PurposeMarketAccessControl} {
		res, err := DeterminePlayerJurisdiction(DetermineParams{
			Purpose: purpose,
			Evidence: EvidenceSet{
				VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "ZZ", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()},
				DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "DE", CapturedAt: asOfFixture.Add(-2 * time.Hour), PlayerAccountID: uuid.New()},
			},
			AsOf: asOfFixture,
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", purpose, err)
		}
		if res.Outcome() != Unresolved || res.Reason() != ReasonEvidenceInvalid {
			t.Fatalf("%s: expected unresolved(evidence_invalid), got %s(%s)", purpose, res.Outcome(), res.Reason())
		}
		found := false
		for _, c := range res.ConsideredEvidence() {
			if c.Basis == BasisPlayerVerifiedResidence && c.Status == StatusInvalid {
				found = true
			}
			if c.Basis == BasisPlayerDeclaredResidence && c.Status == StatusSelected {
				t.Fatalf("%s: declared residence must never be selected when verified residence is invalid", purpose)
			}
		}
		if !found {
			t.Fatalf("%s: expected an invalid considered-evidence entry for verified residence", purpose)
		}
	}
}

// PHASE-C fix round (QA coverage gap): the market-access path has carried
// this same ReasonNoApplicableEvidence upgrade since the original
// implementation, but no test ever exercised it positively - only the
// identity-purpose equivalent above does, after this fix round's P2-2 fix.
func TestDetermine_MarketAccessEmitsReasonNoApplicableEvidenceForLocationOnlyEvidence(t *testing.T) {
	maxAge := 24 * time.Hour
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeMarketAccessControl,
		Evidence: EvidenceSet{
			LocationSignal: &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(-time.Minute), ProviderID: "p", ProviderReference: "r"},
		},
		Policy: EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
		AsOf:   asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Unresolved || res.Reason() != ReasonNoApplicableEvidence {
		t.Fatalf("expected unresolved(no_applicable_evidence) - location alone must never resolve market access residence, got %s(%s)", res.Outcome(), res.Reason())
	}
}

// PHASE-C fix round (architect P1-2): AsOf is mandatory; a zero value would
// silently disable the location-freshness gate rather than failing loudly.
func TestDetermine_ZeroAsOfIsRejected(t *testing.T) {
	for _, purpose := range []Purpose{PurposeIdentityDetermination, PurposeMarketAccessControl} {
		_, err := DeterminePlayerJurisdiction(DetermineParams{Purpose: purpose, AsOf: time.Time{}})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: expected ErrInvalidInput for a zero AsOf, got %v", purpose, err)
		}
	}
}

// PHASE-C fix round (architect P1-1): an unrecognized LocationSignalRequirement
// value must fail closed, never silently behave like LocationAdvisory (the
// more permissive of the two known values).
func TestDetermine_UnrecognizedLocationSignalRequirementIsRejected(t *testing.T) {
	verified := &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()}
	maxAge := 24 * time.Hour
	_, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose:  PurposeMarketAccessControl,
		Evidence: EvidenceSet{VerifiedResidence: verified},
		Policy:   EvaluationPolicy{LocationSignalRequirement: LocationRequirement("bogus"), MaxLocationSignalAge: &maxAge},
		AsOf:     asOfFixture,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for an unrecognized LocationSignalRequirement, got %v", err)
	}
}

// PHASE-C fix round (security SEC-4I-C-03): a future-dated ObservedAt must
// never read as "definitely fresh" - a negative age is treated as stale,
// exactly like an age past the freshness boundary.
func TestDetermine_FutureDatedLocationSignalIsTreatedAsStale(t *testing.T) {
	maxAge := 24 * time.Hour
	verified := &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()}
	ls := &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture.Add(time.Hour)}
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose:  PurposeMarketAccessControl,
		Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: ls},
		Policy:   EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
		AsOf:     asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Unresolved || res.Reason() != ReasonLocationSignalUnusable {
		t.Fatalf("a future-dated ObservedAt must be treated as stale/unusable, got %s(%s)", res.Outcome(), res.Reason())
	}
}

// PHASE-C fix round (security SEC-4I-C-04): a caller-supplied
// LocationSignalState outside the documented closed set must never be
// echoed verbatim into the recorded diagnostic - it normalizes to
// LocationUnavailable, and the decision fails closed regardless.
func TestDetermine_UnrecognizedLocationSignalStateNormalizesToUnavailable(t *testing.T) {
	maxAge := 24 * time.Hour
	verified := &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()}
	ls := &LocationSignalEvidence{State: LocationSignalState("bogus_state"), ObservedAt: asOfFixture.Add(-time.Minute)}
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose:  PurposeMarketAccessControl,
		Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: ls},
		Policy:   EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
		AsOf:     asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Unresolved || res.Reason() != ReasonLocationSignalUnusable {
		t.Fatalf("expected unresolved(location_signal_unusable), got %s(%s)", res.Outcome(), res.Reason())
	}
	if res.LocationSignalState() != LocationUnavailable {
		t.Fatalf("an unrecognized caller-supplied state must normalize to LocationUnavailable, got %s", res.LocationSignalState())
	}
}

// PHASE-C fix round (architect P2-1): the location ConsideredEvidence entry
// - including its EvidenceRef - must be present on the REQUIRED-and-unusable
// path, not only the advisory-and-unusable path: this is the exact path that
// denies a player, and an incident investigator needs the evidence
// reference to determine which vendor call actually failed.
func TestDetermine_LocationRequiredAndUnusableStillRecordsConsideredEvidence(t *testing.T) {
	maxAge := 24 * time.Hour
	verified := &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()}
	ls := &LocationSignalEvidence{State: LocationUnavailable, ObservedAt: asOfFixture.Add(-time.Minute), ProviderID: "p", ProviderReference: "r"}
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose:  PurposeMarketAccessControl,
		Evidence: EvidenceSet{VerifiedResidence: verified, LocationSignal: ls},
		Policy:   EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &maxAge},
		AsOf:     asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Unresolved || res.Reason() != ReasonLocationSignalUnusable {
		t.Fatalf("expected unresolved(location_signal_unusable), got %s(%s)", res.Outcome(), res.Reason())
	}
	found := false
	for _, c := range res.ConsideredEvidence() {
		if c.Basis == BasisGeoSignal && c.Status == StatusUnavailable {
			if c.Ref.ProviderID != "p" || c.Ref.ProviderReference != "r" {
				t.Fatalf("expected the geo-signal considered-evidence entry to carry its EvidenceRef, got %+v", c.Ref)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected a location considered-evidence entry on the required-and-unusable path")
	}
}

// PHASE-C fix round (security SEC-4I-C-05): a structurally invalid or empty
// declared-residence value must never be recorded as "disagreeing" with a
// valid verified residence - that would be a false-positive fraud/review
// signal for every player whose declared-residence row is blank or malformed.
func TestDetermine_InvalidDeclaredResidenceNeverTriggersDisagreementWithVerified(t *testing.T) {
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeIdentityDetermination,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture.Add(-time.Hour), VerificationID: uuid.New()},
			DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "", CapturedAt: asOfFixture.Add(-2 * time.Hour), PlayerAccountID: uuid.New()},
		},
		AsOf: asOfFixture,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.HasDisagreement() {
		t.Fatal("an invalid/empty declared residence must never set HasDisagreement()")
	}
	found := false
	for _, c := range res.ConsideredEvidence() {
		if c.Basis == BasisPlayerDeclaredResidence {
			if c.Status != StatusInvalid {
				t.Fatalf("expected the declared-residence entry status=invalid, got %s", c.Status)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected an invalid considered-evidence entry for declared residence")
	}
}

func TestDetermine_HistoricalReportingIsReadFromTheRecordNeverRecomputed(t *testing.T) {
	evidenceOptions := []EvidenceSet{
		{},
		{VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOfFixture, VerificationID: uuid.New()}},
		{DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "DE", CapturedAt: asOfFixture, PlayerAccountID: uuid.New()}},
		{LocationSignal: &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOfFixture}},
	}
	for _, ev := range evidenceOptions {
		_, err := DeterminePlayerJurisdiction(DetermineParams{Purpose: PurposeHistoricalReporting, Evidence: ev, AsOf: asOfFixture})
		if !errors.Is(err, ErrHistoricalPurposeNotComputable) {
			t.Fatalf("expected ErrHistoricalPurposeNotComputable regardless of evidence, got %v", err)
		}
	}
}
