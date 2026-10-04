//go:build integration

// PRH-2 H review round 1, LF F1: a result obtained in phase B must always be recorded. The item
// deadline applies only AFTER the loop context is cancelled, and deposit phase C / the poll's
// result application run on their own detached, bounded context.
package payments

import (
	"context"
	"testing"
	"time"
)

func lowerItemTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := sweeperItemTimeout
	sweeperItemTimeout = d
	t.Cleanup(func() { sweeperItemTimeout = old })
}

// A deposit provider slower than SweeperItemTimeout, with NO shutdown, must still end `pending`
// with its reference bound (it used to stay `submitting` with a NULL reference forever).
func TestSweeperLoop_SlowDepositBeyondItemTimeout_StillRecordsResult(t *testing.T) {
	lowerItemTimeout(t, 200*time.Millisecond)
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-slow")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	intentID := insertRawDepositIntent(t, pool, f, "pending")
	attemptID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
	spy.onDeposit = func(int) { time.Sleep(700 * time.Millisecond) } // forces the call past the (lowered) item budget

	if st := newLoopSweeper(pool, orch, false, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	got := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if got.State != AttemptPending || got.ProviderReference == nil {
		t.Fatalf("a slow Deposit must end pending with a bound reference, got state=%s ref=%v", got.State, got.ProviderReference)
	}
	if d, _, _ := spy.counts(); d != 1 {
		t.Fatalf("expected exactly one Deposit, got %d", d)
	}
}

// Shutdown variant: the loop is cancelled while Deposit is in flight and the call returns AFTER the
// drain budget has cancelled the item context. The result must still commit.
func TestSweeperLoop_ShutdownDrain_DepositResultNearBudgetStillCommits(t *testing.T) {
	lowerItemTimeout(t, 300*time.Millisecond)
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-near")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	intentID := insertRawDepositIntent(t, pool, f, "pending")
	attemptID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
	entered := make(chan struct{})
	spy.onDeposit = func(int) {
		close(entered)
		time.Sleep(900 * time.Millisecond) // returns well after the 300 ms drain budget expired
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSweeperLoop(ctx, newLoopSweeper(pool, orch, false, nil), nil, make(chan time.Time), nil)
		close(done)
	}()
	waitClosed(t, entered, "Deposit in flight")
	cancel()
	waitClosed(t, done, "the loop to drain")
	got := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if got.State != AttemptPending || got.ProviderReference == nil {
		t.Fatalf("the phase-B result must commit even after the drain budget, got state=%s ref=%v", got.State, got.ProviderReference)
	}
}

// Same for the deposit poll's result application.
func TestSweeperLoop_ShutdownDrain_PollResultNearBudgetStillCommits(t *testing.T) {
	lowerItemTimeout(t, 300*time.Millisecond)
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-nearpoll")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	attempt, ref := pendingDeposit(t, pool, orch, f, "lp-near-poll", 5000)
	spy.Resolve(ref, OutcomeSucceeded, "", false)
	base := ledgerTxCount(t, pool, f.tenantID)
	entered := make(chan struct{})
	spy.onQuery = func(int) {
		close(entered)
		time.Sleep(900 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSweeperLoop(ctx, newLoopSweeper(pool, orch, false, nil), nil, make(chan time.Time), nil)
		close(done)
	}()
	waitClosed(t, entered, "QueryStatus in flight")
	cancel()
	waitClosed(t, done, "the loop to drain")
	if got := mustGetAttempt(t, pool, f.tenantID, attempt.ID); got.State != AttemptSucceeded {
		t.Fatalf("the poll result must commit after the drain budget, got %s", got.State)
	}
	if got := ledgerTxCount(t, pool, f.tenantID); got != base+1 {
		t.Fatalf("expected one posting, got %d", got-base)
	}
}
