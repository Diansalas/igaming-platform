package jurisdiction

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/validation"
)

// This file implements Stage 4I Phase C's player-jurisdiction precedence
// algorithm (docs/governance/stage-4i-canonical-model.md,
// docs/decisions/0042-human-decision-response.md HDR-J-2/HDR-J-3a/HDR-J-4).
//
// DeterminePlayerJurisdiction is a PURE function: no context.Context, no
// database handle, no I/O of any kind. It never calls time.Now() - AsOf is
// the only source of "now", supplied by the caller. This package does not
// consult identity.GetDeclaredResidence or kyc.GetVerifiedResidence itself
// (see EvidenceSet's own doc comment, evidence.go) - a future adapter reads
// those accessors and builds an EvidenceSet, then calls this function.
// There are deliberately zero production callers of this function yet:
// this phase builds the precedence RULE ENGINE only, not its wiring into
// internal/casino, internal/bonus, internal/risk, or resolver.go, all of
// which remain at zero diff.

// LocationRequirement is whether a physical-location signal is required,
// merely advisory, or (the zero value) not yet decided for a given
// evaluation.
type LocationRequirement string

const (
	// LocationRequirementUnset is the zero value. It is DELIBERATELY not a
	// usable default: a caller must explicitly choose LocationRequired or
	// LocationAdvisory before PurposeMarketAccessControl can evaluate a
	// resolved residence dimension's location dimension - this is
	// PC-GAP-1/PC-GAP-2 territory (which decisions require a fresh location
	// check and how strictly), an unmade human/legal decision this package
	// must never guess at by picking a "reasonable" default.
	LocationRequirementUnset LocationRequirement = ""
	LocationRequired         LocationRequirement = "required"
	LocationAdvisory         LocationRequirement = "advisory"
)

// EvaluationPolicy carries the human/legal-decision-gated knobs
// DeterminePlayerJurisdiction needs for PurposeMarketAccessControl. It is
// never consulted at all for PurposeIdentityDetermination (identity
// determination's residence-only rule ordering needs no location policy).
type EvaluationPolicy struct {
	// LocationSignalRequirement's zero value (LocationRequirementUnset) is
	// INTENTIONAL - see that constant's own doc comment.
	LocationSignalRequirement LocationRequirement
	// MaxLocationSignalAge is nil = UNSET. A nil value is not treated as
	// "no freshness limit" - it fails closed with ErrPolicyUnset instead,
	// for the same PC-GAP-1/PC-GAP-2 reason as LocationRequirementUnset.
	//
	// PHASE-C fix round (security SEC-4I-C-06, non-blocking note): a
	// pointer to a ZERO Duration is a real footgun, distinct from nil - it
	// means "fresh only at age == 0", i.e. only a signal observed at
	// exactly AsOf passes, because the freshness check below is inclusive
	// of the boundary (age == *MaxLocationSignalAge counts as fresh). This
	// is almost certainly never what a caller building this policy from
	// config actually wants; there is no code-level fix for a caller
	// supplying a deliberately-or-accidentally-zero duration, since zero is
	// a structurally valid *time.Duration value distinct from nil.
	MaxLocationSignalAge *time.Duration
}

// DetermineParams is DeterminePlayerJurisdiction's input.
type DetermineParams struct {
	Purpose  Purpose
	Evidence EvidenceSet
	Policy   EvaluationPolicy
	// AsOf is supplied by the caller; this function NEVER calls
	// time.Now().
	AsOf time.Time
}

var (
	// ErrUnsupportedPurpose is returned for any Purpose value other than
	// the three declared constants (purpose.go).
	ErrUnsupportedPurpose = errors.New("jurisdiction: unsupported purpose")
	// ErrHistoricalPurposeNotComputable is returned unconditionally for
	// PurposeHistoricalReporting: a historical-reporting jurisdiction must
	// be read from the event-time record, never recomputed from current
	// evidence (HDR-J-2/HDR-J-4).
	ErrHistoricalPurposeNotComputable = errors.New("jurisdiction: historical-reporting jurisdiction must be read from the event-time record, never recomputed from current evidence")
	// ErrPolicyUnset is returned when an EvaluationPolicy field required
	// for this purpose is unset - PC-GAP-1/PC-GAP-2, a human/legal
	// decision required before this can be answered.
	ErrPolicyUnset = errors.New("jurisdiction: evaluation policy field required for this purpose is unset - PC-GAP-1/PC-GAP-2, a human/legal decision is required before this can be answered")
)

// DeterminePlayerJurisdiction resolves a player's jurisdiction for a given
// Purpose from a caller-supplied EvidenceSet, per the exact precedence
// rules HDR-J-2/HDR-J-3a specify. See this file's own package-level doc
// comment for the purity guarantee, and precedence_invariants_test.go /
// precedence_test.go for the full behavioural contract.
func DeterminePlayerJurisdiction(p DetermineParams) (PlayerJurisdictionResult, error) {
	if p.Purpose == PurposeHistoricalReporting {
		return PlayerJurisdictionResult{}, ErrHistoricalPurposeNotComputable
	}
	if !validPurpose(p.Purpose) {
		return PlayerJurisdictionResult{}, ErrUnsupportedPurpose
	}
	// PHASE-C fix round (architect P1-2): a zero AsOf silently disables the
	// location-freshness gate entirely (asOf.Sub(observedAt) becomes hugely
	// negative, so every signal reads as "fresh"). AsOf is a mandatory
	// input with no other validation; a forgotten struct field is a
	// realistic caller bug, so it is rejected explicitly rather than
	// trusted.
	if p.AsOf.IsZero() {
		return PlayerJurisdictionResult{}, fmt.Errorf("%w: AsOf is required and must not be the zero time", ErrInvalidInput)
	}
	if p.Purpose == PurposeIdentityDetermination {
		return determineIdentity(p.Evidence, p.AsOf), nil
	}
	return determineMarketAccess(p.Evidence, p.Policy, p.AsOf)
}

// determineIdentity implements PurposeIdentityDetermination. A location
// signal, if supplied, is recorded as considered-but-inapplicable and is
// NEVER built into a candidate - per HDR-J-3a, location is not an
// identity/residence signal. p.Policy is never consulted for this purpose.
func determineIdentity(ev EvidenceSet, asOf time.Time) PlayerJurisdictionResult {
	result := PlayerJurisdictionResult{
		purpose: PurposeIdentityDetermination, asOf: asOf, policyVersion: PrecedencePolicyVersion,
		locationState: LocationNotConsulted,
	}

	if ev.LocationSignal != nil {
		ls := ev.LocationSignal
		result.locationState = ls.State
		result.considered = append(result.considered, ConsideredEvidence{
			Basis: BasisGeoSignal, Status: StatusInapplicable, ObservedAt: ls.ObservedAt,
			Ref: EvidenceRef{Kind: EvidenceRefLocationSignal, ID: uuid.Nil, ProviderID: ls.ProviderID, ProviderReference: ls.ProviderReference},
		})
	}

	cand, considered, disagreement, outcome, reason := evaluateResidenceDimension(ev)
	result.considered = append(result.considered, considered...)
	result.disagreement = disagreement
	if outcome != Resolved && reason == ReasonNoSignal && ev.VerifiedResidence == nil && ev.DeclaredResidence == nil && ev.LocationSignal != nil {
		// PHASE-C fix round (architect P2-2): a location signal was the
		// ONLY evidence supplied - this purpose never treats location as a
		// residence signal (HDR-J-3a), so distinguish "we had irrelevant
		// evidence" from "we had nothing at all", exactly as
		// determineMarketAccess already does for the identical case.
		reason = ReasonNoApplicableEvidence
	}
	result.outcome = outcome
	result.reason = reason
	if cand != nil {
		result.candidates = []Candidate{*cand}
	}
	return result
}

// determineMarketAccess implements PurposeMarketAccessControl: the
// residence dimension (identical rules to determineIdentity's residence
// evaluation) plus, only when the residence dimension resolves, a location
// dimension that may append an additional-restriction candidate.
func determineMarketAccess(ev EvidenceSet, policy EvaluationPolicy, asOf time.Time) (PlayerJurisdictionResult, error) {
	result := PlayerJurisdictionResult{
		purpose: PurposeMarketAccessControl, asOf: asOf, policyVersion: PrecedencePolicyVersion,
		locationState: LocationNotConsulted,
	}

	cand, considered, disagreement, outcome, reason := evaluateResidenceDimension(ev)
	result.considered = append(result.considered, considered...)
	result.disagreement = disagreement

	if outcome != Resolved {
		// Location can never substitute for an unresolved residence
		// determination - it is recorded, never used as the basis.
		if ev.LocationSignal != nil {
			ls := ev.LocationSignal
			result.locationState = ls.State
			result.considered = append(result.considered, ConsideredEvidence{
				Basis: BasisGeoSignal, Status: StatusInapplicable, ObservedAt: ls.ObservedAt,
				Ref: EvidenceRef{Kind: EvidenceRefLocationSignal, ID: uuid.Nil, ProviderID: ls.ProviderID, ProviderReference: ls.ProviderReference},
			})
			if reason == ReasonNoSignal && ev.VerifiedResidence == nil && ev.DeclaredResidence == nil {
				// A location signal was the ONLY evidence supplied -
				// distinguish "we had irrelevant evidence" from "we had
				// nothing at all".
				reason = ReasonNoApplicableEvidence
			}
		}
		result.outcome = Unresolved
		result.reason = reason
		return result, nil
	}

	// The residence dimension resolved - evaluate the location dimension.
	if policy.LocationSignalRequirement == LocationRequirementUnset {
		return PlayerJurisdictionResult{}, ErrPolicyUnset
	}
	if policy.LocationSignalRequirement != LocationRequired && policy.LocationSignalRequirement != LocationAdvisory {
		// PHASE-C fix round (architect P1-1): every string-backed enum in
		// this package fails closed via an exhaustive switch with a
		// rejecting default - no permissive fallthrough branch, ever. An
		// unrecognized value (wrong case, a stray space, a stale config
		// value) must never silently behave like the more permissive
		// LocationAdvisory.
		return PlayerJurisdictionResult{}, fmt.Errorf("%w: unrecognized LocationSignalRequirement %q", ErrInvalidInput, policy.LocationSignalRequirement)
	}

	var effectiveState LocationSignalState
	switch {
	case ev.LocationSignal == nil:
		effectiveState = LocationNotConsulted
	case ev.LocationSignal.State == LocationObserved:
		if policy.MaxLocationSignalAge == nil {
			return PlayerJurisdictionResult{}, ErrPolicyUnset
		}
		age := asOf.Sub(ev.LocationSignal.ObservedAt)
		// PHASE-C fix round (security SEC-4I-C-03): a future-dated
		// ObservedAt (age < 0) must NEVER read as "definitely fresh" -
		// without this check a signal timestamped arbitrarily far in the
		// future would satisfy age > MaxLocationSignalAge forever, which
		// defeats the entire purpose of a freshness bound on "real-time"
		// location (HDR-J-2). The boundary age == MaxLocationSignalAge
		// counts as fresh (inclusive) - only a strictly greater (or
		// negative) age is stale. The caller's input is never mutated;
		// LocationStale is used only for this function's own
		// decision-making.
		if age < 0 || age > *policy.MaxLocationSignalAge {
			effectiveState = LocationStale
		} else {
			effectiveState = LocationObserved
		}
	case ev.LocationSignal.State == LocationInconclusive, ev.LocationSignal.State == LocationUnavailable,
		ev.LocationSignal.State == LocationProviderError, ev.LocationSignal.State == LocationStale,
		ev.LocationSignal.State == LocationNotConsulted:
		effectiveState = ev.LocationSignal.State
	default:
		// PHASE-C fix round (security SEC-4I-C-04): an unrecognized
		// caller-supplied state must never be echoed verbatim into the
		// recorded diagnostic - a future persistence/reporting phase would
		// otherwise see values outside its documented closed set. The
		// DECISION already fails closed regardless (this function only
		// ever treats it as unusable); this normalizes the diagnostic too.
		effectiveState = LocationUnavailable
	}

	usable := effectiveState == LocationObserved && validation.IsISO3166Alpha2(ev.LocationSignal.CountryCode)
	invalidObservedCode := effectiveState == LocationObserved && !usable

	if usable {
		ls := ev.LocationSignal
		result.candidates = []Candidate{*cand, {
			code:       newPlayerJurisdictionCode(ls.CountryCode),
			basis:      BasisGeoSignal,
			confidence: ConfidenceCorroborated,
			role:       RoleAdditionalRestriction,
			evidenceAt: ls.ObservedAt,
			ref:        EvidenceRef{Kind: EvidenceRefLocationSignal, ID: uuid.Nil, ProviderID: ls.ProviderID, ProviderReference: ls.ProviderReference},
		}}
		result.outcome = Resolved
		result.reason = ReasonDetermined
		result.locationState = LocationObserved
		return result, nil
	}

	// Unusable: not supplied at all, inconclusive/unavailable/provider-
	// errored, stale, or observed-but-invalid-code.
	result.locationState = effectiveState

	// PHASE-C fix round (architect P2-1): record the location
	// ConsideredEvidence entry - including its EvidenceRef - on BOTH the
	// required-and-unusable and advisory-and-unusable paths. Canonical-
	// model §5.3's rule is "persist the decision and REFERENCES to its
	// evidence"; dropping this entry on the Required path (as an earlier
	// revision did) discarded the one thing an incident investigator
	// needs to determine which vendor call actually failed, on the exact
	// path that denies a player.
	status := StatusUnavailable
	if invalidObservedCode {
		status = StatusInvalid
	}
	entry := ConsideredEvidence{Basis: BasisGeoSignal, Status: status}
	if ev.LocationSignal != nil {
		entry.ObservedAt = ev.LocationSignal.ObservedAt
		entry.Ref = EvidenceRef{Kind: EvidenceRefLocationSignal, ID: uuid.Nil, ProviderID: ev.LocationSignal.ProviderID, ProviderReference: ev.LocationSignal.ProviderReference}
	} else {
		entry.Ref = EvidenceRef{Kind: EvidenceRefLocationSignal, ID: uuid.Nil}
	}
	result.considered = append(result.considered, entry)

	if policy.LocationSignalRequirement == LocationRequired {
		// Location may impose an additional restriction, never substitute
		// - but if required and unusable, the operation cannot proceed:
		// the WHOLE result becomes unresolved, discarding the primary
		// candidate that was computed.
		result.candidates = nil
		result.outcome = Unresolved
		result.reason = ReasonLocationSignalUnusable
		return result, nil
	}

	// LocationAdvisory: keep the Resolved result with the single
	// (residence) candidate.
	result.candidates = []Candidate{*cand}
	result.outcome = Resolved
	result.reason = ReasonDetermined
	return result, nil
}

// evaluateResidenceDimension implements the residence-precedence rules
// shared verbatim by PurposeIdentityDetermination and the residence
// dimension of PurposeMarketAccessControl: verified residence is
// authoritative when valid; an invalid verified value is NEVER silently
// demoted to declared residence; declared residence alone is insufficient
// for either purpose; nothing present yields no_signal.
func evaluateResidenceDimension(ev EvidenceSet) (cand *Candidate, considered []ConsideredEvidence, disagreement bool, outcome Outcome, reason Reason) {
	switch {
	case ev.VerifiedResidence != nil && validation.IsISO3166Alpha2(ev.VerifiedResidence.CountryCode):
		v := ev.VerifiedResidence
		c := Candidate{
			code:       newPlayerJurisdictionCode(v.CountryCode),
			basis:      BasisPlayerVerifiedResidence,
			confidence: ConfidenceVerified,
			role:       RolePrimaryDetermination,
			evidenceAt: v.SetAt,
			ref:        EvidenceRef{Kind: EvidenceRefKYCVerification, ID: v.VerificationID},
		}
		if ev.DeclaredResidence != nil {
			d := ev.DeclaredResidence
			switch {
			case !validation.IsISO3166Alpha2(d.CountryCode):
				// PHASE-C fix round (security SEC-4I-C-05): a structurally
				// invalid or empty declared-residence value must never be
				// recorded as "disagreeing" with verified residence - that
				// would be indistinguishable from a genuine contradiction
				// (e.g. MT vs. DE) and could trigger a false-positive
				// fraud/review signal for every player whose declared-
				// residence row happens to be blank or malformed.
				considered = append(considered, ConsideredEvidence{
					Basis: BasisPlayerDeclaredResidence, Status: StatusInvalid, ObservedAt: d.CapturedAt,
					Ref: EvidenceRef{Kind: EvidenceRefPlayerAccount, ID: d.PlayerAccountID},
				})
			case d.CountryCode != v.CountryCode:
				considered = append(considered, ConsideredEvidence{
					Basis: BasisPlayerDeclaredResidence, Status: StatusDisagreed, ObservedAt: d.CapturedAt,
					Ref: EvidenceRef{Kind: EvidenceRefPlayerAccount, ID: d.PlayerAccountID},
				})
				disagreement = true
			default:
				considered = append(considered, ConsideredEvidence{
					Basis: BasisPlayerDeclaredResidence, Status: StatusRejectedLowerPrecedence, ObservedAt: d.CapturedAt,
					Ref: EvidenceRef{Kind: EvidenceRefPlayerAccount, ID: d.PlayerAccountID},
				})
			}
		}
		return &c, considered, disagreement, Resolved, ReasonDetermined

	case ev.VerifiedResidence != nil:
		// Present but structurally invalid - never falls through to
		// declared residence.
		v := ev.VerifiedResidence
		considered = append(considered, ConsideredEvidence{
			Basis: BasisPlayerVerifiedResidence, Status: StatusInvalid, ObservedAt: v.SetAt,
			Ref: EvidenceRef{Kind: EvidenceRefKYCVerification, ID: v.VerificationID},
		})
		return nil, considered, false, Unresolved, ReasonEvidenceInvalid

	case ev.DeclaredResidence != nil && validation.IsISO3166Alpha2(ev.DeclaredResidence.CountryCode):
		d := ev.DeclaredResidence
		considered = append(considered, ConsideredEvidence{
			Basis: BasisPlayerDeclaredResidence, Status: StatusRejectedLowerPrecedence, ObservedAt: d.CapturedAt,
			Ref: EvidenceRef{Kind: EvidenceRefPlayerAccount, ID: d.PlayerAccountID},
		})
		return nil, considered, false, Unresolved, ReasonInsufficientConfidence

	case ev.DeclaredResidence != nil:
		d := ev.DeclaredResidence
		considered = append(considered, ConsideredEvidence{
			Basis: BasisPlayerDeclaredResidence, Status: StatusInvalid, ObservedAt: d.CapturedAt,
			Ref: EvidenceRef{Kind: EvidenceRefPlayerAccount, ID: d.PlayerAccountID},
		})
		return nil, considered, false, Unresolved, ReasonEvidenceInvalid

	default:
		return nil, nil, false, Unresolved, ReasonNoSignal
	}
}
