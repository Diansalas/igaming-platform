package sportsbook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// PROVIDER-REF-BOUND-1: every external_ref in a provider catalogue tree is
// bounded, at every nesting level, before any write.
func TestValidateCatalogueReferences_EveryLevel(t *testing.T) {
	ok, err := NewMockSportsbookProvider().Catalogue(context.Background())
	if err != nil {
		t.Fatalf("the mock catalogue provider must not error: %v", err)
	}
	if err := validateCatalogueReferences(ok); err != nil {
		t.Fatalf("the mock catalogue must pass: %v", err)
	}
	over := strings.Repeat("r", providerref.MaxBytes+1)
	exact := strings.Repeat("r", providerref.MaxBytes)
	build := func(level int, ref string) CatalogueResult {
		sel := CatalogueSelection{ExternalRef: "sel", Name: "S", OddsNumerator: 2, OddsDenominator: 1}
		mkt := CatalogueMarket{ExternalRef: "mkt", Name: "M", Selections: []CatalogueSelection{sel}}
		ev := CatalogueEvent{ExternalRef: "ev", Name: "E", Markets: []CatalogueMarket{mkt}}
		comp := CatalogueCompetition{ExternalRef: "comp", Name: "C", Events: []CatalogueEvent{ev}}
		sport := CatalogueSport{ExternalRef: "sport", Code: "x", Name: "X", Competitions: []CatalogueCompetition{comp}}
		switch level {
		case 0:
			sport.ExternalRef = ref
		case 1:
			sport.Competitions[0].ExternalRef = ref
		case 2:
			sport.Competitions[0].Events[0].ExternalRef = ref
		case 3:
			sport.Competitions[0].Events[0].Markets[0].ExternalRef = ref
		case 4:
			sport.Competitions[0].Events[0].Markets[0].Selections[0].ExternalRef = ref
		}
		return CatalogueResult{Sports: []CatalogueSport{sport}}
	}
	fields := []string{"sport.external_ref", "competition.external_ref", "event.external_ref", "market.external_ref", "selection.external_ref"}
	for level, field := range fields {
		err := validateCatalogueReferences(build(level, over))
		if !errors.Is(err, ErrProviderReferenceInvalid) || !errors.Is(err, providerref.ErrInvalid) {
			t.Fatalf("level %d: expected ErrProviderReferenceInvalid, got %v", level, err)
		}
		if refErr, _ := providerref.AsError(err); refErr == nil || refErr.Field != field || refErr.Reason != providerref.ReasonTooLong {
			t.Fatalf("level %d: expected %s/too_long, got %+v", level, field, refErr)
		}
		if err := validateCatalogueReferences(build(level, exact)); err != nil {
			t.Fatalf("level %d: exact max must pass: %v", level, err)
		}
		if err := validateCatalogueReferences(build(level, "")); !errors.Is(err, ErrProviderReferenceInvalid) {
			t.Fatalf("level %d: empty external_ref must be rejected, got %v", level, err)
		}
		if strings.Contains(validateCatalogueReferences(build(level, over)).Error(), over) {
			t.Fatalf("level %d: error text leaks the value", level)
		}
	}
}

func TestValidateSettlementEvent_AssetCodeBound(t *testing.T) {
	base := SettlementEvent{TenantID: uuid.New(), BetID: uuid.New(), ActorStaffID: uuid.New(), EventType: SettlementEventSettle,
		Generation: 1, Outcome: SettlementOutcomeWon, ClaimPayoutAmount: 10}
	for _, bad := range []string{strings.Repeat("E", providerref.MaxBytes+1), "EU\x00R", "EU\x7fR", "E\xffR"} {
		ev := base
		ev.ClaimAssetCode = bad
		err := validateSettlementEvent(ev)
		if !errors.Is(err, ErrInvalidInput) || !errors.Is(err, ErrProviderReferenceInvalid) {
			t.Fatalf("asset code of %d bytes must be ErrInvalidInput+ErrProviderReferenceInvalid, got %v", len(bad), err)
		}
	}
	ev := base
	ev.ClaimAssetCode = strings.Repeat("E", providerref.MaxBytes)
	if err := validateSettlementEvent(ev); err != nil {
		t.Fatalf("exact-max asset code passes the bound (the asset match rejects it later): %v", err)
	}
}
