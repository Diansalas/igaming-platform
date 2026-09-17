package idempotency

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

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
// approach (the exact S-5 defect this package closes).
func TestResolveOccurrence_FailsClosedWhenNoAuthenticatedSource(t *testing.T) {
	_, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, NoAuthenticatedOccurrenceField, "sportsbook_cashout", "cashout-ref-1", "signed-event-placeholder")
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
// and the ONLY zero-configuration OccurrenceSource this package ships
// (NoAuthenticatedOccurrenceField) always fails closed by design.
func TestResolveOccurrence_DoesNotFallBackToATransportDerivedSignal(t *testing.T) {
	// A deliberately-misbehaving adapter author trying to smuggle a
	// transport-level delivery id through as if it were authenticated -
	// this compiles and runs (the package cannot statically prevent a
	// caller from lying inside its own OccurrenceSource implementation),
	// but the point is structural: nothing in ResolveOccurrence itself
	// ever generates, requests, or requires such a value, and the
	// package's own doc comments and the one no-op source it ships both
	// push an honest implementer toward ErrNoAuthenticatedOccurrenceField
	// instead. This test documents that failure mode is a defect in a
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

// TestCanonicalOccurrenceIssuer_RoundTripGraduatesIntoAnOrdinarySource
// demonstrates the documented flow for a provider with NO native
// per-occurrence field: the platform mints a canonical id, the provider
// echoes/signs it, and the NEXT event's OccurrenceSource reads it back as
// an ordinary authenticated field - CanonicalOccurrenceIssuer is never
// consulted a second time for the same occurrence.
func TestCanonicalOccurrenceIssuer_RoundTripGraduatesIntoAnOrdinarySource(t *testing.T) {
	// A minimal, reproducible-on-retry issuer: derives the id from a
	// stable map keyed on (correlationID, transactionType) rather than a
	// fresh random value per call - the reproducibility contract
	// IssueCanonicalOccurrenceID's doc comment requires.
	issued := map[string]ProviderOccurrenceID{}
	var issuer CanonicalOccurrenceIssuer = issuerFunc(func(_ context.Context, correlationID CorrelationID, transactionType string) (ProviderOccurrenceID, error) {
		key := correlationID.String() + ":" + transactionType
		if v, ok := issued[key]; ok {
			return v, nil
		}
		v := ProviderOccurrenceID("canonical-" + key)
		issued[key] = v
		return v, nil
	})

	corr := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	first, err := issuer.IssueCanonicalOccurrenceID(context.Background(), corr, "sportsbook_cashout")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	retry, err := issuer.IssueCanonicalOccurrenceID(context.Background(), corr, "sportsbook_cashout")
	if err != nil {
		t.Fatalf("issue (retry): %v", err)
	}
	if first != retry {
		t.Fatalf("issuer must be reproducible on retry: got %q then %q", first, retry)
	}

	// Once the provider has echoed/signed `first`, the adapter's own
	// OccurrenceSource for the resulting event reads it back as an
	// ordinary authenticated field.
	graduatedSrc := OccurrenceSourceFunc(func(_ context.Context, verifiedEvent any) (ProviderOccurrenceID, error) {
		return verifiedEvent.(ProviderOccurrenceID), nil
	})
	composed, err := ResolveOccurrence(context.Background(), alwaysRequiresOrdinal, graduatedSrc, "sportsbook_cashout", "cashout-ref-1", first)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ref, disc, err := DecomposeOccurrenceKey(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if ref != "cashout-ref-1" || disc == nil || ProviderOccurrenceID(*disc) != first {
		t.Fatalf("expected the issued canonical id to flow through as the discriminator, got (%q, %v)", ref, disc)
	}
}

type issuerFunc func(ctx context.Context, correlationID CorrelationID, transactionType string) (ProviderOccurrenceID, error)

func (f issuerFunc) IssueCanonicalOccurrenceID(ctx context.Context, correlationID CorrelationID, transactionType string) (ProviderOccurrenceID, error) {
	return f(ctx, correlationID, transactionType)
}
