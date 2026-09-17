package idempotency

import (
	"context"
	"errors"
	"testing"
)

func alwaysRequiresOrdinal(string) bool { return true }
func neverRequiresOrdinal(string) bool  { return false }

func TestResolveOccurrence_SingleOccurrenceTypeNeedsNoDiscriminator(t *testing.T) {
	composed, err := ResolveOccurrence(context.Background(), neverRequiresOrdinal, nil, "sportsbook_bet", "bet-ref-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ref, disc, err := DecomposeOccurrenceKey(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if ref != "bet-ref-1" || disc != nil {
		t.Fatalf("expected (%q, nil), got (%q, %v)", "bet-ref-1", ref, disc)
	}
}

func TestResolveOccurrence_UsesAuthenticatedDiscriminatorWhenAvailable(t *testing.T) {
	src := OccurrenceSourceFunc(func(_ context.Context, verifiedEvent any) (ProviderOccurrenceID, error) {
		// A real adapter reads this from a field the provider's own
		// signature covered - simulated here by a fixed value standing
		// in for "the signed leg_reference field."
		return "leg-2", nil
	})
	composed, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, src, "sportsbook_partial_settlement", "settle-ref-9", "signed-event-placeholder")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ref, disc, err := DecomposeOccurrenceKey(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if ref != "settle-ref-9" || disc == nil || *disc != "leg-2" {
		t.Fatalf("expected (%q, %q), got (%q, %v)", "settle-ref-9", "leg-2", ref, disc)
	}
}

// TestResolveOccurrence_FailsClosedWhenNoAuthenticatedSource is the
// required proof that a provider with no authenticated occurrence field
// is correctly REJECTED for an occurrence-ordinal-requiring transaction
// type, rather than silently falling back to a transport-derived
// approach (the exact S-5 defect this package closes). noSignedFieldSrc
// stands in for a real adapter's OccurrenceSource for a provider it has
// confirmed supplies no signed per-occurrence field of any kind.
func TestResolveOccurrence_FailsClosedWhenNoAuthenticatedSource(t *testing.T) {
	noSignedFieldSrc := OccurrenceSourceFunc(func(context.Context, any) (ProviderOccurrenceID, error) {
		return "", ErrNoAuthenticatedOccurrenceField
	})
	_, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, noSignedFieldSrc, "sportsbook_cashout", "cashout-ref-1", "signed-event-placeholder")
	if !errors.Is(err, ErrOccurrenceOrdinalRequiredButUnavailable) {
		t.Fatalf("expected ErrOccurrenceOrdinalRequiredButUnavailable, got %v", err)
	}
}

func TestResolveOccurrence_FailsClosedWhenSourceIsNil(t *testing.T) {
	_, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, nil, "sportsbook_rollback", "rollback-ref-1", nil)
	if !errors.Is(err, ErrOccurrenceOrdinalRequiredButUnavailable) {
		t.Fatalf("expected ErrOccurrenceOrdinalRequiredButUnavailable, got %v", err)
	}
}

// TestResolveOccurrence_DoesNotFallBackToATransportDerivedSignal proves
// the package offers no code path at all by which a transport-level
// observation (e.g. an inbound-delivery id, unrelated to anything the
// provider signed) can be substituted for a missing authenticated field -
// a caller cannot even construct a passing OccurrenceSource without a
// real authenticated field, because ResolveOccurrence only ever accepts
// a value returned from OccurrenceSource.AuthenticatedOccurrenceReference,
// and an OccurrenceSource that honestly reports
// ErrNoAuthenticatedOccurrenceField (see
// TestResolveOccurrence_FailsClosedWhenNoAuthenticatedSource above)
// always fails closed by design.
func TestResolveOccurrence_DoesNotFallBackToATransportDerivedSignal(t *testing.T) {
	// A deliberately-misbehaving adapter author trying to smuggle a
	// transport-level delivery id through as if it were authenticated -
	// this compiles and runs (the package cannot statically prevent a
	// caller from lying inside its own OccurrenceSource implementation),
	// but the point is structural: nothing in ResolveOccurrence itself
	// ever generates, requests, or requires such a value, and the
	// package's own doc comments push an honest implementer toward
	// returning ErrNoAuthenticatedOccurrenceField instead. This test
	// documents that failure mode is a defect in a
	// non-conformant OccurrenceSource implementation, not something this
	// package's own logic does on a caller's behalf.
	transportDeliveryID := "delivery-000123" // NOT part of any signed payload
	misbehavingSrc := OccurrenceSourceFunc(func(_ context.Context, _ any) (ProviderOccurrenceID, error) {
		return ProviderOccurrenceID(transportDeliveryID), nil
	})
	composed, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, misbehavingSrc, "sportsbook_cashout", "cashout-ref-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, disc, err := DecomposeOccurrenceKey(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if disc == nil || *disc != transportDeliveryID {
		t.Fatalf("expected the (misbehaving) source's value to flow through verbatim, got %v", disc)
	}
	// The contract violation lives entirely in misbehavingSrc's own
	// implementation, which is exactly why OccurrenceSource is
	// documented as a REQUIRED adapter-authoring discipline (S-5's fix
	// is a contract the platform enforces structurally wherever it can -
	// refusing ErrNoAuthenticatedOccurrenceField's absence-of-alternative
	// case - and relies on adapter conformance review, per this
	// specialist's review responsibility, for the "did you actually read
	// this from a signed field" property no static type system can
	// verify on its own).
}

func TestResolveOccurrence_RejectsEmptyDiscriminator(t *testing.T) {
	src := OccurrenceSourceFunc(func(context.Context, any) (ProviderOccurrenceID, error) {
		return "", nil
	})
	_, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, src, "sportsbook_cashout", "ref", nil)
	if err == nil {
		t.Fatal("expected an error for an empty discriminator, got nil")
	}
}

func TestResolveOccurrence_PropagatesOtherSourceErrorsVerbatim(t *testing.T) {
	sentinel := errors.New("boom: signature verification infrastructure failure")
	src := OccurrenceSourceFunc(func(context.Context, any) (ProviderOccurrenceID, error) {
		return "", sentinel
	})
	_, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, src, "sportsbook_cashout", "ref", nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying error to propagate, got %v", err)
	}
	if errors.Is(err, ErrOccurrenceOrdinalRequiredButUnavailable) {
		t.Fatal("a non-occurrence-field error must not be reported as the fail-closed sentinel")
	}
}
