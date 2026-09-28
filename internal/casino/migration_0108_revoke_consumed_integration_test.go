//go:build integration

// CAS-REVOKE-CONSUMED-1 (docs/plans/prh2-hardening-round/plan.md §5-A;
// security's required fix, docs/plans/payment-readiness/rv-prh-i2-casino-
// security.md "Re-review (FH-7, 2026-09-28)"; migration 0108). This file
// covers everything the required-fix spec names beyond the inverted
// characterization test (which lives in
// launch_two_phase_integration_test.go, next to the fixed-behind-a-
// migration production path it replaced):
//
//   - the DB trigger matrix, one transaction per statement, asserting
//     SQLSTATE P0001 and the specific trigger message for every refused
//     cell (code review A5);
//   - token replay after consumed -> revoked;
//   - pre-revoke bets still settle (win and rollback);
//   - tenant isolation on the revoke itself;
//   - migration 0108 up/down/up;
//   - RevokeLaunchSession as a no-op on an already-'expired' or already-
//     'revoked' session, including a phase-C case that still audits
//     (code review A2, kills mutant X5);
//   - a two-connection test proving FOR UPDATE is what keeps a concurrent
//     revoke's own reported prior_status exact (code review A3, kills
//     mutant X4).
package casino

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// The two RAISE EXCEPTION messages casino_launch_sessions_enforce_
// immutable_fields() can produce (both SQLSTATE P0001, Postgres's generic
// "raise_exception" code for a PL/pgSQL RAISE with no explicit SQLSTATE) -
// asserted verbatim below rather than merely checking err != nil (code
// review A5), so a mutant that raises for the wrong reason on the wrong
// cell cannot hide behind a bare non-nil check.
const (
	msgColumnsImmutable  = "casino_launch_sessions: identity/token/expiry columns are immutable after insert"
	msgTerminalImmutable = "casino_launch_sessions: row is immutable once consumed, expired, or revoked"
)

// requirePgError asserts err is (or wraps) a *pgconn.PgError with the
// given SQLSTATE and message - every refused cell in this file's matrix
// is refused by one of this table's own two BEFORE UPDATE RAISE EXCEPTION
// blocks (migration 0036/0042/0108), never by RLS: RLS's WITH CHECK is
// only evaluated after BEFORE ROW triggers run, and every mutation this
// matrix attempts is already caught by one of the two blocks above (the
// tenant_id and expires_at cells, in particular, are caught by 0042's own
// column-immutability block, not by RLS or by 0108's new terminal-status
// block - verified empirically with a throwaway probe, per this file's own
// convention).
func requirePgError(t *testing.T, err error, wantCode, wantMessage string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != wantCode {
		t.Fatalf("expected SQLSTATE %s, got %s: %v", wantCode, pgErr.Code, err)
	}
	if pgErr.Message != wantMessage {
		t.Fatalf("expected trigger message %q, got %q", wantMessage, pgErr.Message)
	}
}

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

	// allow runs a transition expected to succeed.
	allow := func(t *testing.T, from LaunchSessionStatus, sql string) {
		t.Helper()
		id := seedLaunchSessionAtStatus(t, pool, f, game, from)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, id)
			return err
		})
		if err != nil {
			t.Fatalf("expected the transition to be allowed, got %v", err)
		}
	}

	// refuse runs a transition expected to be refused by one of this
	// table's own two RAISE EXCEPTION blocks, asserting the exact SQLSTATE
	// and message (code review A5) rather than a bare err != nil.
	refuse := func(t *testing.T, from LaunchSessionStatus, sql, wantMessage string) {
		t.Helper()
		id := seedLaunchSessionAtStatus(t, pool, f, game, from)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, id)
			return err
		})
		if err == nil {
			t.Fatal("expected the transition to be refused, but it succeeded")
		}
		requirePgError(t, err, "P0001", wantMessage)
	}

	// --- Allowed ---
	t.Run("allowed/consumed_to_revoked", func(t *testing.T) {
		allow(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`)
	})
	t.Run("allowed/active_to_revoked", func(t *testing.T) {
		allow(t, LaunchSessionActive, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`)
	})
	t.Run("allowed/active_to_expired", func(t *testing.T) {
		allow(t, LaunchSessionActive, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`)
	})
	t.Run("allowed/active_to_consumed", func(t *testing.T) {
		allow(t, LaunchSessionActive, `UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1`)
	})

	// --- Refused: every transition out of 'consumed' other than -> revoked.
	// These are caught by 0108's own terminal-status block: OLD.status =
	// 'consumed' is true, but the inner "NEW.status = 'revoked'" condition
	// is false, so it falls through to the RAISE EXCEPTION. ---
	t.Run("refused/consumed_to_active", func(t *testing.T) {
		refuse(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'active' WHERE id = $1`, msgTerminalImmutable)
	})
	t.Run("refused/consumed_to_expired", func(t *testing.T) {
		refuse(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, msgTerminalImmutable)
	})
	t.Run("refused/consumed_to_consumed_noop", func(t *testing.T) {
		refuse(t, LaunchSessionConsumed, `UPDATE casino_launch_sessions SET status = 'consumed' WHERE id = $1`, msgTerminalImmutable)
	})

	// --- Refused: consumed -> revoked combined with any other column
	// change. consumed_at and id are not in 0042's own column-immutability
	// list, so these two cells are the ones that actually exercise 0108's
	// whole-row equality (to_jsonb(NEW) - 'status' = to_jsonb(OLD) -
	// 'status'), not the earlier column block - hence msgTerminalImmutable.
	// expires_at and tenant_id ARE in 0042's column-immutability list, so
	// those two cells are refused by that earlier block instead (verified
	// empirically with a throwaway probe against this exact trigger before
	// writing these assertions) - hence msgColumnsImmutable. Either way
	// the transition is refused; the message pins WHICH block does it. ---
	t.Run("refused/consumed_to_revoked_with_consumed_at_change", func(t *testing.T) {
		refuse(t, LaunchSessionConsumed,
			`UPDATE casino_launch_sessions SET status = 'revoked', consumed_at = consumed_at + interval '1 second' WHERE id = $1`,
			msgTerminalImmutable)
	})
	t.Run("refused/consumed_to_revoked_with_consumed_at_null", func(t *testing.T) {
		refuse(t, LaunchSessionConsumed,
			`UPDATE casino_launch_sessions SET status = 'revoked', consumed_at = NULL WHERE id = $1`,
			msgTerminalImmutable)
	})
	t.Run("refused/consumed_to_revoked_with_id_change", func(t *testing.T) {
		// Code review A4: the whole-row equality was previously exercised
		// only through consumed_at; id is this table's own primary key and
		// was untested (mutant X9). No other row references this fixture's
		// fresh session (no bet/win/rollback was ever posted against it),
		// so nothing FK-related stops the UPDATE itself from reaching the
		// trigger - the trigger's own whole-row equality is what refuses it.
		refuse(t, LaunchSessionConsumed,
			`UPDATE casino_launch_sessions SET status = 'revoked', id = gen_random_uuid() WHERE id = $1`,
			msgTerminalImmutable)
	})
	t.Run("refused/consumed_to_revoked_with_expires_at_change", func(t *testing.T) {
		refuse(t, LaunchSessionConsumed,
			`UPDATE casino_launch_sessions SET status = 'revoked', expires_at = expires_at + interval '1 second' WHERE id = $1`,
			msgColumnsImmutable)
	})
	t.Run("refused/consumed_to_revoked_with_tenant_id_change", func(t *testing.T) {
		// tenant_id is caught by 0042's own column-immutability block
		// (BEFORE ROW, so this fires before RLS's WITH CHECK is ever
		// evaluated) - not by RLS, and not by 0108's terminal-status block.
		id := seedLaunchSessionAtStatus(t, pool, f, game, LaunchSessionConsumed)
		otherTenant := uuid.New()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'revoked', tenant_id = $2 WHERE id = $1`, id, otherTenant)
			return err
		})
		if err == nil {
			t.Fatal("expected the transition to be refused, but it succeeded")
		}
		requirePgError(t, err, "P0001", msgColumnsImmutable)
	})

	// --- Refused: every transition out of 'revoked' ---
	t.Run("refused/revoked_to_active", func(t *testing.T) {
		refuse(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'active' WHERE id = $1`, msgTerminalImmutable)
	})
	t.Run("refused/revoked_to_consumed", func(t *testing.T) {
		refuse(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1`, msgTerminalImmutable)
	})
	t.Run("refused/revoked_to_expired", func(t *testing.T) {
		refuse(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, msgTerminalImmutable)
	})
	t.Run("refused/revoked_to_revoked_noop", func(t *testing.T) {
		refuse(t, LaunchSessionRevoked, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, msgTerminalImmutable)
	})

	// --- Refused: every transition out of 'expired' ---
	t.Run("refused/expired_to_revoked", func(t *testing.T) {
		refuse(t, LaunchSessionExpired, `UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1`, msgTerminalImmutable)
	})
	t.Run("refused/expired_to_active", func(t *testing.T) {
		refuse(t, LaunchSessionExpired, `UPDATE casino_launch_sessions SET status = 'active' WHERE id = $1`, msgTerminalImmutable)
	})
	t.Run("refused/expired_to_consumed", func(t *testing.T) {
		refuse(t, LaunchSessionExpired, `UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1`, msgTerminalImmutable)
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
// tenant B's own connection context can never touch tenant A's session:
// RLS hides the row entirely (tenant B's SELECT ... FOR UPDATE finds no
// row to lock), so RevokeLaunchSession returns ErrLaunchSessionNotFound -
// not a silently-zero-rows success - and nothing about tenant A's session
// is written (security review F-1: the row count is zero, but the
// observable outcome is this error, not a bare no-op return).
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

// TestRevokeLaunchSession_AlreadyExpiredOrRevoked_IsANoOp is code review
// A2: nothing previously tested the contract "revoked=false means the
// session was already 'expired' or 'revoked'". Mutant X5 (the
// application-level CAS in RevokeLaunchSession widened to
// WHERE status IN ('active','consumed','expired')) would make the
// 'expired' case below hit the DB trigger's own RAISE EXCEPTION instead of
// cleanly returning revoked=false, aborting the caller's whole
// transaction - this test pins the correct, current behaviour (a clean,
// no-op, no-error return) so that mutant fails it.
func TestRevokeLaunchSession_AlreadyExpiredOrRevoked_IsANoOp(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	for _, status := range []LaunchSessionStatus{LaunchSessionExpired, LaunchSessionRevoked} {
		t.Run(string(status), func(t *testing.T) {
			id := seedLaunchSessionAtStatus(t, pool, f, game, status)

			var prior LaunchSessionStatus
			var revoked bool
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				prior, revoked, err = RevokeLaunchSession(ctx, tx, id)
				return err
			})
			if err != nil {
				t.Fatalf("expected RevokeLaunchSession to be a clean no-op on an already-%s session, got %v", status, err)
			}
			if prior != status {
				t.Fatalf("expected prior status %q, got %q", status, prior)
			}
			if revoked {
				t.Fatalf("expected revoked=false (the CAS must not match an already-%s session)", status)
			}

			// The row itself is untouched.
			var got LaunchSessionStatus
			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, id).Scan(&got)
			})
			if err != nil {
				t.Fatalf("read session status: %v", err)
			}
			if got != status {
				t.Fatalf("expected the row to remain %q, got %q", status, got)
			}
		})
	}
}

// TestLaunchGame_FailedLaunchOnExpiredSession_RevokeNoOpStillAudits is code
// review A2's second half: a genuine phase-C case where the session is
// already 'expired' by the time the launch is recorded as failed (a slow
// provider outliving the token's own TTL). RevokeLaunchSession's no-op
// must not prevent the casino.launch_failed audit from being written in
// the same transaction - exactly the failure mode mutant X5 would cause
// (the widened CAS would hit the DB trigger's RAISE EXCEPTION, aborting
// the whole phase-C transaction and losing this audit record).
func TestLaunchGame_FailedLaunchOnExpiredSession_RevokeNoOpStillAudits(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)

	base := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, base, 100)

	provider := &spyLaunchProvider{MockCasinoProvider: base}
	provider.onLaunch = func(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
		// Force the session straight to 'expired', simulating a launch
		// call slow enough to outlive the token's own TTL (the natural
		// lazy-expiry path only fires from ResolveLaunchToken/postBet,
		// neither of which runs from inside a provider's own Launch call,
		// so this drives the same end state directly).
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'expired' WHERE token_hash = $1`, hashLaunchToken(req.LaunchToken))
			return err
		}); err != nil {
			t.Fatalf("force session expired from within onLaunch: %v", err)
		}
		return LaunchResult{}, errors.New("simulated transport failure after the session's token TTL lapsed")
	}

	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(base))
	_, err := orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
	})
	if err == nil {
		t.Fatal("expected an error for the simulated transport failure")
	}

	var sessionID uuid.UUID
	var status LaunchSessionStatus
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, status FROM casino_launch_sessions WHERE tenant_id = $1 AND player_account_id = $2`,
			f.tenantID, f.playerAccountID).Scan(&sessionID, &status)
	}); err != nil {
		t.Fatalf("read launch session: %v", err)
	}
	if status != LaunchSessionExpired {
		t.Fatalf("expected the session to remain 'expired' (RevokeLaunchSession's CAS correctly missed it), got %q", status)
	}

	// The audit write must still have happened in the SAME transaction as
	// the no-op revoke attempt - this is exactly what mutant X5 breaks.
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_failed") {
		t.Fatal("expected a casino.launch_failed audit record even though the revoke was a no-op")
	}
	var metadataJSON []byte
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'casino.launch_failed' ORDER BY created_at DESC LIMIT 1`,
			f.tenantID).Scan(&metadataJSON)
	}); err != nil {
		t.Fatalf("read casino.launch_failed audit metadata: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
		t.Fatalf("parse casino.launch_failed audit metadata: %v (%s)", err, metadataJSON)
	}
	if revoked, ok := metadata["revoked"].(bool); !ok || revoked {
		t.Fatalf("expected revoked=false in the audit metadata (the CAS correctly missed an expired session), got %s", metadataJSON)
	}
	if priorStatus, ok := metadata["prior_status"].(string); !ok || priorStatus != string(LaunchSessionExpired) {
		t.Fatalf(`expected prior_status="expired" in the audit metadata, got %s`, metadataJSON)
	}
}

// TestRevokeLaunchSession_ConcurrentRevokes_ForUpdateKeepsPriorStatusExact
// is code review A3: what SELECT ... FOR UPDATE actually guarantees is not
// that the CAS "is atomic" (the UPDATE's own WHERE clause already makes
// the status transition itself atomic, with or without the SELECT's own
// locking) - it is that the PRIOR STATUS this function REPORTS back to the
// caller is never a stale read of a concurrently in-flight revoke.
//
// This reuses this package's own deterministic lock-interleaving harness
// (lockorder_harness_test.go's loHoldWith/loStartRacer/loWaitBlocked - no
// wall-clock sleep, no timing assertion, only a bounded poll on
// pg_blocking_pids, exactly like every other concurrency test in this
// package) rather than a fixed sleep: blocker A calls RevokeLaunchSession
// and holds its transaction open (uncommitted) after the CAS has already
// run; racer B then calls RevokeLaunchSession on the SAME session while
// A is still uncommitted, and the harness confirms B is genuinely blocked
// (on the row A's SELECT ... FOR UPDATE and UPDATE both lock) before A is
// released.
//
// Mutant X4 (drop FOR UPDATE from RevokeLaunchSession's own SELECT) would
// let B's plain SELECT return the STALE 'consumed' snapshot immediately
// (read-committed MVCC, since A has not committed yet), even though B's
// own UPDATE still correctly waits on A's row lock and still correctly
// reports revoked=false once it re-checks the WHERE clause against the
// now-'revoked' row. Under the real code, B's SELECT blocks until A
// commits and so reports the EXACT prior status, 'revoked' - the
// assertion below distinguishes the two.
func TestRevokeLaunchSession_ConcurrentRevokes_ForUpdateKeepsPriorStatusExact(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	sessionID := seedLaunchSessionAtStatus(t, pool, f, game, LaunchSessionConsumed)

	var aPrior LaunchSessionStatus
	var aRevoked bool
	blockerA := loHoldWith(t, pool, f.tenantID, "revoke-A", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		aPrior, aRevoked, err = RevokeLaunchSession(ctx, tx, sessionID)
		return err
	})
	if aPrior != LaunchSessionConsumed || !aRevoked {
		t.Fatalf("expected blocker A to see prior=consumed revoked=true, got prior=%q revoked=%v", aPrior, aRevoked)
	}

	var bPrior LaunchSessionStatus
	var bRevoked bool
	racerB := loStartRacer(t, pool, f.tenantID, "revoke-B", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		bPrior, bRevoked, err = RevokeLaunchSession(ctx, tx, sessionID)
		return err
	})

	if _, blocked := loWaitBlocked(t, pool, racerB.pid, racerB.done); !blocked {
		t.Fatalf("racer B never blocked on A's held lock; the interleaving this test depends on did not happen (err=%v)", racerB.wait())
	}

	// Release A - its transaction commits, the row is now genuinely
	// 'revoked'.
	blockerA.release()

	if err := racerB.wait(); err != nil {
		t.Fatalf("racer B's RevokeLaunchSession failed: %v", err)
	}
	if bPrior != LaunchSessionRevoked {
		t.Fatalf("expected racer B to observe the EXACT prior status 'revoked' (FOR UPDATE waited for A's commit before reading), got %q - dropping FOR UPDATE would let this read the stale 'consumed' snapshot instead", bPrior)
	}
	if bRevoked {
		t.Fatal("expected racer B's own CAS to miss (the session was already revoked by A), got revoked=true")
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
