//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103 §5, SB-7/BS-9): RLS coverage for the two
// new tables. The "0106 pattern extended" exclusion list requires FIVE
// GUCs unset (app.player_account_id, app.platform_admin_principal_id,
// app.platform_service_id, app.acting_tenant_id,
// app.acting_platform_principal_id) alongside app.tenant_id matching -
// these tests exercise each exclusion specifically by setting app.tenant_id
// to the CORRECT value AND one excluded GUC in the SAME transaction, so a
// pass here can only mean the exclusion clause itself is doing the work,
// never a coincidental tenant mismatch.
package casino

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// setExtraGUC sets an additional session GUC inside an already-tenant-
// scoped transaction (pool.WithTenant already set app.tenant_id) - the
// only way to test the "tenant_id matches AND this other GUC is also set"
// mixed-context exclusion, since no production helper sets these two
// together (they are mutually exclusive session shapes in every real
// caller today; this is deliberate defense in depth, ADR 0103 §5).
func setExtraGUC(t *testing.T, tx pgx.Tx, guc, value string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `SELECT set_config($1, $2, true)`, guc, value); err != nil {
		t.Fatalf("set %s: %v", guc, err)
	}
}

func seedOneBootstrapRow(t *testing.T, pool *db.Pool, f casinoFixture, game Game, provider *MockCasinoProvider, orch *Orchestrator) {
	t.Helper()
	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := provider.BootstrapPayload(f.tenantID, token, "req-rls-seed-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed one bootstrap row: %v", err)
	}
}

func assertMixedGUCSeesNothing(t *testing.T, pool *db.Pool, f casinoFixture, guc, value string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		setExtraGUC(t, tx, guc, value)

		var bootstrapCount, refCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE tenant_id = $1`, f.tenantID).Scan(&bootstrapCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_provider_player_refs WHERE tenant_id = $1`, f.tenantID).Scan(&refCount); err != nil {
			return err
		}
		if bootstrapCount != 0 {
			t.Fatalf("expected 0 casino_launch_bootstraps rows visible with %s set, got %d", guc, bootstrapCount)
		}
		if refCount != 0 {
			t.Fatalf("expected 0 casino_provider_player_refs rows visible with %s set, got %d", guc, refCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mixed-GUC read (%s): %v", guc, err)
	}
}

func TestCasinoLaunchBootstrapsRLS_ExtendedExclusionList(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	seedOneBootstrapRow(t, pool, f, game, provider, orch)

	// Sanity: an ordinary tenant-scoped read sees the row.
	var count int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE tenant_id = $1`, f.tenantID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("sanity read: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the sanity read to see 1 row, got %d", count)
	}

	cases := []struct {
		guc   string
		value string
	}{
		{"app.player_account_id", f.playerAccountID.String()},
		{"app.platform_admin_principal_id", f.playerAccountID.String()},
		{"app.platform_service_id", "sportsbook_catalogue_sync"},
		{"app.acting_tenant_id", f.tenantID.String()},
		{"app.acting_platform_principal_id", f.playerAccountID.String()},
	}
	for _, c := range cases {
		t.Run(c.guc, func(t *testing.T) {
			assertMixedGUCSeesNothing(t, pool, f, c.guc, c.value)
		})
	}
}

// TestCasinoLaunchBootstrapsRLS_PlayerScopeCannotInsert proves the write
// side too: a genuinely player-scoped connection (db.Pool.WithPlayerScope,
// the real shape every player-authenticated request runs under) cannot
// insert into either new table - there is no player policy on these
// tables at all, so the attempt is refused by RLS's WITH CHECK, not merely
// unable to see rows.
func TestCasinoLaunchBootstrapsRLS_PlayerScopeCannotInsert(t *testing.T) {
	pool, f, _, _, _ := setupBootstrapFixture(t)

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_provider_player_refs (tenant_id, provider_id, player_account_id) VALUES ($1, $2, $3)`,
			f.tenantID, "mock-casino", f.playerAccountID)
		return err
	})
	if err == nil {
		t.Fatal("expected the player-scoped insert into casino_provider_player_refs to be refused")
	}

	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_launch_bootstraps (tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
			 VALUES ($1, 'mock-casino', 'req-rls-player-insert', repeat('a', 64), repeat('a', 64), gen_random_uuid(), gen_random_uuid(), '{}'::jsonb)`,
			f.tenantID)
		return err
	})
	if err == nil {
		t.Fatal("expected the player-scoped insert into casino_launch_bootstraps to be refused")
	}
}

// TestCasinoLaunchBootstrapsRLS_NonPlayerScopesCannotInsert is F-3's own
// missing case: TestCasinoLaunchBootstrapsRLS_PlayerScopeCannotInsert
// above proves the WRITE side only for the player scope; this proves it
// for the remaining four excluded GUCs
// (app.platform_admin_principal_id, app.platform_service_id,
// app.acting_tenant_id, app.acting_platform_principal_id) - each set
// ALONGSIDE the correct app.tenant_id, exactly like
// TestCasinoLaunchBootstrapsRLS_ExtendedExclusionList's own read-side
// mixed-context technique, so a pass here can only mean the INSERT
// policy's own WITH CHECK exclusion clause is doing the work, never a
// coincidental tenant mismatch.
func TestCasinoLaunchBootstrapsRLS_NonPlayerScopesCannotInsert(t *testing.T) {
	pool, f, _, _, _ := setupBootstrapFixture(t)

	cases := []struct {
		guc   string
		value string
	}{
		{"app.platform_admin_principal_id", f.playerAccountID.String()},
		{"app.platform_service_id", "sportsbook_catalogue_sync"},
		{"app.acting_tenant_id", f.tenantID.String()},
		{"app.acting_platform_principal_id", f.playerAccountID.String()},
	}
	for i, c := range cases {
		t.Run(c.guc, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				setExtraGUC(t, tx, c.guc, c.value)
				_, err := tx.Exec(ctx,
					`INSERT INTO casino_provider_player_refs (tenant_id, provider_id, player_account_id) VALUES ($1, $2, $3)`,
					f.tenantID, "mock-casino", uuid.New())
				return err
			})
			if err == nil {
				t.Fatalf("expected the insert into casino_provider_player_refs to be refused with %s set", c.guc)
			}

			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				setExtraGUC(t, tx, c.guc, c.value)
				_, err := tx.Exec(ctx,
					`INSERT INTO casino_launch_bootstraps (tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
					 VALUES ($1, 'mock-casino', $2, repeat('a', 64), repeat('a', 64), gen_random_uuid(), gen_random_uuid(), '{}'::jsonb)`,
					f.tenantID, fmt.Sprintf("req-rls-nonplayer-insert-%d", i))
				return err
			})
			if err == nil {
				t.Fatalf("expected the insert into casino_launch_bootstraps to be refused with %s set", c.guc)
			}
		})
	}
}
