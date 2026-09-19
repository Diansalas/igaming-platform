package jurisdiction

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// These are pure unit tests: every case here is decided by Resolve's own
// input gates or by its player-scoped short-circuit, both of which run
// BEFORE any database access - mirrors assetregistry's own
// "input/context gates run before the database" test convention
// (internal/assetregistry/authorization_test.go). A nil ReadOnlyQuerier
// is passed deliberately: if any of these cases ever touched it, the
// test would panic, which is itself part of what is being proven.

func TestResolve_ZeroTenantRefusesWithoutTouchingTheDatabase(t *testing.T) {
	res, err := Resolve(context.Background(), nil, Params{
		TenantID: uuid.Nil, OperationClass: OperationPlay, RequestedByActorType: ActorSystem,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Refused {
		t.Fatalf("expected Refused, got %q", res.Outcome())
	}
	if res.Reason() != ReasonScopeMismatch {
		t.Fatalf("expected scope_mismatch, got %q", res.Reason())
	}
	if _, err := res.Code(); err == nil {
		t.Fatal("Code() must be unreachable for a non-resolved outcome")
	}
}

func TestResolve_UnknownOperationClassRefuses(t *testing.T) {
	res, err := Resolve(context.Background(), nil, Params{
		TenantID: uuid.New(), OperationClass: OperationClass("withdrawal"), RequestedByActorType: ActorSystem,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Refused || res.Reason() != ReasonUnsupportedOperationClass {
		t.Fatalf("expected refused(unsupported_operation_class), got %s(%s)", res.Outcome(), res.Reason())
	}
}

func TestResolve_InvalidActorTypeIsRejected(t *testing.T) {
	_, err := Resolve(context.Background(), nil, Params{
		TenantID: uuid.New(), OperationClass: OperationPlay, RequestedByActorType: ActorType("robot"),
	})
	if err == nil {
		t.Fatal("expected an error for an unknown actor type")
	}
}

// This is Stage 4I's single most important behavioural guarantee
// (canonical-model §11.3, §3.2): a player-scoped operation NEVER resolves
// to any jurisdiction in this codebase today, and NEVER touches the
// database to try - there is no player-side signal to look up, so
// looking would be pointless at best and a fail-open trap at worst if a
// future edit accidentally wired a lookup that silently returns a
// tenant-side answer for a player-scoped question.
func TestResolve_PlayerScopedOperationAlwaysUnresolvedNoSignal(t *testing.T) {
	playerID := uuid.New()
	res, err := Resolve(context.Background(), nil, Params{
		TenantID: uuid.New(), PlayerAccountID: &playerID, OperationClass: OperationPlay,
		RequestedByActorType: ActorPlayer,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Unresolved {
		t.Fatalf("expected Unresolved, got %q", res.Outcome())
	}
	if res.Reason() != ReasonNoSignal {
		t.Fatalf("expected no_signal, got %q", res.Reason())
	}
	if res.SelectedBasis() == BasisTenantLicence {
		t.Fatal("a player-scoped resolution must never carry basis=tenant_licence (this IS HDR-J-1 answered 'yes' by accident)")
	}
	if _, err := res.ID(); err == nil {
		t.Fatal("ID() must be unreachable for a non-resolved outcome")
	}
}

// Every operation class must resolve unresolved(no_signal) when
// player-scoped - not just the one exercised above - since a future
// operation_class addition must not accidentally get a different,
// more permissive default.
func TestResolve_EveryOperationClassIsUnresolvedNoSignalForAPlayer(t *testing.T) {
	for _, oc := range []OperationClass{OperationPlay, OperationCatalogueAvailability, OperationBonusIssuance, OperationBonusConversion} {
		playerID := uuid.New()
		res, err := Resolve(context.Background(), nil, Params{
			TenantID: uuid.New(), PlayerAccountID: &playerID, OperationClass: oc, RequestedByActorType: ActorPlayer,
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", oc, err)
		}
		if res.Outcome() != Unresolved || res.Reason() != ReasonNoSignal {
			t.Fatalf("%s: expected unresolved(no_signal), got %s(%s)", oc, res.Outcome(), res.Reason())
		}
	}
}

// --- Resolution non-forgeability (canonical-model §2.3) ---

func TestResolution_ZeroValueIsUnresolvedNeverResolved(t *testing.T) {
	var r Resolution
	if r.Outcome() == Resolved {
		t.Fatal("the zero-value Resolution must never report Resolved")
	}
	if _, err := r.Code(); err == nil {
		t.Fatal("Code() must error on a zero-value Resolution")
	}
	if _, err := r.ID(); err == nil {
		t.Fatal("ID() must error on a zero-value Resolution")
	}
}

func TestResolution_AssertScope(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	brandA := uuid.New()
	playerX := uuid.New()

	resForPlayerX := Resolution{tenantID: tenantA, brandID: &brandA, playerAcctID: &playerX, outcome: Unresolved, reason: ReasonNoSignal}

	if err := resForPlayerX.AssertScope(tenantA, &brandA, &playerX); err != nil {
		t.Fatalf("matching scope must be accepted: %v", err)
	}
	if err := resForPlayerX.AssertScope(tenantB, &brandA, &playerX); err == nil {
		t.Fatal("a resolution must be refused across a tenant mismatch")
	}
	playerY := uuid.New()
	if err := resForPlayerX.AssertScope(tenantA, &brandA, &playerY); err == nil {
		t.Fatal("a resolution for player X must be structurally unusable for player Y")
	}
	brandB := uuid.New()
	if err := resForPlayerX.AssertScope(tenantA, &brandB, &playerX); err == nil {
		t.Fatal("a resolution scoped to brand A must be refused, never silently widened, for brand B (RULING BI-4I-1)")
	}

	// A tenant-level resolution (no brand) may be used by any brand
	// within that tenant (§1.1).
	tenantLevel := Resolution{tenantID: tenantA, outcome: Resolved}
	if err := tenantLevel.AssertScope(tenantA, &brandA, nil); err != nil {
		t.Fatalf("a brand-less resolution must be usable by any brand in its tenant: %v", err)
	}
}

// TestResolve_RemainsUnchangedAndCannotEmitThePhaseCReasons proves Stage 4I
// Phase C's new Reason values (ReasonNoApplicableEvidence/
// ReasonEvidenceInvalid/ReasonLocationSignalUnusable) are structurally
// unreachable from Resolve - they belong exclusively to
// DeterminePlayerJurisdiction (precedence.go), a separate, zero-caller
// entry point this dispatch added. resolver.go itself has zero diff this
// phase.
func TestResolve_RemainsUnchangedAndCannotEmitThePhaseCReasons(t *testing.T) {
	phaseCReasons := map[Reason]bool{
		ReasonNoApplicableEvidence:   true,
		ReasonEvidenceInvalid:        true,
		ReasonLocationSignalUnusable: true,
	}

	playerID := uuid.New()
	cases := []Params{
		{TenantID: uuid.Nil, OperationClass: OperationPlay, RequestedByActorType: ActorSystem},
		{TenantID: uuid.New(), OperationClass: OperationClass("withdrawal"), RequestedByActorType: ActorSystem},
		{TenantID: uuid.New(), PlayerAccountID: &playerID, OperationClass: OperationPlay, RequestedByActorType: ActorPlayer},
	}
	for _, p := range cases {
		res, err := Resolve(context.Background(), nil, p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phaseCReasons[res.Reason()] {
			t.Fatalf("Resolve must never emit a Stage 4I Phase C reason, got %q", res.Reason())
		}
	}
}
