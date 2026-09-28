//go:build integration

package sportsbook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

type fixedCatalogueProvider struct{ result CatalogueResult }

func (p fixedCatalogueProvider) Catalogue(_ context.Context) (CatalogueResult, error) {
	return p.result, nil
}

func prhrefCatalogue(prefix, selectionRef string) CatalogueResult {
	return CatalogueResult{Sports: []CatalogueSport{{
		ExternalRef: prefix + "-sport", Code: prefix, Name: "PRH-REF sport",
		Competitions: []CatalogueCompetition{{
			ExternalRef: prefix + "-comp", Name: "C",
			Events: []CatalogueEvent{{
				ExternalRef: prefix + "-event", Name: "E", StartTime: time.Now().Add(24 * time.Hour), Status: EventScheduled,
				Markets: []CatalogueMarket{{
					ExternalRef: prefix + "-market", Name: "M", Status: MarketOpen,
					Selections: []CatalogueSelection{
						{ExternalRef: prefix + "-home", Name: "Home", OddsNumerator: 2, OddsDenominator: 1},
						{ExternalRef: selectionRef, Name: "Away", OddsNumerator: 3, OddsDenominator: 1},
					},
				}},
			}},
		}},
	}}}
}

// TestSyncCatalogue_OverBoundExternalRefWritesNothing: a catalogue whose
// LAST (deepest) external_ref breaks the bound is rejected before the
// first upsert - not even the sport row (written first in a naive loop)
// exists afterwards. An exact-max ref is stored verbatim and a re-sync is
// idempotent.
func TestSyncCatalogue_OverBoundExternalRefWritesNothing(t *testing.T) {
	pool := testPool(t)
	prefix := "prhref-" + uuid.NewString()[:8]
	count := func() int {
		var n int
		err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM sb_sports WHERE external_ref LIKE $1) +
				(SELECT count(*) FROM sb_competitions WHERE external_ref LIKE $1) +
				(SELECT count(*) FROM sb_events WHERE external_ref LIKE $1) +
				(SELECT count(*) FROM sb_markets WHERE external_ref LIKE $1) +
				(SELECT count(*) FROM sb_selections WHERE external_ref LIKE $1)`,
				prefix+"%").Scan(&n)
		})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	// SB-CATALOGUE-IO-1: fetch+validate happens first, with no transaction
	// open at all - an over-bound reference is rejected here and never even
	// reaches WithPlatformService, let alone the upsert loop.
	sync := func(p Provider) error {
		result, err := FetchCatalogue(context.Background(), p)
		if err != nil {
			return err
		}
		return pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			return SyncCatalogue(ctx, tx, result)
		})
	}

	over := prefix + strings.Repeat("z", providerref.MaxBytes)
	err := sync(fixedCatalogueProvider{prhrefCatalogue(prefix, over)})
	if !errors.Is(err, ErrProviderReferenceInvalid) {
		t.Fatalf("expected ErrProviderReferenceInvalid, got %v", err)
	}
	if strings.Contains(err.Error(), over) {
		t.Fatal("error text leaks the over-bound value")
	}
	if n := count(); n != 0 {
		t.Fatalf("a rejected sync wrote %d catalogue rows", n)
	}

	exact := prefix + strings.Repeat("y", providerref.MaxBytes-len(prefix))
	for i := 0; i < 2; i++ {
		if err := sync(fixedCatalogueProvider{prhrefCatalogue(prefix, exact)}); err != nil {
			t.Fatalf("exact-max sync %d: %v", i, err)
		}
	}
	if n := count(); n != 6 {
		t.Fatalf("expected 6 rows (1 sport, comp, event, market, 2 selections) after an idempotent re-sync, got %d", n)
	}
	var stored string
	if err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT external_ref FROM sb_selections WHERE external_ref = $1`, exact).Scan(&stored)
	}); err != nil || stored != exact {
		t.Fatalf("exact-max external_ref not stored verbatim: %v", err)
	}
}
