package jurisdiction

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// PrecedencePolicyVersion identifies the Stage 4I Phase C precedence
// ALGORITHM in precedence.go (DeterminePlayerJurisdiction), distinct from
// the existing PolicyVersion constant (resolver.go's tenant/brand-subject
// resolver logic) - the two are versioned independently and neither
// overloads the other.
const PrecedencePolicyVersion = "stage-4i-phase-c.v1"

// PlayerJurisdictionCode is a resolved player jurisdiction code. It is a
// STRUCT with an unexported field, deliberately NOT a named string type.
//
// A named string type (`type PlayerJurisdictionCode string`) would permit
// PlayerJurisdictionCode(someTenantCodeString) - a bare conversion that
// launders a tenant-licence code (or any other string) into
// player-jurisdiction shape with no evidence behind it at all. Because the
// field is unexported, this makes it structurally impossible to construct
// a NON-EMPTY PlayerJurisdictionCode outside this package except via the
// unexported newPlayerJurisdictionCode constructor, which is used only by
// the candidate-building code in precedence.go, whose only string sources
// are EvidenceSet's three fields. (Correction, PHASE-C fix round, architect
// P3-2: the EMPTY composite literal PlayerJurisdictionCode{} does compile
// from outside the package - Go permits a field-less struct literal even
// with unexported fields - so callers must use IsSet() below rather than
// assume "externally constructed" implies "non-empty".)
type PlayerJurisdictionCode struct{ code string }

func newPlayerJurisdictionCode(code string) PlayerJurisdictionCode {
	return PlayerJurisdictionCode{code: code}
}

func (c PlayerJurisdictionCode) String() string { return c.code }

// GoString implements fmt.GoStringer so the %#v verb - which bypasses
// Stringer entirely and otherwise dumps unexported field values in full -
// cannot be used to defeat this type's redaction. See Candidate.String's
// own doc comment for why this matters.
func (c PlayerJurisdictionCode) GoString() string {
	return "jurisdiction.PlayerJurisdictionCode{<redacted>}"
}

// IsSet reports whether this code carries a real value, as distinct from
// the zero value PlayerJurisdictionCode{} (which - see the type's own doc
// comment - IS externally constructible, unlike a non-empty code). No
// caller should treat an unset code as a wildcard or "any jurisdiction".
func (c PlayerJurisdictionCode) IsSet() bool { return c.code != "" }

// CandidateRole distinguishes the one primary determination a resolved
// result carries from any additional restriction candidates appended
// after it (Stage 4I Phase C, HDR-J-2).
type CandidateRole string

const (
	RolePrimaryDetermination  CandidateRole = "primary_determination"
	RoleAdditionalRestriction CandidateRole = "additional_restriction"
)

// EvidenceRefKind is the closed set of evidence-reference kinds a
// Candidate or ConsideredEvidence entry can point at.
type EvidenceRefKind string

const (
	EvidenceRefPlayerAccount   EvidenceRefKind = "player_account"
	EvidenceRefKYCVerification EvidenceRefKind = "kyc_verification"
	EvidenceRefLocationSignal  EvidenceRefKind = "location_signal"
)

// EvidenceRef is a REFERENCE to the evidence that produced a candidate or
// considered-evidence entry - never the evidentiary value itself
// (canonical-model §5.3).
type EvidenceRef struct {
	Kind EvidenceRefKind
	// ID is uuid.Nil for a location signal - no persisted row exists for
	// one.
	ID uuid.UUID
	// ProviderID/ProviderReference are meaningful for location evidence
	// only - an opaque vendor reference, never a persisted row id.
	ProviderID        string
	ProviderReference string
}

// Candidate is one resolved determination or additional restriction. All
// fields are unexported - see String()'s own doc comment for why.
type Candidate struct {
	code       PlayerJurisdictionCode
	basis      Basis
	confidence ConfidenceClass
	role       CandidateRole
	evidenceAt time.Time
	ref        EvidenceRef
}

func (c Candidate) Code() PlayerJurisdictionCode { return c.code }
func (c Candidate) Basis() Basis                 { return c.basis }
func (c Candidate) Confidence() ConfidenceClass  { return c.confidence }
func (c Candidate) Role() CandidateRole          { return c.role }
func (c Candidate) EvidenceAt() time.Time        { return c.evidenceAt }
func (c Candidate) EvidenceRef() EvidenceRef     { return c.ref }

// String returns a REDACTED representation - it must NEVER print
// c.code.code (canonical-model §5.3: a resolved candidate's code IS the
// decision and is fine to expose via Code(), but casual log/Printf/error-
// wrapping call sites must not accidentally leak it through fmt's default
// verb behaviour on an unexported-field struct).
func (c Candidate) String() string {
	return fmt.Sprintf("Candidate{basis=%s, role=%s, confidence=%s}", c.basis, c.role, c.confidence)
}

// GoString implements fmt.GoStringer - the %#v verb bypasses Stringer and
// otherwise dumps c.code.code (and every other unexported field) in full,
// defeating String()'s redaction. This is not a hypothetical: independent
// review confirmed %#v genuinely leaks the country code before this method
// existed.
func (c Candidate) GoString() string {
	return fmt.Sprintf("jurisdiction.Candidate{basis:%q, role:%q, confidence:%q, code:<redacted>}", c.basis, c.role, c.confidence)
}

// ConsideredEvidence records that a basis was considered and what happened
// to it during a player-jurisdiction determination - NEVER the country
// code it held. There is deliberately NO code field, and this struct must
// never be extended with one: anything recorded here was NOT the decision
// (rejected/disagreeing/invalid/inapplicable/unavailable), and canonical-
// model §5.3's rule is "persist/log the DECISION and REFERENCES to
// evidence, never the evidence VALUES" - making this struct un-loggable-
// with-a-code by construction is what enforces that rule here, rather than
// relying on every call site to remember not to populate one.
type ConsideredEvidence struct {
	Basis      Basis
	Status     ConsideredBasisStatus
	ObservedAt time.Time
	Ref        EvidenceRef
}

// PlayerJurisdictionResult is DeterminePlayerJurisdiction's output. All
// fields are unexported - identical non-forgeability rationale to
// Resolution (types.go).
type PlayerJurisdictionResult struct {
	outcome       Outcome
	reason        Reason
	purpose       Purpose
	asOf          time.Time
	policyVersion string
	candidates    []Candidate
	considered    []ConsideredEvidence
	locationState LocationSignalState
	disagreement  bool
}

func (r PlayerJurisdictionResult) Outcome() Outcome      { return r.outcome }
func (r PlayerJurisdictionResult) Reason() Reason        { return r.reason }
func (r PlayerJurisdictionResult) Purpose() Purpose      { return r.purpose }
func (r PlayerJurisdictionResult) AsOf() time.Time       { return r.asOf }
func (r PlayerJurisdictionResult) PolicyVersion() string { return r.policyVersion }

// Candidates returns every resolved candidate, primary first. Returns
// ErrNotResolved (the same sentinel Resolution.Code/Resolution.ID use for
// their identical non-forgeability gate) unless Outcome() == Resolved.
//
// Returns a DEFENSIVE COPY, never the internal backing array (PHASE-C fix
// round, architect P1-3 / security SEC-4I-C-01): independent review
// demonstrated that returning the internal slice directly let a caller
// reorder it in place and have PrimaryCandidate() then report an
// additional-restriction candidate (e.g. a geo signal) as the primary
// determination - defeating exactly the "location never substitutes for
// verified residence" guarantee this type exists to make durable.
func (r PlayerJurisdictionResult) Candidates() ([]Candidate, error) {
	if r.outcome != Resolved {
		return nil, fmt.Errorf("%w: outcome is %q", ErrNotResolved, r.outcome)
	}
	return slices.Clone(r.candidates), nil
}

// PrimaryCandidate returns the primary (first) candidate. Same
// ErrNotResolved gating as Candidates. Returned by value (Candidate has no
// slice/pointer fields of its own), so no separate copy is needed here.
func (r PlayerJurisdictionResult) PrimaryCandidate() (Candidate, error) {
	if r.outcome != Resolved {
		return Candidate{}, fmt.Errorf("%w: outcome is %q", ErrNotResolved, r.outcome)
	}
	if len(r.candidates) == 0 {
		return Candidate{}, fmt.Errorf("jurisdiction: resolved result has no candidates")
	}
	return r.candidates[0], nil
}

// ConsideredEvidence returns a DEFENSIVE COPY of the considered-evidence
// trail, never the internal backing array. ConsideredEvidence's fields are
// exported (unlike Candidate's), so returning the internal slice directly
// would let a caller rewrite the audit trail in place - e.g. relabeling an
// entry's Basis to a value this engine can never itself emit. See
// Candidates()'s own doc comment for the identical class of defect this
// closes.
func (r PlayerJurisdictionResult) ConsideredEvidence() []ConsideredEvidence {
	return slices.Clone(r.considered)
}
func (r PlayerJurisdictionResult) LocationSignalState() LocationSignalState { return r.locationState }
func (r PlayerJurisdictionResult) HasDisagreement() bool                    { return r.disagreement }

// String returns a REDACTED representation: outcome/reason/purpose/
// candidate-count only, never a country code.
func (r PlayerJurisdictionResult) String() string {
	return fmt.Sprintf("PlayerJurisdictionResult{outcome=%s, reason=%s, purpose=%s, candidates=%d}",
		r.outcome, r.reason, r.purpose, len(r.candidates))
}

// GoString implements fmt.GoStringer - see Candidate.GoString's own doc
// comment for why %#v needs this too.
func (r PlayerJurisdictionResult) GoString() string {
	return fmt.Sprintf("jurisdiction.PlayerJurisdictionResult{outcome:%q, reason:%q, purpose:%q, candidates:%d, <redacted>}",
		r.outcome, r.reason, r.purpose, len(r.candidates))
}
