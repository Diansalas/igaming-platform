//go:build integration

// PRH-I1 payout security round (rv-prh-i1-payout-security.md; registry
// PAY-SEC-S-M2, PAY-SEC-TESTS-1, PAY-SEC-S-L2). This file covers the
// payout-owner-scoped findings from that review:
//   - S-M2: the /resolve staff audit now records the actual outcome,
//     before/after attempt+withdrawal state and metadata (attempt_id,
//     evidence_class, terminal_reason when set) - previously every call
//     wrote the identical "success, no metadata" row.
//   - TESTS-1: kill SM2a (T12 kill-switch reschedule vs. escalate
//     classification untested) and SM7 (no unlinked/inactive-staff test on
//     /resolve); re-run SM11-SM14, invalidated by the credential-outage
//     incident, especially SM13 (submit accepting pending_review).
//   - S-L2: the sweeper-batch lease_owner and the stale-snapshot relabel
//     path (SP-C).
//
// Runs on the same private scratch-DB harness as the other payout test
// files.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// --- S-M2: /resolve audit records outcome, before/after state, metadata ---

// TestPayoutResolveAudit_SM2_RecordsBeforeAfterStateAndMetadata pins S-M2:
// the no-reference-fallback /resolve call (a real T6, submitting->ambiguous)
// must write an audit row naming attempt_id, evidence_class, and the
// attempt's own before/after state - not merely "success" with no detail.
func TestPayoutResolveAudit_SM2_RecordsBeforeAfterStateAndMetadata(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-sm2-a", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-sm2-a": provider}, MultiWebhookCredentialResolver{"mock-sm2-a": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-sm2-a")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
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
		t.Fatalf("reread attempt: %v", err)
	}
	if attempt.State != AttemptSubmitting {
		t.Fatalf("setup: expected submitting before the call, got %s", attempt.State)
	}

	actor := &SubmitActor{StaffID: uuid.New(), IPAddress: "203.0.113.10", UserAgent: "resolve-test-agent", RequestID: "req-sm2-a"}
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, attempt, time.Now().Add(30*time.Second), actor); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}

	var outcome, evidenceClass, before, after, wrBefore, wrAfter string
	var attemptID string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT outcome, metadata->>'evidence_class', metadata->>'attempt_state_before', metadata->>'attempt_state_after',
			        metadata->>'withdrawal_state_before', metadata->>'withdrawal_state_after', metadata->>'attempt_id'
			 FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`,
			wr.ID.String()).Scan(&outcome, &evidenceClass, &before, &after, &wrBefore, &wrAfter, &attemptID)
	}); err != nil {
		t.Fatalf("query audit_log metadata: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("S-M2: expected outcome=success for a plain state-change resolve, got %q", outcome)
	}
	if evidenceClass != "no_reference_reschedule" {
		t.Fatalf("S-M2: expected evidence_class=no_reference_reschedule, got %q", evidenceClass)
	}
	if before != string(AttemptSubmitting) {
		t.Fatalf("S-M2: expected attempt_state_before=submitting, got %q", before)
	}
	if after != string(AttemptAmbiguous) {
		t.Fatalf("S-M2: expected attempt_state_after=ambiguous, got %q", after)
	}
	if wrBefore != string(withdrawal.StateSubmitted) || wrAfter != string(withdrawal.StateSubmitted) {
		t.Fatalf("S-M2: expected withdrawal state submitted before and after a non-terminal resolve, got before=%q after=%q", wrBefore, wrAfter)
	}
	if attemptID != claim.Attempt.ID.String() {
		t.Fatalf("S-M2: expected metadata attempt_id=%s, got %q", claim.Attempt.ID, attemptID)
	}
}

// TestPayoutResolveAudit_SM2_DisputeRecordsFailureOutcomeAndTerminalReason
// pins the other half of S-M2: a resolve call that DISPUTES the attempt
// (a real anomaly, T10) must record Outcome=failure and the
// terminal_reason, distinctly from a plain no-op/success resolve.
func TestPayoutResolveAudit_SM2_DisputeRecordsFailureOutcomeAndTerminalReason(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-sm2-b", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)

	wr := approvedWithdrawal(t, pool, f, 500, "payout-sm2-b")
	attemptID := uuid.New()
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := withdrawal.MarkSubmittedPending(ctx, tx, wr.ID, "mock-sm2-b"); err != nil {
			return err
		}
		if err := withdrawal.AttachProviderReference(ctx, tx, wr.ID, "sm2-b-ref"); err != nil {
			return err
		}
		var err error
		attempt, err = InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: attemptID, TenantID: f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &wr.ID, ProviderID: "mock-sm2-b",
			PaymentMethod: "bank_transfer", AssetCode: "EUR", Amount: 500,
			Interactive: false, ClaimToken: uuid.New(), LeaseOwner: "payout-dispatch",
			LeaseUntil: time.Now().Add(-time.Hour),
		})
		return err
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A QueryStatus success echoing a MISMATCHED amount is the T10 dispute
	// path (INV-IO-6/B3-H2), independent of S-M2 - reused here purely to
	// drive a real dispute for the audit-metadata assertion.
	scripted := &scriptedWithdrawProvider{
		MockProvider: provider,
		statuses:     []StatusResult{{Outcome: OutcomeSucceeded, Amount: 999, AssetCode: "EUR", ProviderReference: "sm2-b-ref"}},
	}
	registerCapability(t, pool, f.orchFixture, scripted, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-sm2-b": scripted}, MultiWebhookCredentialResolver{"mock-sm2-b": NewMockWebhookCredentials(provider)})

	actor := &SubmitActor{StaffID: uuid.New(), IPAddress: "203.0.113.11", UserAgent: "resolve-test-agent", RequestID: "req-sm2-b"}
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, attempt, time.Now().Add(30*time.Second), actor); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}

	var got PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = GetAttemptByID(ctx, tx, attemptID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
	if got.State != AttemptDisputed {
		t.Fatalf("setup: expected the mismatch to dispute the attempt, got %s", got.State)
	}

	var outcome, terminalReason, after string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT outcome, metadata->>'terminal_reason', metadata->>'attempt_state_after'
			 FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`,
			wr.ID.String()).Scan(&outcome, &terminalReason, &after)
	}); err != nil {
		t.Fatalf("query audit_log metadata: %v", err)
	}
	if outcome != "failure" {
		t.Fatalf("S-M2: expected outcome=failure for a disputed resolve, got %q", outcome)
	}
	if after != string(AttemptDisputed) {
		t.Fatalf("S-M2: expected attempt_state_after=disputed, got %q", after)
	}
	if terminalReason == "" {
		t.Fatalf("S-M2: expected a non-empty terminal_reason in the audit metadata")
	}
}

// --- SM2a: T12 kill-switch reschedule, distinctly from escalate -----------

// TestResubmitPayoutAmbiguous_SM2a_KillSwitchReschedulesNeverEscalates kills
// SM2a: removing the T12 Go-level checkPayoutKillSwitch call still passes
// the payout suite, because the CAS predicate independently refuses the
// resend (fail-closed), but resubmitPayoutAmbiguous then ESCALATES the
// attempt (treating the refusal as "resubmit_cas_refused") instead of
// rescheduling it - permanently parking a payout that a transient,
// operator-controlled kill switch should only pause. This test engages
// the kill switch for the exact provider/operation in scope, calls T12
// directly, and asserts the attempt is a plain reschedule: still
// `ambiguous`, escalated_at still NULL, submit_count unchanged (no resend
// attempted), and next_action_at advanced.
func TestResubmitPayoutAmbiguous_SM2a_KillSwitchReschedulesNeverEscalates(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-sm2a", "EUR")
	spy := &withdrawCountingProvider{MockProvider: inner}
	idem := &idempotentAmbiguousProvider{withdrawCountingProvider: spy}
	registerCapability(t, pool, f.orchFixture, idem, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-sm2a": idem}, MultiWebhookCredentialResolver{"mock-sm2a": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "payout-sm2a")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, idem, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	before := spy.count()

	// EngageKillSwitch's own session-scoping trigger requires an active,
	// tenant-scoped staff principal (not just any RLS-visible uuid) -
	// insert one directly, exactly like migration_0105_integration_test.go's
	// own fixture does for the same reason.
	staffID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, $2, $3, 'x', 'tenant_admin', 'active')`,
			staffID, f.tenantID, "sm2a-"+staffID.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed staff principal: %v", err)
	}
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, "mock-sm2a", KillSwitchOperationPayout, "sm2a-test")
		return err
	}); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
	if attempt.State != AttemptAmbiguous {
		t.Fatalf("setup: expected ambiguous before T12, got %s", attempt.State)
	}
	beforeNextAction := attempt.NextActionAt

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
	if err := sweeper.resubmitPayoutAmbiguous(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("resubmitPayoutAmbiguous: %v", err)
	}

	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		after, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt after T12: %v", err)
	}

	if after.State != AttemptAmbiguous {
		t.Fatalf("SM2a: expected a kill-switch pause to be a plain reschedule (state stays ambiguous), got %s", after.State)
	}
	if after.EscalatedAt != nil {
		t.Fatalf("SM2a regression: a kill-switch pause must never Escalate (T16) - escalated_at is set (%v)", *after.EscalatedAt)
	}
	if spy.count() != before {
		t.Fatalf("SM2a: expected NO resend while the kill switch is engaged, got %d Withdraw calls (was %d)", spy.count(), before)
	}
	if after.SubmitCount != attempt.SubmitCount {
		t.Fatalf("SM2a: expected submit_count unchanged by a kill-switch pause, before=%d after=%d", attempt.SubmitCount, after.SubmitCount)
	}
	if after.NextActionAt == nil || (beforeNextAction != nil && !after.NextActionAt.After(*beforeNextAction)) {
		t.Fatalf("SM2a: expected next_action_at to advance (a real reschedule), before=%v after=%v", beforeNextAction, after.NextActionAt)
	}
}

// --- S-L2: the sweeper-batch lease_owner constant -------------------------
//
// NOTE: SP-C (the stale-snapshot relabel path) is NOT fixed by this round -
// see sweeper.go's claimBatch doc comment and
// docs/plans/payment-readiness/prh-i1-payout-launch-conditions.md for why
// the naive fix regressed a wide swath of the sweeper suite and was
// reverted. Only the distinct lease_owner constant below is implemented.

// TestClaimBatch_SL2_UsesDistinctBatchLeaseOwnerConstant pins the other
// half of S-L2: claimBatch must write the distinct SweeperBatchLeaseOwner
// ("sweeper-batch"), never the bare "sweeper" literal a genuine per-item
// sweeper-driven lease (e.g. drive.go's cascade claim) also happens to use.
func TestClaimBatch_SL2_UsesDistinctBatchLeaseOwnerConstant(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)

	intentID := insertRawDepositIntent(t, pool, f, "pending")
	attemptID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())

	sweeper := &Sweeper{Pool: pool, Lease: SweeperDefaultLease, BatchPerTenant: SweeperDefaultBatchPerTenant}
	claimed, err := sweeper.claimBatch(context.Background(), f.tenantID)
	if err != nil {
		t.Fatalf("claimBatch: %v", err)
	}
	if !a7ContainsUUID(claimed, attemptID) {
		t.Fatalf("expected claimBatch to claim the due attempt %s, got %v", attemptID, claimed)
	}

	after := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if after.LeaseOwner == nil || *after.LeaseOwner != SweeperBatchLeaseOwner {
		t.Fatalf("S-L2: expected lease_owner=%q, got %v", SweeperBatchLeaseOwner, after.LeaseOwner)
	}
	if SweeperBatchLeaseOwner == "sweeper" {
		t.Fatalf("S-L2 regression: SweeperBatchLeaseOwner must be distinct from the bare \"sweeper\" literal")
	}
}
