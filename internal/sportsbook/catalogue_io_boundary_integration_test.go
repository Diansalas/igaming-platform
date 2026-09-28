//go:build integration

// SB-CATALOGUE-IO-1 (ADR 0095 amendment, 2026-09-28): regression coverage
// for the provider-I/O/transaction-boundary split. Provider.Catalogue is
// called by FetchCatalogue only, with no pooled database transaction held
// (ADR 0094 INV-POOL / ADR 0095's no-provider-I/O-with-a-connection-held
// rule) - mirroring payments' callProvider and casino's
// Orchestrator.LaunchGame's own txscope.Held(ctx) defence-in-depth
// refusal. SyncCatalogue then upserts the already-fetched, already-
// validated result inside its own separate db.Pool.WithPlatformService
// transaction.
package sportsbook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// txscopeAssertingProvider records, for every Catalogue call, whether ctx
// was txscope-held at the moment the provider was invoked - the same
// runtime signal a real network adapter would see. It fails the test
// immediately (via t.Errorf, not t.Fatalf, so the call still returns and
// the caller's own error handling is exercised) if a held ctx ever reaches
// the provider, which is exactly the shape a mutant that moves the fetch
// back inside the sync transaction would produce.
type txscopeAssertingProvider struct {
	t       *testing.T
	result  CatalogueResult
	calls   int
	heldSum int
}

func (p *txscopeAssertingProvider) Catalogue(ctx context.Context) (CatalogueResult, error) {
	p.calls++
	if txscope.Held(ctx) {
		p.heldSum++
		p.t.Errorf("provider.Catalogue called with a txscope-held ctx (call #%d) - a pooled database transaction must never be open during a provider call (ADR 0094 INV-POOL / ADR 0095)", p.calls)
	}
	return p.result, nil
}

// fullCatalogueSync is the same two-step sequence cmd/platform-api/main.go
// itself performs: FetchCatalogue (no transaction) then SyncCatalogue
// (inside its own WithPlatformService transaction). Kept here, rather than
// duplicated per test, so every test below exercises the identical call
// shape production wires - a change that reintroduces the provider call
// inside the transaction closure would have to change this helper too,
// which is the point: a mutant that inlines FetchCatalogue's body directly
// into the WithPlatformService closure (see the mutation-kill note in the
// SB-CATALOGUE-IO-1 report) is caught by txscopeAssertingProvider.Catalogue
// observing a held ctx, regardless of which of these two call sites moved.
func fullCatalogueSync(ctx context.Context, pool *db.Pool, provider Provider) error {
	result, err := FetchCatalogue(ctx, provider)
	if err != nil {
		return err
	}
	return pool.WithPlatformService(ctx, db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		return SyncCatalogue(ctx, tx, result)
	})
}

func countCatalogueRowsWithPrefix(t *testing.T, pool *db.Pool, prefix string) int {
	t.Helper()
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

func minimalCatalogue(prefix string) CatalogueResult {
	return CatalogueResult{Sports: []CatalogueSport{{
		ExternalRef: prefix + "-sport", Code: prefix, Name: "IO boundary sport",
		Competitions: []CatalogueCompetition{{
			ExternalRef: prefix + "-comp", Name: "C",
			Events: []CatalogueEvent{{
				ExternalRef: prefix + "-event", Name: "E", Status: EventScheduled,
				Markets: []CatalogueMarket{{
					ExternalRef: prefix + "-market", Name: "M", Status: MarketOpen,
					Selections: []CatalogueSelection{
						{ExternalRef: prefix + "-home", Name: "Home", OddsNumerator: 2, OddsDenominator: 1},
						{ExternalRef: prefix + "-away", Name: "Away", OddsNumerator: 3, OddsDenominator: 1},
					},
				}},
			}},
		}},
	}}}
}

// TestFullCatalogueSync_ProviderNeverSeesAHeldTransaction is the
// deterministic "no DB transaction is open during the provider call"
// proof: a real production-shaped sync (fullCatalogueSync) runs against a
// provider that would fail the test the instant it observed a txscope-
// held ctx. Also proves the sync still succeeds and writes the expected
// rows - this is not merely a refusal test.
func TestFullCatalogueSync_ProviderNeverSeesAHeldTransaction(t *testing.T) {
	pool := testPool(t)
	prefix := "sbio-ok-" + uuid.NewString()[:8]
	provider := &txscopeAssertingProvider{t: t, result: minimalCatalogue(prefix)}

	if err := fullCatalogueSync(context.Background(), pool, provider); err != nil {
		t.Fatalf("fullCatalogueSync: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", provider.calls)
	}
	if provider.heldSum != 0 {
		t.Fatalf("expected 0 held-ctx observations, got %d", provider.heldSum)
	}
	if n := countCatalogueRowsWithPrefix(t, pool, prefix); n != 6 {
		t.Fatalf("expected 6 rows (sport, comp, event, market, 2 selections) after a successful sync, got %d", n)
	}
}

// errCatalogueProvider always fails the fetch - used to prove a fetch
// error writes nothing.
type errCatalogueProvider struct{ err error }

func (p errCatalogueProvider) Catalogue(_ context.Context) (CatalogueResult, error) {
	return CatalogueResult{}, p.err
}

// TestFetchCatalogue_ProviderErrorWritesNothing: a provider.Catalogue
// error surfaces from FetchCatalogue wrapped with plain context (NOT
// ErrCatalogueFetchRefused - code review F5: that sentinel is reserved for
// the tx-held gate refusal only, never an ordinary provider/network
// failure) and the sync never reaches WithPlatformService at all, so
// nothing is ever written - not even a rolled-back attempt.
func TestFetchCatalogue_ProviderErrorWritesNothing(t *testing.T) {
	pool := testPool(t)
	prefix := "sbio-fetcherr-" + uuid.NewString()[:8]
	providerErr := errors.New("boom: upstream sportsbook feed unavailable")
	provider := errCatalogueProvider{err: providerErr}

	err := fullCatalogueSync(context.Background(), pool, provider)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if errors.Is(err, ErrCatalogueFetchRefused) {
		t.Fatalf("an ordinary provider error must NOT be ErrCatalogueFetchRefused (that sentinel is tx-held-only), got %v", err)
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("expected the underlying provider error to be surfaced, got %v", err)
	}
	if n := countCatalogueRowsWithPrefix(t, pool, prefix); n != 0 {
		t.Fatalf("a failed fetch wrote %d catalogue rows", n)
	}
}

// TestFetchCatalogue_RefusesUnderTxscopeHeld proves the defence-in-depth
// refusal: a ctx already marked as holding a pooled database transaction
// (as every db.Pool.With* callback's ctx is) is refused before the
// provider is ever called.
func TestFetchCatalogue_RefusesUnderTxscopeHeld(t *testing.T) {
	pool := testPool(t)
	prefix := "sbio-txheld-" + uuid.NewString()[:8]
	provider := &txscopeAssertingProvider{t: t, result: minimalCatalogue(prefix)}

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := FetchCatalogue(ctx, provider)
		return err
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, ErrCatalogueFetchRefused) {
		t.Fatalf("expected ErrCatalogueFetchRefused, got %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("expected the provider to never be called when ctx is txscope-held, got %d calls", provider.calls)
	}
}

// overBoundCatalogue is minimalCatalogue with its deepest external_ref
// (the "away" selection) pushed one byte past providerref.MaxBytes.
func overBoundCatalogue(prefix string) CatalogueResult {
	result := minimalCatalogue(prefix)
	result.Sports[0].Competitions[0].Events[0].Markets[0].Selections[1].ExternalRef = prefix + strings.Repeat("z", providerref.MaxBytes)
	return result
}

// TestSyncCatalogue_DirectOverBoundResultWritesNothing is code review F3:
// SyncCatalogue's OWN re-validation (defence in depth, independent of
// FetchCatalogue's) is exercised directly, by calling it inside
// WithPlatformService with an already-over-bound CatalogueResult that
// never went through FetchCatalogue at all - proving a caller that skips
// FetchCatalogue still cannot write a half-validated tree. Kills mutant X3
// (re-validation removed from SyncCatalogue): with that mutant, this test
// would see a nil error and 6 written rows instead.
func TestSyncCatalogue_DirectOverBoundResultWritesNothing(t *testing.T) {
	pool := testPool(t)
	prefix := "sbio-syncdirect-" + uuid.NewString()[:8]
	result := overBoundCatalogue(prefix)

	err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		return SyncCatalogue(ctx, tx, result)
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, ErrProviderReferenceInvalid) {
		t.Fatalf("expected ErrProviderReferenceInvalid, got %v", err)
	}
	if n := countCatalogueRowsWithPrefix(t, pool, prefix); n != 0 {
		t.Fatalf("a directly-called SyncCatalogue with an over-bound result wrote %d catalogue rows", n)
	}
}
