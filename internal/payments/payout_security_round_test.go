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
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
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
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-sm2-a": provider}, MultiWebhookCredentialResolver{"mock-sm2-a": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

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
		if err != nil {
			return err
		}
		return snapshotInTx(ctx, tx, f, wr, attemptID)
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
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-sm2-b": scripted}, MultiWebhookCredentialResolver{"mock-sm2-b": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

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
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-sm2a": idem}, MultiWebhookCredentialResolver{"mock-sm2a": NewMockWebhookCredentials(inner)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "payout-sm2a")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, idem, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
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

// --- S-L2/SP-C: the sweeper-batch lease_owner constant, and the V1 fix ----

// TestClaimBatch_SL2_SPC_StaleSnapshotNeverRelabelsALiveSubmittingLease is
// the permanent SP-C reproduction (FH-6 ledger-finance ruling,
// rv-prh-i1-payout-ledger.md, restoring the probe an earlier round removed
// when its first, over-broad fix regressed 7 other tests and was
// reverted): claimBatch's V1 predicate -
//
//	AND NOT (state = 'submitting' AND lease_until > now()
//	         AND lease_owner IS DISTINCT FROM 'sweeper-batch')
//
// - restricted to state='submitting', closes SP-C without touching any
// non-submitting row's ordinary cross-tick continuation. Reproduction,
// exactly as ledger-finance's own probe:
//  1. An ambiguous payout with a reference (a real ClaimForDispatch +
//     DispatchWithdraw + ApplyPayoutResult round trip, MockAmountAmbiguous).
//  2. A T12 claim commits for real (ResubmitAmbiguous itself, not a
//     hand-rolled UPDATE): state->submitting, lease_owner=
//     "sweeper-payout-resubmit", lease_until 2 minutes out - ResubmitAmbiguous
//     itself sets next_action_at = lease_until, so nothing is stale yet.
//  3. /resolve's own stale-snapshot reschedule is simulated directly (the
//     narrow, deterministic way to reproduce a race that would otherwise
//     need real concurrency): next_action_at is pulled back to now(),
//     leaving state/lease_owner/lease_until exactly as T12 left them - the
//     exact artifact a PollPayoutStatus call working from an in-memory
//     read taken BEFORE the T12 commit would produce.
//  4. claimBatch must NOT claim this row: it must stay `submitting`, still
//     owned by "sweeper-payout-resubmit", with its original lease_until -
//     the resend's own phase B/C is still nominally in flight.
func TestClaimBatch_SL2_SPC_StaleSnapshotNeverRelabelsALiveSubmittingLease(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-spc", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-spc": provider}, MultiWebhookCredentialResolver{"mock-spc": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "payout-spc")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	ambiguous := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
	if ambiguous.State != AttemptAmbiguous {
		t.Fatalf("setup: expected ambiguous, got %s", ambiguous.State)
	}

	liveLeaseUntil := time.Now().Add(2 * time.Minute)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Step 2: a REAL T12 claim commit.
		if err := ResubmitAmbiguous(ctx, tx, ambiguous.ID, uuid.New(), "sweeper-payout-resubmit", liveLeaseUntil, 0); err != nil {
			return err
		}
		// Step 3: the stale-snapshot reschedule artifact - next_action_at
		// pulled back to now(), lease left exactly as T12 committed it.
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET next_action_at = now() WHERE id = $1`, ambiguous.ID)
		return err
	}); err != nil {
		t.Fatalf("simulate T12 claim + stale reschedule: %v", err)
	}

	resubmitted := mustGetAttempt(t, pool, f.tenantID, ambiguous.ID)
	if resubmitted.State != AttemptSubmitting {
		t.Fatalf("setup: expected T12 to move the attempt to submitting, got %s", resubmitted.State)
	}

	sweeper := &Sweeper{Pool: pool, Lease: SweeperDefaultLease, BatchPerTenant: SweeperDefaultBatchPerTenant}
	claimed, err := sweeper.claimBatch(context.Background(), f.tenantID)
	if err != nil {
		t.Fatalf("claimBatch: %v", err)
	}
	if a7ContainsUUID(claimed, ambiguous.ID) {
		t.Fatalf("SP-C regression: claimBatch claimed a `submitting` row under a LIVE non-batch lease (lease_until=%v)", liveLeaseUntil)
	}

	after := mustGetAttempt(t, pool, f.tenantID, ambiguous.ID)
	if after.State != AttemptSubmitting {
		t.Fatalf("SP-C regression: expected the attempt to remain submitting, got %s", after.State)
	}
	if after.LeaseOwner == nil || *after.LeaseOwner != "sweeper-payout-resubmit" {
		t.Fatalf("SP-C regression: expected lease_owner to remain \"sweeper-payout-resubmit\", got %v", after.LeaseOwner)
	}
	if after.LeaseUntil == nil || after.LeaseUntil.Sub(liveLeaseUntil).Abs() > time.Millisecond {
		t.Fatalf("SP-C regression: expected lease_until to remain unchanged (%v), got %v", liveLeaseUntil, after.LeaseUntil)
	}
}

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

// TestPayoutResolveAudit_CodeReview_WithdrawalStateAfterAndEvidenceClass
// pins the two S-M2 audit fields the code review found unpinned (mutants
// SURVIVED): `withdrawal_state_after` hard-coded/dropped, and
// `evidence_class` blanked on the QueryStatus path. Drives a real
// resolve-to-completion round trip and asserts both fields concretely.
func TestPayoutResolveAudit_CodeReview_WithdrawalStateAfterAndEvidenceClass(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-cr-audit", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-cr-audit": provider}, MultiWebhookCredentialResolver{"mock-cr-audit": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-cr-audit")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	pending := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
	if pending.State != AttemptPending || pending.ProviderReference == nil {
		t.Fatalf("setup: expected a real pending dispatch with a reference, got state=%s ref=%v", pending.State, pending.ProviderReference)
	}

	provider.Resolve(*pending.ProviderReference, OutcomeSucceeded, "", false)
	actor := &SubmitActor{StaffID: uuid.New(), IPAddress: "203.0.113.30", UserAgent: "resolve-test-agent", RequestID: "req-cr-audit"}
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, pending, time.Now().Add(30*time.Second), actor); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}

	final := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
	if final.State != AttemptSucceeded {
		t.Fatalf("setup: expected the attempt to succeed, got %s", final.State)
	}
	gotWR := mustGetWithdrawal(t, pool, f.tenantID, wr.ID)
	if gotWR.State != withdrawal.StateCompleted {
		t.Fatalf("setup: expected the withdrawal to complete, got %s", gotWR.State)
	}

	var wrStateAfter, evidenceClass string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'withdrawal_state_after', metadata->>'evidence_class'
			 FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`,
			wr.ID.String()).Scan(&wrStateAfter, &evidenceClass)
	}); err != nil {
		t.Fatalf("query audit_log metadata: %v", err)
	}
	if wrStateAfter != string(withdrawal.StateCompleted) {
		t.Fatalf("code review regression: expected withdrawal_state_after=completed, got %q", wrStateAfter)
	}
	if evidenceClass != string(ErrorClassSucceeded) {
		t.Fatalf("code review regression: expected evidence_class=succeeded, got %q", evidenceClass)
	}
}

// --- P-C3 (Low, FH-6 ledger-finance ruling): Escalate on a terminal -------
// --- attempt; payoutResolveAudit's locked "before" and honest no-op ------

// TestEscalate_PC3_RefusesOnATerminalAttempt_StaleSnapshot pins the CAS
// predicate ledger-finance found "correct, but untested" - a mutant
// removing `AND state NOT IN (...)` survived the whole payout/sweeper/A7
// suite. Simulates the exact scenario the predicate exists for: a caller
// holding a stale, pre-transition snapshot (escalated_at still NULL in its
// own copy) calls Escalate AFTER the attempt has already reached a
// terminal state for real - Escalate must refuse (ErrAttemptStateConflict),
// never write next_action_at on a terminal row (payment_attempts_check9's
// own invariant) and never touch escalated_at.
func TestEscalate_PC3_RefusesOnATerminalAttempt_StaleSnapshot(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-pc3-escalate", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-pc3-escalate": provider}, MultiWebhookCredentialResolver{"mock-pc3-escalate": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-pc3-escalate")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	// The attempt's OWN stale, pre-transition snapshot (never re-read after
	// this) still shows escalated_at=nil, state=submitting - exactly like a
	// caller who read the row before a concurrent success landed.
	staleSnapshot := claim.Attempt
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	pending := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
	if pending.State != AttemptPending || pending.ProviderReference == nil {
		t.Fatalf("setup: expected a real pending dispatch with a reference, got state=%s ref=%v", pending.State, pending.ProviderReference)
	}
	// Converge to a REAL terminal success via QueryStatus (the mock never
	// synchronously succeeds a Withdraw call, per its own doc comment).
	provider.Resolve(*pending.ProviderReference, OutcomeSucceeded, "", false)
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, pending, time.Now().Add(time.Minute), nil); err != nil {
		t.Fatalf("PollPayoutStatus (converge to success): %v", err)
	}
	succeeded := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
	if succeeded.State != AttemptSucceeded {
		t.Fatalf("setup: expected a real terminal success, got %s", succeeded.State)
	}
	if staleSnapshot.State == AttemptSucceeded {
		t.Fatalf("setup: the stale snapshot must NOT already show the terminal state")
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Escalate(ctx, tx, staleSnapshot.ID, time.Now().Add(time.Minute))
	})
	if !errors.Is(err, ErrAttemptStateConflict) {
		t.Fatalf("P-C3 regression: expected Escalate to refuse on an already-terminal attempt with ErrAttemptStateConflict, got %v", err)
	}

	after := mustGetAttempt(t, pool, f.tenantID, staleSnapshot.ID)
	if after.State != AttemptSucceeded {
		t.Fatalf("P-C3 regression: expected the attempt to remain succeeded, got %s", after.State)
	}
	if after.EscalatedAt != nil {
		t.Fatalf("P-C3 regression: expected escalated_at to remain NULL on a refused Escalate, got %v", *after.EscalatedAt)
	}
	if after.NextActionAt != nil {
		t.Fatalf("P-C3 regression: expected next_action_at to remain NULL (terminal), got %v", *after.NextActionAt)
	}
}

// TestPayoutResolveAudit_PC3_BeforeStateReadUnderTheLock pins the other
// half of P-C3: payoutResolveAudit's "before" attempt/withdrawal state
// must come from a read taken under the withdrawal lock at the START of
// the SAME transaction that then performs the state change - not the
// caller's own outer, pre-transaction snapshot, which can already be
// stale by the time the transaction actually starts. A real, concurrent
// transition (T11, pending->ambiguous) lands in the gap between the
// caller's own stale read and applyPayoutStatusEvidence's own transaction:
// the caller's own switch logic still (deliberately, unchanged by this
// fix) decides on the STALE state, but the CAS it reaches
// (RescheduleNonTerminal, whose predicate is state-agnostic) still
// succeeds against the REAL row - and the audit's own "before" must show
// the REAL state (ambiguous), never the caller's stale one (pending).
func TestPayoutResolveAudit_PC3_BeforeStateReadUnderTheLock(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-pc3-audit", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-pc3-audit": provider}, MultiWebhookCredentialResolver{"mock-pc3-audit": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 500, "payout-pc3-audit")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	// The caller's own stale snapshot: a real, pending dispatch.
	staleSnapshot := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
	if staleSnapshot.State != AttemptPending {
		t.Fatalf("setup: expected a real pending dispatch, got %s", staleSnapshot.State)
	}

	// A REAL concurrent transition (T11) lands BEFORE the call below's own
	// transaction starts - the caller never sees it.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAmbiguousFromPending(ctx, tx, staleSnapshot.ID, EvidenceQueryStatus, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("simulate concurrent T11: %v", err)
	}
	afterConcurrentTransition := mustGetAttempt(t, pool, f.tenantID, staleSnapshot.ID)
	if afterConcurrentTransition.State != AttemptAmbiguous {
		t.Fatalf("setup: expected the concurrent transition to move the attempt to ambiguous, got %s", afterConcurrentTransition.State)
	}

	// applyPayoutStatusEvidence is called with the STALE snapshot (still
	// `pending`) and a "still pending" QueryStatus result - its own switch
	// decides on the stale `pending` case (RescheduleNonTerminal, a
	// state-agnostic CAS), which still succeeds against the now-`ambiguous`
	// row without error.
	actor := &SubmitActor{StaffID: uuid.New(), IPAddress: "203.0.113.20", UserAgent: "resolve-test-agent", RequestID: "req-pc3-audit"}
	gr2 := GateResult[StatusResult]{Value: StatusResult{ProviderReference: *staleSnapshot.ProviderReference, Outcome: OutcomePending}, Class: ErrorClassPending}
	if err := applyPayoutStatusEvidence(context.Background(), pool, f.tenantID, wr.ID, staleSnapshot, gr2, EvidenceQueryStatus, time.Now().Add(30*time.Second), actor, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("applyPayoutStatusEvidence: %v", err)
	}

	final := mustGetAttempt(t, pool, f.tenantID, staleSnapshot.ID)
	if final.State != AttemptAmbiguous {
		t.Fatalf("expected the attempt to remain ambiguous (RescheduleNonTerminal never changes state), got %s", final.State)
	}

	var before, after, noOp string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'attempt_state_before', metadata->>'attempt_state_after', metadata->>'no_op'
			 FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`,
			wr.ID.String()).Scan(&before, &after, &noOp)
	}); err != nil {
		t.Fatalf("query audit_log metadata: %v", err)
	}
	if before != string(AttemptAmbiguous) {
		t.Fatalf("P-C3 regression: expected attempt_state_before=ambiguous (read under the lock, matching the REAL pre-transaction state), got %q (the caller's stale snapshot said %q)", before, staleSnapshot.State)
	}
	if after != string(AttemptAmbiguous) {
		t.Fatalf("P-C3 regression: expected attempt_state_after=ambiguous (RescheduleNonTerminal is a genuine no-op), got %q", after)
	}
	if noOp != "true" {
		t.Fatalf("P-C3 regression: expected metadata no_op=true for a before==after resolve, got %q", noOp)
	}
}

// TestEscalateAmbiguousPayout_CodeReview_UnintendedConflictIsLoud pins the
// code review's own hardening (rv-prh-i1-payout-code-review.md, e48c8e7):
// escalateAmbiguousPayout must swallow Escalate's ErrAttemptStateConflict
// ONLY when a re-read confirms one of the two legitimate reasons (the
// attempt is already terminal, or already escalated) - never for any
// OTHER reason a 0-row CAS can occur (e.g. the row is simply gone). A
// nonexistent attempt ID reproduces exactly that unintended case
// deterministically: Escalate's own CAS matches 0 rows (conflict), and the
// re-read must surface a real, loud error (never a silent nil that would
// otherwise re-lease and skip this row forever).
func TestEscalateAmbiguousPayout_CodeReview_UnintendedConflictIsLoud(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)

	sweeper := &Sweeper{Pool: pool}
	ghost := PaymentAttempt{ID: uuid.New(), TenantID: f.tenantID}
	err := sweeper.escalateAmbiguousPayout(context.Background(), f.tenantID, ghost, time.Now().Add(time.Minute), "test-ghost")
	if err == nil {
		t.Fatalf("code review regression: expected a loud error for an unintended (nonexistent-row) conflict, got nil")
	}
	if !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("expected ErrAttemptNotFound from the re-read, got %v", err)
	}
}
