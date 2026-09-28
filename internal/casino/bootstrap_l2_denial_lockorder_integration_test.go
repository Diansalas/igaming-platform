//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103) - security review L-2 (re-run of
// e810ece): on the gate-denial path, RevokeLaunchSession UPGRADES the
// session row lock from getLaunchSessionForBootstrap's own FOR NO KEY
// UPDATE to a genuine FOR UPDATE (via its own `SELECT status ... FOR
// UPDATE`, launch.go:428) WHILE bootstrap already holds the RG advisory
// lock (denial always runs after the RG gate check succeeds in resolving
// a decision, per bootstrapGateDenialReason's own order). This file adds
// that upgrade to the lock-order harness against the first postBet of a
// round, per the review's own instruction: "show there is no cycle; if it
// does cycle, fix it, don't just document it."
package casino

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

// waitForBackendsBlockedOnBoth polls pg_stat_activity until at least one
// backend is genuinely queued (wait_event_type = 'Lock') matching each of
// the two query-text patterns - the same query-text-matching technique
// waitForNBackendsBlockedOnSessionLock (bootstrap_c2_forced_contention_
// integration_test.go) uses, extended to two DIFFERENT resources at once
// since this scenario's two racers block on different lock types (a row
// lock and an advisory lock).
func waitForBackendsBlockedOnBoth(t *testing.T, pool *db.Pool, patternA, patternB string, timeout time.Duration) {
	t.Helper()
	matches := func(pattern string) (bool, error) {
		var count int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE $1`,
				pattern).Scan(&count)
		})
		return count > 0, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		okA, err := matches(patternA)
		if err != nil {
			t.Fatalf("poll pg_stat_activity (A): %v", err)
		}
		okB, err := matches(patternB)
		if err != nil {
			t.Fatalf("poll pg_stat_activity (B): %v", err)
		}
		if okA && okB {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for backends blocked matching %q and %q", patternA, patternB)
}

// TestLockOrder_BootstrapGateDenialAndFirstBetOfRound_NoDeadlock drives
// BOTH sides through the real, full production calls (not stand-ins):
// racer A is a genuine BootstrapLaunch call that ends in a definitive gate
// denial (RG-ineligible, via a suspended account) - exercising
// RevokeLaunchSession's own lock upgrade; racer B is a genuine postBet
// call (via receiveCallbackInTx) for the first bet of the same round.
// Neither is a hand-written equivalent statement.
//
// Forced interleaving: blockerSession holds the session row FOR UPDATE,
// forcing racer A to queue at its very first statement
// (getLaunchSessionForBootstrap). blockerRG holds the RG advisory lock,
// forcing racer B to queue at ITS very first statement
// (evaluateAndAuditEligibility) - racer B never touches the session row
// at all until after it already holds RG (BindProviderRound's FK-driven
// FOR KEY SHARE), so it is never blocked by blockerSession. Once both are
// genuinely queued (confirmed via pg_stat_activity, not merely "started"),
// both blockers are released together - unlike the ABBA six-step
// technique in lockorder_harness_test.go/lockorder_integration_test.go's
// own F-1 test, no staged two-step release is needed here: blockerSession
// and blockerRG are two INDEPENDENT single-step gates, not a genuine
// two-resource ABBA cycle - racer A's own two steps are session-row (now
// held) then RG (next), and racer B's are RG (now released, contended)
// then FOR KEY SHARE (only reachable once B actually holds RG). Whichever
// of A/B wins RG's queue proceeds to completion without ever needing a
// lock the other holds: A's later upgrade to FOR UPDATE only happens
// AFTER A itself already holds RG - which is only true once B has either
// released RG (having already finished, since B's own FOR KEY SHARE step
// is compatible with A's still-held FOR NO KEY UPDATE and never blocks)
// or never held it. Both orderings are asserted deadlock-free below.
func TestLockOrder_BootstrapGateDenialAndFirstBetOfRound_NoDeadlock(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	suspendAccount(t, pool, f)

	in := provider.BootstrapPayload(f.tenantID, token, "req-l2-denial-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "req-l2-denial-bet", "", "round-l2-denial", game.ProviderGameID,
		500, "EUR", OutcomeSucceeded, "", f.playerAccountID, session.ID)

	blockerSession := loHoldWith(t, pool, f.tenantID, "session row (FOR UPDATE)", func(ctx context.Context, tx pgx.Tx) error {
		var discard uuid.UUID
		return tx.QueryRow(ctx, `SELECT id FROM casino_launch_sessions WHERE id = $1 FOR UPDATE`, session.ID).Scan(&discard)
	})
	// The same rg.EvaluateEligibility call bootstrapGateDenialReason and
	// postBet's own evaluateAndAuditEligibility both make - the real
	// production entry point for RG's advisory lock, not a hand-written
	// equivalent of the SQL.
	blockerRG := loHoldWith(t, pool, f.tenantID, "RG advisory lock", func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		})
		return err
	})

	var wg sync.WaitGroup
	var bootstrapErr error
	var bootstrapResult BootstrapResult
	var betErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		bootstrapResult, bootstrapErr = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	}()
	go func() {
		defer wg.Done()
		betErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
			return err
		})
	}()

	waitForBackendsBlockedOnBoth(t, pool,
		"%casino_launch_sessions%FOR NO KEY UPDATE%",
		"%pg_advisory_xact_lock%",
		loLockWaitTimeout)

	blockerSession.release()
	blockerRG.release()
	wg.Wait()

	if loIsDeadlock(bootstrapErr) {
		t.Fatalf("bootstrap's gate-denial path deadlocked against the first bet of the round: %s", loDescribeDeadlock(bootstrapErr))
	}
	if loIsDeadlock(betErr) {
		t.Fatalf("the first bet of the round deadlocked against bootstrap's gate-denial path: %s", loDescribeDeadlock(betErr))
	}
	if bootstrapErr != nil {
		t.Fatalf("expected a denial RESULT, not a bootstrap error: %v", bootstrapErr)
	}
	if !bootstrapResult.Denied || bootstrapResult.DeniedReason != "rg_ineligible" {
		t.Fatalf("expected Denied=true reason=rg_ineligible, got %+v", bootstrapResult)
	}
	if betErr != nil {
		t.Fatalf("the bet must resolve to an outcome, never an error: %v", betErr)
	}

	var status LaunchSessionStatus
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, session.ID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if status != LaunchSessionRevoked {
		t.Fatalf("expected the session revoked by the gate denial, got %q", status)
	}
}
