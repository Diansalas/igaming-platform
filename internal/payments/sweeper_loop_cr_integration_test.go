//go:build integration

// PRH-2 H review round 1 (code review H-CR-1, -3, -4, -7, -8, -9, -10): steady-state item timeout,
// the drain bound itself, real deferral, resolution-only before the KYC gate, claim-phase faults.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func kycDecisionCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count kyc decisions: %v", err)
	}
	return n
}

// H-CR-1 steady state: QueryStatus slower than the item timeout (but inside the gate's CallTimeout)
// with a LIVE loop context must still resolve the attempt with no error.
func TestSweeperLoop_SlowQueryStatusBeyondItemTimeout_LiveLoop_Succeeds(t *testing.T) {
	lowerItemTimeout(t, 200*time.Millisecond)
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-slowq")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	attempt, ref := pendingDeposit(t, pool, orch, f, "lp-slowq", 5000)
	spy.Resolve(ref, OutcomeSucceeded, "", false)
	spy.onQuery = func(int) { time.Sleep(700 * time.Millisecond) }
	st := newLoopSweeper(pool, orch, false, nil).RunPass(context.Background(), nil, 0)
	if st.Errors != 0 || st.Processed != 1 {
		t.Fatalf("a slow poll with a live loop must succeed: %+v", st)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, attempt.ID); got.State != AttemptSucceeded {
		t.Fatalf("expected succeeded, got %s", got.State)
	}
}

// H-CR-7 / drain bound: after the loop context is cancelled the item context is still LIVE (it is
// detached), and it is cancelled once sweeperItemTimeout has passed (the bound is enforced).
func TestSweeperLoop_ShutdownDrain_ItemContextDetachedThenBounded(t *testing.T) {
	lowerItemTimeout(t, time.Second)
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-bound")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	attempt, ref := pendingDeposit(t, pool, orch, f, "lp-bound", 5000)
	spy.Resolve(ref, OutcomeSucceeded, "", false)

	entered, cancelled := make(chan struct{}), make(chan struct{})
	var liveAfterCancel, boundedLater bool
	spy.onQueryCtx = func(ctx context.Context, _ int) {
		close(entered)
		<-cancelled
		liveAfterCancel = ctx.Err() == nil
		select {
		case <-ctx.Done():
			boundedLater = true
		case <-time.After(hangGuard / 6):
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSweeperLoop(ctx, newLoopSweeper(pool, orch, false, nil), nil, make(chan time.Time), nil)
		close(done)
	}()
	waitClosed(t, entered, "QueryStatus in flight")
	cancel()
	close(cancelled)
	waitClosed(t, done, "the loop to drain")
	if !liveAfterCancel {
		t.Fatal("the in-flight item's context must outlive loop cancellation (drain)")
	}
	if !boundedLater {
		t.Fatal("the drain budget must cancel the item context (the bound is enforced)")
	}
	if got := mustGetAttempt(t, pool, f.tenantID, attempt.ID); got.State != AttemptSucceeded {
		t.Fatalf("the drained item's result must still commit, got %s", got.State)
	}
}

// H-CR-8 / H-CR-9: a claim attempted on a context cancelled mid-pass is not a failure, and a panic
// inside the claim phase is contained to that tenant.
func TestSweeperLoop_ClaimPhase_CancelNotCountedPanicContained(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-lp-claim")
	orch := spy.orchestrator()
	fA, fB := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var atts [2]PaymentAttempt
	for i, f := range []orchFixture{fA, fB} {
		registerCapability(t, pool, f, spy, 100)
		a, ref := pendingDeposit(t, pool, orch, f, "lp-claim", 5000)
		spy.Resolve(ref, OutcomeSucceeded, "", false)
		atts[i] = a
	}
	// Panic in tenant A's claim phase: B (either order) is still swept.
	s := newLoopSweeper(pool, orch, false, nil)
	s.testBeforeClaim = func(id uuid.UUID) {
		if id == fA.tenantID {
			panic("injected claim-phase panic")
		}
	}
	st := s.RunPass(context.Background(), nil, 0)
	if st.Panics != 1 || st.Processed != 1 || !st.Listed {
		t.Fatalf("want one contained claim-phase panic and one processed tenant: %+v", st)
	}
	if got := mustGetAttempt(t, pool, fB.tenantID, atts[1].ID); got.State != AttemptSucceeded {
		t.Fatalf("tenant B must still be swept, got %s", got.State)
	}

	// Cancellation during the claim phase is not a claim failure and stops the pass.
	pool2 := depositV2ScratchPool(t)
	f2a, f2b := seedOrchFixture(t, pool2), seedOrchFixture(t, pool2)
	for _, f := range []orchFixture{f2a, f2b} {
		registerCapability(t, pool2, f, spy, 100)
		_, ref := pendingDeposit(t, pool2, orch, f, "lp-claim2", 5000)
		spy.Resolve(ref, OutcomeSucceeded, "", false)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s2 := newLoopSweeper(pool2, orch, false, nil)
	s2.testBeforeClaim = func(uuid.UUID) { cancel() }
	st2 := s2.RunPass(ctx, nil, 0)
	if st2.Errors != 0 || st2.Panics != 0 {
		t.Fatalf("a claim cut short by shutdown must not be counted as a failure: %+v", st2)
	}
	if st2.Tenants != 1 {
		t.Fatalf("the pass must stop after cancellation, swept %d tenants", st2.Tenants)
	}
}

// H-CR-10: a pass that could not list tenants is marked as not listed (so the stalled gauge keeps
// its old value).
func TestSweeperLoop_PassWithoutListing_NotMarkedListed(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-lp-listed")
	s := newLoopSweeper(pool, spy.orchestrator(), false, nil)
	if st := s.RunPass(context.Background(), nil, 0); !st.Listed {
		t.Fatal("a normal pass lists tenants")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if st := s.RunPass(ctx, nil, 0); st.Listed {
		t.Fatal("a pass whose listing failed must not be marked listed")
	}
}

// H-CR-4: a non-active tenant's withheld payout dispatch happens BEFORE the KYC gate: with a
// KYC-denied player, a suspended tenant writes no decision row and does not escalate, while the
// active control escalates (so the denial path is real).
func TestSweeperLoop_NonActiveTenant_ResolutionOnlyPrecedesKYCGate(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-payout-lp-kyc")
	m := spy.Capabilities().Manifest
	m.IdempotentSubmission = true
	spy.SetManifest(m)
	orch := spy.orchestrator()
	fNA, fAct := seedPayoutFixture(t, pool, 100_000, true), seedPayoutFixture(t, pool, 100_000, true)
	var created, ambiguous [2]PaymentAttempt
	for i, f := range []payoutFixture{fNA, fAct} {
		registerCapability(t, pool, f.orchFixture, spy, 100)
		_, created[i] = notSentPayoutAttempt(t, pool, orch, f, 500, "lp-kyc-created")
		wr := approvedWithdrawal(t, pool, f, 600, "lp-kyc-amb")
		claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassAmbiguous}, EvidenceSync); err != nil {
			t.Fatalf("apply ambiguous: %v", err)
		}
		dueNow(t, pool, f.tenantID, claim.Attempt.ID)
		ambiguous[i] = mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
		revokeVerification(t, pool, f) // the player is now KYC-denied
	}
	setTenantStatus(t, pool, fNA.tenantID, "suspended")
	decNA, decAct := kycDecisionCount(t, pool, fNA.tenantID), kycDecisionCount(t, pool, fAct.tenantID)

	if st := newLoopSweeper(pool, orch, false, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	for _, a := range []PaymentAttempt{created[0], ambiguous[0]} {
		got := mustGetAttempt(t, pool, fNA.tenantID, a.ID)
		if got.EscalatedAt != nil {
			t.Fatalf("a suspended tenant's withheld attempt %s must not be escalated by the KYC gate", a.ID)
		}
	}
	assertDeferred(t, pool, fNA.tenantID, created[0].ID, created[0].PollCount, 1)
	if got := kycDecisionCount(t, pool, fNA.tenantID); got != decNA {
		t.Fatalf("a suspended tenant's withheld dispatch must not evaluate the KYC gate (decision rows %d -> %d)", decNA, got)
	}
	// Control: the active tenant's denied payout IS evaluated and escalated.
	if got := mustGetAttempt(t, pool, fAct.tenantID, created[1].ID); got.EscalatedAt == nil {
		t.Fatal("control: an active tenant's KYC-denied created payout must escalate")
	}
	if got := kycDecisionCount(t, pool, fAct.tenantID); got <= decAct {
		t.Fatalf("control: the active tenant must write KYC decision rows (%d -> %d)", decAct, got)
	}
}

// H-CR-10: the last-pass gauge advances only after a successful tenant listing, so a database outage
// (every pass failing to list) leaves it stale and a "stalled" rule can fire.
func TestSweeperLoop_LastPassGauge_AdvancesOnlyAfterSuccessfulListing(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-lp-gauge")
	s := newLoopSweeper(pool, spy.orchestrator(), false, nil)

	sweeperLastPassUnix.Store(0)
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	runSweeperLoop(dead, s, nil, make(chan time.Time), nil) // one immediate pass whose listing fails, then returns
	if got := sweeperLastPassUnix.Load(); got != 0 {
		t.Fatalf("a pass that could not list tenants must not advance the gauge, got %d", got)
	}

	ctx, cancel2 := context.WithCancel(context.Background())
	done := make(chan struct{})
	passed := make(chan struct{})
	go func() {
		runSweeperLoop(ctx, s, nil, make(chan time.Time), func(SweepPassStats) { close(passed) })
		close(done)
	}()
	waitClosed(t, passed, "the first pass")
	cancel2()
	waitClosed(t, done, "the loop to stop")
	if sweeperLastPassUnix.Load() == 0 {
		t.Fatal("a successful pass must advance the gauge")
	}
}
