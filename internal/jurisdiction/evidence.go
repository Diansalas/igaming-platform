package jurisdiction

import (
	"time"

	"github.com/google/uuid"
)

// LocationSignalState is the closed set of states a physical-location
// signal can be in when handed to DeterminePlayerJurisdiction
// (precedence.go). Every state other than LocationObserved means "this
// signal cannot be used as a positive input" - the caller still supplies
// which specific unusable state applied, since ReasonLocationSignalUnusable
// and the recorded ConsideredEvidence entry both need it for diagnosis.
type LocationSignalState string

const (
	LocationObserved      LocationSignalState = "observed"
	LocationInconclusive  LocationSignalState = "inconclusive"
	LocationUnavailable   LocationSignalState = "unavailable"
	LocationProviderError LocationSignalState = "provider_error"
	// LocationStale is engine-derived from precedence.go's own freshness
	// check against EvaluationPolicy.MaxLocationSignalAge - it is NEVER
	// caller-supplied.
	LocationStale LocationSignalState = "stale"
	// LocationNotConsulted means no signal was supplied at all
	// (EvidenceSet.LocationSignal == nil).
	LocationNotConsulted LocationSignalState = "not_consulted"
)

// VerifiedResidenceEvidence is a KYC-verified residence fact, mirroring
// kyc.GetVerifiedResidence's own shape.
type VerifiedResidenceEvidence struct {
	CountryCode    string
	SetAt          time.Time
	VerificationID uuid.UUID
}

// DeclaredResidenceEvidence is a player-declared residence fact, mirroring
// identity.GetDeclaredResidence's own shape.
type DeclaredResidenceEvidence struct {
	CountryCode     string
	CapturedAt      time.Time
	PlayerAccountID uuid.UUID
}

// LocationSignalEvidence is a point-in-time physical-location signal.
// CountryCode is meaningful ONLY when State == LocationObserved.
type LocationSignalEvidence struct {
	State             LocationSignalState
	CountryCode       string
	ObservedAt        time.Time
	ProviderID        string
	ProviderReference string
}

// EvidenceSet is DeterminePlayerJurisdiction's evidence input. It has
// EXACTLY three fields - never add a fourth without deliberately breaking
// precedence_invariants_test.go's reflect-based tripwire test
// (TestEvidenceSet_ExposesNoTenantBrandOrLicenceInput).
//
// A nil pointer means the corresponding evidence is ABSENT. This maps
// directly onto identity.GetDeclaredResidence's and kyc.GetVerifiedResidence's
// existing `ok bool` return - both already distinguish "no fact on file"
// from an empty string. A future adapter wiring real database reads into
// this struct must preserve that distinction exactly, never collapse
// "absent" and "empty string" into the same nil-vs-zero-value shape by
// mistake.
//
// This package does NOT call those accessors itself: it has no database
// handle (DeterminePlayerJurisdiction is a pure function - see
// precedence.go's own doc comment) and importing internal/identity or
// internal/kyc from here would create an import cycle risk this package's
// platform-core position must not carry.
type EvidenceSet struct {
	VerifiedResidence *VerifiedResidenceEvidence
	DeclaredResidence *DeclaredResidenceEvidence
	LocationSignal    *LocationSignalEvidence
}
