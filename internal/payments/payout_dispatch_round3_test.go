//go:build integration

// RV-PRH-I1 payout round 3: the code re-review
// (rv-prh-i1-payout-code-review.md, "Re-review — fix round") and the
// ledger-finance re-review (rv-prh-i1-payout-ledger.md, "Re-review (fix
// round)") both landed new findings on TOP of the already-fixed B1-B8/
// C1/H1-H4/M1-M5 set: N1/R6 (the ambiguous branch never stored the
// reference on the attempt itself), R1 (routine non-definite evidence
// racing a callback was disputed instead of converging), R2 (/resolve had
// no lease check), R3 (phase C's lock order regressed after the M3 fix),
// and N6 (a QueryStatus success with a MISMATCHED reference was never
// disputed). This file's probes are the ledger re-review's own J/K/M/N
// probes, turned into permanent tests, plus the code re-review's required
// exact-count/falsifiable fixes that didn't fit payout_dispatch_fixround_test.go
// cleanly. Runs on the same private scratch-DB harness as the other payout
// test files.
package payments

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// --- N1/R6: the ambiguous branch persists the reference on the attempt ------

// ambiguousThenSettleQueryProvider answers Withdraw with Ambiguous plus a
// fresh reference (like a PSP that accepted the instruction but didn't
// confirm synchronously), then answers QueryStatus for that SAME reference
// with a definite Succeeded - the shape N1 proved was unreachable (the
// reference only ever landed on withdrawal_requests, never on the attempt
// itself, so T12's poll-first step and PollPayoutStatus had nothing of
// their own to query).
type ambiguousThenSettleQueryProvider struct {
	*withdrawCountingProvider
	queryCalls int
}

func (p *ambiguousThenSettleQueryProvider) Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error) {
	res, err := p.withdrawCountingProvider.Withdraw(ctx, req)
	res.Outcome = OutcomeAmbiguous
	res.ProviderReference = "n1-settle-ref-" + uuid.New().String()
	return res, err
}

func (p *ambiguousThenSettleQueryProvider) QueryStatus(_ context.Context, ref string) (StatusResult, error) {
	p.queryCalls++
	return StatusResult{ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}, nil
}

// TestSweeper_N1_AmbiguousWithReference_ConvergesByPollZeroResends is the
// N1/R6-required test: an ambiguous payout that carries a reference
// converges via QueryStatus with ZERO resends, proving both that the
// reference is now on the attempt itself and that the poll-first step in
// resubmitPayoutAmbiguous is reachable at all (before this fix, mutant M4
// deleting that block survived because it was never exercised).
func TestSweeper_N1_AmbiguousWithReference_ConvergesByPollZeroResends(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-r3-n1", "EUR")
	spy := &withdrawCountingProvider{MockProvider: inner}
	provider := &ambiguousThenSettleQueryProvider{withdrawCountingProvider: spy}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-n1": provider}, MultiWebhookCredentialResolver{"mock-r3-n1": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-n1")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var afterT1p PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		afterT1p, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if afterT1p.State != AttemptAmbiguous {
		t.Fatalf("setup: expected ambiguous after the sync Withdraw, got %s", afterT1p.State)
	}
	if afterT1p.ProviderReference == nil || *afterT1p.ProviderReference == "" {
		t.Fatalf("N1/R6 regression: expected the attempt itself to carry the provider_reference returned alongside the Ambiguous result, got nil")
	}

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Touch(ctx, tx, afterT1p.ID)
	}); err != nil {
		t.Fatalf("touch: %v", err)
	}
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	st := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(st.Errors) != 0 {
		t.Fatalf("unexpected sweep errors: %v", st.Errors)
	}

	if spy.count() != 1 {
		t.Fatalf("N1/R6 regression: expected ZERO resends (Withdraw still called exactly once, by T1p), got %d total calls", spy.count())
	}
	if provider.queryCalls == 0 {
		t.Fatalf("N1/R6 regression: expected the poll-first step to actually call QueryStatus at least once")
	}

	var got withdrawal.WithdrawalRequest
	var final PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		final, err = GetAttemptByID(ctx, tx, afterT1p.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if final.State != AttemptSucceeded {
		t.Fatalf("expected the poll to settle the attempt as succeeded, got %s", final.State)
	}
	if got.State != withdrawal.StateCompleted {
		t.Fatalf("expected the withdrawal to complete via the poll, got %s", got.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- R1: routine non-definite evidence racing a faster piece must converge --

// TestPayoutDispatch_R1_SyncPendingAfterCallbackPending_ConvergesNoDispute
// is ledger re-review probe J turned into a test: a verified `pending`
// webhook (simulated here by calling MarkAccepted directly - receipt.go
// itself is owned by a different agent and is not touched by this file)
// races phase C's own slower synchronous Pending result. The synchronous
// result CAS-conflicts (the attempt is no longer `submitting`) but this
// must converge (reschedule), never park the attempt as `disputed`.
func TestPayoutDispatch_R1_SyncPendingAfterCallbackPending_ConvergesNoDispute(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r3-r1a", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-r1a": provider}, MultiWebhookCredentialResolver{"mock-r3-r1a": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-r1a")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	// Simulate a callback that races phase C and moves the SAME attempt to
	// `pending` first, with its own reference (T4).
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, claim.Attempt.ID, EvidenceCallback, "r1a-callback-ref", time.Now().Add(30*time.Second))
	}); err != nil {
		t.Fatalf("simulate racing callback: %v", err)
	}

	// The ORIGINAL synchronous Withdraw call now returns, ALSO Pending -
	// weaker-or-equal evidence arriving second, not a contradiction.
	gr := GateResult[WithdrawResult]{Class: ErrorClassPending, Value: WithdrawResult{Outcome: OutcomePending, ProviderReference: "sync-slow-ref"}}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptPending {
		t.Fatalf("PROBE J regression: expected the attempt to stay pending (routine convergence, not a dispute), got %s", attempt.State)
	}
	if attempt.ProviderReference == nil || *attempt.ProviderReference != "r1a-callback-ref" {
		t.Fatalf("expected the callback's own reference to remain on file (set-once), got %v", attempt.ProviderReference)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_R1_SyncTimeoutAfterCallbackPending_ConvergesNoDispute
// is probe K: the synchronous call times out (Ambiguous) instead of
// returning Pending - transport silence is not the provider "forgetting",
// so this must ALSO converge, never dispute.
func TestPayoutDispatch_R1_SyncTimeoutAfterCallbackPending_ConvergesNoDispute(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r3-r1b", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-r1b": provider}, MultiWebhookCredentialResolver{"mock-r3-r1b": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-r1b")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, claim.Attempt.ID, EvidenceCallback, "r1b-callback-ref", time.Now().Add(30*time.Second))
	}); err != nil {
		t.Fatalf("simulate racing callback: %v", err)
	}

	gr := GateResult[WithdrawResult]{Class: ErrorClassAmbiguous}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptPending {
		t.Fatalf("PROBE K regression: expected the attempt to stay pending (transport silence is not a contradiction), got %s", attempt.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_R1_NotSentAfterCallbackPending_IsNoOp covers R1's
// third listed case: "Sync NotSent on a non-submitting attempt: no-op."
func TestPayoutDispatch_R1_NotSentAfterCallbackPending_IsNoOp(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r3-r1c", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-r1c": provider}, MultiWebhookCredentialResolver{"mock-r3-r1c": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-r1c")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, claim.Attempt.ID, EvidenceCallback, "r1c-callback-ref", time.Now().Add(30*time.Second))
	}); err != nil {
		t.Fatalf("simulate racing callback: %v", err)
	}

	gr := GateResult[WithdrawResult]{Class: ErrorClassNotSent}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptPending {
		t.Fatalf("PROBE R1(c) regression: expected a NotSent result on a non-submitting attempt to be a no-op, got %s", attempt.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- R2: /resolve must never touch a live-lease submitting attempt --------

// TestPollPayoutStatus_R2_LeaseStillLive_RefusesInFlight is probe M turned
// into a test.
func TestPollPayoutStatus_R2_LeaseStillLive_RefusesInFlight(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r3-r2", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-r2": provider}, MultiWebhookCredentialResolver{"mock-r3-r2": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-r2")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if claim.Attempt.LeaseUntil == nil || !claim.Attempt.LeaseUntil.After(time.Now()) {
		t.Fatalf("setup: expected a live lease right after T1p")
	}

	// Staff presses /resolve WHILE phase B is (conceptually) still running.
	err = PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, claim.Attempt, time.Now().Add(30*time.Second))
	if !errors.Is(err, ErrPayoutDispatchInFlight) {
		t.Fatalf("R2 regression: expected ErrPayoutDispatchInFlight for a live-lease submitting attempt, got %v", err)
	}

	var untouched PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		untouched, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if untouched.State != AttemptSubmitting {
		t.Fatalf("R2 regression: the in-flight attempt must be left untouched, got %s", untouched.State)
	}
	if untouched.EverPossiblySent {
		t.Fatalf("R2 regression: a never-sent payout must not lose M3 eligibility while /resolve refuses it")
	}

	// The ORIGINAL phase B/C then completes normally - proving the refused
	// poll above didn't corrupt anything.
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult after the refused /resolve poll: %v", err)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- R3: one lock order everywhere - withdrawal, then attempt --------------

// TestPayoutDispatch_R3_LockOrderConsistentAcrossConcurrentEvidence_NoDeadlock
// is ledger re-review probe N turned into a test: a concurrent transaction
// taking the SAME withdrawal-then-attempt lock order phase C now uses (the
// receipt path's own order) must never deadlock against phase C, only
// serialize behind it.
func TestPayoutDispatch_R3_LockOrderConsistentAcrossConcurrentEvidence_NoDeadlock(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-r3-n", "EUR")
	provider := &syncSuccessWithdrawProvider{MockProvider: inner, ref: "r3-n-ref"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-n": provider}, MultiWebhookCredentialResolver{"mock-r3-n": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-n")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	holderReady := make(chan struct{})
	holderRelease := make(chan struct{})
	holderDone := make(chan struct{})
	var holderErr error
	go func() {
		defer close(holderDone)
		holderErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			// The receipt path's own order: withdrawal FIRST, then the
			// attempt row - the SAME order phase C now uses (R3).
			if _, err := tx.Exec(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, wr.ID); err != nil {
				return err
			}
			close(holderReady)
			<-holderRelease
			_, err := tx.Exec(ctx, `SELECT id FROM payment_attempts WHERE id = $1 FOR UPDATE`, claim.Attempt.ID)
			return err
		})
	}()
	<-holderReady

	applyDone := make(chan struct{})
	var applyErr error
	go func() {
		defer close(applyDone)
		gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
		applyErr = ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync)
	}()

	// Give ApplyPayoutResult a moment to reach (and block behind) its own
	// withdrawal lock, then release the holder - if the lock orders ever
	// diverge again, THIS is exactly the shape that deadlocks (40P01).
	time.Sleep(300 * time.Millisecond)
	close(holderRelease)
	<-holderDone
	<-applyDone

	if holderErr != nil {
		t.Fatalf("PROBE N regression: lock holder failed (expected clean serialization, not a deadlock victim): %v", holderErr)
	}
	if applyErr != nil {
		t.Fatalf("PROBE N regression: ApplyPayoutResult failed (expected clean serialization, not a deadlock victim): %v", applyErr)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptSucceeded {
		t.Fatalf("expected the sync success to still apply cleanly after serializing behind the lock holder, got %s", attempt.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- C-T1: NotSent on a RESEND must go to ambiguous, never `created` -------

// TestPayoutDispatch_CT1_NotSentOnResendRoutesToAmbiguousNotCreated pins
// mutant MH (the code re-review's own catalogue: "the branch disabled
// survives - no test covers it").
func TestPayoutDispatch_CT1_NotSentOnResendRoutesToAmbiguousNotCreated(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r3-ct1", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-ct1": provider}, MultiWebhookCredentialResolver{"mock-r3-ct1": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "payout-r3-ct1")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var ambiguous PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ambiguous, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if !ambiguous.EverPossiblySent {
		t.Fatalf("setup: expected ever_possibly_sent after the first ambiguous transition")
	}

	resendToken := uuid.New()
	var resent PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := ResubmitAmbiguous(ctx, tx, ambiguous.ID, resendToken, "test-resend", time.Now().Add(time.Minute), 0); err != nil {
			return err
		}
		var err error
		resent, err = GetAttemptByID(ctx, tx, ambiguous.ID)
		return err
	}); err != nil {
		t.Fatalf("resubmit: %v", err)
	}

	// C-T1 (mutant MH): the resend's own call this time never reaches the
	// provider at all (NotSent) - because ever_possibly_sent is ALREADY
	// true from the first ambiguous transition, this MUST go back to
	// ambiguous (T6), never to `created` (MarkNotSent's own CAS requires
	// NOT ever_possibly_sent and would simply fail/roll back if this
	// branch were disabled).
	notSent := GateResult[WithdrawResult]{Class: ErrorClassNotSent}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, resent, notSent, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (NotSent on resend): %v", err)
	}

	var final PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetAttemptByID(ctx, tx, ambiguous.ID)
		return err
	}); err != nil {
		t.Fatalf("reread final: %v", err)
	}
	if final.State != AttemptAmbiguous {
		t.Fatalf("mutant MH regression: expected NotSent-on-resend to route to ambiguous (T6), got %s", final.State)
	}
	if final.SubmitCount != 2 {
		t.Fatalf("expected submit_count to remain 2 (the resend, not a THIRD send), got %d", final.SubmitCount)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- N6: a mismatched (not merely missing) echoed reference disputes ------

// TestPollPayoutStatus_ProviderReferenceMismatch_Disputes proves N6: a
// QueryStatus success that echoes a DIFFERENT non-empty reference from the
// one already on file is fail-closed disputed, never silently settled
// against the echoed value.
func TestPollPayoutStatus_ProviderReferenceMismatch_Disputes(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-r3-n6", "EUR")
	provider := &scriptedWithdrawProvider{
		MockProvider: inner,
		withdraws:    []WithdrawResult{{Outcome: OutcomePending, ProviderReference: "n6-orig-ref"}},
		statuses:     []StatusResult{{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", ProviderReference: "n6-different-ref"}},
	}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-n6": provider}, MultiWebhookCredentialResolver{"mock-r3-n6": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-n6")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptPending || attempt.ProviderReference == nil || *attempt.ProviderReference != "n6-orig-ref" {
		t.Fatalf("setup: expected pending with the original reference on file, got state=%s ref=%v", attempt.State, attempt.ProviderReference)
	}

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Touch(ctx, tx, attempt.ID)
	}); err != nil {
		t.Fatalf("touch: %v", err)
	}
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	st := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(st.Errors) != 0 {
		t.Fatalf("unexpected sweep errors: %v", st.Errors)
	}

	var got withdrawal.WithdrawalRequest
	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		after, err = GetAttemptByID(ctx, tx, attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if after.State != AttemptDisputed || after.TerminalReason == nil || *after.TerminalReason != "provider_reference_mismatch" {
		t.Fatalf("N6 regression: expected a provider_reference_mismatch dispute, got state=%s reason=%v", after.State, after.TerminalReason)
	}
	if got.State == withdrawal.StateCompleted {
		t.Fatalf("N6 regression: a reference-mismatched QueryStatus success must never complete the withdrawal")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- M5: the B6 staff audit row (code re-review mutant catalogue) ----------

// TestClaimForDispatch_M5_AllowPathWritesStaffAuditRow pins mutant M5 (the
// code re-review's own catalogue: "the allow-path staff audit made a
// no-op - survives the full payments and httpserver suites. No test
// anywhere asserts this audit row."): the SAME transaction that commits
// the T1p claim must also contain a `withdrawal.submit.http` audit row
// attributing it to the ACTUAL staff actor, with a success outcome.
func TestClaimForDispatch_M5_AllowPathWritesStaffAuditRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r3-m5", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-m5": provider}, MultiWebhookCredentialResolver{"mock-r3-m5": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-m5")
	actor := testSubmitActor()
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", actor)
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	var count int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.submit.http' AND target_id = $1`, wr.ID.String()).Scan(&count)
	}); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count != 1 {
		t.Fatalf("M5 regression: expected exactly 1 withdrawal.submit.http audit row for this claim, got %d", count)
	}
	var actorID uuid.UUID
	var outcome string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_id, outcome FROM audit_log WHERE action = 'withdrawal.submit.http' AND target_id = $1`, wr.ID.String()).Scan(&actorID, &outcome)
	}); err != nil {
		t.Fatalf("query audit_log row: %v", err)
	}
	if actorID != actor.StaffID {
		t.Fatalf("M5 regression: expected the audit row's actor to be the real staff actor %s, got %s", actor.StaffID, actorID)
	}
	if outcome != "success" {
		t.Fatalf("M5 regression: expected outcome=success on the allow path, got %q", outcome)
	}
	_ = claim
}

// --- M9/M10: late/contradicting evidence must still dispute, not vanish ----

// TestPayoutDispatch_M10_SuccessAfterDecline_DisputesT14 pins mutant M10
// (code re-review catalogue: "T14 late-success-after-decline routing
// removed"): a definite success reaching an attempt that ALREADY reached
// `declined` (its hold already released) is a genuine late/contradicting-
// evidence event and must dispute (T14), with a P1 audit record - never
// silently roll back or, worse, complete the withdrawal a second time.
func TestPayoutDispatch_M10_SuccessAfterDecline_DisputesT14(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := &syncDeclineWithdrawProvider{MockProvider: NewMockProvider("mock-r3-m10", "EUR"), reason: "account_closed"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-m10": provider}, MultiWebhookCredentialResolver{"mock-r3-m10": NewMockWebhookCredentials(provider.MockProvider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-m10")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (decline): %v", err)
	}
	var declined PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		declined, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if declined.State != AttemptDeclined {
		t.Fatalf("setup: expected declined, got %s", declined.State)
	}

	// A LATE success now arrives for the SAME attempt (e.g. a delayed
	// synchronous retry response, or an independent second evidence
	// source) - the hold is already released, so this is exactly the
	// double-payout signal T14 exists to record.
	late := GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "m10-late-success-ref"}}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, declined, late, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (late success): %v", err)
	}

	var final PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread final: %v", err)
	}
	if final.State != AttemptDisputed {
		t.Fatalf("M10 regression: expected a late success after decline to dispute (T14), got %s", final.State)
	}
	if final.TerminalReason == nil || *final.TerminalReason != "late_success_after_terminal" {
		t.Fatalf("M10 regression: expected terminal_reason=late_success_after_terminal, got %v", final.TerminalReason)
	}
	var auditCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payments.payout_late_contradicting_evidence' AND target_id = $1`, declined.ID.String()).Scan(&auditCount)
	}); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("M10 regression: expected a P1 audit record for the late-evidence dispute, got %d", auditCount)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPollPayoutStatus_M9_DefiniteMismatchAfterTerminal_StillDisputes pins
// mutant M9 (code re-review catalogue: "payoutHandleContradiction
// late-evidence routing removed") on payoutHandleContradiction's OTHER
// entry point - the generic tail reached from
// applyPayoutSuccessCheckedFromStatus's OWN dispute call CAS-conflicting
// because the attempt is ALREADY terminal (declined) by the time a
// mismatched QueryStatus success is applied. This is a DEFINITE result
// class conflicting with an ALREADY-TERMINAL state, so - unlike the R1
// tests above (all non-definite) - it must still dispute via
// applyPayoutLateEvidence, never silently no-op.
func TestPollPayoutStatus_M9_DefiniteMismatchAfterTerminal_StillDisputes(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := &syncDeclineWithdrawProvider{MockProvider: NewMockProvider("mock-r3-m9", "EUR"), reason: "account_closed"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-m9": provider}, MultiWebhookCredentialResolver{"mock-r3-m9": NewMockWebhookCredentials(provider.MockProvider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-m9")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (decline): %v", err)
	}
	var declined PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		declined, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if declined.State != AttemptDeclined {
		t.Fatalf("setup: expected declined, got %s", declined.State)
	}

	// A mismatched QueryStatus success now arrives for the SAME, already-
	// terminal attempt - applyPayoutSuccessCheckedFromStatus's OWN
	// ApplyDisputeFromNonTerminal fails its CAS (declined is not one of
	// submitting/pending/ambiguous), which must fall through to
	// payoutHandleContradiction's DEFINITE branch and still dispute/audit,
	// not silently swallow the conflict.
	// NOTE: deliberately pass the STALE, pre-decline claim.Attempt (still
	// showing state=submitting in memory) - applyPayoutStatusEvidence
	// switches on the CALLER's copy of attempt.State to pick its evidence-
	// mapping branch (case AttemptSubmitting -> ErrorClassSucceeded), while
	// the CAS calls inside always act against the REAL, current row by ID.
	// This is exactly the shape a real caller sees: it read the attempt
	// before calling QueryStatus, and the row moved on underneath it.
	statusGR := GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: StatusResult{Outcome: OutcomeSucceeded, Amount: 50_000, AssetCode: "EUR", ProviderReference: "m9-mismatch-ref"}}
	if err := applyPayoutStatusEvidence(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, statusGR, EvidenceQueryStatus, time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("applyPayoutStatusEvidence: %v", err)
	}

	var final PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread final: %v", err)
	}
	if final.State != AttemptDisputed {
		t.Fatalf("M9 regression: expected a definite mismatch after a terminal state to still dispute, got %s", final.State)
	}
	var auditCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payments.payout_late_contradicting_evidence' AND target_id = $1`, declined.ID.String()).Scan(&auditCount)
	}); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("M9 regression: expected a P1 audit record for the late-evidence dispute, got %d", auditCount)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- M11: decline_reason is always canonical, never raw vendor text -------

// TestPayoutDispatch_M11_RawVendorDeclineReasonNeverPersisted pins mutant
// M11 (code re-review catalogue: "canonicalDeclineReason passes raw vendor
// text through - survives").
func TestPayoutDispatch_M11_RawVendorDeclineReasonNeverPersisted(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	const rawVendorText = "ACCT-99-XYZ: unexpected vendor-specific free text, never allow-listed"
	provider := &syncDeclineWithdrawProvider{MockProvider: NewMockProvider("mock-r3-m11", "EUR"), reason: rawVendorText}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-m11": provider}, MultiWebhookCredentialResolver{"mock-r3-m11": NewMockWebhookCredentials(provider.MockProvider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-m11")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.DeclineReason == nil || *attempt.DeclineReason != "provider_declined" {
		t.Fatalf("M11 regression: expected the canonical fallback code 'provider_declined', got %v (raw vendor text must never reach a permanent column)", attempt.DeclineReason)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- M12: a repeat KYC deny at T2/T12 is idempotent, never a hard error ----

// TestSweeper_M12_RepeatedKYCDenyOnEscalatedAttempt_IsIdempotent pins
// mutant M12 (code re-review catalogue: "repeated-deny idempotence removed
// - survives"): a SECOND KYC deny on an attempt already escalated by a
// FIRST deny must reschedule quietly, never error/roll back every sweep
// tick forever.
func TestSweeper_M12_RepeatedKYCDenyOnEscalatedAttempt_IsIdempotent(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r3-m12", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r3-m12": provider}, MultiWebhookCredentialResolver{"mock-r3-m12": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r3-m12")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassNotSent}, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (NotSent): %v", err)
	}
	revokeVerification(t, pool, f)

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}

	// First T2 re-claim attempt: KYC denies, attempt escalates.
	if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("reclaimPayoutCreated (first deny): %v", err)
	}
	var afterFirst PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		afterFirst, err = GetAttemptByID(ctx, tx, attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if afterFirst.EscalatedAt == nil {
		t.Fatalf("setup: expected the first deny to escalate the attempt")
	}

	// Second (and third) T2 re-claim: still denied, already escalated -
	// must be a quiet, idempotent reschedule, never an error.
	for i := 0; i < 2; i++ {
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return Touch(ctx, tx, attempt.ID)
		}); err != nil {
			t.Fatalf("touch: %v", err)
		}
		if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, afterFirst); err != nil {
			t.Fatalf("M12 regression: repeat KYC deny on an already-escalated attempt must not error, got: %v", err)
		}
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
