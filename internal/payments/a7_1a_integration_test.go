//go:build integration

// A7-TESTS-1 item #1a (rv-a7-tests.md, 1944de0), routed by the coordinator
// to the payments/callback agent (owner of receipt.go/cascade.go) to land
// together with a fix. Kept in its own file (not
// a7_lockorder_integration_test.go, which the payout agent's own FH-6 adds
// on its own branch) to avoid a merge conflict.
//
// Originally reproduced a real double-capture bug: a cascadable decline's
// child attempt could be claimed and dispatched to a second provider while
// a late/duplicate callback for the FIRST (already-declined) attempt was
// independently treated as a legitimate T13(c) second capture and posted -
// with nothing, at the time, re-checking whether the OTHER (still live)
// sibling might also succeed and post a SECOND time later. ADR 0095 §28
// AM-2/INV-DEP-1 (FH-3, 8ce538c) closes this generally: every deposit
// success posting site (including drive.go's own ErrorClassSucceeded
// branch, via postDepositSuccessOrDispute) now checks resolvedForOther
// Deposit(intent, attempt, idempotencyKey) BEFORE ever posting - so
// attempt 2's later success, arriving after attempt 1's late success has
// already posted, takes T13d (multiple_success_for_intent, disputed) via
// the SAME choke point instead of a second posting. This test now asserts
// exactly that outcome against current HEAD.
package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// a7HoldRow and a7WaitAnyLockWaiter now live in
// a7_helpers_integration_test.go (code review, rv-fh3-code-review.md,
// 95a1c34: FH-6's canonical versions, collision-resolved).

// TestA7_1a_SweeperClaimVsCallbackPhaseC_SameDepositIntent is A7-TESTS-1
// item #1a: a cascadable decline creates a REAL 'created' cascade-child
// attempt on the intent (attempt 2); the sweeper's real lease
// (claimBatch) plus per-item claim/drive (driveCreatedAttempt: RG+KYC,
// parent lock, ClaimCreatedForSubmission, phase B, phase C) races a REAL,
// late/duplicate callback delivering evidence for the SAME intent's
// FIRST (already-declined, terminal) attempt. Both take
// `deposit_intents ... FOR UPDATE` before touching any attempt row - the
// exact pair this ADR names.
//
// Must now PASS against ADR 0095 §28 AM-2/INV-DEP-1 (FH-3): at most one
// posting for the whole intent, the waiter-blocks assertions still hold
// (the race itself is real), and the ledger stays balanced with the
// projection matching a full rebuild.
func TestA7_1a_SweeperClaimVsCallbackPhaseC_SameDepositIntent(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	providerA := NewMockProvider("a7-1a-p1", "EUR")
	providerB := NewMockProvider("a7-1a-p2", "EUR")
	providerB.AcceptAllAmounts = true
	registerCapability(t, pool, f, providerA, 100)
	registerCapability(t, pool, f, providerB, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"a7-1a-p1": providerA, "a7-1a-p2": providerB},
		MultiWebhookCredentialResolver{"a7-1a-p1": NewMockWebhookCredentials(providerA), "a7-1a-p2": NewMockWebhookCredentials(providerB)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "a7-1a",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	ref := *res.Attempt.ProviderReference
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	providerA.Resolve(ref, OutcomeDeclined, "provider_unavailable", true) // cascadable

	// Pre-race sweep: decline attempt 1, create attempt 2 ('created',
	// due). Not part of the race itself - this is real setup, exactly
	// like TestSweeper_PendingDeclineCascades_ThenSweptAttemptConvergesOnSecondProvider.
	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	if stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID}); len(stats.Errors) != 0 {
		t.Fatalf("pre-race sweep (decline): %v", stats.Errors)
	}
	declined := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if declined.State != AttemptDeclined {
		t.Fatalf("expected attempt 1 declined, got %s", declined.State)
	}
	child := mustGetLiveDepositAttempt(t, pool, f.tenantID, *declined.DepositIntentID)
	if child.AttemptNo != 2 {
		t.Fatalf("expected a cascade child (attempt_no=2), got %d", child.AttemptNo)
	}

	blocker := a7HoldRow(t, pool, f.tenantID, "deposit_intents", *declined.DepositIntentID, "blocker-intent")

	// Racer A: the sweeper's real lease + per-item claim/drive of the
	// cascade child - a plain goroutine (driveCreatedAttempt manages its
	// own transactions and an outbound call).
	doneA := make(chan struct{})
	var statsA SweepStats
	go func() {
		defer close(doneA)
		statsA = sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	}()
	pidA, ok := a7WaitAnyLockWaiter(t, pool, map[int]bool{blocker.pid: true}, loLockWaitTimeout)
	if !ok {
		blocker.release()
		<-doneA
		t.Fatalf("racer A (sweeper claim) never blocked on the held deposit_intents row; statsA=%+v", statsA)
	}
	if !loContains(loBlockingPIDs(t, pool, pidA), blocker.pid) {
		blocker.release()
		<-doneA
		t.Fatalf("racer A is blocked by someone other than the blocker (pid %d)", blocker.pid)
	}

	// Racer B: a REAL, late/duplicate callback for attempt 1 (already
	// terminal) - single tx, fits loStartRacer directly.
	latePayload := providerA.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)
	racerB := loStartRacer(t, pool, f.tenantID, "late-callback-attempt1", func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "a7-1a-p1", latePayload)
		return err
	})
	if _, ok := loWaitBlocked(t, pool, racerB.pid, racerB.done); !ok {
		blocker.release()
		<-doneA
		t.Fatalf("racer B (late callback) never blocked on the held deposit_intents row")
	}

	blocker.release()
	<-doneA
	errB := racerB.wait()

	loAssertNoDeadlock(t, "A7-1a (sweeper claim vs. callback phase C, same deposit intent)",
		map[string]error{"late-callback-attempt1": errB})
	if len(statsA.Errors) != 0 {
		t.Fatalf("sweeper errors (racing sweep): %v", statsA.Errors)
	}
	if errB != nil {
		t.Fatalf("the late callback for the already-declined attempt 1 must not error (M4-style late evidence, never a 500): %v", errB)
	}

	// Outcome: the cascade child must have been claimed and driven to a
	// live state on the SECOND provider (never left 'created'). This
	// relies on PostgreSQL's own FIFO lock queue: racer A (the sweeper's
	// claim) was already queued on the held deposit_intents row BEFORE
	// racer B (the late callback) even started (a7WaitAnyLockWaiter above
	// confirms this), so when the blocker releases, Postgres grants the
	// lock to racer A first - racer A's own claim/drive therefore always
	// completes (routing the cascade child to the second provider) before
	// racer B's late callback ever runs. A hypothetical scheduler that
	// granted the lock out of queue order could route the child
	// differently, but every currently-supported PostgreSQL version
	// guarantees FIFO granting among waiters on the same row lock.
	finalChild := mustGetAttempt(t, pool, f.tenantID, child.ID)
	if finalChild.State == AttemptCreated {
		t.Fatalf("expected the cascade child to have been claimed and driven past 'created', got %s", finalChild.State)
	}
	if finalChild.ProviderID == nil || *finalChild.ProviderID != "a7-1a-p2" {
		t.Fatalf("expected the cascade child to route to the second provider, got %v", finalChild.ProviderID)
	}

	// INV-DEP-1 (ADR 0095 §28 AM-2, FH-3): resolve provider 2's pending
	// reference to Succeeded and re-sweep to let it reach a terminal state
	// for real, then assert AT MOST ONE distinct ledger posting exists for
	// the whole intent - the choke point (resolvedForOtherDeposit, called
	// from postDepositSuccessOrDispute) must route this second success to
	// T13d (multiple_success_for_intent, disputed), never a second post.
	//
	// Ledger-finance review C7 (rv-fh3-ledger.md, 076e42e): the original
	// version of this check (a) ran only inside `if finalChild.
	// ProviderReference != nil` and (b) counted DISTINCT payment_attempts.
	// ledger_transaction_id, not ledger rows - an unlinked second posting
	// (one that posted to the ledger but was never linked back to an
	// attempt row) would have escaped it entirely. Replaced with an
	// UNCONDITIONAL ledgerDepositTxCount call (the matrix's own helper,
	// inv_dep1_matrix_integration_test.go) - a direct
	// `count(*) FROM ledger_transactions WHERE transaction_type='deposit'
	// AND correlation_id = intent` - run regardless of whether the child
	// ever reached a provider_reference at all.
	if finalChild.ProviderReference != nil {
		providerB.Resolve(*finalChild.ProviderReference, OutcomeSucceeded, "", false)
		setNextActionNow(t, pool, f.tenantID, child.ID)
		if stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID}); len(stats.Errors) != 0 {
			t.Logf("post-resolve sweep errors: %v", stats.Errors)
		}
	}
	if n := ledgerDepositTxCount(t, pool, f.tenantID, *declined.DepositIntentID); n != 1 {
		t.Fatalf("A7-1a / INV-DEP-1: expected exactly 1 deposit ledger posting for deposit_intent_id=%s, got %d",
			*declined.DepositIntentID, n)
	}

	// Adapted from the original scratchpad reproduction (which assumed
	// attempt 1 must be the one left declined/disputed): under ADR 0095
	// §28 AM-2/INV-DEP-1, whichever of the two independent successes
	// posts FIRST (here, attempt 1's late callback - T13(c) treats a
	// late, matching-amount/asset success on a declined attempt as a
	// legitimate capture) legitimately succeeds; the OTHER (attempt 2,
	// whose own later success loses the resolvedForOtherDeposit race) is
	// disputed via the SAME choke point, never silently re-succeeding
	// with a second posting. The actual, load-bearing invariant is
	// "exactly one of the two succeeded, the other disputed" - not which
	// specific one, since that depends on real timing this test does not
	// control past the initial lock-based race.
	afterAttempt1 := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	afterAttempt2 := mustGetAttempt(t, pool, f.tenantID, child.ID)
	succeededCount, disputedCount := 0, 0
	for _, st := range []AttemptState{afterAttempt1.State, afterAttempt2.State} {
		switch st {
		case AttemptSucceeded:
			succeededCount++
		case AttemptDisputed:
			disputedCount++
		}
	}
	if succeededCount != 1 || disputedCount != 1 {
		t.Fatalf("A7-1a / INV-DEP-1: expected exactly one attempt succeeded and the other disputed, got attempt1=%s attempt2=%s", afterAttempt1.State, afterAttempt2.State)
	}

	assertLedgerBalanced(t, pool, f.tenantID)
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
