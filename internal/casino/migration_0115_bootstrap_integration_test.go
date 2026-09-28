//go:build integration

// CAS-PLAY-BOOTSTRAP-1, migration 0115 (ADR 0103 §5, §9 "MIG"): the two
// new tables' own DB-level enforcement - the BEFORE INSERT session-binding
// guard, the request_digest format CHECK, append-only enforcement
// (UPDATE/DELETE/TRUNCATE refused), and the down migration refusing while
// rows exist.
package casino

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func requireCasinoP0001(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !isPgErrorWithCode(err, "P0001") && !isPgErrorWithCode(err, "23514") {
		t.Fatalf("expected a database-level refusal (P0001 raise_exception or 23514 check_violation), got %v", err)
	}
}

func isPgErrorWithCode(err error, code string) bool {
	for err != nil {
		if pe, ok := err.(*pgconn.PgError); ok {
			return pe.Code == code
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestCasinoLaunchBootstraps_InsertGuard_RefusesMismatch proves the BEFORE
// INSERT trigger refuses a row whose tenant/provider/token_hash does not
// match its own referenced session, or whose session is not 'consumed'.
func TestCasinoLaunchBootstraps_InsertGuard_RefusesMismatch(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)

	// A genuinely consumed session (via a real bootstrap), to isolate the
	// trigger's OWN checks from "the session isn't consumed at all".
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := provider.BootstrapPayload(f.tenantID, token, "req-guard-good", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed a consumed session: %v", err)
	}
	tokenHash := hashLaunchToken(token)

	// A second, still-active session for the "wrong session" cases.
	otherSession, _ := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	attempt := func(t *testing.T, requestID, sessionID string, providerID, hash string) error {
		t.Helper()
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO casino_launch_bootstraps (tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
				 VALUES ($1, $2, $3, $4, $5, $6, (SELECT player_ref FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = 'mock-casino' LIMIT 1), '{}'::jsonb)`,
				f.tenantID, providerID, requestID, hash, "0000000000000000000000000000000000000000000000000000000000000000"[:64], sessionID)
			return err
		})
	}

	t.Run("wrong_provider", func(t *testing.T) {
		err := attempt(t, "req-guard-wrong-provider", session.ID.String(), "some-other-provider", tokenHash)
		requireCasinoP0001(t, err)
	})
	t.Run("wrong_token_hash", func(t *testing.T) {
		err := attempt(t, "req-guard-wrong-hash", session.ID.String(), "mock-casino", "deadbeef00000000000000000000000000000000000000000000000000000000"[:64])
		requireCasinoP0001(t, err)
	})
	t.Run("session_not_consumed", func(t *testing.T) {
		otherHash := hashLaunchTokenOf(t, pool, f, otherSession.ID)
		err := attempt(t, "req-guard-not-consumed", otherSession.ID.String(), "mock-casino", otherHash)
		requireCasinoP0001(t, err)
	})
}

// hashLaunchTokenOf reads the stored token_hash for a session id directly
// (the raw token itself was never persisted, by design - this test needs
// the session's OWN hash to build a self-consistent, but still-invalid,
// insert attempt).
func hashLaunchTokenOf(t *testing.T, pool *db.Pool, f casinoFixture, sessionID interface{ String() string }) string {
	t.Helper()
	var hash string
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT token_hash FROM casino_launch_sessions WHERE id = $1`, sessionID.String()).Scan(&hash)
	})
	if err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	return hash
}

// TestCasinoLaunchBootstraps_RequestDigestFormatCheck proves the
// request_digest CHECK constraint (64-hex).
func TestCasinoLaunchBootstraps_RequestDigestFormatCheck(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := provider.BootstrapPayload(f.tenantID, token, "req-digest-good", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed a consumed session: %v", err)
	}
	tokenHash := hashLaunchToken(token)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_launch_bootstraps (tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
			 VALUES ($1, 'mock-casino', 'req-digest-bad', $2, 'not-a-hex-digest', $3,
				(SELECT player_ref FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = 'mock-casino' LIMIT 1), '{}'::jsonb)`,
			f.tenantID, tokenHash, session.ID)
		return err
	})
	requireCasinoP0001(t, err)
}

// TestCasinoLaunchBootstraps_UniqueLaunchSessionID proves BS-3 ("at most
// one bootstrap row per session") is enforced at the database layer: a
// second row referencing the SAME already-consumed session (a different
// request_id, so it does not collide on the (tenant,provider,request_id)
// key) is refused by the UNIQUE (launch_session_id) constraint. Under
// normal operation the application-level CAS already makes this
// unreachable (a session transitions active -> consumed exactly once), so
// this test inserts the second row directly to exercise the constraint
// itself, mirroring this file's own convention for testing the insert
// trigger and the digest CHECK directly.
func TestCasinoLaunchBootstraps_UniqueLaunchSessionID(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := provider.BootstrapPayload(f.tenantID, token, "req-unique-session-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed a consumed session: %v", err)
	}
	tokenHash := hashLaunchToken(token)
	digest := bootstrapRequestDigest("mock-casino", "req-unique-session-2", game.ProviderGameID, "EUR", "real")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_launch_bootstraps (tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
			 VALUES ($1, 'mock-casino', 'req-unique-session-2', $2, $3, $4,
				(SELECT player_ref FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = 'mock-casino' LIMIT 1), '{}'::jsonb)`,
			f.tenantID, tokenHash, digest, session.ID)
		return err
	})
	if err == nil {
		t.Fatal("expected a second bootstrap row for the same launch_session_id to be refused")
	}
}

// TestCasinoLaunchBootstraps_AppendOnly_RLSLayer proves layer 1: no
// UPDATE/DELETE policy exists on either table, so under FORCE RLS even the
// table owner's own tenant-scoped UPDATE/DELETE matches zero rows -
// mirrors casino_callback_rejections' own identical, established test
// shape (rejections_integration_test.go).
func TestCasinoLaunchBootstraps_AppendOnly_RLSLayer(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := provider.BootstrapPayload(f.tenantID, token, "req-append-only", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed a row: %v", err)
	}

	for _, stmt := range []string{
		`UPDATE casino_launch_bootstraps SET request_id = 'x' WHERE tenant_id = $1`,
		`DELETE FROM casino_launch_bootstraps WHERE tenant_id = $1`,
	} {
		var affected int64
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, stmt, f.tenantID)
			affected = tag.RowsAffected()
			return err
		})
		if err == nil && affected != 0 {
			t.Fatalf("expected %q to affect no row (no UPDATE/DELETE policy exists), affected %d", stmt, affected)
		}
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE casino_provider_player_refs SET player_ref = gen_random_uuid() WHERE tenant_id = $1`, f.tenantID)
		if err == nil && tag.RowsAffected() != 0 {
			t.Fatalf("expected the player-ref UPDATE to affect no row, affected %d", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error (expected 0 rows affected, not an error): %v", err)
	}
}

// TestCasinoLaunchBootstraps_AppendOnly_TriggerLayer proves layer 2, on a
// scratch database: even with a PERMISSIVE policy added (so RLS alone no
// longer blocks the mutation), the BEFORE UPDATE/DELETE/TRUNCATE deny
// trigger (ledger_deny_mutation(), migration 0021's shared function)
// itself still refuses - the actual binding control, not merely "no
// policy happens to grant it today." A kill control (dropping the
// trigger) proves this test would actually catch a missing/removed
// trigger, mirroring TestMigration0097_DenyTriggerBindsEvenWithA
// PermissiveUpdatePolicy exactly.
func TestCasinoLaunchBootstraps_AppendOnly_TriggerLayer(t *testing.T) {
	pool, _ := migration0099Scratch(t, "cas0115trg_", 115)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	fundWallet(t, pool, f, 10000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := provider.BootstrapPayload(f.tenantID, token, "req-trigger-layer", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed a row: %v", err)
	}

	exec := func(sql string) error {
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
	}
	if err := exec(`CREATE POLICY test_permissive_mutate ON casino_launch_bootstraps FOR ALL USING (true) WITH CHECK (true)`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE casino_launch_bootstraps SET request_id = 'permissive-update'`,
		`DELETE FROM casino_launch_bootstraps`,
		`TRUNCATE casino_launch_bootstraps`,
	} {
		if err := exec(stmt); err == nil {
			t.Fatalf("%q must be refused by the deny trigger even under a permissive policy", stmt)
		} else {
			requireCasinoP0001(t, err)
		}
	}

	// Kill control: without the row trigger, the same UPDATE goes through.
	if err := exec(`DROP TRIGGER casino_launch_bootstraps_immutable ON casino_launch_bootstraps`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`UPDATE casino_launch_bootstraps SET request_id = 'permissive-update'`); err != nil {
		t.Fatalf("control: with the trigger dropped the UPDATE must succeed, got %v", err)
	}
}

// TestMigration0115_DownRefusesWhileRowsExist proves the down migration
// refuses rather than silently discarding history, on a scratch database.
func TestMigration0115_DownRefusesWhileRowsExist(t *testing.T) {
	pool, dir := migration0099Scratch(t, "cas0115_", 115)

	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	fundWallet(t, pool, f, 10000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := provider.BootstrapPayload(f.tenantID, token, "req-scratch-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("seed a row on the scratch database: %v", err)
	}

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err == nil {
		t.Fatal("expected the down migration to refuse while rows exist")
	}
}
