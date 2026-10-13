//go:build integration

package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// Ledger-finance condition C1 (ADR 0116 s5.1): at the payout CLAIM and at the sweeper's T2 / T12 re-checks an AML hold-EVALUATOR
// FAILURE is the existing retryable `unavailable` outcome (the staff-approved request stays approved / the attempt is rescheduled),
// never a terminal rejection and never the sticky T16 escalation. A DEFINITE hold still denies (TestAMLHold_PayoutClaim_Held_*).

func failingHoldProbe(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (bool, error) {
	return false, errors.New("injected evaluator fault")
}

func TestAMLHold_PayoutClaim_EvaluatorFailure_IsRetryableNotTerminal(t *testing.T) {
	pool := testPool(t)
	amlProofs(t)
	f, orch, provider := fpOrch(t, pool, "mock-aml-evalfail-claim")
	wr := approvedWithdrawal(t, pool, f, 2_000, "aml-claim-evalfail")
	cashBefore := cashBalance(t, pool, f.orchFixture)

	restore := aml.SetHoldProbeForTest(failingHoldProbe)
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	restore()
	if !errors.Is(err, ErrPayoutKYCUnavailable) {
		t.Fatalf("an evaluator failure at the claim must be the retryable unavailable error, got claim=%+v err=%v", claim, err)
	}
	if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateApproved {
		t.Fatalf("the staff-approved request must stay approved, got %s", got.State)
	}
	if after := cashBalance(t, pool, f.orchFixture); after != cashBefore {
		t.Fatalf("no funds moved: %d -> %d", cashBefore, after)
	}
	if countAttempts(t, pool, f.tenantID, wr.ID) != 0 || provider.AttemptCount() != 0 {
		t.Fatal("no attempt and no provider call")
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "unavailable"); n != 1 {
		t.Fatalf("exactly one unavailable decision row, got %d", n)
	}
	// Retry once the evaluator is healthy: the claim goes through.
	retry, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil || retry.Denied {
		t.Fatalf("retry after the fault must claim cleanly: %+v %v", retry, err)
	}
}

func TestAMLHold_SweeperT2_EvaluatorFailure_ReschedulesNeverEscalates(t *testing.T) {
	pool := testPool(t)
	amlProofs(t)
	f, orch, _ := fpOrch(t, pool, "mock-aml-evalfail-t2")
	wr, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, "aml-t2-evalfail")
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}

	restore := aml.SetHoldProbeForTest(failingHoldProbe)
	err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt)
	restore()
	if err != nil {
		t.Fatalf("an evaluator failure must reschedule, not error: %v", err)
	}
	after := fpAttempt(t, pool, f, attempt.ID)
	if after.State != AttemptCreated || after.EscalatedAt != nil || after.SubmitCount != attempt.SubmitCount {
		t.Fatalf("created, un-escalated, no resend expected; got state=%s escalated=%v submit_count=%d", after.State, after.EscalatedAt, after.SubmitCount)
	}
	if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("the request is not terminal: %s", got.State)
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "unavailable"); n != 1 {
		t.Fatalf("one unavailable decision row, got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_reclaim_denied_by_kyc'`, f.tenantID); n != 0 {
		t.Fatalf("an evaluator fault must not write the deny audit, got %d", n)
	}
	// A DEFINITE hold at the same re-check still escalates (T16): the conversion is for the evaluator fault only.
	healthy := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
	if err := healthy.reclaimPayoutCreated(context.Background(), f.tenantID, after); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rec := fpAttempt(t, pool, f, attempt.ID); rec.State == AttemptCreated {
		t.Fatal("the attempt must progress once the evaluator is healthy")
	}
}

func TestAMLHold_SweeperT2_DefiniteHold_StillEscalates(t *testing.T) {
	pool := testPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-aml-hold-t2")
	_, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, "aml-t2-hold")
	holdAllFor(t, pool, f.orchFixture, aml.ScopeWithdrawalAndPayout)
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
	if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("a definite hold escalates, it does not error: %v", err)
	}
	if after := fpAttempt(t, pool, f, attempt.ID); after.EscalatedAt == nil {
		t.Fatalf("a definite hold at T2 escalates (T16), got state=%s", after.State)
	}
}

func TestAMLHold_SweeperT12_EvaluatorFailure_ReschedulesNeverEscalates(t *testing.T) {
	pool := testPool(t)
	amlProofs(t)
	f, orch, mp := fpOrch(t, pool, "mock-aml-evalfail-t12")
	manifest := mp.Capabilities().Manifest
	manifest.IdempotentSubmission = true
	mp.SetManifest(manifest)
	wr := approvedWithdrawal(t, pool, f, 500, "aml-t12-evalfail")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassAmbiguous}, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult (Ambiguous): %v", err)
	}
	attempt := fpAttempt(t, pool, f, claim.Attempt.ID)
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}

	restore := aml.SetHoldProbeForTest(failingHoldProbe)
	err = sweeper.resubmitPayoutAmbiguous(context.Background(), f.tenantID, attempt)
	restore()
	if err != nil {
		t.Fatalf("an evaluator failure must reschedule, not error: %v", err)
	}
	after := fpAttempt(t, pool, f, attempt.ID)
	if after.State != AttemptAmbiguous || after.EscalatedAt != nil || after.SubmitCount != attempt.SubmitCount {
		t.Fatalf("ambiguous, un-escalated, no resend expected; got state=%s escalated=%v submit_count=%d", after.State, after.EscalatedAt, after.SubmitCount)
	}
	if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("the request is not terminal: %s", got.State)
	}
}
