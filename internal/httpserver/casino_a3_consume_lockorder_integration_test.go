//go:build integration

// LF-A3-2 (ledger-finance review of A3, ADR 0082): the MOCK vendor's session
// consume must NOT sit ahead of the L0.1 delivery lock inside the callback's
// transaction. The consume is its OWN committed transaction that finishes
// before ReceiveVerifiedCallback opens, so the delivery advisory lock is the
// first lock the callback transaction takes and no session row lock is held
// while it waits.
package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestA3_HTTP_SimulationConsumeCommitsBeforeTheDeliveryLockIsTaken(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)
	if s := a3SessionStatus(t, pool, tenant.ID, launched.SessionID); s != "active" {
		t.Fatalf("setup: expected an active session, got %q", s)
	}

	body := wagerBody(100)
	providerTxID := deterministicSimulatedProviderTxID(sessionID, "wager", body["idempotency_key"].(string))

	// Hold the L0.1 delivery lock of exactly the reference the wager will use
	// (same key expression as casino.acquireProviderTxDeliveryLock).
	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`SELECT pg_advisory_xact_lock(hashtextextended('casino_bet_delivery:' || $1::text || ':' || $2 || ':' || $3, 0))`,
				tenant.ID, "mock-casino", providerTxID); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case <-time.After(10 * time.Second):
		t.Fatal("could not take the delivery lock")
	}

	status := make(chan int, 1)
	go func() {
		resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, body)
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()

	// While the callback is parked on the delivery lock, the consume must
	// already be COMMITTED (visible from another connection) ...
	deadline := time.Now().Add(15 * time.Second)
	for a3SessionStatus(t, pool, tenant.ID, launched.SessionID) != "consumed" {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the session consume did not commit ahead of the delivery lock: the callback transaction holds the session row before L0.1")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// ... and no session row lock may be held by the waiting callback.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		return tx.QueryRow(ctx, `SELECT id FROM casino_launch_sessions WHERE id = $1 FOR UPDATE NOWAIT`, sessionID).Scan(&id)
	}); err != nil {
		close(release)
		t.Fatalf("the waiting callback holds a lock on the session row: %v", err)
	}
	select {
	case code := <-status:
		close(release)
		t.Fatalf("the wager finished (%d) while its delivery lock was held elsewhere", code)
	default:
	}

	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-status:
		if code != http.StatusOK {
			t.Fatalf("the wager must succeed once the lock is released, got %d", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the wager did not complete after the delivery lock was released")
	}
}
