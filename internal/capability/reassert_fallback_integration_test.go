//go:build integration

package capability

import (
	"context"
	"errors"
	"testing"
)

// ADR 0119 S5 + S6 review fix F7: the integration-build fallback is ON by default (token-less fixtures) and, when a
// test switches it OFF, reassertActor refuses exactly as the production build does.
func TestReassertActor_IntegrationFallbackSwitch(t *testing.T) {
	if !reassertFallbackOn() {
		t.Fatal("the integration build's fallback must default to on")
	}
	if err := reassertActor(context.Background(), nil); err != nil {
		t.Fatalf("fallback on, no ProofActor: %v", err)
	}
	SetTestReassertFallback(false)
	t.Cleanup(func() { SetTestReassertFallback(true) })
	if err := reassertActor(context.Background(), nil); !errors.Is(err, ErrActorNotLive) {
		t.Fatalf("fallback off, no ProofActor: got %v, want ErrActorNotLive", err)
	}
}
