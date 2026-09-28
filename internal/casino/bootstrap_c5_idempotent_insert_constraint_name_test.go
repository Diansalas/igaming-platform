//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103) - code review C-5: F-7's own branch in
// BootstrapLaunch (bootstrap.go) decides between the legitimate replay/
// race path and ErrBootstrapInvariantBroken purely on the constraint NAME
// db.IdempotentInsert returns. This pins that INPUT directly against
// casino_launch_bootstraps' own two real unique constraints, independent
// of whether BootstrapLaunch's own alternate-constraint branch can be
// driven end to end (bootstrap.go's own comment on that branch explains
// why it structurally cannot be, for any real execution).
package casino

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestIdempotentInsert_ReturnsViolatedConstraintName_ForBothBootstrapConstraints
// seeds one real, legitimate casino_launch_bootstraps row (via a real
// bootstrap), then issues two further raw INSERTs through
// db.IdempotentInsert - one that conflicts on
// casino_launch_bootstraps_once_per_request (same tenant/provider/
// request_id), and one that conflicts on
// casino_launch_bootstraps_once_per_session (same launch_session_id, a
// fresh request_id) - and asserts IdempotentInsert reports each
// constraint's own name exactly. This is the input F-7's branch
// (`constraintName != casinoLaunchBootstrapsOncePerRequestConstraint`)
// depends on; pinning it here catches any future change to either
// constraint's name or to db.IdempotentInsert's own parsing.
func TestIdempotentInsert_ReturnsViolatedConstraintName_ForBothBootstrapConstraints(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	in := provider.BootstrapPayload(f.tenantID, token, "req-c5-seed", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed a legitimate bootstrap row: %v", err)
	}

	tokenHash := hashLaunchToken(token)
	digest := bootstrapRequestDigest("mock-casino", "req-c5-seed", game.ProviderGameID, "EUR", "real")

	var playerRef uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT player_ref FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = 'mock-casino' LIMIT 1`,
			f.tenantID).Scan(&playerRef)
	})
	if err != nil {
		t.Fatalf("read the seeded player_ref: %v", err)
	}

	t.Run("casino_launch_bootstraps_once_per_request", func(t *testing.T) {
		var conflict bool
		var constraintName string
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			conflict, constraintName, err = db.IdempotentInsert(ctx, tx, func(stx pgx.Tx) error {
				_, err := stx.Exec(ctx,
					`INSERT INTO casino_launch_bootstraps
						(tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
					 VALUES ($1, 'mock-casino', 'req-c5-seed', $2, $3, $4, $5, '{}'::jsonb)`,
					f.tenantID, tokenHash, digest, session.ID, playerRef)
				return err
			})
			return err
		})
		if err != nil {
			t.Fatalf("run once_per_request sub-test: %v", err)
		}
		if !conflict {
			t.Fatal("expected a conflict on the same (tenant, provider, request_id)")
		}
		if constraintName != casinoLaunchBootstrapsOncePerRequestConstraint {
			t.Fatalf("expected constraint name %q, got %q", casinoLaunchBootstrapsOncePerRequestConstraint, constraintName)
		}
	})

	t.Run("casino_launch_bootstraps_once_per_session", func(t *testing.T) {
		const wantConstraint = "casino_launch_bootstraps_once_per_session"
		var conflict bool
		var constraintName string
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			conflict, constraintName, err = db.IdempotentInsert(ctx, tx, func(stx pgx.Tx) error {
				_, err := stx.Exec(ctx,
					`INSERT INTO casino_launch_bootstraps
						(tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
					 VALUES ($1, 'mock-casino', 'req-c5-different-request-id', $2, $3, $4, $5, '{}'::jsonb)`,
					f.tenantID, tokenHash, digest, session.ID, playerRef)
				return err
			})
			return err
		})
		if err != nil {
			t.Fatalf("run once_per_session sub-test: %v", err)
		}
		if !conflict {
			t.Fatal("expected a conflict on the same launch_session_id")
		}
		if constraintName != wantConstraint {
			t.Fatalf("expected constraint name %q, got %q", wantConstraint, constraintName)
		}
	})
}
