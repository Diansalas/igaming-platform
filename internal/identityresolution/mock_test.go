package identityresolution

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestMockPersonResolver_NoVerifiedAttributes_ResolvesNoMatch(t *testing.T) {
	m := NewMockPersonResolver()
	result, err := m.Resolve(context.Background(), ResolutionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != NoMatch {
		t.Errorf("expected NoMatch for empty verified attributes, got %q", result.Outcome)
	}
}

func TestMockPersonResolver_VerifiedButNoGovIDReference_ResolvesUncertain(t *testing.T) {
	m := NewMockPersonResolver()
	result, err := m.Resolve(context.Background(), ResolutionInput{
		Verified: VerifiedAttributes{LegalName: "Jane Doe"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Uncertain {
		t.Errorf("expected Uncertain when no configured matching signal is present, got %q", result.Outcome)
	}
}

func TestMockPersonResolver_ConfiguredMatch_ResolvesMatch(t *testing.T) {
	m := NewMockPersonResolver()
	personID := uuid.New()
	m.SetMatch("gov-ref-1", personID)

	result, err := m.Resolve(context.Background(), ResolutionInput{
		Verified: VerifiedAttributes{GovernmentIDReference: "gov-ref-1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Match {
		t.Fatalf("expected Match, got %q", result.Outcome)
	}
	if result.MatchedPersonID != personID {
		t.Errorf("expected matched person id %s, got %s", personID, result.MatchedPersonID)
	}
}

func TestMockPersonResolver_UnconfiguredGovIDReference_ResolvesNoMatch(t *testing.T) {
	m := NewMockPersonResolver()
	result, err := m.Resolve(context.Background(), ResolutionInput{
		Verified: VerifiedAttributes{GovernmentIDReference: "unknown-ref"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != NoMatch {
		t.Errorf("expected NoMatch for an unconfigured government id reference, got %q", result.Outcome)
	}
}

func TestMockPersonResolver_ConfiguredUncertain_ResolvesUncertain(t *testing.T) {
	m := NewMockPersonResolver()
	m.SetUncertain("gov-ref-ambiguous")

	result, err := m.Resolve(context.Background(), ResolutionInput{
		Verified: VerifiedAttributes{GovernmentIDReference: "gov-ref-ambiguous"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != Uncertain {
		t.Errorf("expected Uncertain, got %q", result.Outcome)
	}
}

func TestMockPersonResolver_ForcedUnavailable_ReturnsWrappedSentinel(t *testing.T) {
	m := NewMockPersonResolver()
	m.SetUnavailable(true)

	_, err := m.Resolve(context.Background(), ResolutionInput{
		Verified: VerifiedAttributes{GovernmentIDReference: "gov-ref-1"},
	})
	if !errors.Is(err, ErrResolverUnavailable) {
		t.Fatalf("expected an error wrapping ErrResolverUnavailable, got: %v", err)
	}

	// Clearing it restores normal resolution.
	m.SetUnavailable(false)
	result, err := m.Resolve(context.Background(), ResolutionInput{})
	if err != nil {
		t.Fatalf("unexpected error after clearing unavailability: %v", err)
	}
	if result.Outcome != NoMatch {
		t.Errorf("expected NoMatch after clearing unavailability, got %q", result.Outcome)
	}
}

func TestVerifiedAttributes_IsEmpty(t *testing.T) {
	var empty VerifiedAttributes
	if !empty.IsEmpty() {
		t.Error("expected zero-value VerifiedAttributes to be empty")
	}
	nonEmpty := VerifiedAttributes{Email: "a@example.com"}
	if nonEmpty.IsEmpty() {
		t.Error("expected VerifiedAttributes with a field set to not be empty")
	}
}
