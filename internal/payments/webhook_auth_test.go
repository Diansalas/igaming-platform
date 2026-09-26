package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestMultiWebhookCredentialResolver_NilEntry_FailsClosed is the
// architect review PW-6.1 test: a nil entry in the composite (a
// construction mistake, e.g. MultiWebhookCredentialResolver{"x": nil})
// must fail closed with ErrWebhookCredentialUnavailable - the SAME
// sentinel a genuinely absent key gets - never panic with a nil-pointer
// dereference. This matters specifically because Resolve sits on an
// unauthenticated request path (a callback's credential lookup runs
// BEFORE signature verification succeeds): a panic there would be a
// caller-triggerable crash, not just a wrong-but-safe answer.
func TestMultiWebhookCredentialResolver_NilEntry_FailsClosed(t *testing.T) {
	resolver := MultiWebhookCredentialResolver{"broken-provider": nil}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Resolve must never panic on a nil entry, got panic: %v", r)
		}
	}()

	_, err := resolver.ResolveKey(context.Background(), uuid.New(), "broken-provider", "mock-v1")
	if !errors.Is(err, ErrWebhookCredentialUnavailable) {
		t.Fatalf("expected ErrWebhookCredentialUnavailable for a nil entry, got %v", err)
	}
}

// TestMultiWebhookCredentialResolver_AbsentEntry_FailsClosed is the
// existing, unchanged behavior for a providerID never registered at all -
// asserted here alongside the nil-entry case so both "missing" shapes are
// pinned side by side.
func TestMultiWebhookCredentialResolver_AbsentEntry_FailsClosed(t *testing.T) {
	resolver := MultiWebhookCredentialResolver{}
	_, err := resolver.ResolveKey(context.Background(), uuid.New(), "never-registered", "mock-v1")
	if !errors.Is(err, ErrWebhookCredentialUnavailable) {
		t.Fatalf("expected ErrWebhookCredentialUnavailable for an absent entry, got %v", err)
	}
}
