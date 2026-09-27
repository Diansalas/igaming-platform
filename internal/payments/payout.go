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
	// payoutOutboundCallBound (B1/security review item, RV-PRH-I1 code
	// review): every outbound provider call this file makes (Withdraw,
	// QueryStatus) is bounded by this ceiling on a context.WithoutCancel-
	// detached context, inside DispatchWithdraw/PollPayoutStatus
	// themselves - never left to a caller's own ctx (which, for the HTTP
	// submit/resolve handlers, is r.Context() and would abort the outbound
	// call the instant a staff browser disconnects, misclassifying a
	// perfectly good in-flight send as Ambiguous).
	payoutOutboundCallBound = 60 * time.Second
	// payoutMaxResubmits is ADR 0095 §16.1 item 2's T12 resend cap
	// (C1/B1): an ambiguous, IdempotentSubmission=true payout may be
	// resent at most this many times before T12 is refused and the
	// attempt escalates (T16) instead, no matter how many sweeper ticks
	// pass. Sweeper.MaxResubmits overrides this per-instance; see
	// Sweeper.maxResubmits().
	payoutMaxResubmits = 3
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

// SubmitActor is the staff identity/request context ClaimForDispatch must
// have to record the "which staff member triggered this payout" audit
// entry INSIDE the T1p (or W-KYC-deny) transaction (B6, RV-PRH-I1 code
// review) - Stage 3B security review P2-4's "the ONLY record anywhere
// that attributes the actual payout-triggering action to a human" can
// never be a best-effort, discardable, post-commit side effect.
type SubmitActor struct {
	StaffID   uuid.UUID
	IPAddress string
	UserAgent string
	RequestID string
}

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
// A0 for deposits. actor's staff-attribution audit entry
// ("withdrawal.submit.http") is written in the SAME transaction as
// whichever outcome (deny or allow) actually commits (B6).
//
// Lock ruling (RV-PRH-I1 ledger-finance review, "Ruling on the
// implementer's disclosure"): the outer LockApprovedForSubmission call
// below is NOT required for dispatch exclusivity (MarkSubmittedPending's
// own FOR UPDATE plus its `WHERE state='approved'` CAS, and the
// payment_attempts UNIQUE(tenant_id, withdrawal_request_id) constraint,
// already make two concurrent claims mutually exclusive on their own -
// see TestConcurrentClaimForDispatch_ExactlyOneWithdraws). It IS kept,
// per that ruling, for A7/LF95-C10(a) ordering: the KYC gate must read
// the request under L1 so a gate decision is never recorded against a
// row some other transaction is concurrently transitioning underneath it
// - see TestClaimForDispatch_GateRunsUnderOuterLock, which pins this by
// asserting a spy gate is not invoked until a concurrent holder of the
// same row lock releases it.
func (o *Orchestrator) ClaimForDispatch(ctx context.Context, pool *db.Pool, kycGate PayoutKYCGate, tenantID, requestID uuid.UUID, paymentMethod string, actor SubmitActor) (ClaimResult, error) {
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
			// B6: the staff-attribution audit commits in the SAME
			// transaction as the deny, with the correct (denied) outcome -
			// not the "success" outcome a generic post-commit audit call
			// would otherwise record regardless of what actually happened.
			if err := audit.Record(actx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor.StaffID,
				Action: "withdrawal.submit.http", TargetType: "withdrawal_request", TargetID: requestID.String(),
				Outcome: audit.OutcomeDenied, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
				Metadata: map[string]any{"denied_by_kyc": true, "reason_code": decision.Code, "outcome": string(decision.Outcome)},
			}); err != nil {
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

		// B6: staff-attribution audit, same transaction, before commit -
		// never a best-effort call after the fact.
		if err := audit.Record(actx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor.StaffID,
			Action: "withdrawal.submit.http", TargetType: "withdrawal_request", TargetID: requestID.String(),
			Outcome: audit.OutcomeSuccess, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
			Metadata: map[string]any{"provider_id": routedCapability.ProviderID, "attempt_id": attempt.ID.String()},
		}); err != nil {
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
	// B1 (RV-PRH-I1 code review): the provider call must never run on a
	// caller's own request-scoped context - an HTTP staff browser
	// disconnecting mid-call must not abort a possibly-already-executing
	// Withdraw and misclassify it. Detach from cancellation and apply this
	// package's own outbound ceiling; callProvider's own per-manifest
	// CallTimeout deadline still applies on top of (and within) this bound.
	dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutOutboundCallBound)
	defer cancel()
	manifest := provider.Capabilities().Manifest
	in := callProviderInput{
		TenantID: attempt.TenantID, ProviderID: *attempt.ProviderID,
		AttemptState: attempt.State, ClaimToken: *attempt.ClaimToken, ExpectedClaim: *attempt.ClaimToken,
		IdempotencyKey: attempt.ExternalIdempotencyKey, Domain: "payments", Manifest: manifest,
	}
	return callProvider(dispatchCtx, credResolver, in, payoutAdapterCall(provider, attempt))
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
	// L1 (RV-PRH-I1 ledger review): a caller bug applying attempt Y's
	// evidence to withdrawal X must never silently proceed under RLS's
	// tenant-only bound.
	if attempt.WithdrawalRequestID == nil || *attempt.WithdrawalRequestID != requestID {
		return fmt.Errorf("payments: apply payout result: attempt %s does not belong to withdrawal request %s", attempt.ID, requestID)
	}

	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutPhaseCTimeout)
	defer cancel()

	return pool.WithTenant(phaseCtx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		// A7 (RV-PRH-I1 ledger review M3): the attempt row is CAS'd BEFORE
		// any ledger-affecting call (withdrawal.Complete/Fail, which
		// itself locks withdrawal_requests and the ledger accounts) - the
		// opposite order from an earlier revision, which locked
		// withdrawal_requests FIRST via an explicit SELECT ... FOR UPDATE
		// here. That earlier order would deadlock against a future
		// payout-success callback that locks attempt-then-withdrawal
		// (receipt.go), once that callback is wired to Complete. No
		// explicit withdrawal-row lock is taken by this function itself
		// any more - Complete/Fail/AttachProviderReference each take their
		// own via lockRequestForUpdate.
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
			// M2 (RV-PRH-I1 ledger review): NotSent on a RESEND (the
			// attempt already carries ever_possibly_sent=true, e.g. a T12
			// resend whose own send provably never reached the provider
			// this time) must route to T6 (ambiguous), never T5
			// (MarkNotSent's own CAS already refuses this via `AND NOT
			// ever_possibly_sent`, fail-closed, but this branch now takes
			// the CORRECT transition instead of erroring every tick).
			if attempt.EverPossiblySent {
				return MarkAmbiguousFromSubmitting(actx, tx, attempt.ID, evidence, nextPoll)
			}
			return MarkNotSent(actx, tx, attempt.ID, *attempt.ClaimToken, nextPoll)

		case ErrorClassPending:
			if err := MarkAccepted(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, err)
			}
			if res.ProviderReference != "" {
				if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
					return err
				}
			}
			return nil

		case ErrorClassSucceeded:
			return applyPayoutSuccess(actx, tx, requestID, attempt, res.ProviderReference, evidence)

		case ErrorClassDefiniteDecline:
			reason := res.DeclineReason
			if reason == "" {
				reason = "provider_declined"
			}
			return applyPayoutDecline(actx, tx, requestID, attempt, res.ProviderReference, reason, res.Cascadable, evidence)

		default: // ErrorClassAmbiguous and anything unclassified - never a
			// failure without a definite decline (task item 4).
			// C1/B1 (RV-PRH-I1 review): the reference returned alongside
			// an Ambiguous result is PERSISTED, not dropped - dropping it
			// makes a later QueryStatus resolution impossible and pushes
			// every case toward an unbounded resend.
			if res.ProviderReference != "" {
				if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
					return err
				}
			}
			if err := MarkAmbiguousFromSubmitting(actx, tx, attempt.ID, evidence, nextPoll); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, err)
			}
			return nil
		}
	})
}

// applyPayoutSuccess performs the attempt-CAS-before-posting sequence
// (A7) for a definite success: ApplySuccess (T7, payment_attempts) FIRST,
// then AttachProviderReference + withdrawal.Complete (the ledger-affecting
// L3/L4 posting). A CAS conflict on ApplySuccess - because the attempt is
// no longer submitting/pending/ambiguous, most notably because it was
// already declined and its hold already released - is a genuine late/
// contradicting-evidence case (M4): routed to T14
// (ApplyDisputeFromDeclinedPayout, a P1 audit) instead of silently rolling
// back and losing the signal that a payout the platform already reversed
// may in fact have been executed twice.
func applyPayoutSuccess(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, attempt PaymentAttempt, providerReference string, evidence EvidenceKind) error {
	// L2 (RV-PRH-I1 ledger review): an empty echoed reference on a
	// definite success (e.g. a QueryStatus success reporting no reference
	// itself) falls back to the attempt's OWN already-stored reference -
	// never call AttachProviderReference/Complete with "", which
	// AttachProviderReference's own ErrInvalidInput would refuse and the
	// caller would then see as an ordinary (and permanently repeating)
	// error rather than the exact reference it already has on file.
	ref := providerReference
	if ref == "" && attempt.ProviderReference != nil {
		ref = *attempt.ProviderReference
	}
	if ref == "" {
		return fmt.Errorf("%w: a definite success has no provider reference to settle against (attempt %s)", ErrInvalidPayoutEvidence, attempt.ID)
	}

	if err := ApplySuccess(ctx, tx, attempt.ID, SuccessEvidence{Evidence: evidence, ProviderReference: ref}); err != nil {
		if errors.Is(err, ErrAttemptStateConflict) {
			return applyPayoutLateEvidence(ctx, tx, attempt, evidence, "late_success_after_terminal", requestID)
		}
		return err
	}
	if err := withdrawal.AttachProviderReference(ctx, tx, requestID, ref); err != nil {
		return err
	}
	providerID := ""
	if attempt.ProviderID != nil {
		providerID = *attempt.ProviderID
	}
	return withdrawal.Complete(ctx, tx, requestID, providerID, ref)
}

// applyPayoutDecline is applyPayoutSuccess's mirror for a definite
// decline: ApplyDecline (T8, attempt) FIRST, then withdrawal.Fail (the
// release posting). Same M4 late-evidence handling on a CAS conflict.
func applyPayoutDecline(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, attempt PaymentAttempt, providerReference, reason string, cascadable bool, evidence EvidenceKind) error {
	var refPtr *string
	if providerReference != "" {
		refPtr = &providerReference
	}
	if err := ApplyDecline(ctx, tx, attempt.ID, DeclineEvidence{
		Evidence: evidence, Reason: canonicalDeclineReason(reason), Stage: DeclineAtSubmission,
		Cascadable: &cascadable, ProviderRef: refPtr,
	}); err != nil {
		if errors.Is(err, ErrAttemptStateConflict) {
			return applyPayoutLateEvidence(ctx, tx, attempt, evidence, "late_decline_after_terminal", requestID)
		}
		return err
	}
	if providerReference != "" {
		if err := withdrawal.AttachProviderReference(ctx, tx, requestID, providerReference); err != nil {
			return err
		}
	}
	return withdrawal.Fail(ctx, tx, requestID, canonicalDeclineReason(reason))
}

// applyPayoutLateEvidence re-reads the attempt's actual current state and
// routes contradicting/late evidence to the correct terminal dispute
// transition (M4, RV-PRH-I1 ledger review) instead of letting the whole
// phase-C transaction roll back with the signal reduced to a log line. A
// success arriving after the SAME attempt already reached `declined` is
// T14 (a real double-payout candidate - P1). Any other contradiction (an
// attempt that somehow is not in a state this file expects) is parked via
// T10, never silently dropped.
func applyPayoutLateEvidence(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, evidence EvidenceKind, terminalReason string, requestID uuid.UUID) error {
	current, err := GetAttemptByID(ctx, tx, attempt.ID)
	if err != nil {
		return err
	}
	var applyErr error
	switch current.State {
	case AttemptDeclined:
		applyErr = ApplyDisputeFromDeclinedPayout(ctx, tx, attempt.ID, evidence, terminalReason)
	case AttemptSucceeded, AttemptDisputed, AttemptRejected:
		// Already resolved (a benign replay, or an already-parked
		// dispute) - nothing further to do; not itself a P1.
		return nil
	default:
		applyErr = ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, evidence, terminalReason)
	}
	if applyErr != nil {
		return applyErr
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_late_contradicting_evidence",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{"withdrawal_request_id": requestID.String(), "terminal_reason": terminalReason, "observed_state": string(current.State)},
	})
}

// payoutHandleContradiction is applyPayoutSuccess/applyPayoutDecline's
// shared tail for the Pending/Ambiguous branches above, which do not have
// their own dedicated helper: on a CAS conflict, route through the same
// late-evidence handling rather than surfacing a bare, repeating error.
func payoutHandleContradiction(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, evidence EvidenceKind, err error) error {
	if errors.Is(err, ErrAttemptStateConflict) && attempt.WithdrawalRequestID != nil {
		return applyPayoutLateEvidence(ctx, tx, attempt, evidence, "late_contradicting_evidence", *attempt.WithdrawalRequestID)
	}
	return err
}

// ErrInvalidPayoutEvidence is returned when a definite outcome carries no
// provider reference at all (neither echoed nor already on file) - a
// caller/adapter contract violation, never silently treated as a success.
var ErrInvalidPayoutEvidence = errors.New("payments: invalid payout evidence")

// canonicalDeclineReason maps an unbounded, unvalidated vendor decline
// string onto a small allow-listed set (S95-C10, B8/RV-PRH-I1 code review:
// "sanitize or enum-map vendor free-text decline reasons before they reach
// audit or decline_reason"). An unrecognized/empty reason maps to the
// generic "provider_declined" code rather than persisting arbitrary
// vendor text into an audited, permanent column.
func canonicalDeclineReason(reason string) string {
	switch reason {
	case "account_closed", "provider_unavailable", "insufficient_funds",
		"compliance_hold", "invalid_destination", "provider_declined":
		return reason
	default:
		return "provider_declined"
	}
}

// applyPayoutStatusEvidence applies a QueryStatus-derived outcome to a
// payout attempt whose CURRENT state may be `submitting` (lease expired,
// never got evidence), `pending` (already accepted) or `ambiguous` (T6) -
// the full ADR 0095 §4.4 evidence matrix, state-aware, unlike
// ApplyPayoutResult above (written only for the sync-Withdraw-from-
// submitting case - B4/M1, RV-PRH-I1 code/ledger review). A Succeeded
// outcome is cross-checked against the withdrawal's own requested amount/
// asset (INV-IO-6, B3/H2): a mismatch parks the attempt (T10) and audits
// it, never completes.
func applyPayoutStatusEvidence(ctx context.Context, pool *db.Pool, tenantID, requestID uuid.UUID, attempt PaymentAttempt, gr GateResult[StatusResult], evidence EvidenceKind, nextPoll time.Time) error {
	if attempt.WithdrawalRequestID == nil || *attempt.WithdrawalRequestID != requestID {
		return fmt.Errorf("payments: apply payout status evidence: attempt %s does not belong to withdrawal request %s", attempt.ID, requestID)
	}
	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutPhaseCTimeout)
	defer cancel()

	return pool.WithTenant(phaseCtx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		if gr.Class == ErrorClassProviderRefInvalid {
			reason := "invalid_provider_reference"
			if perr, ok := providerref.AsError(gr.Err); ok {
				reason = "invalid_provider_reference:" + string(perr.Reason)
			}
			if err := ApplyDisputeFromNonTerminal(actx, tx, attempt.ID, evidence, reason); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, err)
			}
			return audit.Record(actx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "payments.payout_parked_invalid_reference",
				TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{"withdrawal_request_id": requestID.String(), "reason": reason},
			})
		}

		// A transport/credential failure resolving the query itself (a
		// gate refusal, a timeout on the READ) is always inconclusive,
		// never treated as failure - reschedule regardless of current
		// state.
		if gr.Err != nil {
			return RescheduleNonTerminal(actx, tx, attempt.ID, nextPoll)
		}

		res := gr.Value
		switch attempt.State {
		case AttemptSubmitting:
			switch gr.Class {
			case ErrorClassPending:
				if err := MarkAccepted(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, err)
				}
				if res.ProviderReference != "" {
					return withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference)
				}
				return nil
			case ErrorClassSucceeded:
				return applyPayoutSuccessCheckedFromStatus(actx, tx, requestID, attempt, res, evidence)
			case ErrorClassDefiniteDecline:
				return applyPayoutDecline(actx, tx, requestID, attempt, res.ProviderReference, res.DeclineReason, res.Cascadable, evidence)
			default: // Ambiguous - a status QUERY result, never a Withdraw
				// NotSent, so this is never routed through MarkNotSent.
				if res.ProviderReference != "" {
					if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
						return err
					}
				}
				if err := MarkAmbiguousFromSubmitting(actx, tx, attempt.ID, evidence, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, err)
				}
				return nil
			}

		case AttemptPending:
			switch gr.Class {
			case ErrorClassPending:
				// M1/B4: still pending is a no-op reschedule, never a CAS
				// error (MarkAccepted's CAS does not accept a same-state
				// write) - poll_count/backoff still advance.
				return RescheduleNonTerminal(actx, tx, attempt.ID, nextPoll)
			case ErrorClassSucceeded:
				return applyPayoutSuccessCheckedFromStatus(actx, tx, requestID, attempt, res, evidence)
			case ErrorClassDefiniteDecline:
				return applyPayoutDecline(actx, tx, requestID, attempt, res.ProviderReference, res.DeclineReason, res.Cascadable, evidence)
			default: // Ambiguous/NotSent: the provider "forgot" an accepted
				// attempt - T11, a real anomaly, not a plain reschedule.
				if err := MarkAmbiguousFromPending(actx, tx, attempt.ID, evidence, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, err)
				}
				return nil
			}

		case AttemptAmbiguous:
			switch gr.Class {
			case ErrorClassPending:
				if err := MarkAccepted(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, err)
				}
				if res.ProviderReference != "" {
					return withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference)
				}
				return nil
			case ErrorClassSucceeded:
				return applyPayoutSuccessCheckedFromStatus(actx, tx, requestID, attempt, res, evidence)
			case ErrorClassDefiniteDecline:
				return applyPayoutDecline(actx, tx, requestID, attempt, res.ProviderReference, res.DeclineReason, res.Cascadable, evidence)
			default: // Still ambiguous/NotSent after polling - a no-op
				// reschedule; the CALLER (resubmitPayoutAmbiguous) decides
				// resend-vs-escalate by re-reading state afterward (C1/B1).
				return RescheduleNonTerminal(actx, tx, attempt.ID, nextPoll)
			}

		default:
			// Terminal, or `created` (not pollable by reference) - nothing
			// to do.
			return nil
		}
	})
}

// applyPayoutSuccessCheckedFromStatus is applyPayoutSuccess's QueryStatus
// entry point: cross-checks status.Amount/AssetCode against the
// withdrawal's own requested amount/asset BEFORE ever calling
// applyPayoutSuccess (INV-IO-6, B3/H2) - a mismatch parks the attempt
// (T10) with an audit record instead of completing.
func applyPayoutSuccessCheckedFromStatus(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, attempt PaymentAttempt, status StatusResult, evidence EvidenceKind) error {
	wr, err := withdrawal.GetByID(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if status.Amount != wr.Amount || status.AssetCode != wr.AssetCode {
		reason := "amount_asset_mismatch"
		if err := ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, evidence, reason); err != nil {
			return payoutHandleContradiction(ctx, tx, attempt, evidence, err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_amount_asset_mismatch",
			TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
			Metadata: map[string]any{
				"withdrawal_request_id": requestID.String(),
				"requested_amount": wr.Amount, "requested_asset": wr.AssetCode,
				"provider_amount": status.Amount, "provider_asset": status.AssetCode,
			},
		})
	}
	return applyPayoutSuccess(ctx, tx, requestID, attempt, status.ProviderReference, evidence)
}

// payoutStatusQuery is the shared phase-B/gate closure QueryStatus-driven
// resolution uses - factored out so PollPayoutStatus (used directly by
// /resolve) and the sweeper's own callers never duplicate the outcome
// classification/providerref-validation rule.
func payoutStatusQuery(provider PaymentProvider, providerReference string) AdapterCall[StatusResult] {
	return func(callCtx context.Context, cc CallContext) (StatusResult, ErrorClass, error) {
		status, err := provider.QueryStatus(callCtx, providerReference)
		if err != nil {
			return status, ErrorClassAmbiguous, err
		}
		if status.ProviderReference != "" {
			if verr := providerref.ValidateOptional("query_status.provider_reference", status.ProviderReference); verr != nil {
				return status, ErrorClassProviderRefInvalid, verr
			}
		}
		switch status.Outcome {
		case OutcomePending:
			return status, ErrorClassPending, nil
		case OutcomeSucceeded:
			return status, ErrorClassSucceeded, nil
		case OutcomeDeclined:
			return status, ErrorClassDefiniteDecline, nil
		default:
			return status, ErrorClassAmbiguous, nil
		}
	}
}

// PollPayoutStatus is phase B+C for QueryStatus-driven payout resolution:
// calls QueryStatus with NO transaction held (on a context.WithoutCancel-
// detached, bounded context - B1/H4), then applies the result via
// applyPayoutStatusEvidence, state-aware. Exported so both the sweeper
// (payout_sweep.go) and internal/httpserver's /resolve handler (H4/B7)
// share exactly one implementation - /resolve no longer calls QueryStatus
// itself, and no longer transitions withdrawal_requests without also
// transitioning the matching payment_attempts row.
//
// If attempt has no provider reference at all, this cannot query anything
// (H3/B2): a `submitting` attempt in that shape is moved to `ambiguous`
// (T6, fail-closed - the platform cannot prove the original send never
// reached the provider), never rescheduled forever.
func PollPayoutStatus(ctx context.Context, pool *db.Pool, orch *Orchestrator, credResolver OutboundCredentialResolver, tenantID uuid.UUID, attempt PaymentAttempt, nextPoll time.Time) error {
	if attempt.WithdrawalRequestID == nil {
		return fmt.Errorf("payments: poll payout status: attempt %s has no withdrawal_request_id", attempt.ID)
	}
	requestID := *attempt.WithdrawalRequestID

	if attempt.ProviderID == nil || attempt.ProviderReference == nil {
		return pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
			if attempt.State == AttemptSubmitting {
				return MarkAmbiguousFromSubmitting(actx, tx, attempt.ID, EvidencePlatform, nextPoll)
			}
			return RescheduleNonTerminal(actx, tx, attempt.ID, nextPoll)
		})
	}
	provider, ok := orch.Provider(*attempt.ProviderID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSweeperProviderNotRegistered, *attempt.ProviderID)
	}
	dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutOutboundCallBound)
	defer cancel()
	manifest := provider.Capabilities().Manifest
	in := callProviderInput{
		TenantID: tenantID, ProviderID: *attempt.ProviderID,
		AttemptState: attempt.State, ClaimToken: uuid.Nil, ExpectedClaim: uuid.Nil, ReadOnly: true,
		Domain: "payments", Manifest: manifest,
	}
	gr := callProvider(dispatchCtx, credResolver, in, payoutStatusQuery(provider, *attempt.ProviderReference))
	return applyPayoutStatusEvidence(ctx, pool, tenantID, requestID, attempt, gr, EvidenceQueryStatus, nextPoll)
}
