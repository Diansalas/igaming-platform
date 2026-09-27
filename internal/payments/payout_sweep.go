// PRH-I1 payout dispatch, sweeper extension (ADR 0095 §7, §5; this task's
// item 5): T2 re-claim of a `created` payout attempt (reachable only via
// ApplyPayoutResult's NotSent branch - the call provably never reached the
// provider), T12 resubmission of an `ambiguous` payout attempt, and
// QueryStatus-driven resolution of a submitting/pending/ambiguous payout
// attempt whose lease/poll window is due. Every claim before a Withdraw
// call - T1p (payout.go), T2 and T12 (this file) - re-runs the ADR 0096
// payout KYC gate; see evaluatePayoutGate's own doc comment for why this
// is ONE shared code path.
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
package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// payoutResubmitLease mirrors payoutAttemptClaimLease for a T2/T12 re-claim
// (a fresh lease/claim token, distinct from the original T1p one).
const payoutResubmitLease = 2 * time.Minute

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
// request behind attempt (locked FOR UPDATE by the caller's own
// withdrawal.LockSubmittedForResolution-equivalent read below), and on
// DENY escalates the attempt (T16) with NO resend and NO hold release -
// returning allowed=false so the caller stops before ever touching a
// provider. This is the ONE shared re-check point T2 and T12 both use
// (task item 8's "every claim runs the gate", extended to the re-claim
// paths as item 5 requires).
func (s *Sweeper) gateAndEscalateOnDeny(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest, attempt PaymentAttempt, nextActionAt time.Time) (allowed bool, err error) {
	decision, _, err := evaluatePayoutGate(ctx, tx, s.PayoutKYCGate, wr)
	if err != nil {
		return false, err
	}
	if decision.Allowed {
		return true, nil
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

// reclaimPayoutCreated performs the T2 re-claim of a payout attempt that
// was returned to `created` by ApplyPayoutResult's NotSent branch (the
// only route into this state for a payout - there is no cascade sibling
// insert for payouts, per payout.go's own scope note). On ALLOW, it
// re-claims (ClaimCreatedForSubmission, the SAME generic T2 transition
// deposits use) with a fresh claim token/lease, commits, then drives
// phase B/C exactly like the original T1p dispatch.
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
// payout, gated identically to reclaimPayoutCreated. Unlike a deposit's
// T12 (not built by this sweeper step per its own package doc comment),
// this is the payout-specific resend the ADR 0095 §5 payout state machine
// requires for an attempt whose outcome never resolved via QueryStatus.
func (s *Sweeper) resubmitPayoutAmbiguous(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	nextPoll := s.backoff(attempt.PollCount)
	var claimed PaymentAttempt
	var allowed bool
	err := s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		wr, err := lockSubmittedRequest(actx, tx, *attempt.WithdrawalRequestID)
		if err != nil {
			return err
		}
		allowed, err = s.gateAndEscalateOnDeny(actx, tx, wr, attempt, nextPoll)
		if err != nil || !allowed {
			return err
		}
		claimToken := uuid.New()
		if err := ResubmitAmbiguous(actx, tx, attempt.ID, claimToken, "sweeper-payout-resubmit", time.Now().Add(payoutResubmitLease)); err != nil {
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

// dispatchPayoutAttempt runs phase B/C for an already-committed
// `submitting` payout attempt (from a fresh T1p claim, a T2 re-claim, or a
// T12 resubmission) - the shared tail every payout claim path converges on.
func (s *Sweeper) dispatchPayoutAttempt(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	if attempt.ProviderID == nil {
		return fmt.Errorf("payments: dispatch payout attempt %s: no provider_id", attempt.ID)
	}
	provider, ok := s.Orchestrator.Provider(*attempt.ProviderID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSweeperProviderNotRegistered, *attempt.ProviderID)
	}
	gr := DispatchWithdraw(ctx, s.CredResolver, provider, attempt)
	return ApplyPayoutResult(ctx, s.Pool, tenantID, *attempt.WithdrawalRequestID, attempt, gr, EvidenceSweeper)
}

// resolvePayoutViaQueryStatus resolves a submitting(lease-expired)/pending
// payout attempt through QueryStatus, converting the read-only StatusResult
// into the same WithdrawResult-shaped evidence ApplyPayoutResult already
// knows how to apply - including providerref.Validate on the echoed
// reference (task item 3's "QueryStatus inputs" requirement) and an
// amount/asset cross-check against the withdrawal request identical to
// internal/httpserver's own resolve handler, so an automated sweep can
// never silently complete/fail a payout against a provider-reported amount
// that disagrees with what was requested.
func (s *Sweeper) resolvePayoutViaQueryStatus(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt) error {
	if attempt.ProviderID == nil || attempt.ProviderReference == nil {
		return s.Pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
			return RescheduleNonTerminal(actx, tx, attempt.ID, s.backoff(attempt.PollCount))
		})
	}
	provider, ok := s.Orchestrator.Provider(*attempt.ProviderID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSweeperProviderNotRegistered, *attempt.ProviderID)
	}
	manifest := provider.Capabilities().Manifest
	in := callProviderInput{
		TenantID: tenantID, ProviderID: *attempt.ProviderID,
		AttemptState: attempt.State, ClaimToken: uuid.Nil, ExpectedClaim: uuid.Nil, ReadOnly: true,
		Domain: "payments", Manifest: manifest,
	}
	gr := callProvider(ctx, s.CredResolver, in, func(callCtx context.Context, cc CallContext) (WithdrawResult, ErrorClass, error) {
		status, err := provider.QueryStatus(callCtx, *attempt.ProviderReference)
		if err != nil {
			return WithdrawResult{}, ErrorClassAmbiguous, err
		}
		if status.ProviderReference != "" {
			if verr := providerref.ValidateOptional("query_status.provider_reference", status.ProviderReference); verr != nil {
				return WithdrawResult{}, ErrorClassProviderRefInvalid, verr
			}
		}
		res := WithdrawResult{Outcome: status.Outcome, ProviderReference: status.ProviderReference, DeclineReason: status.DeclineReason, Cascadable: status.Cascadable}
		switch status.Outcome {
		case OutcomePending:
			return res, ErrorClassPending, nil
		case OutcomeSucceeded:
			return res, ErrorClassSucceeded, nil
		case OutcomeDeclined:
			return res, ErrorClassDefiniteDecline, nil
		default:
			return res, ErrorClassAmbiguous, nil
		}
	})
	return ApplyPayoutResult(ctx, s.Pool, tenantID, *attempt.WithdrawalRequestID, attempt, gr, EvidenceQueryStatus)
}

// lockSubmittedRequest locks and returns a WithdrawalRequest in `submitted`
// state, mirroring withdrawal.LockSubmittedForResolution exactly (that
// function is not reused directly because it lives in a package this file
// already imports as `withdrawal`, but is kept private/re-implemented-free
// here by calling it - no duplication).
func lockSubmittedRequest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (withdrawal.WithdrawalRequest, error) {
	return withdrawal.LockSubmittedForResolution(ctx, tx, requestID)
}
