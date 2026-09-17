package idempotency

import (
	"testing"
)

func TestAssign_ExternalProviderMode(t *testing.T) {
	a, err := Assign(ModeExternalProvider, "3:ABC", "mock-casino", "mock-casino:3:ABC")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if a.ProviderID == nil || *a.ProviderID != "mock-casino" {
		t.Fatalf("expected provider id to be set, got %v", a.ProviderID)
	}
	if a.ProviderTxID == nil || *a.ProviderTxID != "3:ABC" {
		t.Fatalf("expected provider_tx_id to be the composed key, got %v", a.ProviderTxID)
	}
	if a.IdempotencyKey != "mock-casino:3:ABC" {
		t.Fatalf("expected idempotency key %q, got %q", "mock-casino:3:ABC", a.IdempotencyKey)
	}
}

func TestAssign_InHouseMode(t *testing.T) {
	a, err := Assign(ModeInHouse, "3:ABC", "", "")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if a.ProviderID != nil {
		t.Fatalf("expected provider_id to stay nil for in-house mode, got %v", *a.ProviderID)
	}
	if a.ProviderTxID != nil {
		t.Fatalf("expected provider_tx_id to stay nil for in-house mode, got %v", *a.ProviderTxID)
	}
	if a.IdempotencyKey != "3:ABC" {
		t.Fatalf("expected idempotency_key %q, got %q", "3:ABC", a.IdempotencyKey)
	}
}

func TestAssign_ExternalProviderModeRequiresProviderID(t *testing.T) {
	if _, err := Assign(ModeExternalProvider, "3:ABC", "", "idem"); err == nil {
		t.Fatal("expected an error when provider id is empty for external-provider mode")
	}
}

func TestAssign_ExternalProviderModeRequiresIdempotencyKey(t *testing.T) {
	if _, err := Assign(ModeExternalProvider, "3:ABC", "mock-casino", ""); err == nil {
		t.Fatal("expected an error when the external-mode idempotency key is empty")
	}
}

func TestAssign_RejectsEmptyComposedKey(t *testing.T) {
	if _, err := Assign(ModeInHouse, "", "", ""); err == nil {
		t.Fatal("expected an error for an empty composed key")
	}
}

func TestAssign_NeverPopulatesProviderTxIDWithoutProviderID(t *testing.T) {
	// Structural proof of ADR 0038 §14.6's "never half-populated" rule:
	// for every mode this package supports, ProviderID and ProviderTxID
	// are either both nil or both non-nil, never mixed.
	inHouse, err := Assign(ModeInHouse, "ref", "", "")
	if err != nil {
		t.Fatalf("assign in-house: %v", err)
	}
	if (inHouse.ProviderID == nil) != (inHouse.ProviderTxID == nil) {
		t.Fatal("in-house assignment must not mix a nil and a non-nil provider field")
	}
	external, err := Assign(ModeExternalProvider, "ref", "p", "idem")
	if err != nil {
		t.Fatalf("assign external: %v", err)
	}
	if (external.ProviderID == nil) != (external.ProviderTxID == nil) {
		t.Fatal("external-provider assignment must not mix a nil and a non-nil provider field")
	}
}

func TestAssign_UnknownModeRejected(t *testing.T) {
	if _, err := Assign(Mode(99), "ref", "p", "idem"); err == nil {
		t.Fatal("expected an error for an unrecognized mode")
	}
}
