// PRH-I1 payout dispatch (ADR 0095 §4.5-§4.8, §5, §14; ADR 0082 A7; ADR
// 0096 §15-§17 payout KYC gate; security review
// docs/plans/payment-readiness/prh-ref-provider-reference-bound.md §10 C1).
//
// This file replaces the old in-tx `provider.Withdraw` call
// (internal/httpserver/withdrawal_handlers.go's newSubmitWithdrawalHandler,
// pre-cutover) with the ADR 0095 three-phase pattern already used for
// deposits (drive.go): T1p (claim, one short tx, commit BEFORE any
// provider call), phase B (the outbound call, no tx), phase C (apply
// evidence, its own short tx). The double-payout hazard the security
// review named (F-POOL-2 / prh-ref-provider-reference-bound.md §9.1 item
// 1 / §10 C1) is exactly the "provider I/O inside the transaction" defect
// this file removes for the payout leg.
//
// Scope note: this step does NOT implement payout cascade-on-decline
// (mirroring the pre-cutover handler's own documented scope boundary -
// "no cascade-on-decline across a chain of providers" - payment-
// orchestration.md's cascade section is written for deposits; extending it
// to payouts is a separate, not-yet-authorized decision). A definite
// decline fails the withdrawal outright (Fail); it does not insert a
// sibling attempt against a different provider.
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// payoutAttemptClaimLease/payoutPhaseCTimeout mirror deposit_v2.go's
// depositAttemptClaimLease and the ADR-0095-elsewhere
// context.WithTimeout(context.WithoutCancel(ctx), <bound>) pattern
// (internal/casino/orchestrator.go's phaseCTimeout use, docs/decisions/
// 0095 §"phase C"). Phase C for a payout MUST still commit even if the
// original request's context is cancelled (e.g. an HTTP client
// disconnecting mid-call) once the call may have reached the provider -
// losing that commit would leave a payout that was possibly sent with no
// attempt/withdrawal state reflecting it at all.
const (
	payoutAttemptClaimLease = 2 * time.Minute
	payoutPhaseCTimeout     = 5 * time.Second
	payoutNextPollInterval  = 30 * time.Second
)

// ErrorClassProviderRefInvalid is a documented, minimal extension to ADR
// 0095 §8's outcome set (security review prh-ref-provider-reference-
// bound.md §10 C1 / this task's item 3): a provider-returned reference
// that fails providerref.Validate. It is never returned by a deposit
// adapter call in this package - only payoutAdapterCall below produces it.
// Distinguishing it from ErrorClassAmbiguous is the whole point: an
// ambiguous outcome may still resolve via QueryStatus/resend later, but an
// invalid reference can never be looked up or resent safely (a payload
// that violates the platform-wide bound cannot be trusted to round-trip
// through any index or comparison), so phase C must PARK it instead
// (ApplyDisputeFromNonTerminal, terminal) - never leave it in a state a
// sweeper might retry.
const ErrorClassProviderRefInvalid ErrorClass = "provider_ref_invalid"

// PayoutKYCGate is the payments-side seam for ADR 0096 §8 item 3's
// payout-dispatch enforcement point (EnforcementWithdrawalPayout). Unlike
// DepositKYCGate, it returns the full kyc.EnforcementDecision plus the
// kyc.EnforcementParams used to obtain it, because a DENY must be handed
// to withdrawal.DenyForCompliance verbatim (that function re-validates
// Allowed/Operation itself - B6, rv-prh-i3-code-review.md - so this seam
// must never summarize the decision down to a bool before that call).
type PayoutKYCGate interface {
	EvaluatePayout(ctx context.Context, tx pgx.Tx, params kyc.EnforcementParams) (kyc.EnforcementDecision, error)
}

// KYCEnforcementPayoutGate is the real PayoutKYCGate, wired directly to
// ADR 0096's EvaluateEnforcement - a pure DB read, no vendor call, no lock
// beyond EvaluateEnforcement's own plain SELECTs (mirroring
// KYCEnforcementDepositGate's identical contract for deposits).
type KYCEnforcementPayoutGate struct{}

func (KYCEnforcementPayoutGate) EvaluatePayout(ctx context.Context, tx pgx.Tx, params kyc.EnforcementParams) (kyc.EnforcementDecision, error) {
	return kyc.EvaluateEnforcement(ctx, tx, params)
}

// payoutEnforcementParams builds the ADR 0096 EnforcementParams for a
// payout-dispatch check on wr, given personID (resolved from the
// withdrawing player account - WithdrawalRequest itself carries no
// person_id column, per withdrawal-state-machine.md §2). A fresh
// CorrelationID is minted per evaluation (never reused across T1p/T2/T12
// re-checks - each is its own decision instant).
func payoutEnforcementParams(wr withdrawal.WithdrawalRequest, personID uuid.UUID) kyc.EnforcementParams {
	return kyc.EnforcementParams{
		TenantID: wr.TenantID, BrandID: wr.BrandID, PlayerAccountID: wr.PlayerAccountID,
		PersonID: personID, Operation: kyc.EnforcementWithdrawalPayout,
		AssetCode: wr.AssetCode, Amount: wr.Amount, CorrelationID: uuid.New(),
	}
}

// evaluatePayoutGate loads the withdrawing player account's PersonID and
// runs the payout KYC gate. Shared by ClaimForDispatch (T1p), the sweeper's
// T2 re-claim and its T12 resubmission re-check - "every claim before
// Withdraw runs the KYC gate", per this task's own requirement (item 8),
// implemented as ONE shared code path rather than three copies that could
// silently drift apart.
func evaluatePayoutGate(ctx context.Context, tx pgx.Tx, kycGate PayoutKYCGate, wr withdrawal.WithdrawalRequest) (kyc.EnforcementDecision, kyc.EnforcementParams, error) {
	account, err := identity.GetPlayerAccountByID(ctx, tx, wr.PlayerAccountID)
	if err != nil {
		return kyc.EnforcementDecision{}, kyc.EnforcementParams{}, fmt.Errorf("payments: load player account for payout kyc gate: %w", err)
	}
	params := payoutEnforcementParams(wr, account.PersonID)
	decision, err := kycGate.EvaluatePayout(ctx, tx, params)
	if err != nil {
		return kyc.EnforcementDecision{}, kyc.EnforcementParams{}, fmt.Errorf("%w: %w", ErrPayoutKYCUnavailable, err)
	}
	return decision, params, nil
}

// ErrPayoutKYCUnavailable wraps a hard failure evaluating the payout KYC
// gate itself (a DB error, not a business denial) - fail-closed: the
// caller's transaction rolls back with no domain effect, exactly like
// internal/withdrawal.ErrKYCUnavailable's identical contract for the
// pending_review-time gate.
var ErrPayoutKYCUnavailable = errors.New("payments: payout kyc enforcement evaluation unavailable")

// ClaimResult is ClaimForDispatch's outcome.
type ClaimResult struct {
	// Denied is true when the payout KYC gate denied dispatch -
	// withdrawal.DenyForCompliance has already committed (approved ->
	// rejected, hold released) in the SAME transaction; Attempt/Capability
	// are zero and Request reflects the now-rejected row.
	Denied     bool
	Request    withdrawal.WithdrawalRequest
	Attempt    PaymentAttempt
	Capability ProviderCapability
}

// ClaimForDispatch performs ADR 0095's T1p: ONE short transaction that
// locks the withdrawal row (ADR 0082 canonical order: the L1 parent lock,
// via withdrawal.LockApprovedForSubmission, taken before any attempt-row
// work - the payout KYC gate itself takes no row lock of its own, so
// there is no L0.x-advisory-lock-before-L1 ordering hazard here; ADR 0082
// A7-N1 binds only when a gate DOES take such a lock, which
// kyc.EvaluateEnforcement documents that it never does), runs the ADR
// 0096 payout KYC gate, and on DENY commits withdrawal.DenyForCompliance
// with NO payment_attempts row ever inserted. On ALLOW, it transitions the
// withdrawal request approved -> submitted (withdrawal.MarkSubmittedPending,
// provider_reference left NULL per P95-C2 - the provider has not been
// called yet) and inserts the payout's payment_attempts row ∅ ->
// submitting (InsertSubmittingAttempt, T1p) IN THE SAME TRANSACTION, which
// commits BEFORE this function ever returns to a caller that might invoke
// a PaymentProvider. Routing itself (RankRoutingCandidates) runs OUTSIDE
// any transaction (ADR 0095 §9.6), exactly like driveCreatedAttempt's own
// A0 for deposits.
func (o *Orchestrator) ClaimForDispatch(ctx context.Context, pool *db.Pool, kycGate PayoutKYCGate, tenantID, requestID uuid.UUID, paymentMethod string) (ClaimResult, error) {
	// A0: route outside any tx.
	var routingInput withdrawal.WithdrawalRequest
	var candidates []ProviderCapability
	if err := pool.WithTenantReadOnly(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		var err error
		routingInput, err = withdrawal.GetByID(actx, tx, requestID)
		if err != nil {
			return err
		}
		candidates, err = ListRoutingCandidates(actx, tx, tenantID, routingInput.BrandID)
		return err
	}); err != nil {
		return ClaimResult{}, fmt.Errorf("payments: load routing candidates (payout claim): %w", err)
	}
	// The selected adapter instance itself is intentionally NOT retained
	// here - the caller resolves it fresh via Provider(capability.ProviderID)
	// for phase B, so nothing about routing crosses the A0/T1p boundary
	// except the plain data RoutingRequest/ProviderCapability already are.
	_, routedCapability, routeErr := RankRoutingCandidates(ctx, candidates, o.providers, o.breaker, RoutingRequest{
		TenantID: tenantID, BrandID: routingInput.BrandID, AssetCode: routingInput.AssetCode,
		PaymentMethod: paymentMethod, Amount: routingInput.Amount, Operation: OperationWithdrawal,
	})

	var result ClaimResult
	err := pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		wr, err := withdrawal.LockApprovedForSubmission(actx, tx, requestID)
		if err != nil {
			return err
		}

		decision, kycParams, err := evaluatePayoutGate(actx, tx, kycGate, wr)
		if err != nil {
			return err
		}
		if !decision.Allowed {
			denied, err := withdrawal.DenyForCompliance(actx, tx, requestID, decision, kycParams)
			if err != nil {
				return err
			}
			result = ClaimResult{Denied: true, Request: denied}
			return nil
		}

		if routeErr != nil {
			// No routable provider: fail closed with no state change (the
			// request stays `approved`, retriable once routing succeeds) -
			// never a partial claim.
			return fmt.Errorf("payments: route payout: %w", routeErr)
		}

		if err := withdrawal.MarkSubmittedPending(actx, tx, requestID, routedCapability.ProviderID); err != nil {
			return err
		}

		attemptID := uuid.New()
		claimToken := uuid.New()
		attempt, err := InsertSubmittingAttempt(actx, tx, NewSubmittingAttempt{
			ID: attemptID, TenantID: tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &requestID, ProviderID: routedCapability.ProviderID,
			PaymentMethod: paymentMethod, AssetCode: wr.AssetCode, Amount: wr.Amount,
			Interactive: false, ClaimToken: claimToken, LeaseOwner: "payout-dispatch",
			LeaseUntil: time.Now().Add(payoutAttemptClaimLease),
		})
		if err != nil {
			return err
		}

		refreshed, err := withdrawal.GetByID(actx, tx, requestID)
		if err != nil {
			return err
		}
		result = ClaimResult{Request: refreshed, Attempt: attempt, Capability: routedCapability}
		return nil
	})
	if err != nil {
		return ClaimResult{}, err
	}
	return result, nil
}

// payoutAdapterCall builds phase B's AdapterCall closure. Every provider-
// returned reference is validated with providerref.Validate BEFORE this
// function's own outcome switch ever runs (security review §10 C1 / this
// task's item 3) - a violation returns ErrorClassProviderRefInvalid
// unconditionally, regardless of what Outcome the adapter also reported,
// so phase C can never mistake a same-call "succeeded, but with a
// dangerous reference" for an ordinary success.
func payoutAdapterCall(provider PaymentProvider, attempt PaymentAttempt) AdapterCall[WithdrawResult] {
	return func(callCtx context.Context, cc CallContext) (WithdrawResult, ErrorClass, error) {
		res, err := provider.Withdraw(callCtx, WithdrawRequest{
			MerchantReference: attempt.MerchantReference, Amount: attempt.Amount,
			AssetCode: attempt.AssetCode, PaymentMethod: attempt.PaymentMethod,
		})
		if err != nil {
			return res, ErrorClassAmbiguous, err
		}
		if res.ProviderReference != "" {
			if verr := providerref.Validate("withdraw.provider_reference", res.ProviderReference); verr != nil {
				return res, ErrorClassProviderRefInvalid, verr
			}
		}
		switch res.Outcome {
		case OutcomePending:
			return res, ErrorClassPending, nil
		case OutcomeSucceeded:
			if res.ProviderReference == "" {
				// A definite success with no reference is not itself a
				// providerref violation (empty is a distinct, deliberate
				// "absent" case - ValidateOptional's own contract) but it
				// cannot be trusted as a synchronous success with nothing to
				// reconcile against later - treat conservatively as
				// Ambiguous, exactly like depositAdapterCall's identical
				// rule for a deposit's SyncSuccessPossible check.
				return res, ErrorClassAmbiguous, nil
			}
			return res, ErrorClassSucceeded, nil
		case OutcomeDeclined:
			return res, ErrorClassDefiniteDecline, nil
		default:
			return res, ErrorClassAmbiguous, nil
		}
	}
}

// DispatchWithdraw is phase B: the outbound Withdraw call, made with NO
// transaction held (INV-IO-1), through the same provider-call gate every
// other outbound call in this package uses. attempt MUST have been loaded
// AFTER ClaimForDispatch's own commit (state='submitting', a non-nil
// ClaimToken/ProviderID) - callProvider's own step 2 (INV-IO-2) refuses
// otherwise.
func DispatchWithdraw(ctx context.Context, credResolver OutboundCredentialResolver, provider PaymentProvider, attempt PaymentAttempt) GateResult[WithdrawResult] {
	if attempt.ProviderID == nil || attempt.ClaimToken == nil {
		return GateResult[WithdrawResult]{Class: ErrorClassNotSent,
			Err: fmt.Errorf("%w: payout attempt %s has no committed provider_id/claim_token", ErrProviderCallRefused, attempt.ID)}
	}
	manifest := provider.Capabilities().Manifest
	in := callProviderInput{
		TenantID: attempt.TenantID, ProviderID: *attempt.ProviderID,
		AttemptState: attempt.State, ClaimToken: *attempt.ClaimToken, ExpectedClaim: *attempt.ClaimToken,
		IdempotencyKey: attempt.ExternalIdempotencyKey, Domain: "payments", Manifest: manifest,
	}
	return callProvider(ctx, credResolver, in, payoutAdapterCall(provider, attempt))
}

// ApplyPayoutResult is phase C: applies DispatchWithdraw's GateResult under
// its own short transaction, on context.WithTimeout(context.WithoutCancel
// (ctx), payoutPhaseCTimeout) - once phase B may have reached the
// provider, this commit must survive the original request context being
// cancelled (an HTTP client disconnecting, a load-balancer deadline)
// exactly like internal/casino/orchestrator.go's identical phase-C bound.
//
// Every branch below either (a) posts nothing and returns the withdrawal
// to a re-claimable/re-pollable state (NotSent, Pending, Ambiguous), or
// (b) reaches a definite decline/success and calls withdrawal.Fail/
// Complete exactly once, under that function's own CAS guard - never a
// bare "failed" without a definite decline (task item 4), and never a
// resend of an attempt that may already have been sent (NotSent is the
// ONLY branch that returns to a re-claimable 'created' state, and it is
// reached only when the gate/adapter proves the call never reached the
// provider).
func ApplyPayoutResult(ctx context.Context, pool *db.Pool, tenantID, requestID uuid.UUID, attempt PaymentAttempt, gr GateResult[WithdrawResult], evidence EvidenceKind) error {
	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutPhaseCTimeout)
	defer cancel()

	return pool.WithTenant(phaseCtx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(actx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, requestID); err != nil {
			return fmt.Errorf("payments: lock withdrawal request (payout phase C): %w", err)
		}

		res := gr.Value
		nextPoll := time.Now().Add(payoutNextPollInterval)

		if gr.Class == ErrorClassProviderRefInvalid {
			reason := "invalid_provider_reference"
			if perr, ok := providerref.AsError(gr.Err); ok {
				reason = "invalid_provider_reference:" + string(perr.Reason)
			}
			if err := ApplyDisputeFromNonTerminal(actx, tx, attempt.ID, evidence, reason); err != nil {
				return err
			}
			return audit.Record(actx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "payments.payout_parked_invalid_reference",
				TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{"withdrawal_request_id": requestID.String(), "reason": reason},
			})
		}

		switch gr.Class {
		case ErrorClassNotSent:
			if attempt.ClaimToken == nil {
				return fmt.Errorf("payments: apply payout result: NotSent with no claim token on attempt %s", attempt.ID)
			}
			return MarkNotSent(actx, tx, attempt.ID, *attempt.ClaimToken, nextPoll)

		case ErrorClassPending:
			if err := MarkAccepted(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
				return err
			}
			if res.ProviderReference != "" {
				if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
					return err
				}
			}
			return nil

		case ErrorClassSucceeded:
			if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
				return err
			}
			providerID := ""
			if attempt.ProviderID != nil {
				providerID = *attempt.ProviderID
			}
			if err := withdrawal.Complete(actx, tx, requestID, providerID, res.ProviderReference); err != nil {
				return err
			}
			return ApplySuccess(actx, tx, attempt.ID, SuccessEvidence{Evidence: evidence, ProviderReference: res.ProviderReference})

		case ErrorClassDefiniteDecline:
			reason := res.DeclineReason
			if reason == "" {
				reason = "provider_declined"
			}
			var refPtr *string
			if res.ProviderReference != "" {
				if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
					return err
				}
				refPtr = &res.ProviderReference
			}
			if err := withdrawal.Fail(actx, tx, requestID, reason); err != nil {
				return err
			}
			cascadable := res.Cascadable
			return ApplyDecline(actx, tx, attempt.ID, DeclineEvidence{
				Evidence: evidence, Reason: reason, Stage: DeclineAtSubmission,
				Cascadable: &cascadable, ProviderRef: refPtr,
			})

		default: // ErrorClassAmbiguous and anything unclassified - never a
			// failure without a definite decline (task item 4).
			return MarkAmbiguousFromSubmitting(actx, tx, attempt.ID, evidence, nextPoll)
		}
	})
}
