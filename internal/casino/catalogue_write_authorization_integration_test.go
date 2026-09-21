//go:build integration

// Migration 0084 (ADR 0081, ARCH-DB-2) regression coverage specific to
// this package: UpsertGame's own Go-level scope assertion.
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestUpsertGame_RequiresPlatformAdminScope proves UpsertGame's own
// assertPlatformScope call is a real, in-function control: a
// WithoutTenant transaction - the exact scope newUpsertCasinoGameHandler
// used before migration 0084 - is rejected with ErrTransactionScope
// before any row is written, never silently falling through to an RLS
// error deep inside the INSERT.
func TestUpsertGame_RequiresPlatformAdminScope(t *testing.T) {
	pool := testPool(t)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: "scope-test", ProviderGameID: "scope-test-" + uuid.New().String()[:8],
			Name: "Scope Test Game", GameType: "slot",
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope, got %v", err)
	}
}

// TestUpsertGame_RequiresPlatformAdminScope_TenantScoped confirms the
// same refusal under a genuinely tenant-scoped transaction, not merely
// WithoutTenant.
func TestUpsertGame_RequiresPlatformAdminScope_TenantScoped(t *testing.T) {
	pool := testPool(t)

	err := pool.WithTenant(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: "scope-test", ProviderGameID: "scope-test-" + uuid.New().String()[:8],
			Name: "Scope Test Game", GameType: "slot",
		})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope, got %v", err)
	}
}
