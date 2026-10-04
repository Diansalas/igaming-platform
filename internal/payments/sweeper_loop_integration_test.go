//go:build integration

// PRH-2 H (CP-W1), correctness half: exactly-once under lease expiry, the claim
// token CAS, the advisory lock as a pure efficiency hint, multi-instance races, S-8
// (no transaction open during a provider call) and shutdown drain.
package payments

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LF-8: a lease that expires mid-pass must still yield ONE posting. Instance A is
// held inside QueryStatus (no transaction open), its lease is forced to lapse, instance
// B sweeps the same row to completion, then A resumes and applies its stale result.
// Run with the advisory hint on and off: correctness must not depend on it.
func TestSweeperLoop_LeaseExpiryMidPass_DepositOnePosting(t *testing.T) {
	for _, hint := range []bool{false, true} {
		t.Run(fmt.Sprintf("advisory_hint_%v", hint), func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedOrchFixture(t, pool)
			spy := newLoopProvider("mock-psp-lp-dep")
			registerCapability(t, pool, f, spy, 100)
			orch := spy.orchestrator()
			attempt, ref := pendingDeposit(t, pool, orch, f, "lp-dep-lease", 5000)
			spy.Resolve(ref, OutcomeSucceeded, "", false)
			base := ledgerTxCount(t, pool, f.tenantID)

			entered, release := make(chan struct{}), make(chan struct{})
			releaseOnce := onceCloser(t, release)
			spy.onQuery = func(n int) {
				if n == 1 {
					close(entered)
					<-release
				}
			}
			a, b := newLoopSweeper(pool, orch, hint, nil), newLoopSweeper(pool, orch, hint, nil)
			doneA := make(chan SweepPassStats, 1)
			go func() { doneA <- a.RunPass(context.Background(), nil, 0) }()
			waitClosed(t, entered, "A inside QueryStatus")

			forceLeaseExpired(t, pool, f.tenantID, attempt.ID)
			stB := b.RunPass(context.Background(), nil, 0)
			if stB.Processed != 1 || stB.Errors != 0 {
				t.Fatalf("instance B must sweep the lapsed lease once cleanly: %+v", stB)
			}
			if got := mustGetAttempt(t, pool, f.tenantID, attempt.ID); got.State != AttemptSucceeded {
				t.Fatalf("expected succeeded after B, got %s", got.State)
			}

			releaseOnce()
			stA := <-doneA
			if stA.Errors != 0 {
				t.Fatalf("A's stale completion must be a clean no-op, got %+v", stA)
			}
			if got := ledgerTxCount(t, pool, f.tenantID); got != base+1 {
				t.Fatalf("exactly one posting expected (base %d), got %d", base, got)
			}
			if _, _, q := spy.counts(); q != 2 {
				t.Fatalf("both instances polled (A held, B completed): want 2 QueryStatus calls, got %d", q)
			}
			assertLedgerBalanced(t, pool, f.tenantID)
		})
	}
}

// LF-8, payout half: A claims T2 and is held inside Withdraw; the lease lapses and B
// sweeps. B must NEVER send a second Withdraw (a non-idempotent provider is escalated,
// not resent), and when A's call finally returns the payout converges to ONE settlement.
func TestSweeperLoop_LeaseExpiryMidPass_PayoutOneDispatchOnePosting(t *testing.T) {
	for _, hint := range []bool{false, true} {
		t.Run(fmt.Sprintf("advisory_hint_%v", hint), func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedPayoutFixture(t, pool, 10_000, true)
			spy := newLoopProvider("mock-payout-lp")
			registerCapability(t, pool, f.orchFixture, spy, 100)
			orch := spy.orchestrator()
			_, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, fmt.Sprintf("lp-payout-lease-%v", hint))

			entered, release := make(chan struct{}), make(chan struct{})
			releaseOnce := onceCloser(t, release)
			spy.onWithdraw = func(n int) {
				if n == 1 {
					close(entered)
					<-release
				}
			}
			a, b := newLoopSweeper(pool, orch, hint, nil), newLoopSweeper(pool, orch, hint, nil)
			doneA := make(chan SweepPassStats, 1)
			go func() { doneA <- a.RunPass(context.Background(), nil, 0) }()
			waitClosed(t, entered, "A inside Withdraw")

			forceLeaseExpired(t, pool, f.tenantID, attempt.ID)
			if st := b.RunPass(context.Background(), nil, 0); st.Errors != 0 {
				t.Fatalf("B pass 1: %+v", st)
			}
			// B's lease-expired submitting attempt (no reference yet) becomes ambiguous.
			dueNow(t, pool, f.tenantID, attempt.ID)
			if st := b.RunPass(context.Background(), nil, 0); st.Errors != 0 {
				t.Fatalf("B pass 2: %+v", st)
			}
			if _, w, _ := spy.counts(); w != 1 {
				t.Fatalf("B must not send a second Withdraw while A's is outstanding, got %d calls", w)
			}

			releaseOnce() // A's Withdraw returns pending with a reference
			if st := <-doneA; st.Panics != 0 {
				t.Fatalf("A panicked: %+v", st)
			}
			cur := mustGetAttempt(t, pool, f.tenantID, attempt.ID)
			if cur.ProviderReference == nil {
				t.Fatalf("A's late Pending result did not bind its reference (state %s): the converge-to-one-settlement path was not exercised", cur.State)
			}
			spy.Resolve(*cur.ProviderReference, OutcomeSucceeded, "", false)
			dueNow(t, pool, f.tenantID, attempt.ID)
			base := ledgerTxCount(t, pool, f.tenantID)
			if st := b.RunPass(context.Background(), nil, 0); st.Errors != 0 {
				t.Fatalf("B resolve pass: %+v", st)
			}
			final := mustGetAttempt(t, pool, f.tenantID, attempt.ID)
			if final.State != AttemptSucceeded {
				t.Fatalf("expected the single dispatch to converge to succeeded, got %s", final.State)
			}
			if got := ledgerTxCount(t, pool, f.tenantID); got != base+1 {
				t.Fatalf("exactly one settlement posting expected (base %d), got %d", base, got)
			}
			if _, w, _ := spy.counts(); w != 1 {
				t.Fatalf("exactly one Withdraw expected, got %d", w)
			}
			loAssertBalanced(t, pool, f.tenantID)
			loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
		})
	}
}

// The claim-token CAS: a stale claimant (token A) whose NotSent result arrives after
// a fresh claimant (token B) re-claimed the same attempt must not revert B's claim.
// ever_possibly_sent is still false on both claims, so the token is the only thing that
// tells the two claimants apart. A mutant that drops `claim_token = $2` from MarkNotSent
// lets A's stale result flip B's submitting claim back to created.
func TestSweeperLoop_StaleClaimantCannotRevertFreshClaim_TokenCAS(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	spy := newLoopProvider("mock-payout-lp-cas")
	registerCapability(t, pool, f.orchFixture, spy, 100)
	orch := spy.orchestrator()
	wr := approvedWithdrawal(t, pool, f, 500, "lp-token-cas")
	claimA, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	staleA := claimA.Attempt // submitting, token A
	// A's NotSent is applied (legitimately) and the attempt is back to created...
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, staleA, GateResult[WithdrawResult]{Class: ErrorClassNotSent}, EvidenceSync); err != nil {
		t.Fatalf("apply A NotSent: %v", err)
	}
	// ...and B re-claims it with a fresh token (T2).
	tokenB := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, staleA.ID, *staleA.ProviderID, tokenB, "sweeper-payout-reclaim", timeNowPlus(payoutResubmitLease))
	}); err != nil {
		t.Fatalf("claim B: %v", err)
	}
	// A's stale NotSent now arrives a second time (a lapsed-lease double run).
	_ = ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, staleA, GateResult[WithdrawResult]{Class: ErrorClassNotSent}, EvidenceSync)
	cur := mustGetAttempt(t, pool, f.tenantID, staleA.ID)
	if cur.State != AttemptSubmitting || cur.ClaimToken == nil || *cur.ClaimToken != tokenB {
		t.Fatalf("a stale claimant reverted a fresh claim: state=%s token=%v (want submitting/%s)", cur.State, cur.ClaimToken, tokenB)
	}
}

// The advisory lock is an efficiency hint ONLY. With the hint on and another session
// holding the tenant's phase-A advisory lock, the pass skips the tenant (de-duplicated
// phase A); with the hint off the same pass sweeps normally. Neither outcome is a
// correctness matter: the lease/CAS tests above run with the hint both ways.
func TestSweeperLoop_AdvisoryHint_OnlyDeduplicatesPhaseA(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-adv")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	attempt, ref := pendingDeposit(t, pool, orch, f, "lp-adv", 5000)
	spy.Resolve(ref, OutcomeSucceeded, "", false)

	holding, releaseHold, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseHoldOnce := onceCloser(t, releaseHold)
	go func() {
		held <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('payments.sweeper.phaseA'), hashtext($1::text))`, f.tenantID.String()); err != nil {
				return err
			}
			close(holding)
			<-releaseHold
			return nil
		})
	}()
	waitClosed(t, holding, "the advisory holder")

	withHint := newLoopSweeper(pool, orch, true, nil)
	if st := withHint.RunPass(context.Background(), nil, 0); st.Claimed != 0 {
		t.Fatalf("with the hint on and the lock held elsewhere the tenant is skipped, got %+v", st)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, attempt.ID); got.State != AttemptPending {
		t.Fatalf("skipped tenant must be untouched, got %s", got.State)
	}
	noHint := newLoopSweeper(pool, orch, false, nil)
	if st := noHint.RunPass(context.Background(), nil, 0); st.Processed != 1 {
		t.Fatalf("without the hint the very same pass sweeps the tenant: %+v", st)
	}
	releaseHoldOnce()
	if err := <-held; err != nil {
		t.Fatalf("holder: %v", err)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, attempt.ID); got.State != AttemptSucceeded {
		t.Fatalf("expected succeeded, got %s", got.State)
	}
}

// Multi-instance race (a): two loops run concurrently over many pending deposits; the
// lease means each attempt is polled once and each posts once.
func TestSweeperLoop_TwoLoopsConcurrently_OnePostingEach(t *testing.T) {
	for _, hint := range []bool{false, true} {
		t.Run(fmt.Sprintf("advisory_hint_%v", hint), func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedOrchFixture(t, pool)
			spy := newLoopProvider("mock-psp-lp-two")
			registerCapability(t, pool, f, spy, 100)
			orch := spy.orchestrator()
			const n = 8
			for i := 0; i < n; i++ {
				_, ref := pendingDeposit(t, pool, orch, f, fmt.Sprintf("lp-two-%d", i), int64(5000+i))
				spy.Resolve(ref, OutcomeSucceeded, "", false)
			}
			base := ledgerTxCount(t, pool, f.tenantID)

			var wg sync.WaitGroup
			start := make(chan struct{})
			stats := make([]SweepPassStats, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					s := newLoopSweeper(pool, orch, hint, nil)
					s.BatchPerTenant = 3
					<-start
					for pass := 0; pass < 4; pass++ {
						st := s.RunPass(context.Background(), nil, pass)
						stats[i].Processed += st.Processed
						stats[i].Errors += st.Errors
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if stats[0].Errors+stats[1].Errors != 0 {
				t.Fatalf("errors: %+v", stats)
			}
			if got := stats[0].Processed + stats[1].Processed; got != n {
				t.Fatalf("each attempt must be processed exactly once across both loops, got %d of %d", got, n)
			}
			if got := ledgerTxCount(t, pool, f.tenantID); got != base+n {
				t.Fatalf("expected %d postings, got %d", n, got-base)
			}
			if _, _, q := spy.counts(); q != n {
				t.Fatalf("expected %d QueryStatus calls (one per attempt), got %d", n, q)
			}
			assertLedgerBalanced(t, pool, f.tenantID)
		})
	}
}

// Multi-instance race (b): bypass the lease entirely and run the SAME attempt in two
// goroutines, both held inside QueryStatus until both have arrived. The parent lock plus
// the fresh re-read plus ledger idempotency still yield exactly one posting.
func TestSweeperLoop_SameAttemptTwoWorkers_OnePosting(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-same")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	attempt, ref := pendingDeposit(t, pool, orch, f, "lp-same", 5000)
	spy.Resolve(ref, OutcomeSucceeded, "", false)
	base := ledgerTxCount(t, pool, f.tenantID)

	arrived := make(chan struct{}, 2)
	gate := make(chan struct{})
	gateOnce := onceCloser(t, gate)
	spy.onQuery = func(int) {
		arrived <- struct{}{}
		<-gate
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			s := newLoopSweeper(pool, orch, false, nil)
			errs <- s.processAttempt(context.Background(), f.tenantID, attempt.ID)
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(hangGuard):
			t.Fatal("timed out waiting for both workers to reach QueryStatus")
		}
	}
	gateOnce()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("worker error: %v", err)
		}
	}
	if got := ledgerTxCount(t, pool, f.tenantID); got != base+1 {
		t.Fatalf("exactly one posting expected, got %d", got-base)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// S-8: no transaction is open while the sweeper makes a provider call - QueryStatus
// (deposit poll), Deposit (created-attempt dispatch) and Withdraw (payout T2).
func TestSweeperLoop_NoTransactionOpenDuringProviderCalls(t *testing.T) {
	pool := depositV2ScratchPool(t)

	// Non-vacuity: the probe really sees an open transaction.
	probeSeen := make(chan int, 1)
	ctlDone := make(chan struct{})
	holdDone := make(chan struct{})
	holdDoneOnce := onceCloser(t, holdDone)
	holdOpen := make(chan struct{})
	go func() {
		defer close(ctlDone)
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
				return err
			}
			close(holdOpen)
			<-holdDone
			return nil
		})
	}()
	waitClosed(t, holdOpen, "the probe control transaction")
	probeSeen <- idleInTxCount(t, pool)
	holdDoneOnce()
	waitClosed(t, ctlDone, "the probe control transaction to commit") // H-CR-12
	if n := <-probeSeen; n < 1 {
		t.Fatalf("probe is vacuous: it did not see a deliberately open transaction (%d)", n)
	}

	f := seedPayoutFixture(t, pool, 10_000, true)
	spy := newLoopProvider("mock-lp-s8")
	registerCapability(t, pool, f.orchFixture, spy, 100)
	orch := spy.orchestrator()

	var mu sync.Mutex
	observed := map[string]int{}
	probe := func(kind string) {
		n := idleInTxCount(t, pool)
		mu.Lock()
		observed[kind] = n
		mu.Unlock()
	}
	spy.onQuery = func(int) { probe("QueryStatus") }
	spy.onDeposit = func(int) { probe("Deposit") }
	spy.onWithdraw = func(int) { probe("Withdraw") }

	// QueryStatus: a pending deposit.
	_, ref := pendingDeposit(t, pool, orch, f.orchFixture, "lp-s8-poll", 5000)
	spy.Resolve(ref, OutcomeSucceeded, "", false)
	// Deposit: a created, non-interactive attempt that the sweeper must dispatch.
	intentID := insertRawDepositIntent(t, pool, f.orchFixture, "pending")
	insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, timeNowPlus(0))
	// Withdraw: a re-claimable created payout.
	notSentPayoutAttempt(t, pool, orch, f, 500, "lp-s8-payout")

	s := newLoopSweeper(pool, orch, true, nil)
	if st := s.RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	for _, kind := range []string{"QueryStatus", "Deposit", "Withdraw"} {
		n, ok := observed[kind]
		if !ok {
			t.Fatalf("%s was never called by the sweeper pass: the test would be vacuous", kind)
		}
		if n != 0 {
			t.Fatalf("%d transaction(s) open during provider %s", n, kind)
		}
	}
}

// Shutdown drain: once the loop's context is cancelled the loop starts NO new item, but
// the item already in flight completes (on a context detached from the cancellation) and
// its result is committed, then the loop returns.
func TestSweeperLoop_ShutdownDrain_InFlightItemCompletesNoNewItemStarts(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-drain")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	var atts [2]PaymentAttempt
	for i := range atts {
		a, ref := pendingDeposit(t, pool, orch, f, fmt.Sprintf("lp-drain-%d", i), int64(5000+i))
		spy.Resolve(ref, OutcomeSucceeded, "", false)
		atts[i] = a
	}
	// The earlier-due attempt is processed first.
	dueNow(t, pool, f.tenantID, atts[0].ID)

	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := onceCloser(t, release)
	spy.onQuery = func(n int) {
		if n == 1 {
			close(entered)
			<-release
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		runSweeperLoop(ctx, newLoopSweeper(pool, orch, false, nil), nil, ticks, nil)
		close(done)
	}()
	waitClosed(t, entered, "the first item inside QueryStatus")
	cancel() // shutdown signal while the first item is inside the provider call
	releaseOnce()
	waitClosed(t, done, "the loop to drain and return")

	first, second := mustGetAttempt(t, pool, f.tenantID, atts[0].ID), mustGetAttempt(t, pool, f.tenantID, atts[1].ID)
	states := map[AttemptState]int{first.State: 1}
	states[second.State]++
	if states[AttemptSucceeded] != 1 || states[AttemptPending] != 1 {
		t.Fatalf("want exactly one drained success and one untouched pending attempt, got %s and %s", first.State, second.State)
	}
	if _, _, q := spy.counts(); q != 1 {
		t.Fatalf("no new item may start after cancellation: want 1 QueryStatus, got %d", q)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// The batch claim never waits on a row another session holds (FOR UPDATE SKIP LOCKED): a locked
// attempt is skipped this pass and the free one is claimed. Without SKIP LOCKED the claim blocks on
// the held row (the bounded context turns that into a failure instead of a hang).
func TestSweeperLoop_BatchClaimSkipsRowLockedElsewhere(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lp-skip")
	registerCapability(t, pool, f, spy, 100)
	orch := spy.orchestrator()
	locked, _ := pendingDeposit(t, pool, orch, f, "lp-skip-locked", 5000)
	free, _ := pendingDeposit(t, pool, orch, f, "lp-skip-free", 5001)

	holding, release, held := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseOnce := onceCloser(t, release)
	go func() {
		held <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT id FROM payment_attempts WHERE id = $1 FOR UPDATE`, locked.ID); err != nil {
				return err
			}
			close(holding)
			<-release
			return nil
		})
	}()
	waitClosed(t, holding, "the row-lock holder")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ids, err := newLoopSweeper(pool, orch, false, nil).claimBatch(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("claimBatch must skip the locked row, not wait on it: %v", err)
	}
	if len(ids) != 1 || ids[0] != free.ID {
		t.Fatalf("expected only the free attempt %s, got %v", free.ID, ids)
	}
	releaseOnce()
	if err := <-held; err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// A tenant row that cannot be read fails CLOSED (resolution-only): a broken invariant is never a
// licence to dispatch. An active tenant is not resolution-only; every other status is.
func TestSweeperLoop_TenantResolutionOnly_StatusTable_MissingRowFailsClosed(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	read := func(id uuid.UUID) bool {
		var got bool
		if err := pool.WithTenant(context.Background(), id, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = tenantResolutionOnly(ctx, tx, id)
			return err
		}); err != nil {
			t.Fatalf("tenantResolutionOnly: %v", err)
		}
		return got
	}
	if read(f.tenantID) {
		t.Fatal("an active tenant must not be resolution-only")
	}
	for _, status := range []string{"suspended", "closed"} {
		setTenantStatus(t, pool, f.tenantID, status)
		if !read(f.tenantID) {
			t.Fatalf("a %s tenant must be resolution-only", status)
		}
	}
	setTenantStatus(t, pool, f.tenantID, "active")
	if read(f.tenantID) {
		t.Fatal("reactivation must lift resolution-only immediately (no caching)")
	}
	if !read(uuid.New()) {
		t.Fatal("a missing tenant row must fail closed")
	}
}
