//go:build integration

// RV-PRH-I1 payout round 4 (narrow): the code re-review's own re-review 2
// found N7 (HIGH, a regression of H3/B2 - the R2 in-flight guard also
// blocked the sweeper's own crash-recovery path once a real, non-zero
// Sweeper.Lease is configured) and confirmed N3 was reported fixed but
// wasn't (the /resolve staff audit still committed in a separate
// transaction after PollPayoutStatus). Both are fixed in payout.go; this
// file adds the tests the review required. Runs on the same private
// scratch-DB harness as the other payout test files.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// TestSweeper_N7_CrashRecoveryWithRealLease_ActuallyRecovers is N7's
// required test: unlike every other payout sweeper test in this package
// (which build &Sweeper{} directly, leaving Lease at its zero value and so
// never actually exercising the sweeper's own fresh batch lease against
// the in-flight guard), this one uses NewSweeper's real, non-zero
// SweeperDefaultLease - exactly the shape that reproduced N7 (claimed=1,
// processed=0, erroring "payout dispatch is still in flight" every tick,
// no recovery at all).
func TestSweeper_N7_CrashRecoveryWithRealLease_ActuallyRecovers(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r4-n7", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4-n7": provider}, MultiWebhookCredentialResolver{"mock-r4-n7": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r4-n7-crash")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	// Simulate a crash: phase B/C never ran. Expire BOTH the T1p dispatch
	// lease and next_action_at, exactly like the H3/B2 crash-recovery
	// scenario - a process crash or an unhandled panic between the T1p
	// commit and phase B/C.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET lease_until = now() - interval '1 hour', next_action_at = now() - interval '1 hour' WHERE id = $1`, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	// NewSweeper, NOT &Sweeper{} - a REAL, non-zero Lease
	// (SweeperDefaultLease, 60s) is exactly the shape N7 needed to
	// reproduce: claimBatch sets a fresh, FUTURE lease_until on this row
	// as part of claiming it, and the OLD in-flight guard could not tell
	// that fresh sweeper-owned lease apart from a genuinely live dispatch
	// lease.
	sweeper := NewSweeper(pool, orch, nil, MockCredentialResolver{})
	sweeper.PayoutKYCGate = KYCEnforcementPayoutGate{}

	st := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if st.Claimed != 1 {
		t.Fatalf("N7 regression: expected the sweeper to claim the crashed attempt, got claimed=%d", st.Claimed)
	}
	if len(st.Errors) != 0 {
		t.Fatalf("N7 regression: expected zero sweep errors (a real Lease must not make the sweeper refuse its own claim as \"in flight\"), got: %v", st.Errors)
	}
	if st.Processed != 1 {
		t.Fatalf("N7 regression: expected the sweeper to actually PROCESS the crashed attempt (not just claim it), got processed=%d", st.Processed)
	}

	var recovered PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recovered, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if recovered.State != AttemptAmbiguous {
		t.Fatalf("N7 regression: expected the crashed, lease-expired attempt to actually be recovered onto ambiguous (T6), got %s", recovered.State)
	}
	if !recovered.EverPossiblySent {
		t.Fatalf("N7 regression: expected ever_possibly_sent to be set once T6 fires")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestSweeper_N7_LiveDispatchLease_StillRefusesResolve proves the OTHER
// half of the N7 fix did not overcorrect: a genuinely live DISPATCH lease
// (lease_owner="payout-dispatch", set by ClaimForDispatch itself, never
// touched by the sweeper) must still refuse /resolve's own direct
// PollPayoutStatus call with ErrPayoutDispatchInFlight - only a
// lease_owner="sweeper" lease is exempt.
func TestSweeper_N7_LiveDispatchLease_StillRefusesResolve(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r4-n7b", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4-n7b": provider}, MultiWebhookCredentialResolver{"mock-r4-n7b": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r4-n7b-live")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if claim.Attempt.LeaseOwner == nil || *claim.Attempt.LeaseOwner != "payout-dispatch" {
		t.Fatalf("setup: expected T1p's own dispatch lease_owner, got %v", claim.Attempt.LeaseOwner)
	}

	err = PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, claim.Attempt, time.Now().Add(30*time.Second), nil)
	if err != ErrPayoutDispatchInFlight {
		t.Fatalf("expected ErrPayoutDispatchInFlight for a live payout-dispatch lease, got %v", err)
	}
}

// --- N3: the /resolve staff audit commits in the SAME transaction --------

// TestPollPayoutStatus_N3_StaffAuditSameTransactionAsStateChange pins N3
// (reported fixed in round 3, confirmed NOT fixed by the code re-review):
// the withdrawal.resolve_attempted.http audit row for a staff-initiated
// PollPayoutStatus call must exist and reference the real actor, having
// been written in the SAME transaction as the resulting state change -
// this test asserts the row exists after a real state-changing call
// (ambiguous, via the no-reference fallback), not merely that the request
// as a whole "succeeded".
func TestPollPayoutStatus_N3_StaffAuditSameTransactionAsStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r4-n3", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4-n3": provider}, MultiWebhookCredentialResolver{"mock-r4-n3": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r4-n3")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	// Expire the lease so PollPayoutStatus (called as /resolve would call
	// it - no reference, submitting, lease_owner="payout-dispatch" but
	// expired) actually performs a state change (T6) rather than refusing.
	// Re-read the attempt afterward: /resolve's own handler always reads a
	// fresh copy before calling PollPayoutStatus, and the check is against
	// the IN-MEMORY struct's LeaseUntil, not a live DB read inside this
	// function - the stale claim.Attempt (captured before the update)
	// would still show the ORIGINAL, still-live lease.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET lease_until = now() - interval '1 hour' WHERE id = $1`, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt after expiring lease: %v", err)
	}

	actor := &SubmitActor{StaffID: uuid.New(), IPAddress: "203.0.113.9", UserAgent: "resolve-test-agent", RequestID: "req-n3-test"}
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, attempt, time.Now().Add(30*time.Second), actor); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}

	var got PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != AttemptAmbiguous {
		t.Fatalf("setup: expected the no-reference fallback to move to ambiguous, got %s", got.State)
	}

	var count int
	var actorID uuid.UUID
	var outcome string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`, wr.ID.String()).Scan(&count)
	}); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count != 1 {
		t.Fatalf("N3 regression: expected exactly 1 withdrawal.resolve_attempted.http audit row, got %d", count)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_id, outcome FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`, wr.ID.String()).Scan(&actorID, &outcome)
	}); err != nil {
		t.Fatalf("query audit_log row: %v", err)
	}
	if actorID != actor.StaffID {
		t.Fatalf("N3 regression: expected the audit row's actor to be the real staff actor %s, got %s", actor.StaffID, actorID)
	}
	if outcome != "success" {
		t.Fatalf("N3 regression: expected outcome=success, got %q", outcome)
	}
}

// TestSweeper_N3_NoActor_NoAuditRow proves the sweeper's own calls (actor
// == nil) never write a staff audit row - the audit is attribution for a
// STAFF action, and the sweeper is not staff.
func TestSweeper_N3_NoActor_NoAuditRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r4-n3b", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4-n3b": provider}, MultiWebhookCredentialResolver{"mock-r4-n3b": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r4-n3b")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET lease_until = now() - interval '1 hour', next_action_at = now() - interval '1 hour' WHERE id = $1`, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	sweeper := NewSweeper(pool, orch, nil, MockCredentialResolver{})
	sweeper.PayoutKYCGate = KYCEnforcementPayoutGate{}
	st := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(st.Errors) != 0 {
		t.Fatalf("unexpected sweep errors: %v", st.Errors)
	}

	var count int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`, wr.ID.String()).Scan(&count)
	}); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected NO withdrawal.resolve_attempted.http audit row from a sweeper-driven (actor=nil) call, got %d", count)
	}
}

// TestPollPayoutStatus_N3_FullEvidencePath_StaffAuditSameTransaction covers
// the OTHER N3 call site: applyPayoutStatusEvidence's own top-of-tx audit
// (as opposed to PollPayoutStatus's no-reference fallback branch, which
// TestPollPayoutStatus_N3_StaffAuditSameTransactionAsStateChange already
// covers) - a staff-initiated /resolve call that goes all the way through
// a REAL QueryStatus response (not the no-ref shortcut) must still write
// its audit row in the same transaction as the resulting state change.
func TestPollPayoutStatus_N3_FullEvidencePath_StaffAuditSameTransaction(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-r4-n3c", "EUR")
	provider := &scriptedWithdrawProvider{
		MockProvider: inner,
		withdraws:    []WithdrawResult{{Outcome: OutcomePending, ProviderReference: "n3c-ref"}},
		statuses:     []StatusResult{{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}},
	}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4-n3c": provider}, MultiWebhookCredentialResolver{"mock-r4-n3c": NewMockWebhookCredentials(inner)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r4-n3c")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var pending PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pending, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if pending.State != AttemptPending {
		t.Fatalf("setup: expected pending, got %s", pending.State)
	}

	actor := &SubmitActor{StaffID: uuid.New(), IPAddress: "203.0.113.10", UserAgent: "resolve-test-agent-2", RequestID: "req-n3c-test"}
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, pending, time.Now().Add(30*time.Second), actor); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}

	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		after, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread final: %v", err)
	}
	if after.State != AttemptSucceeded {
		t.Fatalf("setup: expected the full QueryStatus path to settle succeeded, got %s", after.State)
	}

	var count int
	var actorID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`, wr.ID.String()).Scan(&count)
	}); err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count != 1 {
		t.Fatalf("N3 regression: expected exactly 1 withdrawal.resolve_attempted.http audit row via the full evidence path, got %d", count)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_id FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`, wr.ID.String()).Scan(&actorID)
	}); err != nil {
		t.Fatalf("query audit_log row: %v", err)
	}
	if actorID != actor.StaffID {
		t.Fatalf("expected the real staff actor %s, got %s", actor.StaffID, actorID)
	}
}

// --- LOW: R1's early return on stray non-definite evidence against an ---
// --- already-terminal attempt --------------------------------------------

// TestPayoutDispatch_R1_StrayEvidenceAgainstTerminalAttempt_IsNoOp pins the
// "already terminal" half of payoutHandleContradiction's R1 branch (the
// other half - a non-terminal attempt racing a faster piece of evidence -
// is already covered by the round-3 R1 tests): a stray non-definite result
// arriving for an attempt that has ALREADY reached a terminal state must
// be a pure no-op, not an error and not a second dispute/audit record.
func TestPayoutDispatch_R1_StrayEvidenceAgainstTerminalAttempt_IsNoOp(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := &syncSuccessWithdrawProvider{MockProvider: NewMockProvider("mock-r4-r1-term", "EUR"), ref: "r4-r1-term-ref"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-r4-r1-term": provider}, MultiWebhookCredentialResolver{"mock-r4-r1-term": NewMockWebhookCredentials(provider.MockProvider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r4-r1-term")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, orch.PayoutOptions()...); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var succeeded PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		succeeded, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if succeeded.State != AttemptSucceeded {
		t.Fatalf("setup: expected succeeded, got %s", succeeded.State)
	}

	// A stray, non-definite (Pending) result now arrives for the SAME,
	// already-terminal (succeeded) attempt - e.g. a duplicate/delayed
	// callback replay. This must be a complete no-op: no error, no state
	// change, no dispute.
	stray := GateResult[WithdrawResult]{Class: ErrorClassPending, Value: WithdrawResult{Outcome: OutcomePending, ProviderReference: "stray-late-pending-ref"}}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, stray, EvidenceSync, orch.PayoutOptions()...); err != nil {
		t.Fatalf("ApplyPayoutResult (stray, should no-op): %v", err)
	}

	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		after, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread final: %v", err)
	}
	if after.State != AttemptSucceeded {
		t.Fatalf("R1 regression: expected the already-terminal attempt to stay succeeded (stray evidence is a no-op), got %s", after.State)
	}
	if after.TerminalReason != nil {
		t.Fatalf("R1 regression: expected no terminal_reason change (no dispute) from stray non-definite evidence, got %v", after.TerminalReason)
	}
	var disputeAuditCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payments.payout_late_contradicting_evidence' AND target_id = $1`, claim.Attempt.ID.String()).Scan(&disputeAuditCount)
	}); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if disputeAuditCount != 0 {
		t.Fatalf("R1 regression: expected NO late-contradicting-evidence audit record for stray non-definite evidence against an already-terminal attempt, got %d", disputeAuditCount)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- N6: the fallback (withdrawal-level) reference must ALSO be compared -

// TestPollPayoutStatus_N6_MismatchAgainstFallbackWithdrawalReference_Disputes
// proves N6's own extension: when the ATTEMPT has no stored reference at
// all (only the withdrawal does - the N1/R6 fallback shape), a QueryStatus
// success echoing a DIFFERENT reference from the withdrawal's own must
// STILL dispute, not silently settle - the original N6 fix only compared
// against attempt.ProviderReference, which is nil in exactly this shape.
func TestPollPayoutStatus_N6_MismatchAgainstFallbackWithdrawalReference_Disputes(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-r4-n6", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)

	wr := approvedWithdrawal(t, pool, f, 500, "payout-r4-n6")

	// Move to `submitted` first (AttachProviderReference's own
	// precondition), then attach a reference to the WITHDRAWAL ONLY
	// (simulating an attempt that reached ambiguous before the N1/R6
	// attempt-level persistence fix, or any other path that left the
	// attempt's own reference NULL).
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := withdrawal.MarkSubmittedPending(ctx, tx, wr.ID, "mock-r4-n6"); err != nil {
			return err
		}
		return withdrawal.AttachProviderReference(ctx, tx, wr.ID, "n6-fallback-withdrawal-ref")
	}); err != nil {
		t.Fatalf("attach withdrawal-level reference: %v", err)
	}
	// Withdrawal must be `submitted` for AttachProviderReference to
	// succeed and for PollPayoutStatus's own flow to apply - drive a
	// normal claim now, on the SAME (now-submitted) withdrawal, is not
	// possible (ClaimForDispatch requires `approved`). Instead, construct
	// the attempt directly (via the same InsertSubmittingAttempt T1p uses)
	// in the shape PollPayoutStatus expects: no attempt-level reference,
	// submitting, expired lease.
	attemptID := uuid.New()
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: attemptID, TenantID: f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &wr.ID, ProviderID: "mock-r4-n6",
			PaymentMethod: "bank_transfer", AssetCode: "EUR", Amount: 500,
			Interactive: false, ClaimToken: uuid.New(), LeaseOwner: "payout-dispatch",
			LeaseUntil: time.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		return snapshotInTx(ctx, tx, f, wr, attemptID)
	}); err != nil {
		t.Fatalf("construct attempt directly: %v", err)
	}
	if attempt.ProviderReference != nil {
		t.Fatalf("setup: expected the attempt's own reference to be nil (the fallback shape), got %v", attempt.ProviderReference)
	}

	scripted := &scriptedWithdrawProvider{
		MockProvider: provider,
		statuses:     []StatusResult{{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", ProviderReference: "n6-different-echoed-ref"}},
	}
	registerCapability(t, pool, f.orchFixture, scripted, 100)
	orch2 := NewOrchestrator(map[string]PaymentProvider{"mock-r4-n6": scripted}, MultiWebhookCredentialResolver{"mock-r4-n6": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	if err := PollPayoutStatus(context.Background(), pool, orch2, MockCredentialResolver{}, f.tenantID, attempt, time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}

	var after PaymentAttempt
	var gotWr withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		after, err = GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		gotWr, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if after.State != AttemptDisputed || after.TerminalReason == nil || *after.TerminalReason != "provider_reference_mismatch" {
		t.Fatalf("N6 regression: expected a provider_reference_mismatch dispute even when comparing against the FALLBACK withdrawal reference, got state=%s reason=%v", after.State, after.TerminalReason)
	}
	if gotWr.State == withdrawal.StateCompleted {
		t.Fatalf("N6 regression: must never complete against a mismatched fallback reference")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
