//go:build integration

// CAS-REVOKE-CONSUMED-1 (docs/plans/prh2-hardening-round/plan.md §5-A;
// security's required fix, docs/plans/payment-readiness/rv-prh-i2-casino-
// security.md "Re-review (FH-7, 2026-09-28)"; migration 0108). This file
// covers everything the required-fix spec names beyond the inverted
// characterization test (which lives in
// launch_two_phase_integration_test.go, next to the fixed-behind-a-
// migration production path it replaced):
//
//   - the DB trigger matrix, one transaction per statement;
//   - token replay after consumed -> revoked;
//   - pre-revoke bets still settle (win and rollback);
//   - tenant isolation on the revoke itself;
//   - migration 0108 up/down/up.
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedLaunchSessionAtStatus mints a genuine casino_launch_sessions row via
// CreateLaunchSession (so every FK and NOT NULL column is populated
// exactly like production) and, unless status is already 'active',
// transitions it once with a plain UPDATE. Every one of these starting
// transitions (active -> consumed/expired/revoked) is unrestricted by
// migration 0036/0042/0108's trigger - only what happens FROM a terminal
// status is what this migration changes - so a single UPDATE from the
// freshly-minted 'active' row reaches every starting state the matrix
// below needs.
func seedLaunchSessionAtStatus(t *testing.T, pool *db.Pool, f casinoFixture, game Game, status LaunchSessionStatus) uuid.UUID {
	t.Helper()
	var sessionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		if err != nil {
			return err
		}
		sessionID = s.ID
		switch status {
		case LaunchSessionActive:
			return nil
		case LaunchSessionConsumed:
			_, err = tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1`, sessionID)
		case LaunchSessionExpired:
			_, err = tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, sessionID)
		case LaunchSessionRevoked:
			_, err = tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, sessionID)
		default:
			t.Fatalf("seedLaunchSessionAtStatus: unhandled status %q", status)
		}
		return err
	})
	if err != nil {
		t.Fatalf("seed launch session at status %q: %v", status, err)
	}
	return sessionID
}

// TestMigration0108_TriggerMatrix is the security spec's own trigger
// matrix, one transaction per statement (a refused statement's transaction
// is rolled back and never affects the next case's fresh row).
func TestMigration0108_TriggerMatrix(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	attempt := func(t *testing.T, from LaunchSessionStatus, sql string, wantRefused bool) {
		t.Helper()
		id := seedLaunchSessionAtStatus(t, pool, f, game, from)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, id)
			return err
		})
		if wantRefused && err == nil {
			t.Fatalf("expected the transition to be refused, but it succeeded")
		}
		if !wantRefused && err != nil {
			t.Fatalf("expected the transition to be allowed, got %v", err)
		}
	}

	// --- Allowed ---
	t.Run("allowed/consumed_to_revoked", func(t *testing.T) {
		attempt(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, false)
	})
	t.Run("allowed/active_to_revoked", func(t *testing.T) {
		attempt(t, LaunchSessionActive, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, false)
	})
	t.Run("allowed/active_to_expired", func(t *testing.T) {
		attempt(t, LaunchSessionActive, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, false)
	})
	t.Run("allowed/active_to_consumed", func(t *testing.T) {
		attempt(t, LaunchSessionActive, `UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1`, false)
	})

	// --- Refused: every transition out of 'consumed' other than -> revoked ---
	t.Run("refused/consumed_to_active", func(t *testing.T) {
		attempt(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'active' WHERE id = $1`, true)
	})
	t.Run("refused/consumed_to_expired", func(t *testing.T) {
		attempt(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, true)
	})
	t.Run("refused/consumed_to_consumed_noop", func(t *testing.T) {
		attempt(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'consumed' WHERE id = $1`, true)
	})

	// --- Refused: consumed -> revoked combined with any other column change ---
	t.Run("refused/consumed_to_revoked_with_consumed_at_change", func(t *testing.T) {
		attempt(t, LaunchSessionConsumed,
			`UPDATE casino_launch_sessions SET status = 'revoked', consumed_at = consumed_at + interval '1 second' WHERE id = $1`, true)
	})
	t.Run("refused/consumed_to_revoked_with_consumed_at_null", func(t *testing.T) {
		attempt(t, LaunchSessionConsumed,
			`UPDATE casino_launch_sessions SET status = 'revoked', consumed_at = NULL WHERE id = $1`, true)
	})
	t.Run("refused/consumed_to_revoked_with_expires_at_change", func(t *testing.T) {
		attempt(t, LaunchSessionConsumed,
			`UPDATE casino_launch_sessions SET status = 'revoked', expires_at = expires_at + interval '1 second' WHERE id = $1`, true)
	})
	t.Run("refused/consumed_to_revoked_with_tenant_id_change", func(t *testing.T) {
		// Already caught by 0042's own column-immutability block (tenant_id
		// is in that list regardless of status), and/or by this table's
		// own RLS WITH CHECK (a tenant-scoped connection can never write a
		// row whose tenant_id no longer matches app.tenant_id) - either
		// way, the transition must be refused.
		id := seedLaunchSessionAtStatus(t, pool, f, game, LaunchSessionConsumed)
		otherTenant := uuid.New()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'revoked', tenant_id = $2 WHERE id = $1`, id, otherTenant)
			return err
		})
		if err == nil {
			t.Fatal("expected the transition to be refused, but it succeeded")
		}
	})

	// --- Refused: every transition out of 'revoked' ---
	t.Run("refused/revoked_to_active", func(t *testing.T) {
		attempt(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'active' WHERE id = $1`, true)
	})
	t.Run("refused/revoked_to_consumed", func(t *testing.T) {
		attempt(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1`, true)
	})
	t.Run("refused/revoked_to_expired", func(t *testing.T) {
		attempt(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, true)
	})
	t.Run("refused/revoked_to_revoked_noop", func(t *testing.T) {
		attempt(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, true)
	})

	// --- Refused: every transition out of 'expired' ---
	t.Run("refused/expired_to_revoked", func(t *testing.T) {
		attempt(t, LaunchSessionExpired, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, true)
	})
	t.Run("refused/expired_to_active", func(t *testing.T) {
		attempt(t, LaunchSessionExpired, `UPDATE casino_launch_sessions SET status = 'active' WHERE id = $1`, true)
	})
	t.Run("refused/expired_to_consumed", func(t *testing.T) {
		attempt(t, LaunchSessionExpired, `UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1`, true)
	})
}

// TestRevokeLaunchSession_ConsumedToRevoked_TokenReplayRefused proves the
// security spec's "token replay is unaffected" line end to end: once
// RevokeLaunchSession moves a genuinely vendor-consumed session to
// 'revoked', a later replay of the SAME raw token still resolves nothing -
// ResolveLaunchToken accepts only 'active' - now for the additional
// reason that the row itself is 'revoked', not merely 'consumed'.
func TestRevokeLaunchSession_ConsumedToRevoked_TokenReplayRefused(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	var sessionID uuid.UUID
	var rawToken string
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, tok, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		if err != nil {
			return err
		}
		sessionID, rawToken = s.ID, tok
		return nil
	})
	if err != nil {
		t.Fatalf("create launch session: %v", err)
	}

	// The vendor's normal bootstrap call: consumes the token once.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := ResolveLaunchToken(ctx, tx, rawToken)
		if err != nil {
			return err
		}
		if s.ID != sessionID {
			t.Fatalf("resolved the wrong session: got %s, want %s", s.ID, sessionID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first token resolution: %v", err)
	}

	// Phase C's revoke: consumed -> revoked.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		prior, revoked, err := RevokeLaunchSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if prior != LaunchSessionConsumed {
			t.Fatalf("expected prior status 'consumed', got %q", prior)
		}
		if !revoked {
			t.Fatal("expected the CAS to match the consumed session")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The replay: same raw token, now against a 'revoked' row.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ResolveLaunchToken(ctx, tx, rawToken)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionNotActive) {
		t.Fatalf("expected ErrLaunchSessionNotActive on replay after revoke, got %v", err)
	}
}

// TestRevokeLaunchSession_PreRevokeBetsStillSettle proves a bet placed
// while the session was genuinely 'consumed' (the normal in-play state,
// CAS-SESSION-EXPIRY-1) keeps settling correctly - via a win, and via a
// rollback - even after phase C later revokes that same session, and the
// ledger stays balanced throughout.
func TestRevokeLaunchSession_PreRevokeBetsStillSettle(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 10000)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	result, err := orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err != nil {
		t.Fatalf("launch game: %v", err)
	}
	token := extractToken(t, result.LaunchURL)

	// The vendor consumes the token - the session is now genuinely
	// 'consumed' and in-play.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ResolveLaunchToken(ctx, tx, token)
		return err
	})
	if err != nil {
		t.Fatalf("consume launch token: %v", err)
	}

	// Two bets placed while the session is still 'consumed', BEFORE any
	// revoke - one will be won, the other rolled back.
	betWin := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-settle-win", "", "round-settle-win", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, result.SessionID)
	betRollback := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-settle-rb", "", "round-settle-rb", "game-1", 700, "EUR", OutcomeSucceeded, "", f.playerAccountID, result.SessionID)

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betWin)
		return err
	}); err != nil {
		t.Fatalf("post bet-settle-win: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betRollback)
		return err
	}); err != nil {
		t.Fatalf("post bet-settle-rb: %v", err)
	}

	// Phase C revokes the (still 'consumed') session.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		prior, revoked, err := RevokeLaunchSession(ctx, tx, result.SessionID)
		if err != nil {
			return err
		}
		if prior != LaunchSessionConsumed || !revoked {
			t.Fatalf("expected the CAS to match a consumed session, got prior=%q revoked=%v", prior, revoked)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The win settles the first bet, after the revoke.
	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-settle-win", "", "round-settle-win", "game-1", 2500, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	}); err != nil {
		t.Fatalf("post win after revoke: %v", err)
	}

	// The rollback reverses the second bet, also after the revoke.
	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-settle-rb", "bet-settle-rb", "round-settle-rb", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	}); err != nil {
		t.Fatalf("post rollback after revoke: %v", err)
	}

	// 10000 - 1000 (bet-settle-win) + 2500 (win) - 700 (bet-settle-rb) + 700 (rollback refund) = 11500.
	if balance := cashBalance(t, pool, f); balance != 11500 {
		t.Fatalf("expected cash balance 11500, got %d", balance)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("ledger not balanced: debits=%d credits=%d", debits, credits)
	}
}

// TestRevokeLaunchSession_TenantIsolation proves a revoke executed under
// tenant B's own connection context can never touch tenant A's session -
// RLS scopes the UPDATE to zero rows, not an error, exactly like every
// other tenant-scoped write in this codebase.
func TestRevokeLaunchSession_TenantIsolation(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	gameA := seedGame(t, pool, "mock-casino", "EUR")

	sessionID := seedLaunchSessionAtStatus(t, pool, fA, gameA, LaunchSessionConsumed)

	// Attempt the revoke under tenant B's own RLS-scoped context.
	var prior LaunchSessionStatus
	var revoked bool
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		prior, revoked, err = RevokeLaunchSession(ctx, tx, sessionID)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionNotFound) {
		t.Fatalf("expected ErrLaunchSessionNotFound (RLS hides A's row from B), got prior=%q revoked=%v err=%v", prior, revoked, err)
	}

	// Tenant A's own view is untouched: still 'consumed'.
	var status LaunchSessionStatus
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session under tenant A: %v", err)
	}
	if status != LaunchSessionConsumed {
		t.Fatalf("expected tenant A's session to remain 'consumed' (unaffected by B's attempt), got %q", status)
	}
}

// migration0108Version is the version this migration was allocated.
const migration0108Version = int64(108)

// TestMigration0108_UpDownUp proves the round trip on a scratch database:
// migrating up through 0108 permits consumed -> revoked; migrating back
// down one step restores 0042's stricter body (the SAME transition is
// refused again); migrating up again re-permits it.
func TestMigration0108_UpDownUp(t *testing.T) {
	pool, dir := migration0099Scratch(t, "cas0108_", migration0108Version)

	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	tryConsumedToRevoked := func() error {
		id := seedLaunchSessionAtStatus(t, pool, f, game, LaunchSessionConsumed)
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, id)
			return err
		})
	}

	// Up through 0108: consumed -> revoked is permitted.
	if err := tryConsumedToRevoked(); err != nil {
		t.Fatalf("expected consumed -> revoked to be permitted after migrating up through 0108, got %v", err)
	}

	// Down one step (0108's own down): restores 0042's body verbatim.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	if len(rolledBack) != 1 || rolledBack[0] != migration0108Version {
		t.Fatalf("expected to roll back exactly [%d], got %v", migration0108Version, rolledBack)
	}
	if err := tryConsumedToRevoked(); err == nil {
		t.Fatal("expected consumed -> revoked to be refused again after rolling back 0108")
	}

	// Up again: re-permits it.
	appliedAgain, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if len(appliedAgain) != 1 || appliedAgain[0] != migration0108Version {
		t.Fatalf("expected to re-apply exactly [%d], got %v", migration0108Version, appliedAgain)
	}
	if err := tryConsumedToRevoked(); err != nil {
		t.Fatalf("expected consumed -> revoked to be permitted again after re-applying 0108, got %v", err)
	}
}
