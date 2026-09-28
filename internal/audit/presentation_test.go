package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestResolvePresentation_CompiledInDefaults pins §5.3's compiled-in
// PRH-2 default: identified actor, no network metadata, no free-form
// metadata.
func TestResolvePresentation_CompiledInDefaults(t *testing.T) {
	p, err := ResolvePresentation(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ActorPresentation != ActorPresentationIdentified {
		t.Errorf("expected identified actor presentation, got %q", p.ActorPresentation)
	}
	if p.ShowNetworkMetadata {
		t.Error("expected ShowNetworkMetadata=false by compiled-in default")
	}
	if p.ShowFreeFormMetadata {
		t.Error("expected ShowFreeFormMetadata=false by compiled-in default")
	}
}

// TestResolvePresentationOrRestrictive_FailsClosed is the §7 "a forced
// resolver error fails closed" test: a resolver that returns a WIDER
// presentation (ShowNetworkMetadata/ShowFreeFormMetadata = true) alongside
// a non-nil error must never have that wider value observed - the error
// forces the restrictive default, unconditionally.
func TestResolvePresentationOrRestrictive_FailsClosed(t *testing.T) {
	widerButErroring := func(ctx context.Context, tenantID uuid.UUID) (Presentation, error) {
		return Presentation{ActorPresentation: "pseudonymous", ShowNetworkMetadata: true, ShowFreeFormMetadata: true}, errors.New("boom")
	}
	got := ResolvePresentationOrRestrictive(context.Background(), widerButErroring, uuid.New())
	want := restrictivePresentation()
	if got != want {
		t.Fatalf("expected the restrictive presentation on a resolver error, got %+v", got)
	}
}

// TestResolvePresentationOrRestrictive_NilUsesDefault confirms the nil
// resolver falls back to production wiring's ResolvePresentation.
func TestResolvePresentationOrRestrictive_NilUsesDefault(t *testing.T) {
	got := ResolvePresentationOrRestrictive(context.Background(), nil, uuid.New())
	want := restrictivePresentation()
	if got != want {
		t.Fatalf("expected the compiled-in default via nil resolver, got %+v", got)
	}
}

// TestResolvePresentationOrRestrictive_SuccessPassesThrough confirms a
// SUCCESSFUL resolver call is not itself forced to the restrictive value -
// the fail-closed behavior is specific to an error, not to every call.
func TestResolvePresentationOrRestrictive_SuccessPassesThrough(t *testing.T) {
	custom := Presentation{ActorPresentation: ActorPresentationIdentified, ShowNetworkMetadata: false, ShowFreeFormMetadata: true}
	resolver := func(ctx context.Context, tenantID uuid.UUID) (Presentation, error) {
		return custom, nil
	}
	got := ResolvePresentationOrRestrictive(context.Background(), resolver, uuid.New())
	if got != custom {
		t.Fatalf("expected the resolver's own successful value, got %+v", got)
	}
}
