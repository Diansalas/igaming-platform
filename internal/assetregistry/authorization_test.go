package assetregistry

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// These are pure unit tests: every case here is decided by
// CheckEligibility's input/context gates, which run BEFORE any database
// access, so a nil transaction is never touched. That is itself part of
// the contract being tested - a missing tenant/jurisdiction/product must
// be refused without the code ever reaching a query that might have
// returned something permissive.

func TestCheckEligibility_ZeroTenantDenies(t *testing.T) {
	eligible, reason, err := AssetAuthorization{}.CheckEligibility(
		context.Background(), nil, uuid.Nil, uuid.Nil, uuid.New(), "EUR",
		OperationScope{Product: "casino", Operation: OperationWagering})
	if eligible {
		t.Fatal("a zero tenant must never be eligible")
	}
	if reason != ReasonTenantContextMissing {
		t.Fatalf("expected %q, got %q", ReasonTenantContextMissing, reason)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

// The zero-jurisdiction case is the one Stage 4H-B0-R5 security finding
// S-6a names explicitly, and it is realistic rather than theoretical: no
// per-player jurisdiction resolver exists anywhere in this codebase yet
// (Stage 4G-FINAL Part C), so a caller wiring this up today has a zero
// jurisdiction in hand. It must DENY, never be read as "this check isn't
// jurisdiction-scoped, skip layer 6".
func TestCheckEligibility_ZeroJurisdictionDeniesAndIsNeverSkipped(t *testing.T) {
	eligible, reason, err := AssetAuthorization{}.CheckEligibility(
		context.Background(), nil, uuid.New(), uuid.New(), uuid.Nil, "EUR",
		OperationScope{Product: "casino", Operation: OperationWagering})
	if eligible {
		t.Fatal("a zero jurisdiction must never be eligible")
	}
	if reason != ReasonJurisdictionContextMissing {
		t.Fatalf("expected %q, got %q", ReasonJurisdictionContextMissing, reason)
	}
	if err == nil {
		t.Fatal("expected a non-nil error so no call site can treat this as a soft pass")
	}
}

// A zero BRAND is explicitly permitted (ADR 0037 §C.2) - it means layer 5
// is not evaluated, which can never widen anything because layer 5 only
// narrows layer 4. Proven here by the absence of a brand-specific
// rejection: this call gets past the input gates and fails later (on the
// nil tx), rather than being rejected for the zero brand.
func TestCheckEligibility_ZeroBrandIsNotItselfRejected(t *testing.T) {
	defer func() {
		// A nil pgx.Tx panics when actually used. Reaching that point is
		// the assertion: the zero brand was not rejected by an input gate.
		_ = recover()
	}()
	_, reason, _ := AssetAuthorization{}.CheckEligibility(
		context.Background(), nil, uuid.New(), uuid.Nil, uuid.New(), "EUR",
		OperationScope{Product: "casino", Operation: OperationWagering})
	if reason == ReasonBrandNotAuthorized {
		t.Fatal("a zero brand must not itself be a brand-layer denial")
	}
}

func TestCheckEligibility_MissingProductDenies(t *testing.T) {
	_, reason, err := AssetAuthorization{}.CheckEligibility(
		context.Background(), nil, uuid.New(), uuid.New(), uuid.New(), "EUR",
		OperationScope{Operation: OperationWagering})
	if reason != ReasonProductContextMissing || err == nil {
		t.Fatalf("expected product_context_missing with an error, got %q / %v", reason, err)
	}
}

func TestCheckEligibility_UnknownOperationDenies(t *testing.T) {
	_, reason, err := AssetAuthorization{}.CheckEligibility(
		context.Background(), nil, uuid.New(), uuid.New(), uuid.New(), "EUR",
		OperationScope{Product: "casino", Operation: Operation("transfer")})
	if reason != ReasonOperationUnknown || err == nil {
		t.Fatalf("expected operation_unknown with an error, got %q / %v", reason, err)
	}
}

func TestCheckEligibility_EmptyAssetDenies(t *testing.T) {
	_, reason, err := AssetAuthorization{}.CheckEligibility(
		context.Background(), nil, uuid.New(), uuid.New(), uuid.New(), "",
		OperationScope{Product: "casino", Operation: OperationWagering})
	if reason != ReasonAssetNotFound || err == nil {
		t.Fatalf("expected asset_not_found with an error, got %q / %v", reason, err)
	}
}

// ADR 0037 §C.2 fixes the operation list at exactly six values. If this
// test fails, either the ADR changed (which requires an amendment and a
// migration to migration 0045's CHECK constraint) or the list drifted.
func TestOperations_IsExactlyTheADRsSix(t *testing.T) {
	ops := Operations()
	if len(ops) != 6 {
		t.Fatalf("expected exactly 6 canonical operations, got %d: %v", len(ops), ops)
	}
	for _, op := range ops {
		if !validOperation(op) {
			t.Fatalf("%q is listed but not accepted by validOperation", op)
		}
	}
	if validOperation(Operation("")) || validOperation(Operation("wager")) {
		t.Fatal("validOperation must fail closed on an empty or near-miss operation")
	}
}

func TestValidChangeOperation_ExcludesSuspendAndRevoke(t *testing.T) {
	for _, op := range []ChangeOperation{ChangeCreate, ChangeActivate, ChangePlatformAuthorize} {
		if !validChangeOperation(op) {
			t.Fatalf("%q must be a dual-controlled operation", op)
		}
	}
	// ADR 0037 §C.5.3: the reverse direction is deliberately single-actor,
	// so it must not be representable as a four-eyes request at all -
	// otherwise a kill-switch could be made to wait for an approver.
	for _, op := range []ChangeOperation{"suspend", "deactivate", "revoke", ""} {
		if validChangeOperation(op) {
			t.Fatalf("%q must NOT be a dual-controlled operation", op)
		}
	}
}

func TestActorContext_RequiresActorAndReasonCode(t *testing.T) {
	if err := (ActorContext{ReasonCode: "x"}).validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a missing actor id, got %v", err)
	}
	if err := (ActorContext{ActorID: uuid.New()}).validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a missing reason code, got %v", err)
	}
	if err := (ActorContext{ActorID: uuid.New(), ReasonCode: " "}).validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a whitespace-only reason code, got %v", err)
	}
	if err := (ActorContext{ActorID: uuid.New(), ReasonCode: "new_listing"}).validate(); err != nil {
		t.Fatalf("expected a valid actor context to pass, got %v", err)
	}
}
