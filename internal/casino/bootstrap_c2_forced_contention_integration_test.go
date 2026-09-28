//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103) - code review C-2 (forced-contention
// re-run of e810ece): the race between a DIFFERENT token and the SAME
// request_id (the db.IdempotentInsert conflict path, where the loser must
// roll back its own step-5 consume) was previously only exercised as a
// plain goroutine race with no forced interleaving - reviewer mutant MC2
// (the conflict branch returns success instead of refusing) survived the
// whole suite. This file forces the interleaving deterministically: a
// blocker holds BOTH sessions' rows FOR UPDATE, both BootstrapLaunch
// callers are started, the test polls pg_stat_activity until BOTH are
// genuinely queued on the lock (not merely "started"), then releases the
// blocker so both proceed into their own step 5 CAS and the step 6 INSERT
// race at essentially the same instant.
package casino

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// waitForNBackendsBlockedOnSessionLock polls pg_stat_activity until at
// least n backends are genuinely queued (wait_event_type = 'Lock') on a
// query matching getLaunchSessionForBootstrap's own statement shape - the
// SAME "poll until truly queued, not merely dispatched" discipline
// lockorder_harness_test.go's own loWaitBlocked uses, adapted here because
// BootstrapLaunch owns its whole transaction internally (it takes a POOL,
// not a tx), so this file cannot capture a per-racer backend pid the way
// loStartRacer does.
func waitForNBackendsBlockedOnSessionLock(t *testing.T, pool *db.Pool, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var count int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity
				 WHERE wait_event_type = 'Lock'
				   AND query ILIKE '%casino_launch_sessions%'
				   AND query ILIKE '%FOR NO KEY UPDATE%'`).Scan(&count)
		})
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if count >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d backends blocked on the session row lock", n)
}

// TestBootstrapLaunch_CrossTokenSameRequestID_ForcedContention is C-2's own
// forced re-run. Run at -count=10 per the review's own instruction.
func TestBootstrapLaunch_CrossTokenSameRequestID_ForcedContention(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	sessionA, tokenA := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	sessionB, tokenB := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	const sharedRequestID = "req-c2-cross-token-contention"
	inA := provider.BootstrapPayload(f.tenantID, tokenA, sharedRequestID, game.ProviderGameID, "EUR", "real")
	inB := provider.BootstrapPayload(f.tenantID, tokenB, sharedRequestID, game.ProviderGameID, "EUR", "real")
	vA := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inA)
	vB := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inB)

	// The blocker holds BOTH sessions' own rows FOR UPDATE - the same
	// strength getLaunchSessionForBootstrap's FOR NO KEY UPDATE always
	// conflicts with - forcing BOTH callers to genuinely queue before
	// either can reach step 5 at all.
	blocker := loHoldWith(t, pool, f.tenantID, "both session rows (FOR UPDATE)", func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []uuid.UUID{sessionA.ID, sessionB.ID} {
			var discard uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM casino_launch_sessions WHERE id = $1 FOR UPDATE`, id).Scan(&discard); err != nil {
				return err
			}
		}
		return nil
	})

	var wg sync.WaitGroup
	var resultA, resultB BootstrapResult
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		resultA, errA = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vA)
	}()
	go func() {
		defer wg.Done()
		resultB, errB = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vB)
	}()

	waitForNBackendsBlockedOnSessionLock(t, pool, 2, loLockWaitTimeout)
	blocker.release()
	wg.Wait()

	// Exactly one of A/B succeeds; the other is refused as a replay
	// mismatch (different token racing an existing request_id) and its
	// OWN consume rolls back with it - CLAUDE.md's "a refusal never looks
	// like a partial write", now pinned under genuine forced contention
	// rather than only under a sequential/best-effort goroutine race.
	succeededA, succeededB := errA == nil, errB == nil
	if succeededA == succeededB {
		t.Fatalf("expected exactly one of A/B to succeed under forced contention, got errA=%v errB=%v", errA, errB)
	}

	var loserErr error
	var loserSessionID uuid.UUID
	var winnerSessionID uuid.UUID
	if succeededA {
		loserErr, loserSessionID, winnerSessionID = errB, sessionB.ID, resultA.SessionID
	} else {
		loserErr, loserSessionID, winnerSessionID = errA, sessionA.ID, resultB.SessionID
	}

	var refused *BootstrapRefusedError
	if !errors.As(loserErr, &refused) {
		t.Fatalf("expected the loser to be refused with *BootstrapRefusedError, got %T: %v", loserErr, loserErr)
	}
	if refused.Reason != BootstrapRefusalReplayMismatch {
		t.Fatalf("expected the loser's refusal reason to be %q, got %q", BootstrapRefusalReplayMismatch, refused.Reason)
	}

	// The loser's OWN session must have rolled back to 'active' - its
	// step-5 consume must not survive its own transaction's rollback.
	var loserStatus LaunchSessionStatus
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, loserSessionID).Scan(&loserStatus)
	})
	if err != nil {
		t.Fatalf("read loser session status: %v", err)
	}
	if loserStatus != LaunchSessionActive {
		t.Fatalf("expected the loser's OWN session to remain 'active' (its consume rolled back), got %q", loserStatus)
	}

	// The winner's session is consumed, and exactly one bootstrap row
	// exists for the shared request_id.
	var winnerStatus LaunchSessionStatus
	var bootstrapCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, winnerSessionID).Scan(&winnerStatus); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND request_id = $2`,
			f.tenantID, sharedRequestID).Scan(&bootstrapCount)
	})
	if err != nil {
		t.Fatalf("read winner session status / bootstrap count: %v", err)
	}
	if winnerStatus != LaunchSessionConsumed {
		t.Fatalf("expected the winner's session consumed, got %q", winnerStatus)
	}
	if bootstrapCount != 1 {
		t.Fatalf("expected exactly one bootstrap row for the shared request_id, got %d", bootstrapCount)
	}
}
