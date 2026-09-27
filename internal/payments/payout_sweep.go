// PRH-I1 payout dispatch, sweeper extension (ADR 0095 §7, §5; ADR 0095
// §4.3 T2/T6/T12; RV-PRH-I1 code review + ledger-finance review fix
// round): T2 re-claim of a `created` payout attempt (reachable only via
// ApplyPayoutResult's NotSent branch - the call provably never reached the
// provider), T12 resubmission of an `ambiguous` payout attempt (gated on
// the provider's manifest and a resend cap - C1/B1, see
// resubmitPayoutAmbiguous's own doc comment), and QueryStatus-driven
// resolution of a submitting/pending/ambiguous payout attempt whose
// lease/poll window is due (delegated to payments.PollPayoutStatus, shared
// with internal/httpserver's /resolve handler - H4/B7). Every claim before
// a Withdraw call - T1p (payout.go), T2 and T12 (this file) - re-runs the
// ADR 0096 payout KYC gate; see evaluatePayoutGate's own doc comment for
// why this is ONE shared code path.
//
// A KYC deny at T2/T12 is NOT the same as a T1p deny: the withdrawal
// request is already `submitted` (not `approved`), so
// withdrawal.DenyForCompliance - legal ONLY from `approved` - cannot and
// must not be called. Denying here Escalates the attempt (T16, no state
// change) instead: no resend, no hold release. The ONLY way out of that
// escalated state is a staff action proving the attempt was NEVER possibly
// sent (M3 - RejectCreated on a 'created' row with ever_possibly_sent
// still false) - a KYC outcome, allow or deny, never itself releases a
// hold once dispatch may have happened (ADR 0095 §5).
//
// M5 (RV-PRH-I1 ledger-finance review): the INV-IO-15 kill switch
// (migration 0105, another agent's work - killswitch.go/attempt.go's own
// CAS predicates, never edited here) is now checked at every claim/resend
// point via checkPayoutKillSwitch: T2 (reclaimPayoutCreated) and T12
// (resubmitPayoutAmbiguous) check it explicitly before the claim/resend
// (so a block reschedules rather than looking like an ordinary CAS
// conflict); T1p (payout.go's ClaimForDispatch) relies on
// InsertSubmittingAttempt's own INSERT ... SELECT ... WHERE predicate and
// surfaces ErrPayoutKillSwitchEngaged distinctly. Every path fails closed:
// no Withdraw call, no state change (the hold/hold-adjacent state is left
// exactly where it was), and the identical claim/resend is safe to retry
// once the switch is released - there is no separate "unblock" action.
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// payoutResubmitLease mirrors payoutAttemptClaimLease for a T2/T12 re-claim
// (a fresh lease/claim token, distinct from the original T1p one).
const payoutResubmitLease = 2 * time.Minute

// maxResubmits returns s.MaxResubmits if positive, else the package
// default payoutMaxResubmits (C1/B1).
func (s *Sweeper) maxResubmits() int {
	if s.MaxResubmits > 0 {
		return s.MaxResubmits
	}
	return payoutMaxResubmits
}

// processPayoutAttempt dispatches one leased payout payment_attempts row
// (attempt.Operation == AttemptOperationPayout) per its current state. A
// nil s.PayoutKYCGate means payout sweeping is not wired on this deployment
// - the row is left exactly as leased (reconsidered next tick), mirroring
// processAttempt's own "out of this step's scope" no-op for a row this
// sweeper is not configured to drive.
func (s *Sweeper) processPayoutAttempt(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	if s.PayoutKYCGate == nil || attempt.WithdrawalRequestID == nil {
		return nil
	}
	switch attempt.State {
	case AttemptCreated:
		return s.reclaimPayoutCreated(ctx, tenantID, attempt)
	case AttemptAmbiguous:
		return s.resubmitPayoutAmbiguous(ctx, tenantID, attempt)
	case AttemptSubmitting, AttemptPending:
		return s.resolvePayoutViaQueryStatus(ctx, tenantID, attempt)
	default:
		return nil
	}
}

// gateAndEscalateOnDeny re-runs the payout KYC gate for the withdrawal
// request behind attempt, and on DENY escalates the attempt (T16) with NO
// resend and NO hold release - returning allowed=false so the caller stops
// before ever touching a provider. This is the ONE shared re-check point
// T2 and T12 both use (item 8's "every claim runs the gate", extended to
// the re-claim paths as item 5 requires).
//
// L3/B8 (RV-PRH-I1 code review): a repeat KYC deny on an ALREADY-escalated
// attempt is an idempotent no-op (a plain reschedule of the next look),
// never a second Escalate call - Escalate's own CAS (`escalated_at IS
// NULL`) would otherwise fail every lease period once escalated, forcing
// the caller to treat "still denied, still escalated" as an ERROR.
func (s *Sweeper) gateAndEscalateOnDeny(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest, attempt PaymentAttempt, nextActionAt time.Time) (allowed bool, err error) {
	// Kill-switch check is the CALLER's job (checkPayoutKillSwitch, run
	// BEFORE this function - see reclaimPayoutCreated/resubmitPayoutAmbiguous),
	// deliberately not duplicated here: unlike a KYC deny, a kill-switch
	// block is transient and must reschedule, not Escalate.
	decision, _, err := evaluatePayoutGate(ctx, tx, s.PayoutKYCGate, wr)
	if err != nil {
		return false, err
	}
	if decision.Allowed {
		return true, nil
	}
	if attempt.EscalatedAt != nil {
		if err := RescheduleNonTerminal(ctx, tx, attempt.ID, nextActionAt); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := Escalate(ctx, tx, attempt.ID, nextActionAt); err != nil {
		return false, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: wr.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_reclaim_denied_by_kyc",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{"withdrawal_request_id": wr.ID.String(), "reason_code": decision.Code, "outcome": string(decision.Outcome)},
	}); err != nil {
		return false, err
	}
	return false, nil
}

// checkPayoutKillSwitch is T2/T12's shared pre-check (the kill-switch
// migration, 0105, is another agent's work - this file only reacts to an
// already-engaged switch, never engages/releases one). Called BEFORE
// attempting a re-claim/resend, inside the SAME transaction that will
// perform it, so the decision is made against a fresh read. Distinct from
// gateAndEscalateOnDeny's KYC-deny handling: a kill switch is a TRANSIENT,
// operator-controlled pause, not a permanent compliance denial - blocking
// here reschedules the attempt (a plain backoff, never Escalate/T16), so
// the exact same claim/resend is retried automatically, with no special
// "release" action required beyond the switch itself being released
// (idempotent: the CAS predicate re-evaluates it fresh every tick).
func checkPayoutKillSwitch(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, attempt PaymentAttempt, nextActionAt time.Time) (blocked bool, err error) {
	engaged, err := KillSwitchEngaged(ctx, tx, tenantID, providerID, AttemptOperationPayout)
	if err != nil {
		return false, err
	}
	if !engaged {
		return false, nil
	}
	if err := RescheduleNonTerminal(ctx, tx, attempt.ID, nextActionAt); err != nil {
		return false, err
	}
	return true, nil
}

// escalateAmbiguousPayout is resubmitPayoutAmbiguous's non-idempotent-or-
// exhausted path (C1/B1): T16, no resend, no hold release. Idempotent if
// already escalated (L3/B8, same rationale as gateAndEscalateOnDeny).
func (s *Sweeper) escalateAmbiguousPayout(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt, nextPoll time.Time, reason string) error {
	return s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		if attempt.EscalatedAt != nil {
			return RescheduleNonTerminal(actx, tx, attempt.ID, nextPoll)
		}
		if err := Escalate(actx, tx, attempt.ID, nextPoll); err != nil {
			return err
		}
		wrID := ""
		if attempt.WithdrawalRequestID != nil {
			wrID = attempt.WithdrawalRequestID.String()
		}
		return audit.Record(actx, tx, audit.Entry{
			TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_resend_escalated",
			TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
			Metadata: map[string]any{"withdrawal_request_id": wrID, "reason": reason, "submit_count": attempt.SubmitCount},
		})
	})
}

// reclaimPayoutCreated performs the T2 re-claim of a payout attempt that
// was returned to `created` by ApplyPayoutResult's NotSent branch (the
// only route into this state for a payout - there is no cascade sibling
// insert for payouts, per payout.go's own scope note). On ALLOW, it
// re-claims (ClaimCreatedForSubmission, the SAME generic T2 transition
// deposits use) with a fresh claim token/lease, commits, then drives
// phase B/C exactly like the original T1p dispatch. T2 needs no
// IdempotentSubmission/max_resubmits gate: a `created` payout attempt is
// guaranteed NEVER to have been sent (INV-IO-9's own CHECK constraint), so
// resending it can never double-pay - unlike T12 (resubmitPayoutAmbiguous),
// which resends an attempt that MAY already have been sent.
func (s *Sweeper) reclaimPayoutCreated(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	if attempt.ProviderID == nil {
		// Structurally unreachable for a payout (T1p always sets
		// provider_id before ever reaching 'submitting', and NotSent never
		// clears it) - fail closed rather than re-claim with no provider.
		return fmt.Errorf("payments: reclaim payout attempt %s: no provider_id recorded", attempt.ID)
	}
	nextPoll := s.backoff(attempt.PollCount)
	var claimed PaymentAttempt
	var allowed bool
	err := s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		wr, err := lockSubmittedRequest(actx, tx, *attempt.WithdrawalRequestID)
		if err != nil {
			return err
		}
		// Kill switch (migration 0105): checked BEFORE the KYC gate/claim,
		// fail closed, transient - a plain reschedule, never Escalate.
		blocked, err := checkPayoutKillSwitch(actx, tx, tenantID, *attempt.ProviderID, attempt, nextPoll)
		if err != nil || blocked {
			return err
		}
		allowed, err = s.gateAndEscalateOnDeny(actx, tx, wr, attempt, nextPoll)
		if err != nil || !allowed {
			return err
		}
		claimToken := uuid.New()
		if err := ClaimCreatedForSubmission(actx, tx, attempt.ID, *attempt.ProviderID, claimToken, "sweeper-payout-reclaim", time.Now().Add(payoutResubmitLease)); err != nil {
			return err
		}
		claimed, err = GetAttemptByID(actx, tx, attempt.ID)
		return err
	})
	if err != nil || !allowed {
		return err
	}
	return s.dispatchPayoutAttempt(ctx, tenantID, claimed)
}

// resubmitPayoutAmbiguous performs T12 (ambiguous -> submitting) for a
// payout - C1/B1 (RV-PRH-I1 CRITICAL finding, ledger-finance veto): unlike
// the earlier revision, which resent unconditionally, this now follows
// ADR 0095 §4.3 T12/§16.1 item 2 exactly:
//
//  1. ALWAYS poll first via QueryStatus (payments.PollPayoutStatus) -
//     the ADR's default convergence path, and the only way a NON-
//     idempotent provider's ambiguous send can ever resolve without a
//     resend. N1/R6 (RV-PRH-I1 re-review): PollPayoutStatus itself now
//     resolves the effective reference to query (the attempt's own, or -
//     defence in depth - the withdrawal's), so this no longer needs its
//     own `attempt.ProviderReference != nil` gate to decide whether
//     polling is possible; an attempt with genuinely no reference
//     anywhere is a cheap no-op reschedule inside PollPayoutStatus, not a
//     wasted resend. If the poll resolves the attempt (succeeded,
//     declined, or moved to pending), this function returns without ever
//     considering a resend.
//  2. Only if the attempt is STILL `ambiguous` after that poll does this
//     function consider resending - and ONLY when the routed provider's
//     manifest declares IdempotentSubmission=true AND submit_count is
//     below the configured cap (s.maxResubmits()). ResubmitAmbiguous's own
//     CAS repeats the cap independently (defense in depth).
//  3. A non-idempotent provider, or a cap already reached, is escalated
//     (T16) - never resent, no matter how many sweeper ticks pass.
func (s *Sweeper) resubmitPayoutAmbiguous(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	nextPoll := s.backoff(attempt.PollCount)

	if attempt.ProviderID != nil {
		if err := PollPayoutStatus(ctx, s.Pool, s.Orchestrator, s.CredResolver, tenantID, attempt, nextPoll); err != nil {
			return err
		}
		refreshed, err := getAttemptInTenant(ctx, s.Pool, tenantID, attempt.ID)
		if err != nil {
			return err
		}
		if refreshed.State != AttemptAmbiguous {
			// Resolved by the poll (succeeded, declined, or moved to
			// pending) - nothing left for T12 to do.
			return nil
		}
		attempt = refreshed
	}

	if attempt.ProviderID == nil {
		return fmt.Errorf("payments: resubmit payout attempt %s: no provider_id recorded", attempt.ID)
	}
	provider, ok := s.Orchestrator.Provider(*attempt.ProviderID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSweeperProviderNotRegistered, *attempt.ProviderID)
	}
	manifest := provider.Capabilities().Manifest
	if !manifest.IdempotentSubmission {
		return s.escalateAmbiguousPayout(ctx, tenantID, attempt, nextPoll, "non_idempotent_manifest")
	}
	if attempt.SubmitCount >= s.maxResubmits() {
		return s.escalateAmbiguousPayout(ctx, tenantID, attempt, nextPoll, "max_resubmits_exhausted")
	}

	var claimed PaymentAttempt
	var allowed bool
	err := s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		wr, err := lockSubmittedRequest(actx, tx, *attempt.WithdrawalRequestID)
		if err != nil {
			return err
		}
		// Kill switch (migration 0105): checked BEFORE the KYC gate/resend,
		// fail closed, transient - a plain reschedule, never Escalate (a
		// kill-switch pause must not permanently park the payout the way
		// exhausting max_resubmits or a non-idempotent manifest does).
		blocked, err := checkPayoutKillSwitch(actx, tx, tenantID, *attempt.ProviderID, attempt, nextPoll)
		if err != nil || blocked {
			return err
		}
		allowed, err = s.gateAndEscalateOnDeny(actx, tx, wr, attempt, nextPoll)
		if err != nil || !allowed {
			return err
		}
		claimToken := uuid.New()
		if err := ResubmitAmbiguous(actx, tx, attempt.ID, claimToken, "sweeper-payout-resubmit", time.Now().Add(payoutResubmitLease), s.maxResubmits()); err != nil {
			return err
		}
		claimed, err = GetAttemptByID(actx, tx, attempt.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrAttemptStateConflict) {
			// The CAS-level max_resubmits/legacy_backfill/sibling guard
			// refused (defense in depth caught what the Go-level check
			// above should already have excluded, or a concurrent
			// resolution won the race) - escalate rather than surface a
			// bare, repeating conflict error.
			return s.escalateAmbiguousPayout(ctx, tenantID, attempt, nextPoll, "resubmit_cas_refused")
		}
		return err
	}
	if !allowed {
		return nil
	}
	return s.dispatchPayoutAttempt(ctx, tenantID, claimed)
}

// dispatchPayoutAttempt runs phase B/C for an already-committed
// `submitting` payout attempt (from a fresh T1p claim, a T2 re-claim, or a
// T12 resubmission) - the shared tail every payout claim path converges
// on. H1 (RV-PRH-I1 ledger-finance review, CRITICAL): evidence is
// EvidenceSync, never EvidenceSweeper - the outcome comes from a REAL
// synchronous Withdraw call regardless of what triggered it, and the 0101
// guard trigger only accepts (sync, callback, query_status) on a
// ->succeeded or a payout's ->declined transition. Passing EvidenceSweeper
// here made every sweeper-driven definite outcome roll back with the hold
// still held (decline) or the payout unsettled in the ledger (success).
func (s *Sweeper) dispatchPayoutAttempt(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	if attempt.ProviderID == nil {
		return fmt.Errorf("payments: dispatch payout attempt %s: no provider_id", attempt.ID)
	}
	provider, ok := s.Orchestrator.Provider(*attempt.ProviderID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSweeperProviderNotRegistered, *attempt.ProviderID)
	}
	gr := DispatchWithdraw(ctx, s.CredResolver, provider, attempt)
	return ApplyPayoutResult(ctx, s.Pool, tenantID, *attempt.WithdrawalRequestID, attempt, gr, EvidenceSync)
}

// resolvePayoutViaQueryStatus resolves a submitting(lease-expired)/pending
// payout attempt via payments.PollPayoutStatus (H4/B7: the SAME phase-B/
// phase-C function internal/httpserver's /resolve handler now uses, so the
// sweeper and staff-triggered resolution can never diverge on evidence
// mapping or the amount/asset cross-check).
func (s *Sweeper) resolvePayoutViaQueryStatus(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	nextPoll := s.backoff(attempt.PollCount)
	return PollPayoutStatus(ctx, s.Pool, s.Orchestrator, s.CredResolver, tenantID, attempt, nextPoll)
}

// lockSubmittedRequest locks and returns a WithdrawalRequest in `submitted`
// state - a thin, intentional pass-through to
// withdrawal.LockSubmittedForResolution (kept as its own function only so
// every T2/T12 call site in this file reads identically; no behavior is
// added here).
func lockSubmittedRequest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (withdrawal.WithdrawalRequest, error) {
	return withdrawal.LockSubmittedForResolution(ctx, tx, requestID)
}
