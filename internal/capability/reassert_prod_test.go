//go:build !integration

package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// ADR 0119 S5 + S6 review fix F7, behavioural (the PRODUCTION build: no integration tag): a K1 approve / revoke with
// no authenticated ProofActor in its context is REFUSED with ErrActorNotLive before any database access (tx is nil
// here: the refusal must not reach it), and an incomplete ProofActor is refused the same way.
func TestReassertActor_ProductionBuild_NoProofActorRefused(t *testing.T) {
	if reassertFallbackOn() {
		t.Fatal("the production build must not carry the token-less fallback")
	}
	if err := reassertActor(context.Background(), nil); !errors.Is(err, ErrActorNotLive) {
		t.Fatalf("no ProofActor in a production build: got %v, want ErrActorNotLive", err)
	}
	for name, a := range map[string]ProofActor{
		"nil actor":    {Actor: uuid.Nil, Role: "platform_admin", StaffVersion: 1},
		"empty role":   {Actor: uuid.New(), StaffVersion: 1},
		"zero version": {Actor: uuid.New(), Role: "platform_admin"},
	} {
		ctx := context.WithValue(context.Background(), proofActorKey{}, a)
		if err := reassertActor(ctx, nil); !errors.Is(err, ErrActorNotLive) {
			t.Fatalf("%s: got %v, want ErrActorNotLive", name, err)
		}
	}
}
