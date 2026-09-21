//go:build integration

// Migration 0084 (ADR 0081, ARCH-DB-2) regression coverage specific to
// this package: SyncCatalogue's own scope requirement and its continued
// idempotency under the new WithPlatformService-only write path (ADR 0081
// §7.6 items 9 and 10).
package sportsbook

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestSyncCatalogue_RequiresPlatformServiceScope proves SyncCatalogue's
// own AssertPlatformServiceScope call is a real, in-function control: a
// WithoutTenant transaction - the exact scope this function used before
// migration 0084 - is rejected with db.ErrPlatformServiceScope before any
// catalogue row is written, never silently falling through to an RLS
// error deep inside the first INSERT.
func TestSyncCatalogue_RequiresPlatformServiceScope(t *testing.T) {
	pool := testPool(t)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return SyncCatalogue(ctx, tx, NewMockSportsbookProvider())
	})
	if err == nil {
		t.Fatal("expected SyncCatalogue to fail under WithoutTenant, got nil")
	}
	if !errors.Is(err, db.ErrPlatformServiceScope) {
		t.Fatalf("expected db.ErrPlatformServiceScope, got %v", err)
	}
}

// TestSyncCatalogue_IdempotentAcrossTwoRuns is ADR 0081 §7.6 item 10:
// SyncCatalogue stays idempotent (upserts keyed by external_ref, never
// duplicate rows) when run twice under its new, sole legitimate scope,
// db.Pool.WithPlatformService(ctx, db.ServiceSportsbookCatalogueSync, ...).
func TestSyncCatalogue_IdempotentAcrossTwoRuns(t *testing.T) {
	pool := testPool(t)
	provider := NewMockSportsbookProvider()

	runSync := func() {
		err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			return SyncCatalogue(ctx, tx, provider)
		})
		if err != nil {
			t.Fatalf("SyncCatalogue under WithPlatformService failed: %v", err)
		}
	}

	// Scoped to this test's own rows only (external_ref LIKE 'mock-%',
	// MockSportsbookProvider's fixed prefix - see mock.go), never an
	// unscoped whole-table count: internal/db's own
	// catalogue_write_authorization_integration_test.go writes to these
	// same five tables concurrently under `go test ./...` (different
	// packages run concurrently by default), using its own disjoint
	// "archdb2-*" external_ref prefix, so an unscoped count here is a
	// genuine cross-package test-isolation collision, not a fixture-
	// staleness issue - reproduced by qa at a 1-in-3 failure rate running
	// both packages together.
	countRows := func(table string) int {
		var count int
		err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE external_ref LIKE 'mock-%'`).Scan(&count)
		})
		if err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return count
	}

	runSync()
	firstCounts := map[string]int{
		"sb_sports":       countRows("sb_sports"),
		"sb_competitions": countRows("sb_competitions"),
		"sb_events":       countRows("sb_events"),
		"sb_markets":      countRows("sb_markets"),
		"sb_selections":   countRows("sb_selections"),
	}
	for table, count := range firstCounts {
		if count < 1 {
			t.Fatalf("expected at least 1 row in %s after the first sync, got %d", table, count)
		}
	}

	runSync()
	for table, wantCount := range firstCounts {
		gotCount := countRows(table)
		if gotCount != wantCount {
			t.Errorf("%s: expected row count to stay %d across a second SyncCatalogue run (idempotent upsert by external_ref), got %d", table, wantCount, gotCount)
		}
	}
}
