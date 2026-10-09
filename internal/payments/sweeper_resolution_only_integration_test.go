//go:build integration

// PRH-2 H, security addendum §2: non-active tenants are RESOLUTION-ONLY for the sweeper.
// A suspended or closed tenant's in-flight money still resolves (QueryStatus, evidence,
// dispute, T17 re-drive), audited exactly as for an active tenant; it never gets a NEW
// money-moving call (created-attempt dispatch, cascade child, payout T2 re-claim or T12
// resend). Also: kill switch, tenant fault isolation and loop mechanics (T-1 clock).
package payments

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func attemptsForIntent(t *testing.T, pool *db.Pool, tenantID, intentID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1`, intentID).Scan(&n)
	}); err != nil {
		t.Fatalf("count attempts for intent: %v", err)
	}
	return n
}

// assertDeferred proves a withheld dispatch was really RESCHEDULED by the deferral itself (H-CR-3):
// claimBatch already moves next_action_at to lease_until, so "next_action_at is in the future" proves
// nothing. A real deferral bumps poll_count and sets a next_action_at different from the batch lease.
func assertDeferred(t *testing.T, pool *db.Pool, tenantID, attemptID uuid.UUID, pollBefore, minIncrease int) PaymentAttempt {
	t.Helper()
	a := mustGetAttempt(t, pool, tenantID, attemptID)
	if a.PollCount < pollBefore+minIncrease {
		t.Fatalf("withheld attempt %s was not rescheduled: poll_count %d -> %d (want >= +%d)", attemptID, pollBefore, a.PollCount, minIncrease)
	}
	var sameAsLease bool
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT next_action_at = lease_until FROM payment_attempts WHERE id = $1`, attemptID).Scan(&sameAsLease)
	}); err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if sameAsLease {
		t.Fatalf("withheld attempt %s still carries the batch lease's next_action_at: nothing rescheduled it", attemptID)
	}
	return a
}

// A suspended (or closed) tenant's pending deposit resolves by poll, with the same posting
// and the same audit trail as an active tenant's.
func TestSweeperLoop_NonActiveTenant_PendingDepositResolvesByPoll_AuditedLikeActive(t *testing.T) {
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			spy := newLoopProvider("mock-psp-lp-ro1")
			orch := spy.orchestrator()
			fNA, fAct := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
			var attempts [2]PaymentAttempt
			for i, f := range []orchFixture{fNA, fAct} {
				registerCapability(t, pool, f, spy, 100)
				a, ref := pendingDeposit(t, pool, orch, f, "lp-ro1", 5000)
				spy.Resolve(ref, OutcomeSucceeded, "", false)
				attempts[i] = a
			}
			setTenantStatus(t, pool, fNA.tenantID, status)
			baseNA, baseAct := ledgerTxCount(t, pool, fNA.tenantID), ledgerTxCount(t, pool, fAct.tenantID)

			st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0)
			if st.Errors != 0 || st.Processed != 2 {
				t.Fatalf("both tenants' pending attempts must be polled: %+v", st)
			}
			for i, f := range []orchFixture{fNA, fAct} {
				if got := mustGetAttempt(t, pool, f.tenantID, attempts[i].ID); got.State != AttemptSucceeded {
					t.Fatalf("tenant %d: expected succeeded, got %s", i, got.State)
				}
			}
			if ledgerTxCount(t, pool, fNA.tenantID) != baseNA+1 || ledgerTxCount(t, pool, fAct.tenantID) != baseAct+1 {
				t.Fatal("each tenant must have exactly one new posting")
			}
			if a, b := auditActions(t, pool, fNA.tenantID), auditActions(t, pool, fAct.tenantID); !reflect.DeepEqual(a, b) {
				t.Fatalf("resolution must be audited exactly as for an active tenant:\n non-active: %v\n active:     %v", a, b)
			}
			assertLedgerBalanced(t, pool, fNA.tenantID)
		})
	}
}

// A non-active tenant's `created` deposit attempt is NOT dispatched; it is deferred like an
// engaged kill switch, and an active tenant in the same pass is unaffected. Reactivation
// resumes it with no special action.
func TestSweeperLoop_NonActiveTenant_CreatedDepositNotDispatched_ThenResumesOnReactivation(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-lp-ro2")
	orch := spy.orchestrator()
	fNA, fAct := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var ids [2]uuid.UUID
	for i, f := range []orchFixture{fNA, fAct} {
		registerCapability(t, pool, f, spy, 100)
		intentID := insertRawDepositIntent(t, pool, f, "pending")
		ids[i] = insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
	}
	setTenantStatus(t, pool, fNA.tenantID, "suspended")

	s := newLoopSweeper(pool, orch, true, nil)
	if st := s.RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	if d, _, _ := spy.counts(); d != 1 {
		t.Fatalf("exactly the ACTIVE tenant's attempt may be dispatched, got %d Deposit calls", d)
	}
	na := mustGetAttempt(t, pool, fNA.tenantID, ids[0])
	if na.State != AttemptCreated || na.ProviderID != nil || na.SubmitCount != 0 {
		t.Fatalf("suspended tenant's created attempt must be untouched, got state=%s provider=%v submits=%d", na.State, na.ProviderID, na.SubmitCount)
	}
	if got := assertDeferred(t, pool, fNA.tenantID, ids[0], 0, 1); got.PollCount != 1 {
		t.Fatalf("exactly one deferral expected, poll_count=%d", got.PollCount)
	}
	if act := mustGetAttempt(t, pool, fAct.tenantID, ids[1]); act.State == AttemptCreated {
		t.Fatal("control: the active tenant's created attempt must have been dispatched")
	}

	// Reactivate and make it due: the identical attempt is now dispatched.
	setTenantStatus(t, pool, fNA.tenantID, "active")
	dueNow(t, pool, fNA.tenantID, ids[0])
	if st := s.RunPass(context.Background(), nil, 1); st.Errors != 0 {
		t.Fatalf("pass 2: %+v", st)
	}
	if d, _, _ := spy.counts(); d != 2 {
		t.Fatalf("after reactivation the attempt must dispatch, got %d Deposit calls", d)
	}
}

// A non-active tenant's cascadable poll decline stands (the decline is resolution) but NO
// cascade child is created; an active tenant gets its child.
func TestSweeperLoop_NonActiveTenant_NoCascadeChild(t *testing.T) {
	pool := depositV2ScratchPool(t)
	provA, provB := newLoopProvider("mock-psp-lp-ro3a"), newLoopProvider("mock-psp-lp-ro3b")
	provB.AcceptAllAmounts = true
	orch := NewOrchestrator(
		map[string]PaymentProvider{provA.providerID: provA, provB.providerID: provB},
		MultiWebhookCredentialResolver{provA.providerID: NewMockWebhookCredentials(provA.MockProvider), provB.providerID: NewMockWebhookCredentials(provB.MockProvider)}).WithPayoutDestinations(pitest.Shared())
	fNA, fAct := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var atts [2]PaymentAttempt
	for i, f := range []orchFixture{fNA, fAct} {
		registerCapability(t, pool, f, provA, 100)
		registerCapability(t, pool, f, provB, 200)
		a, ref := pendingDeposit(t, pool, orch, f, "lp-ro3", 5000)
		if *a.ProviderID != provA.providerID {
			t.Fatalf("test premise: routed to %s", *a.ProviderID)
		}
		provA.Resolve(ref, OutcomeDeclined, "provider_unavailable", true)
		atts[i] = a
	}
	setTenantStatus(t, pool, fNA.tenantID, "suspended")

	if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	for i, f := range []orchFixture{fNA, fAct} {
		if got := mustGetAttempt(t, pool, f.tenantID, atts[i].ID); got.State != AttemptDeclined {
			t.Fatalf("tenant %d: the poll decline must apply, got %s", i, got.State)
		}
	}
	if n := attemptsForIntent(t, pool, fNA.tenantID, *atts[0].DepositIntentID); n != 1 {
		t.Fatalf("a non-active tenant must get no cascade child, got %d attempts", n)
	}
	if n := auditActions(t, pool, fNA.tenantID)["payment.cascade_skipped_resolution_only"]; n != 1 {
		t.Fatalf("the skipped cascade must be audited once (H-CR-6), got %d", n)
	}
	if n := auditActions(t, pool, fAct.tenantID)["payment.cascade_skipped_resolution_only"]; n != 0 {
		t.Fatalf("an active tenant must not get that audit, got %d", n)
	}
	if n := attemptsForIntent(t, pool, fAct.tenantID, *atts[1].DepositIntentID); n != 2 {
		t.Fatalf("control: the active tenant must get its cascade child, got %d attempts", n)
	}
	if d, _, _ := provB.counts(); d != 0 {
		t.Fatalf("no Deposit may have reached the second provider yet, got %d", d)
	}
}

// Payout half: a non-active tenant's created payout is not re-claimed or sent, its ambiguous
// payout is not resent (even for an idempotent provider), but its pending payout resolves by
// poll with a settlement and the same audit trail as an active tenant's.
func TestSweeperLoop_NonActiveTenant_PayoutResolutionOnly(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-payout-lp-ro4")
	m := spy.Capabilities().Manifest
	m.IdempotentSubmission = true // so a resend WOULD happen for an active tenant (control)
	spy.SetManifest(m)
	orch := spy.orchestrator()

	fNA, fAct := seedPayoutFixture(t, pool, 100_000, true), seedPayoutFixture(t, pool, 100_000, true)
	var created, ambiguous, pending [2]PaymentAttempt
	for i, f := range []payoutFixture{fNA, fAct} {
		registerCapability(t, pool, f.orchFixture, spy, 100)
		_, created[i] = notSentPayoutAttempt(t, pool, orch, f, 500, fmt.Sprintf("lp-ro4-created-%d", i))

		wrA := approvedWithdrawal(t, pool, f, 600, fmt.Sprintf("lp-ro4-amb-%d", i))
		claimA, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wrA.ID, "bank_transfer", testSubmitActor())
		if err != nil {
			t.Fatalf("claim ambiguous: %v", err)
		}
		if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wrA.ID, claimA.Attempt, GateResult[WithdrawResult]{Class: ErrorClassAmbiguous}, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
			t.Fatalf("apply ambiguous: %v", err)
		}
		dueNow(t, pool, f.tenantID, claimA.Attempt.ID)
		ambiguous[i] = mustGetAttempt(t, pool, f.tenantID, claimA.Attempt.ID)

		wrP := approvedWithdrawal(t, pool, f, 700, fmt.Sprintf("lp-ro4-pend-%d", i))
		claimP, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wrP.ID, "bank_transfer", testSubmitActor())
		if err != nil {
			t.Fatalf("claim pending: %v", err)
		}
		gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, spy, claimP.Attempt, WithDestinations(pitest.Shared()))
		if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wrP.ID, claimP.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
			t.Fatalf("apply pending: %v", err)
		}
		pending[i] = mustGetAttempt(t, pool, f.tenantID, claimP.Attempt.ID)
		if pending[i].State != AttemptPending || pending[i].ProviderReference == nil {
			t.Fatalf("test premise: pending payout, got %s", pending[i].State)
		}
		spy.Resolve(*pending[i].ProviderReference, OutcomeSucceeded, "", false)
		dueNow(t, pool, f.tenantID, pending[i].ID)
	}
	setTenantStatus(t, pool, fNA.tenantID, "suspended")
	_, withdrawsBefore, _ := spy.counts()
	baseNA, baseAct := ledgerTxCount(t, pool, fNA.tenantID), ledgerTxCount(t, pool, fAct.tenantID)

	if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}

	// Non-active: nothing new sent.
	if got := mustGetAttempt(t, pool, fNA.tenantID, created[0].ID); got.State != AttemptCreated || got.SubmitCount != created[0].SubmitCount {
		t.Fatalf("suspended tenant's created payout must not be re-claimed: state=%s submits=%d", got.State, got.SubmitCount)
	}
	if got := mustGetAttempt(t, pool, fNA.tenantID, ambiguous[0].ID); got.State != AttemptAmbiguous || got.SubmitCount != ambiguous[0].SubmitCount {
		t.Fatalf("suspended tenant's ambiguous payout must not be resent: state=%s submits=%d", got.State, got.SubmitCount)
	}
	if got := assertDeferred(t, pool, fNA.tenantID, created[0].ID, created[0].PollCount, 1); got.PollCount != created[0].PollCount+1 {
		t.Fatalf("created payout: exactly one deferral expected, poll_count %d -> %d", created[0].PollCount, got.PollCount)
	}
	// The ambiguous payout is polled first (one reschedule) and then its resend is deferred (a second).
	assertDeferred(t, pool, fNA.tenantID, ambiguous[0].ID, ambiguous[0].PollCount, 2)
	// Active control: both WERE driven (one T2 re-claim Withdraw and one T12 resend Withdraw).
	if got := mustGetAttempt(t, pool, fAct.tenantID, created[1].ID); got.State == AttemptCreated {
		t.Fatal("control: the active tenant's created payout must be re-claimed")
	}
	if got := mustGetAttempt(t, pool, fAct.tenantID, ambiguous[1].ID); got.SubmitCount <= ambiguous[1].SubmitCount {
		t.Fatalf("control: the active tenant's ambiguous payout must be resent (idempotent manifest), submits %d -> %d", ambiguous[1].SubmitCount, got.SubmitCount)
	}
	if _, w, _ := spy.counts(); w-withdrawsBefore != 2 {
		t.Fatalf("exactly the ACTIVE tenant's two new Withdraw calls expected, got %d", w-withdrawsBefore)
	}
	// Resolution still works, with a settlement and identical audit actions for the pending payout.
	for i, f := range []payoutFixture{fNA, fAct} {
		if got := mustGetAttempt(t, pool, f.tenantID, pending[i].ID); got.State != AttemptSucceeded {
			t.Fatalf("tenant %d: the pending payout must resolve by poll, got %s", i, got.State)
		}
	}
	if ledgerTxCount(t, pool, fNA.tenantID) != baseNA+1 || ledgerTxCount(t, pool, fAct.tenantID) != baseAct+1 {
		t.Fatal("each tenant: exactly one settlement posting from the resolved payout")
	}
	var wrState withdrawal.State
	if err := pool.WithTenant(context.Background(), fNA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		wr, err := withdrawal.GetByID(ctx, tx, *pending[0].WithdrawalRequestID)
		wrState = wr.State
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if wrState != withdrawal.StateCompleted {
		t.Fatalf("the suspended tenant's resolved payout must complete its withdrawal, got %s", wrState)
	}
	loAssertBalanced(t, pool, fNA.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, fNA.tenantID)
}

// Tenant isolation: one tenant's suspension neither affects nor exposes another's sweep.
// Credentials are resolved only for the attempt's own tenant.
func TestSweeperLoop_TenantIsolation_SuspensionNeitherAffectsNorExposesAnother(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-lp-iso")
	orch := spy.orchestrator()
	fA, fB := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var pollA, pollB PaymentAttempt
	var createdA, createdB uuid.UUID
	for _, f := range []orchFixture{fA, fB} {
		registerCapability(t, pool, f, spy, 100)
		a, ref := pendingDeposit(t, pool, orch, f, "lp-iso-poll", 5000)
		spy.Resolve(ref, OutcomeSucceeded, "", false)
		intentID := insertRawDepositIntent(t, pool, f, "pending")
		c := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
		if f.tenantID == fA.tenantID {
			pollA, createdA = a, c
		} else {
			pollB, createdB = a, c
		}
	}
	setTenantStatus(t, pool, fA.tenantID, "suspended")
	res := &recordingResolver{}
	if st := newLoopSweeper(pool, orch, true, res).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	// A (suspended): polled only. B (active): polled and dispatched.
	if got := mustGetAttempt(t, pool, fA.tenantID, pollA.ID); got.State != AttemptSucceeded {
		t.Fatalf("A's poll: %s", got.State)
	}
	if got := mustGetAttempt(t, pool, fA.tenantID, createdA); got.State != AttemptCreated {
		t.Fatalf("A's created attempt must not move: %s", got.State)
	}
	if got := mustGetAttempt(t, pool, fB.tenantID, pollB.ID); got.State != AttemptSucceeded {
		t.Fatalf("B's poll: %s", got.State)
	}
	if got := mustGetAttempt(t, pool, fB.tenantID, createdB); got.State == AttemptCreated {
		t.Fatal("B's created attempt must have been dispatched, unaffected by A's suspension")
	}
	seen := res.tenants()
	if seen[fA.tenantID] != 1 || seen[fB.tenantID] != 2 || len(seen) != 2 {
		t.Fatalf("credentials must be resolved per attempt tenant only (A: poll; B: poll + deposit), got %v", seen)
	}
	// Exposure: B's tenant context sees none of A's attempts.
	var visibleToB int
	if err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE id = ANY($1)`, []uuid.UUID{pollA.ID, createdA}).Scan(&visibleToB)
	}); err != nil {
		t.Fatal(err)
	}
	if visibleToB != 0 {
		t.Fatalf("tenant B can see %d of tenant A's attempts", visibleToB)
	}
}

// The kill switch still applies on top of everything: new dispatch is withheld, polls are not.
func TestSweeperLoop_KillSwitch_WithholdsDispatchButNeverPolls(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	spy := newLoopProvider("mock-psp-lp-ks")
	registerCapability(t, pool, f.orchFixture, spy, 100)
	orch := spy.orchestrator()
	pend, ref := pendingDeposit(t, pool, orch, f.orchFixture, "lp-ks-poll", 5000)
	spy.Resolve(ref, OutcomeSucceeded, "", false)
	intentID := insertRawDepositIntent(t, pool, f.orchFixture, "pending")
	createdDep := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
	_, createdPay := notSentPayoutAttempt(t, pool, orch, f, 500, "lp-ks-payout")

	principal := depositKillSwitchStaffPrincipal(t, pool, f.tenantID)
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, "*", KillSwitchOperationAny, "lp-ks")
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}
	_, w0, _ := spy.counts()
	if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Panics != 0 {
		t.Fatalf("pass: %+v", st)
	}
	if d, w, _ := spy.counts(); d != 1 || w != w0 {
		// d==1 is the single Deposit of the pendingDeposit() setup above.
		t.Fatalf("kill switch must withhold every new dispatch: deposits=%d (setup made 1), new withdraws=%d", d, w-w0)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, pend.ID); got.State != AttemptSucceeded {
		t.Fatalf("polls are never stopped by the kill switch, got %s", got.State)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, createdDep); got.State == AttemptPending || got.State == AttemptSucceeded || got.State == AttemptSubmitting {
		t.Fatalf("created deposit must not have been dispatched under the kill switch, got %s", got.State)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, createdPay.ID); got.State != AttemptCreated {
		t.Fatalf("created payout must not be re-claimed under the kill switch, got %s", got.State)
	}
}

// Tenant fault isolation: a panic while sweeping one tenant (here outside the provider gate, in
// Capabilities()) does not stop the others, whatever the tenant order, and is counted.
func TestSweeperLoop_TenantFaultIsolation_PanicInOneTenantDoesNotStopOthers(t *testing.T) {
	for offset := 0; offset < 2; offset++ {
		t.Run(fmt.Sprintf("offset_%d", offset), func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			bad, good := newLoopProvider("mock-psp-lp-bad"), newLoopProvider("mock-psp-lp-good")
			orch := NewOrchestrator(
				map[string]PaymentProvider{bad.providerID: bad, good.providerID: good},
				MultiWebhookCredentialResolver{bad.providerID: NewMockWebhookCredentials(bad.MockProvider), good.providerID: NewMockWebhookCredentials(good.MockProvider)}).WithPayoutDestinations(pitest.Shared())
			fBad, fGood := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
			registerCapability(t, pool, fBad, bad, 100)
			registerCapability(t, pool, fGood, good, 100)
			aBad, refBad := pendingDeposit(t, pool, orch, fBad, "lp-bad", 5000)
			aGood, refGood := pendingDeposit(t, pool, orch, fGood, "lp-good", 5000)
			bad.Resolve(refBad, OutcomeSucceeded, "", false)
			good.Resolve(refGood, OutcomeSucceeded, "", false)
			bad.setPanicCapabilities(true)

			st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, offset)
			if st.Panics != 1 || st.Processed != 1 {
				t.Fatalf("want exactly one contained panic and one processed item, got %+v", st)
			}
			if got := mustGetAttempt(t, pool, fGood.tenantID, aGood.ID); got.State != AttemptSucceeded {
				t.Fatalf("the healthy tenant must still be swept, got %s", got.State)
			}
			if got := mustGetAttempt(t, pool, fBad.tenantID, aBad.ID); got.State != AttemptPending {
				t.Fatalf("the faulting tenant's attempt is left for the next lease, got %s", got.State)
			}
		})
	}
}

// Loop mechanics on an injected tick source (T-1 clock): an immediate first pass, one pass per
// tick, a panic inside a pass is recovered and the loop continues, and cancellation stops it.
func TestSweeperLoop_LoopMechanics_ImmediatePassPerTickPanicRecoveredCancelStops(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-lp-loop")
	s := newLoopSweeper(pool, spy.orchestrator(), true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	passes := make(chan struct{}, 8)
	calls := 0
	done := make(chan struct{})
	go func() {
		runSweeperLoop(ctx, s, nil, ticks, func(SweepPassStats) {
			calls++
			passes <- struct{}{}
			if calls == 2 {
				panic("injected panic inside a pass")
			}
		})
		close(done)
	}()
	recvPass := func(what string) {
		t.Helper()
		select {
		case <-passes:
		case <-time.After(hangGuard):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	recvPass("the immediate first pass (no tick sent)")
	sendTick(t, ticks)
	recvPass("the second pass (its callback panics and must be recovered)")
	sendTick(t, ticks)
	recvPass("a third pass, proving the loop survived the panic")
	cancel()
	waitClosed(t, done, "the loop to stop on cancel")
}
