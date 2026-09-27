//go:build integration

// Security review RV-PRH-I2 (I3/item 2): ADR 0095 §15.1's "an orphaned/
// never-resolved 'active' session is harmless" claim is only true if
// postBet actually bounds bet PLACEMENT by the session's own expiry -
// before this fix it did not, so a session left 'active' forever (a
// process crash, or a phase-C failure) would accept a bet indefinitely.
// These tests pin the fix: a NEW bet against an expired/non-eligible
// session is rejected, while a WIN/ROLLBACK for a bet placed BEFORE
// expiry still settles even after the session has since expired
// (postWin/postRollback never consult session status/expiry - they
// resolve accounts from the ledger's own prior entries via
// correlation_id).
package casino

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// markSessionExpired flips sessionID's status to 'expired' directly - the
// SAME lazy transition ResolveLaunchToken itself performs on first post-
// expiry access (launch.go). casino_launch_sessions.expires_at is
// immutable after insert (migration 0036), so this - not rewriting
// expires_at - is the deterministic way to simulate "this session's TTL
// has already lapsed and something (a lazy read, an ops sweep) already
// flipped its status", without a real sleep.
func markSessionExpired(t *testing.T, pool *db.Pool, tenantID, sessionID uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, sessionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to mark exactly 1 session row expired, affected %d", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("mark session expired: %v", err)
	}
}

// mintSessionWithTTL is mintSession with an explicit, short TTL - used to
// prove the time-based (expires_at) rejection specifically, for a session
// whose status was NEVER flipped away from 'active' (the genuine "crash
// left it active forever" scenario security's review named), as distinct
// from markSessionExpired's status-based case above.
func mintSessionWithTTL(t *testing.T, pool *db.Pool, f casinoFixture, providerID, assetCode string, ttl time.Duration) uuid.UUID {
	t.Helper()
	game := seedGame(t, pool, providerID, assetCode)
	var sessionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		session, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: providerID, ProviderGameID: game.ProviderGameID,
			AssetCode: assetCode, Mode: ModeReal, TTL: ttl,
		})
		if err != nil {
			return err
		}
		sessionID = session.ID
		return nil
	})
	if err != nil {
		t.Fatalf("mint launch session with TTL: %v", err)
	}
	return sessionID
}

func TestReceiveCallback_NewBetRejectedOnExpiredStatus_LedgerBalanced(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	markSessionExpired(t, pool, f.tenantID, sessionID)

	debitsBefore, creditsBefore := sumDebitsCredits(t, pool, f.tenantID)

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-after-expiry", "", "round-after-expiry", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("expected ErrLaunchSessionRequired for a bet against an expired session, got %v", err)
	}

	debitsAfter, creditsAfter := sumDebitsCredits(t, pool, f.tenantID)
	if debitsAfter != creditsAfter {
		t.Fatalf("ledger invariant violated: debits=%d credits=%d", debitsAfter, creditsAfter)
	}
	if debitsAfter != debitsBefore || creditsAfter != creditsBefore {
		t.Fatalf("expected no ledger effect from a bet against an expired session, before=(%d,%d) after=(%d,%d)",
			debitsBefore, creditsBefore, debitsAfter, creditsAfter)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the balance untouched, got %d", balance)
	}
	var count int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = 'bet-after-expiry'`,
			f.tenantID).Scan(&count)
	}); err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero ledger_transactions rows for the rejected bet, got %d", count)
	}
}

// TestReceiveCallback_NewBetRejectedPastExpiresAtEvenIfStillActive proves
// the time-based half specifically: a session whose status was NEVER
// flipped away from 'active' (the exact "a crash/phase-C failure left it
// active forever" scenario ADR 0095 §15.1 must bound) is still rejected
// once wall-clock time passes its own expires_at.
func TestReceiveCallback_NewBetRejectedPastExpiresAtEvenIfStillActive(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSessionWithTTL(t, pool, f, "mock-casino", "EUR", 30*time.Millisecond)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	time.Sleep(200 * time.Millisecond)

	var status LaunchSessionStatus
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status)
	}); err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if status != LaunchSessionActive {
		t.Fatalf("test setup: expected the session to still be 'active' (never lazily flipped), got %q", status)
	}

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-past-expires-at", "", "round-past-expires-at", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("expected ErrLaunchSessionRequired for a bet past expires_at on a still-'active' session, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected the balance untouched, got %d", balance)
	}
}

// TestReceiveCallback_WinSettlesForPreExpiryBetAfterSessionExpires proves
// the OTHER half of item 2: a bet placed BEFORE expiry still has its win
// settle even after the session has since expired, because postWin
// resolves its own accounts via correlation_id from the ledger's prior
// entries, never by re-checking session status/expiry.
func TestReceiveCallback_WinSettlesForPreExpiryBetAfterSessionExpires(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// The bet posts BEFORE expiry, while the session is still eligible.
	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-pre-expiry", "", "round-pre-expiry", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("pre-expiry bet: %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected 4000 after the 1000 bet, got %d", balance)
	}

	// The session expires - postBet would now reject a NEW bet against it
	// (the sibling tests above), but this WIN settles the round the bet
	// above already opened.
	markSessionExpired(t, pool, f.tenantID, sessionID)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-post-expiry", "", "round-pre-expiry", "game-1", 2500, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win after session expiry (for a pre-expiry bet): %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected the win to settle despite the session having since expired, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 6500 {
		t.Fatalf("expected cash balance 6500 (5000 - 1000 bet + 2500 win), got %d", balance)
	}
	if debits, credits := sumDebitsCredits(t, pool, f.tenantID); debits != credits {
		t.Fatalf("ledger invariant violated: debits=%d credits=%d", debits, credits)
	}
}
