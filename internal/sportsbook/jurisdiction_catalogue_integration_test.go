//go:build integration

// Stage 9.2 (ADR 0083 §5.4.1, §12.1 items 14-15): the catalogue-visibility
// enforcement point - MARK unavailable, never omit; anonymous reads
// unchanged.
package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSportsbookCatalogue_RestrictedRowIsMarkedUnavailableNotOmitted
// proves §5.4.1's decision directly on the response shape: a
// jurisdiction-restricted event is still present in ListSportsCatalogue's
// output (never dropped), with Available=false and a non-empty
// UnavailableReason; an event under a DIFFERENT, unrestricted competition
// stays Available=true.
func TestSportsbookCatalogue_RestrictedRowIsMarkedUnavailableNotOmitted(t *testing.T) {
	pool := testPool(t)
	_, restrictedEventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	_, unrestrictedEventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	code := seedJurisdictionCode(t, pool)
	seedRestriction(t, pool, restrictionScope{ScopeKind: "event", EventID: uuidPtr(restrictedEventID)}, code)

	f := seedFixture(t, pool)
	ac := AvailabilityContext{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID}

	var catalogue []SportCatalogue
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		catalogue, err = ListSportsCatalogue(ctx, tx)
		if err != nil {
			return err
		}
		return AnnotateCatalogueAvailability(ctx, tx, catalogue, ac)
	})
	if err != nil {
		t.Fatalf("list + annotate catalogue: %v", err)
	}

	var foundRestricted, foundUnrestricted bool
	for _, sc := range catalogue {
		for _, cc := range sc.Competitions {
			for _, e := range cc.Events {
				switch e.ID {
				case restrictedEventID:
					foundRestricted = true
					if e.Available {
						t.Fatal("expected the restricted event to be marked unavailable, got Available=true")
					}
					if e.UnavailableReason == "" {
						t.Fatal("expected a non-empty unavailable_reason on the restricted event")
					}
				case unrestrictedEventID:
					foundUnrestricted = true
					if !e.Available || e.UnavailableReason != "" {
						t.Fatalf("expected the unrestricted event to remain available, got Available=%v UnavailableReason=%q", e.Available, e.UnavailableReason)
					}
				}
			}
		}
	}
	if !foundRestricted {
		t.Fatal("expected the restricted event to be PRESENT in the catalogue (marked, never omitted)")
	}
	if !foundUnrestricted {
		t.Fatal("expected the unrestricted event to be present and unaffected")
	}
}

// TestSportsbookCatalogue_AnonymousReadIsUnchanged proves the zero-value
// AvailabilityContext is a true no-op: every event/market/selection stays
// Available=true/UnavailableReason="" even when an active restriction
// exists, exactly matching this package's pre-Stage-9.2 output shape for
// the same fixture data (both catalogue read routes are genuinely
// anonymous today - ADR 0083 §1.7).
func TestSportsbookCatalogue_AnonymousReadIsUnchanged(t *testing.T) {
	pool := testPool(t)
	sel, eventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	code := seedJurisdictionCode(t, pool)
	seedRestriction(t, pool, restrictionScope{ScopeKind: "event", EventID: uuidPtr(eventID)}, code)

	var catalogue []SportCatalogue
	var detail EventDetail
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		catalogue, err = ListSportsCatalogue(ctx, tx)
		if err != nil {
			return err
		}
		if err := AnnotateCatalogueAvailability(ctx, tx, catalogue, AvailabilityContext{}); err != nil {
			return err
		}
		detail, err = GetEventDetail(ctx, tx, eventID)
		if err != nil {
			return err
		}
		return AnnotateEventAvailability(ctx, tx, &detail, AvailabilityContext{})
	})
	if err != nil {
		t.Fatalf("list/get + annotate with zero-value context: %v", err)
	}

	var found bool
	for _, sc := range catalogue {
		for _, cc := range sc.Competitions {
			for _, e := range cc.Events {
				if e.ID == eventID {
					found = true
					if !e.Available || e.UnavailableReason != "" {
						t.Fatalf("expected an anonymous read to be unaffected by the active restriction, got Available=%v UnavailableReason=%q", e.Available, e.UnavailableReason)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("expected the event to be present")
	}
	for _, m := range detail.Markets {
		if !m.Available || m.UnavailableReason != "" {
			t.Fatalf("expected an anonymous GetEventDetail read to leave every market Available=true, got %+v", m)
		}
		for _, s := range m.Selections {
			if !s.Available || s.UnavailableReason != "" {
				t.Fatalf("expected an anonymous GetEventDetail read to leave every selection Available=true, got %+v", s)
			}
		}
	}
	if sel.ID == uuid.Nil {
		t.Fatal("fixture sanity: selection id must not be nil")
	}
}

// TestSportsbookCatalogue_EventDetailDownwardPropagation proves
// AnnotateEventAvailability's own downward-propagation rule directly on
// GetEventDetail's full tree: an event-level restriction marks every
// market and selection beneath it unavailable.
func TestSportsbookCatalogue_EventDetailDownwardPropagation(t *testing.T) {
	pool := testPool(t)
	sel, eventID, _ := seedSelectionWithContext(t, pool, seedSelectionParams{})
	code := seedJurisdictionCode(t, pool)
	seedRestriction(t, pool, restrictionScope{ScopeKind: "event", EventID: uuidPtr(eventID)}, code)

	f := seedFixture(t, pool)
	ac := AvailabilityContext{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID}

	var detail EventDetail
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		detail, err = GetEventDetail(ctx, tx, eventID)
		if err != nil {
			return err
		}
		return AnnotateEventAvailability(ctx, tx, &detail, ac)
	})
	if err != nil {
		t.Fatalf("get + annotate event detail: %v", err)
	}
	if len(detail.Markets) == 0 {
		t.Fatal("fixture sanity: expected at least one market")
	}
	for _, m := range detail.Markets {
		if m.Available {
			t.Fatalf("expected market %s to be unavailable (event-level propagation), got Available=true", m.ID)
		}
		for _, s := range m.Selections {
			if s.ID == sel.ID && s.Available {
				t.Fatalf("expected selection %s to be unavailable (event-level propagation), got Available=true", s.ID)
			}
		}
	}
}
