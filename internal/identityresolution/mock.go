package identityresolution

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// MockPersonResolver is the only PersonResolver implementation this stage
// ships - there is no real identity-resolution vendor contracted (ADR
// 0027 §3). Its DEFAULT behavior for any input carrying no verified
// attributes (VerifiedAttributes.IsEmpty()) is NoMatch - this is not a
// simplification for testing convenience, it is the honest, correct
// answer given today's registration flow collects no verified evidence
// at all: with nothing to match against, "no match" is the only truthful
// outcome, never a silent Uncertain or a fabricated Match.
//
// Tests configure specific outcomes for specific GovernmentIDReference
// values via SetMatch/SetUncertain, and can force every subsequent call
// to fail via SetUnavailable - exactly mirroring MockCasinoProvider's
// "magic value" testing convention (docs/decisions/0025), never a real
// matching ALGORITHM (directive §10's "do not implement deterministic
// matching rules unless explicitly justified" - the only "rule" here is
// an exact-match lookup table a TEST populates, standing in for whatever
// a real vendor's own black-box matching would decide).
type MockPersonResolver struct {
	mu               sync.Mutex
	matchByGovID     map[string]uuid.UUID
	uncertainGovID   map[string]bool
	forceUnavailable bool
}

// NewMockPersonResolver returns a MockPersonResolver with no
// preconfigured outcomes - every call resolves NoMatch until a test
// configures otherwise.
func NewMockPersonResolver() *MockPersonResolver {
	return &MockPersonResolver{
		matchByGovID:   make(map[string]uuid.UUID),
		uncertainGovID: make(map[string]bool),
	}
}

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind.
func (m *MockPersonResolver) SyntheticComponent() {}

// SetMatch configures Resolve to return Match/personID for any input
// whose VerifiedAttributes.GovernmentIDReference equals govIDRef.
func (m *MockPersonResolver) SetMatch(govIDRef string, personID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.matchByGovID[govIDRef] = personID
}

// SetUncertain configures Resolve to return Uncertain for any input
// whose VerifiedAttributes.GovernmentIDReference equals govIDRef.
func (m *MockPersonResolver) SetUncertain(govIDRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uncertainGovID[govIDRef] = true
}

// SetUnavailable makes every subsequent Resolve call return
// ErrResolverUnavailable, simulating a vendor outage - directive §9's
// "provider unavailable" scenario. Pass false to clear it.
func (m *MockPersonResolver) SetUnavailable(unavailable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceUnavailable = unavailable
}

func (m *MockPersonResolver) Resolve(ctx context.Context, input ResolutionInput) (ResolutionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.forceUnavailable {
		return ResolutionResult{}, fmt.Errorf("%w: mock resolver forced unavailable", ErrResolverUnavailable)
	}

	if input.Verified.IsEmpty() {
		// The honest, expected case for every registration today - see
		// this type's own doc comment.
		return ResolutionResult{Outcome: NoMatch, Reason: "no_verified_attributes"}, nil
	}

	key := input.Verified.GovernmentIDReference
	if key == "" {
		// Verified attributes exist but carry no government id reference
		// (the only field this mock keys on) - a real resolver might
		// still reach a decision from other fields, but this mock cannot,
		// so it reports Uncertain rather than guessing NoMatch (directive
		// §5: never force an unclear result into a definite one).
		return ResolutionResult{Outcome: Uncertain, Reason: "no_configured_matching_signal"}, nil
	}
	if m.uncertainGovID[key] {
		return ResolutionResult{Outcome: Uncertain, Reason: "configured_uncertain"}, nil
	}
	if personID, ok := m.matchByGovID[key]; ok {
		return ResolutionResult{Outcome: Match, MatchedPersonID: personID, Reason: "government_id_reference_match"}, nil
	}
	return ResolutionResult{Outcome: NoMatch, Reason: "government_id_reference_not_found"}, nil
}
