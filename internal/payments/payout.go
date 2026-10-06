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
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/providercred"
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

// ErrPayoutKYCUnavailable means the payout KYC gate could not reach a
// compliance decision - fail-closed AND retryable, never a terminal outcome
// (no Withdraw call, no rejection, no hold release; the request stays
// `approved`). Two shapes, both mapped to a retryable 503 by the handler:
//   - evaluatePayoutGate's own failure (the gate returned a Go error, i.e. a
//     structurally invalid call, or the account lookup failed): the
//     caller's transaction rolls back with no domain effect and no decision
//     row, like internal/withdrawal.ErrKYCUnavailable.
//   - ClaimForDispatch (PAY-KYC-UNAVAIL-1): the evaluator returned
//     Outcome=unavailable (a genuine DB-read outage contained by its
//     savepoint). The decision row and the denied `withdrawal.submit.http`
//     audit COMMIT, then this error is returned. Nothing else commits.
var ErrPayoutKYCUnavailable = errors.New("payments: payout kyc enforcement evaluation unavailable")

// ErrPayoutKillSwitchEngaged wraps a payout claim/resend refused by the
// ADR 0095 §10 kill switch (migration 0105, another agent's work - this
// file never engages/releases a switch itself, only reacts to one already
// engaged). Every claim/resend point (T1p, T2, T12) fails closed on it:
// no Withdraw call, the hold/state left exactly as it was (the whole claim
// transaction rolls back), and the SAME claim/resend is safe to retry once
// the switch is released - the CAS predicate re-evaluates it fresh on
// every attempt, there is nothing to reset.
var ErrPayoutKillSwitchEngaged = errors.New("payments: payout claim refused, kill switch engaged")

// recordPayoutKillSwitchHoldAudit labels a payout claim refused by an
// engaged kill switch as a "hold" - a Denied audit row, so an operator can
// see WHY a request stayed `approved` instead of only inferring it from
// the absence of a payment_attempts row. Runs in its OWN transaction,
// separate from ClaimForDispatch's rolled-back attempt (the whole claim
// transaction is undone on ErrPayoutKillSwitchEngaged - see that error's
// own doc comment - so nothing inside it could have durably recorded
// this). Mirrors recordKillSwitchRefusalAudit's identical pattern
// (internal/httpserver/payments_kill_switch_handlers.go, RV-PRH-I1
// security review L5): a refusal committed independently of the failed
// attempt. Best-effort: a failure to write this label is swallowed, never
// surfaced as a different error than the kill-switch refusal itself - the
// caller (httpserver) already logs the refusal on its own
// (submit_withdrawal_kill_switch_engaged), and the in-statement predicate,
// not this audit row, is what actually blocks the call.
func recordPayoutKillSwitchHoldAudit(ctx context.Context, pool *db.Pool, tenantID, requestID uuid.UUID, providerID string, actor SubmitActor) {
	// C3 (RV-PRH-I1 kill-switch phase 2 code review): this repeats the
	// exact RV2-L1 defect internal/httpserver's own denied-audit writers
	// were just fixed for - writing under the caller's own (cancellable)
	// request context would let a client disconnect silently drop this
	// label. Detached and bounded, exactly like
	// internal/httpserver's deniedAuditCtx.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutPhaseCTimeout)
	defer cancel()
	if err := pool.WithTenant(dctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		return audit.Record(actx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor.StaffID,
			Action: "withdrawal.submit.http", TargetType: "withdrawal_request", TargetID: requestID.String(),
			Outcome: audit.OutcomeDenied, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
			Metadata: map[string]any{"denied_by_kill_switch": true, "provider_id": providerID, "reason_code": "kill_switch"},
		})
	}); err != nil {
		// Never surfaced as a different error than the kill-switch
		// refusal itself (ClaimForDispatch already returns
		// ErrPayoutKillSwitchEngaged regardless of this outcome) - logged
		// so the label's own durability failure is at least visible,
		// mirroring payments_kill_switch_denied_audit_failed's identical
		// convention in internal/httpserver.
		slog.Default().Error("payments_payout_kill_switch_hold_audit_failed",
			"tenant_id", tenantID.String(), "withdrawal_request_id", requestID.String(), "provider_id", providerID, "error", err.Error())
	}
}

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
	// kycUnavailable is set (and the closure returns nil, so the tx COMMITS)
	// when the evaluator could not decide: only the decision row and the
	// denied-submit audit were written, see the branch below.
	var kycUnavailable bool
	err := pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		wr, err := withdrawal.LockApprovedForSubmission(actx, tx, requestID)
		if err != nil {
			return err
		}

		decision, kycParams, err := evaluatePayoutGate(actx, tx, kycGate, wr)
		if err != nil {
			return err
		}
		// PAY-KYC-UNAVAIL-1 (security C-F1, LF F-2): an `unavailable`
		// outcome means the evaluator could not decide - it is never
		// evidence of a compliance failure. It MUST be handled BEFORE the
		// deny branch: DenyForCompliance refuses it (N5, ErrKYCUnavailable)
		// and the resulting rollback would leave no decision row and no
		// submit audit, and the caller would see a non-retryable 500.
		// Instead (the LF-I3-3 / F-kyc pattern) record the decision and the
		// denied-submit audit and COMMIT them - and nothing else: the
		// request stays `approved`, no attempt row, no ledger posting, no
		// hold release. The caller maps this to a retryable 503. The
		// evaluator's reads ran inside a savepoint (KYC-ENF-OUTAGE-1), so
		// tx is still usable here.
		if decision.Outcome == kyc.OutcomeUnavailable {
			if err := kyc.RecordDecision(actx, tx, kycParams, decision); err != nil {
				return err
			}
			if err := audit.Record(actx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor.StaffID,
				Action: "withdrawal.submit.http", TargetType: "withdrawal_request", TargetID: requestID.String(),
				Outcome: audit.OutcomeDenied, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
				Metadata: map[string]any{"denied_by_kyc_unavailable": true, "reason_code": decision.Code, "outcome": string(decision.Outcome)},
			}); err != nil {
				return err
			}
			kycUnavailable = true
			return nil
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

		// DECISION-ROWS-1: the ALLOW decision row (and its audit) commits in
		// the SAME transaction as the domain effect it authorizes (ADR 0096
		// §3.6); a kill-switch or routing rollback below discards it together
		// with the claim, so a row never exists for a claim that did not
		// happen. This transaction contains no provider I/O.
		if err := kyc.RecordDecision(actx, tx, kycParams, decision); err != nil {
			return err
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
			if errors.Is(err, ErrKillSwitchEngaged) {
				// Kill switch (migration 0105): fail closed, whole tx rolls
				// back - MarkSubmittedPending's own approved->submitted
				// transition above is undone with it, so the hold stays
				// exactly where it was and the request is left `approved`,
				// unchanged, for an idempotent retry once the switch is
				// released (no partial claim, never a Withdraw call).
				return fmt.Errorf("%w: payout T1p claim refused: %w", ErrPayoutKillSwitchEngaged, err)
			}
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
		if errors.Is(err, ErrPayoutKillSwitchEngaged) {
			recordPayoutKillSwitchHoldAudit(ctx, pool, tenantID, requestID, routedCapability.ProviderID, actor)
		}
		return ClaimResult{}, err
	}
	if kycUnavailable {
		// Returned only AFTER the commit above: the decision and the denied
		// audit are durable. The handler maps ErrPayoutKYCUnavailable to a
		// retryable 503; the request is still `approved`.
		return ClaimResult{}, fmt.Errorf("%w: kyc evaluation unavailable, payout not claimed (decision recorded, request left approved)", ErrPayoutKYCUnavailable)
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
		// PAY-PAYOUT-ERRREF-1 (code review of PRH-2 C, F1): the reference is
		// validated BEFORE the error return, exactly like depositAdapterCall
		// (ADR 0095 §34). An adapter that returns an error TOGETHER with a
		// hostile reference must park (ErrorClassProviderRefInvalid ->
		// ApplyPayoutResult's T10), never reach phase C's persistence
		// (payoutMarkAmbiguousFromSubmitting / AttachProviderReference) raw,
		// where it would hit the 0099 CHECK, roll back and loop. An empty
		// reference stays valid (P95-C2: a payout may be accepted without
		// one). The returned result is SCRUBBED to the outcome alone, so the
		// raw value cannot leak through gr.Value on any path.
		if res.ProviderReference != "" {
			if verr := providerref.ValidatePaymentReference("withdraw.provider_reference", res.ProviderReference); verr != nil {
				return WithdrawResult{Outcome: res.Outcome}, ErrorClassProviderRefInvalid, verr
			}
		}
		if err != nil {
			return res, ErrorClassAmbiguous, err
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
// otherwise. pool (PROV-OUTBOUND-CRED-1, phase 2 orchestrator wiring) is
// threaded through to the credential resolver only - never used for
// anything else here, and never held across the call itself.
func DispatchWithdraw(ctx context.Context, pool providercred.TenantTxRunner, credResolver OutboundCredentialResolver, provider PaymentProvider, attempt PaymentAttempt) GateResult[WithdrawResult] {
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
	return callProvider(dispatchCtx, pool, credResolver, in, payoutAdapterCall(provider, attempt))
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
		// R3 (RV-PRH-I1 ledger re-review, ADR 0082 A7): withdrawal FIRST,
		// unconditionally, as the very first statement of this
		// transaction - ONE lock order for this pair everywhere (matching
		// the T2/T12 claim statements and the receipt path), not "lock it
		// only on the branches that happen to need it". applyPayoutSuccess/
		// applyPayoutDecline below take the SAME lock again defensively
		// (a harmless re-lock within one transaction/session), since they
		// are also reached from applyPayoutStatusEvidence's own
		// transaction, which repeats this same top-of-tx lock.
		if _, err := withdrawal.LockForPayoutEvidence(actx, tx, requestID); err != nil {
			return err
		}

		res := gr.Value
		nextPoll := time.Now().Add(payoutNextPollInterval)

		if gr.Class == ErrorClassProviderRefInvalid {
			reason := "invalid_provider_reference"
			if perr, ok := providerref.AsError(gr.Err); ok {
				reason = "invalid_provider_reference:" + string(perr.Reason)
			}
			if err := ApplyDisputeFromNonTerminal(actx, tx, attempt.ID, evidence, reason); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
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
				if parked, err := payoutGuardReferenceBinding(actx, tx, attempt, requestID, res.ProviderReference, evidence, gr.Class); err != nil || parked {
					return err
				}
				if err := payoutMarkAmbiguousFromSubmitting(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
				}
				return nil
			}
			// R1 (ledger re-review): a CAS conflict here means SOME other
			// evidence (most plausibly a callback) already advanced this
			// attempt past `submitting` before this NotSent result was
			// applied - that is not a contradiction for a NotSent result,
			// which by definition means "this call never reached the
			// provider" and has nothing left to correct.
			if err := MarkNotSent(actx, tx, attempt.ID, *attempt.ClaimToken, nextPoll); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
			}
			return nil

		case ErrorClassPending:
			if parked, err := payoutGuardReferenceBinding(actx, tx, attempt, requestID, res.ProviderReference, evidence, gr.Class); err != nil || parked {
				return err
			}
			if err := MarkAccepted(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
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
			// C1/B1/N1/R6 (RV-PRH-I1 review rounds): the reference returned
			// alongside an Ambiguous result is PERSISTED on BOTH the
			// attempt itself (payoutMarkAmbiguousFromSubmitting - required
			// so T12's poll-first step and PollPayoutStatus can ever query
			// it) and, as defence in depth, on the withdrawal
			// (AttachProviderReference) - dropping it makes a later
			// QueryStatus resolution impossible and pushes every case
			// toward an unbounded resend.
			if parked, err := payoutGuardReferenceBinding(actx, tx, attempt, requestID, res.ProviderReference, evidence, gr.Class); err != nil || parked {
				return err
			}
			if err := payoutMarkAmbiguousFromSubmitting(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
			}
			if res.ProviderReference != "" {
				if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
					return err
				}
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
	// R3 (RV-PRH-I1 ledger re-review, ADR 0082 A7): withdrawal FIRST - a
	// harmless re-lock when the caller (ApplyPayoutResult/
	// applyPayoutStatusEvidence) already took it at the top of its own
	// transaction, and essential defence in depth otherwise, since this
	// function is the one that eventually calls withdrawal.Complete.
	if _, err := withdrawal.LockForPayoutEvidence(ctx, tx, requestID); err != nil {
		return err
	}
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
	// FH-5 security re-verification gap (c): N6's provider-reference-
	// mismatch rule, centralized here so every source (sync dispatch,
	// receipt/callback, QueryStatus poll) that ever calls
	// applyPayoutSuccess gets the SAME check, rather than each caller
	// re-implementing its own copy. "The one already on file" is the
	// attempt's own stored reference if it has one, else the
	// withdrawal's own (applyPayoutSuccessCheckedFromStatus's identical,
	// now-redundant-but-harmless N6 comment explains the fallback
	// further). A non-empty echoed reference that CONFLICTS with it is
	// fail-closed disputed (T10), never silently settled.
	if providerReference != "" {
		wr, err := withdrawal.GetByID(ctx, tx, requestID)
		if err != nil {
			return err
		}
		storedRef := attempt.ProviderReference
		if storedRef == nil || *storedRef == "" {
			storedRef = wr.ProviderReference
		}
		if storedRef != nil && *storedRef != "" && providerReference != *storedRef {
			if err := ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, evidence, "provider_reference_mismatch"); err != nil {
				return payoutHandleContradiction(ctx, tx, attempt, evidence, ErrorClassSucceeded, err)
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_provider_reference_mismatch",
				TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{
					"withdrawal_request_id": requestID.String(),
					"stored_reference":      *storedRef,
					"echoed_reference":      providerReference,
				},
			})
		}
	}

	// PAY-PAYOUT-REFBIND-1: the reference this success binds (attempt, withdrawal
	// AND the withdrawal_completed ledger key) must not belong to anything else.
	// Parked BEFORE ApplySuccess/Complete, so nothing posts and the hold stays.
	if parked, err := payoutGuardReferenceBinding(ctx, tx, attempt, requestID, ref, evidence, ErrorClassSucceeded); err != nil || parked {
		return err
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
	// R3 (RV-PRH-I1 ledger re-review, ADR 0082 A7): withdrawal FIRST - see
	// applyPayoutSuccess's identical comment.
	if _, err := withdrawal.LockForPayoutEvidence(ctx, tx, requestID); err != nil {
		return err
	}
	// PAY-PAYOUT-REFBIND-1: a decline that echoes a foreign reference parks, with
	// the hold kept, instead of binding it (ApplyDecline/Attach) and releasing.
	if parked, err := payoutGuardReferenceBinding(ctx, tx, attempt, requestID, providerReference, evidence, ErrorClassDefiniteDecline); err != nil || parked {
		return err
	}
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

// isPayoutAttemptTerminal reports whether s is one of the four states ADR
// 0095 §4.1 treats as final for a payout attempt - used by
// payoutHandleContradiction (R1) to tell "this attempt already resolved,
// stray evidence has nothing left to do" from "this attempt is still open,
// a conflict here is worth recording".
func isPayoutAttemptTerminal(s AttemptState) bool {
	switch s {
	case AttemptSucceeded, AttemptDeclined, AttemptDisputed, AttemptRejected:
		return true
	default:
		return false
	}
}

// payoutHandleContradiction is the shared tail for every phase-C branch
// above on an attempt CAS conflict (a 0-row UPDATE, `ErrAttemptStateConflict`).
//
// R1 (RV-PRH-I1 ledger re-review, HIGH): NOT every CAS conflict is
// contradicting evidence. The callback cutover makes a verified `pending`
// webhook arriving before phase C's own synchronous `Withdraw` response an
// everyday race, not an anomaly - phase C's own attempt to also apply
// `Pending`/`Ambiguous`/`NotSent` then loses the CAS simply because a
// FASTER, weaker-or-equal piece of evidence already got there first. Only a
// DEFINITE result (`Succeeded`/`DefiniteDecline`) reaching an attempt that
// is ALREADY terminal is real late/contradicting evidence (T14/T10, a P1) -
// resultClass is the ErrorClass of the evidence phase C was trying to apply
// when the conflict happened.
func payoutHandleContradiction(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, evidence EvidenceKind, resultClass ErrorClass, err error) error {
	if !errors.Is(err, ErrAttemptStateConflict) || attempt.WithdrawalRequestID == nil {
		return err
	}
	if resultClass != ErrorClassSucceeded && resultClass != ErrorClassDefiniteDecline {
		current, gerr := GetAttemptByID(ctx, tx, attempt.ID)
		if gerr != nil {
			return gerr
		}
		if isPayoutAttemptTerminal(current.State) {
			// Stray non-definite evidence after the attempt already
			// resolved (e.g. a delayed sync Pending arriving after a
			// callback already settled it) - nothing to do, not a P1.
			return nil
		}
		// Some OTHER non-definite evidence already advanced the attempt
		// (the routine race R1 describes) - just reschedule the next
		// look; this is convergence, not a contradiction.
		return RescheduleNonTerminal(ctx, tx, attempt.ID, time.Now().Add(payoutNextPollInterval))
	}
	return applyPayoutLateEvidence(ctx, tx, attempt, evidence, "late_contradicting_evidence", *attempt.WithdrawalRequestID)
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

// payoutMarkAmbiguousFromSubmitting performs T6 for every payout call site
// applying REAL evidence just received (a synchronous Withdraw result in
// ApplyPayoutResult, or a QueryStatus result in applyPayoutStatusEvidence) -
// this file only; the deposit path's own T6 call sites in drive.go and
// receipt.go are unmodified and keep calling the shared, generic
// MarkAmbiguousFromSubmitting in attempt.go. N1/R6 (RV-PRH-I1 re-review):
// persists a provider reference the adapter returned alongside the
// Ambiguous/no-evidence result directly onto payment_attempts (set-once via
// COALESCE/NULLIF, exactly like MarkAccepted's own reference handling). The
// earlier revision only attached it to withdrawal_requests (kept as
// defence in depth by callers), which left T12's poll-first step and
// PollPayoutStatus with nothing of their own to query for the single most
// common ambiguous shape. Deliberately carries NO lease predicate: this
// function fires immediately after phase B's own response actually
// arrives, regardless of how much of the lease window is left - see
// payoutMarkAmbiguousFromSubmittingIfLeaseExpired for the ONE call site
// (PollPayoutStatus's own no-reference fallback) that must NOT touch a
// still-live lease (R2).
func payoutMarkAmbiguousFromSubmitting(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, providerReference string, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "T6 submitting->ambiguous (payout, reference-preserving)",
		`UPDATE payment_attempts
		 SET state = 'ambiguous', last_evidence_kind = $2, ever_possibly_sent = true,
		     provider_reference = COALESCE(provider_reference, NULLIF($3, '')),
		     next_action_at = $4, poll_count = poll_count + 1, updated_at = now()
		 WHERE id = $1 AND state = 'submitting'`,
		attemptID, evidence, providerReference, nextActionAt,
	)
}

// payoutMarkAmbiguousFromSubmittingIfLeaseExpired is PollPayoutStatus's own
// no-reference fallback transition ONLY (R2, ledger re-review): the CAS
// predicate itself refuses (0 rows affected -> ErrAttemptStateConflict)
// when the attempt's OWN dispatch lease is still live, so a submitting
// attempt whose phase B may be running RIGHT NOW is left completely
// untouched. This is defence in depth behind PollPayoutStatus's own
// Go-level pre-check for the exact same condition
// (ErrPayoutDispatchInFlight) - by the time this function is ever called,
// that check has already passed, so this predicate should never actually
// reject anything in practice; it exists so a future caller of this
// function alone cannot reintroduce R2.
//
// N7 (RV-PRH-I1 re-review 2): the lease_owner = SweeperBatchLeaseOwner
// exemption mirrors PollPayoutStatus's own Go-level check exactly - the
// sweeper's OWN batch-claim lease (set by claimBatch immediately before
// routing a due `submitting` row to QueryStatus resolution) must never be
// mistaken for a live dispatch lease, or the sweeper could never recover a
// crashed payout with a real (non-zero) Lease configured (H3/B2's own
// scenario). PAY-SEC-S-L2: this now names the distinct SweeperBatchLeaseOwner
// constant, never the bare "sweeper" literal, since claimBatch itself no
// longer writes that literal.
func payoutMarkAmbiguousFromSubmittingIfLeaseExpired(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "T6 submitting->ambiguous (payout, no reference, lease-respecting)",
		`UPDATE payment_attempts
		 SET state = 'ambiguous', last_evidence_kind = $2, ever_possibly_sent = true,
		     next_action_at = $3, poll_count = poll_count + 1, updated_at = now()
		 WHERE id = $1 AND state = 'submitting'
		   AND (lease_until IS NULL OR lease_until <= now() OR lease_owner = $4)`,
		attemptID, evidence, nextActionAt, SweeperBatchLeaseOwner,
	)
}

// ErrPayoutDispatchInFlight is PollPayoutStatus's refusal (R2, ledger
// re-review) when a `submitting` attempt's own DISPATCH lease (not the
// sweeper's own batch-claim lease - see PollPayoutStatus's own doc comment
// and N7, RV-PRH-I1 re-review 2, for why that distinction is load-bearing)
// has not yet expired - its phase B outbound call may be running RIGHT
// NOW. Forcing it to `ambiguous` here would race the original dispatch's
// own phase C and, on a NotSent result, permanently strip the attempt's
// T2/M3 eligibility for a payout that in fact never reached the provider.
// Never touches the attempt at all; the caller (internal/httpserver's
// /resolve handler) maps this to a 409 "dispatch in progress, try again
// shortly".
var ErrPayoutDispatchInFlight = errors.New("payments: payout dispatch is still in flight, its lease has not expired")

// applyPayoutStatusEvidence applies a QueryStatus-derived outcome to a
// payout attempt whose CURRENT state may be `submitting` (lease expired,
// never got evidence), `pending` (already accepted) or `ambiguous` (T6) -
// the full ADR 0095 §4.4 evidence matrix, state-aware, unlike
// ApplyPayoutResult above (written only for the sync-Withdraw-from-
// submitting case - B4/M1, RV-PRH-I1 code/ledger review). A Succeeded
// outcome is cross-checked against the withdrawal's own requested amount/
// asset (INV-IO-6, B3/H2): a mismatch parks the attempt (T10) and audits
// it, never completes.
func applyPayoutStatusEvidence(ctx context.Context, pool *db.Pool, tenantID, requestID uuid.UUID, attempt PaymentAttempt, gr GateResult[StatusResult], evidence EvidenceKind, nextPoll time.Time, actor *SubmitActor) error {
	if attempt.WithdrawalRequestID == nil || *attempt.WithdrawalRequestID != requestID {
		return fmt.Errorf("payments: apply payout status evidence: attempt %s does not belong to withdrawal request %s", attempt.ID, requestID)
	}
	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), payoutPhaseCTimeout)
	defer cancel()

	return pool.WithTenant(phaseCtx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		// P-C3 (FH-6 ledger-finance ruling, rv-prh-i1-payout-ledger.md):
		// take the SAME withdrawal lock applyPayoutStatusEvidenceInTx below
		// takes (re-locking an already-held row lock inside the SAME
		// transaction is a harmless no-op in Postgres) and re-read the
		// attempt HERE, under that lock, before anything below can change
		// it - this is the staff audit's own "before" snapshot, never the
		// caller's outer, pre-transaction one (which can be stale relative
		// to a concurrent transition that landed between the caller's read
		// and this transaction's start).
		wrBefore, err := withdrawal.LockForPayoutEvidence(actx, tx, requestID)
		if err != nil {
			return err
		}
		lockedBefore, err := GetAttemptByID(actx, tx, attempt.ID)
		if err != nil {
			return fmt.Errorf("payments: apply payout status evidence: re-read attempt under lock: %w", err)
		}
		if err := applyPayoutStatusEvidenceInTx(actx, tx, tenantID, requestID, attempt, gr, evidence, nextPoll); err != nil {
			return err
		}
		// N3 (RV-PRH-I1 re-review 2): the staff-attribution audit for a
		// /resolve-driven call commits in the SAME transaction as the
		// state change above, never a separate best-effort call after the
		// fact - the same B6 rule, now on /resolve. A nil actor (every
		// sweeper-driven call) is a no-op.
		return payoutResolveAudit(actx, tx, tenantID, requestID, lockedBefore, wrBefore.State, string(gr.Class), actor)
	})
}

// applyPayoutStatusEvidenceInTx is applyPayoutStatusEvidence's actual
// evidence-mapping body, factored out so the transaction wrapper above can
// run the N3 staff audit AFTER it, inside the exact same transaction,
// without duplicating it into every one of this switch's many return
// points.
func applyPayoutStatusEvidenceInTx(actx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID, attempt PaymentAttempt, gr GateResult[StatusResult], evidence EvidenceKind, nextPoll time.Time) error {
	{
		// R3 (RV-PRH-I1 ledger re-review, ADR 0082 A7): withdrawal FIRST,
		// unconditionally - see ApplyPayoutResult's identical top-of-tx
		// lock. applyPayoutSuccess/applyPayoutDecline below re-take it
		// defensively. Re-locking the same row the wrapper above already
		// locked, inside the SAME transaction, is a harmless no-op.
		if _, err := withdrawal.LockForPayoutEvidence(actx, tx, requestID); err != nil {
			return err
		}

		if gr.Class == ErrorClassProviderRefInvalid {
			reason := "invalid_provider_reference"
			if perr, ok := providerref.AsError(gr.Err); ok {
				reason = "invalid_provider_reference:" + string(perr.Reason)
			}
			if err := ApplyDisputeFromNonTerminal(actx, tx, attempt.ID, evidence, reason); err != nil {
				return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
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
				if parked, err := payoutGuardReferenceBinding(actx, tx, attempt, requestID, res.ProviderReference, evidence, gr.Class); err != nil || parked {
					return err
				}
				if err := MarkAccepted(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
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
				// N1/R6: the reference is persisted on the attempt itself.
				if parked, err := payoutGuardReferenceBinding(actx, tx, attempt, requestID, res.ProviderReference, evidence, gr.Class); err != nil || parked {
					return err
				}
				if err := payoutMarkAmbiguousFromSubmitting(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
				}
				if res.ProviderReference != "" {
					if err := withdrawal.AttachProviderReference(actx, tx, requestID, res.ProviderReference); err != nil {
						return err
					}
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
					return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
				}
				return nil
			}

		case AttemptAmbiguous:
			switch gr.Class {
			case ErrorClassPending:
				if parked, err := payoutGuardReferenceBinding(actx, tx, attempt, requestID, res.ProviderReference, evidence, gr.Class); err != nil || parked {
					return err
				}
				if err := MarkAccepted(actx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
					return payoutHandleContradiction(actx, tx, attempt, evidence, gr.Class, err)
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
	}
}

// payoutResolveAudit writes the staff-attribution audit for a
// PollPayoutStatus effect INSIDE the caller's own transaction (N3,
// RV-PRH-I1 re-review 2 - the same class of defect as B6, now on
// /resolve: "the effect is committed inside PollPayoutStatus... the audit
// is then written in a separate transaction afterwards"). A nil actor
// (every sweeper-driven call - the sweeper has no staff to attribute to)
// is a deliberate no-op.
//
// PAY-SEC-S-M2 (rv-prh-i1-payout-security.md): before this fix, every
// /resolve call - a reschedule no-op, a T6, a T10 dispute, a completion or
// a failure - wrote the SAME row (Outcome: success, no metadata), so an
// investigator could not tell from the staff row what the action actually
// did. P-C3/code-review (rv-prh-i1-payout-code-review.md): before and
// wrStateBefore are both read under the withdrawal lock by the caller
// (LockForPayoutEvidence's own return value, and a fresh GetAttemptByID
// right after it) - never the caller's own outer, pre-transaction
// snapshot, and never a hard-coded `submitted` - so both are exact even
// under a concurrent transition landing in the gap between the caller's
// original read and this transaction's start. evidenceClass names what
// evidence this call actually processed (ErrorClass for the QueryStatus
// matrix path, or a plain description for the no-reference reschedule
// fallback). Outcome reflects a dispute (failure) distinctly from every
// other, non-anomalous result (success), per the finding's "reflect a
// dispute or no-op".
func payoutResolveAudit(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID, before PaymentAttempt, wrStateBefore withdrawal.State, evidenceClass string, actor *SubmitActor) error {
	if actor == nil {
		return nil
	}
	after, err := GetAttemptByID(ctx, tx, before.ID)
	if err != nil {
		return fmt.Errorf("payments: payout resolve audit: re-read attempt: %w", err)
	}
	wr, err := withdrawal.GetByID(ctx, tx, requestID)
	if err != nil {
		return fmt.Errorf("payments: payout resolve audit: re-read withdrawal: %w", err)
	}
	// P-C3 (FH-6 ledger-finance ruling): Outcome is limited to the three
	// values the audit_log table's own CHECK constraint allows
	// (success/failure/denied - migration 0014) - there is no honest
	// fourth value to add here without a schema migration, well outside
	// this fix's scope. A genuine no-op (before == after: a bare
	// reschedule that changed nothing) still records Outcome=success
	// (procedurally, the /resolve action itself completed without error
	// or anomaly), but metadata now carries an explicit, unambiguous
	// "no_op" boolean rather than requiring a reader to compare the two
	// state strings themselves.
	outcome := audit.OutcomeSuccess
	if after.State == AttemptDisputed {
		outcome = audit.OutcomeFailure
	}
	metadata := map[string]any{
		"attempt_id":              before.ID.String(),
		"evidence_class":          evidenceClass,
		"attempt_state_before":    string(before.State),
		"attempt_state_after":     string(after.State),
		"withdrawal_state_before": string(wrStateBefore),
		"withdrawal_state_after":  string(wr.State),
		"no_op":                   before.State == after.State,
	}
	if after.TerminalReason != nil {
		metadata["terminal_reason"] = *after.TerminalReason
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor.StaffID,
		Action: "withdrawal.resolve_attempted.http", TargetType: "withdrawal_request", TargetID: requestID.String(),
		Outcome: outcome, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Metadata: metadata,
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
			return payoutHandleContradiction(ctx, tx, attempt, evidence, ErrorClassSucceeded, err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_amount_asset_mismatch",
			TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
			Metadata: map[string]any{
				"withdrawal_request_id": requestID.String(),
				"requested_amount":      wr.Amount, "requested_asset": wr.AssetCode,
				"provider_amount": status.Amount, "provider_asset": status.AssetCode,
			},
		})
	}
	// N6 (RV-PRH-I1 ledger re-review, ADR 0095 §4.4; extended by RV-PRH-I1
	// re-review 2's own "N6 must also compare against the fallback
	// withdrawal reference when the attempt has none"): a QueryStatus
	// success that echoes a DIFFERENT non-empty reference from the one
	// already on file is fail-closed disputed (T10), never silently
	// settled with the echoed value - applyPayoutSuccess's own L2 fallback
	// exists for the OPPOSITE case (an empty echo, not a conflicting one),
	// and must never be reached for a genuine mismatch. "The one already
	// on file" is the attempt's own stored reference if it has one, else
	// (the N1/R6 fallback shape - an attempt that reached `ambiguous`
	// before that fix, or any other path that left it NULL) the
	// withdrawal's own - never compare against nothing just because the
	// attempt-level column happens to be empty.
	storedRef := attempt.ProviderReference
	if storedRef == nil || *storedRef == "" {
		storedRef = wr.ProviderReference
	}
	if status.ProviderReference != "" && storedRef != nil && *storedRef != "" &&
		status.ProviderReference != *storedRef {
		reason := "provider_reference_mismatch"
		if err := ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, evidence, reason); err != nil {
			return payoutHandleContradiction(ctx, tx, attempt, evidence, ErrorClassSucceeded, err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_provider_reference_mismatch",
			TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
			Metadata: map[string]any{
				"withdrawal_request_id": requestID.String(),
				"stored_reference":      *storedRef,
				"echoed_reference":      status.ProviderReference,
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
			if verr := providerref.ValidatePaymentReferenceOptional("query_status.provider_reference", status.ProviderReference); verr != nil {
				// F-L3: the hostile reference (and every other field of the
				// adapter's result) must not travel past this closure; only the
				// outcome survives.
				return StatusResult{Outcome: status.Outcome}, ErrorClassProviderRefInvalid, verr
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
//
// R2 (RV-PRH-I1 ledger re-review, MEDIUM) / N7 (RV-PRH-I1 re-review 2,
// HIGH, a regression of H3/B2): a `submitting` attempt whose OWN dispatch
// lease has not yet expired is left completely untouched and this function
// returns ErrPayoutDispatchInFlight instead - its phase B may be running
// RIGHT NOW.
//
// CORRECTED DOC COMMENT (N7): an earlier version of this comment claimed
// "the sweeper never hits this... claimBatch's own next_action_at
// predicate already only selects a submitting row once its lease is due."
// That is false. claimBatch's SELECT predicate is on next_action_at, not
// lease_until, and it then OVERWRITES lease_until with a FRESH lease
// (lease_owner=SweeperBatchLeaseOwner) for every row it claims - including
// a crashed, long-expired `submitting` payout attempt with no reference
// (exactly H3/B2's own crash-recovery case). With a real (non-zero)
// Sweeper.Lease, that fresh lease_until is now in the future, so a naive
// `attempt.LeaseUntil.After(time.Now())` check WOULD wrongly refuse the
// sweeper's own attempt to resolve the very row it just claimed for that
// purpose - reproducing H3/B2 (claimed=1, processed=0, erroring every
// tick, no recovery). Fixed by checking `attempt.LeaseOwner` too: only a
// lease NOT owned by SweeperBatchLeaseOwner (i.e. `lease_owner =
// "payout-dispatch"`, set by ClaimForDispatch's own InsertSubmittingAttempt
// - the ONLY writer of a live dispatch lease on a `submitting` row) can
// mean "phase B may be running right now". SweeperBatchLeaseOwner always
// means "the batch claim itself, about to resolve this via QueryStatus",
// never an in-flight outbound call - refusing on that lease would starve
// the sweeper's own recovery path forever. internal/httpserver's /resolve
// handler never re-leases before calling this function, so it is
// unaffected by this distinction and still correctly refuses a genuinely
// in-flight dispatch. PAY-SEC-S-L2: this now names the distinct
// SweeperBatchLeaseOwner constant, never the bare "sweeper" literal.
func PollPayoutStatus(ctx context.Context, pool *db.Pool, orch *Orchestrator, credResolver OutboundCredentialResolver, tenantID uuid.UUID, attempt PaymentAttempt, nextPoll time.Time, actor *SubmitActor) error {
	if attempt.WithdrawalRequestID == nil {
		return fmt.Errorf("payments: poll payout status: attempt %s has no withdrawal_request_id", attempt.ID)
	}
	requestID := *attempt.WithdrawalRequestID

	if attempt.State == AttemptSubmitting && attempt.LeaseOwner != nil && *attempt.LeaseOwner != SweeperBatchLeaseOwner &&
		attempt.LeaseUntil != nil && attempt.LeaseUntil.After(time.Now()) {
		return ErrPayoutDispatchInFlight
	}

	// N1/R6 (RV-PRH-I1 re-review): the attempt's own reference is now
	// persisted directly by payoutMarkAmbiguousFromSubmitting, but fall
	// back to the withdrawal's own reference as defence in depth for any
	// attempt that reached `ambiguous` before that fix, or by any other
	// path this file has not anticipated - INV-IO-8 guarantees exactly one
	// payout attempt per withdrawal request, so this can never resolve the
	// wrong attempt's reference.
	ref := attempt.ProviderReference
	if ref == nil || *ref == "" {
		var wrRef *string
		if err := pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
			wr, err := withdrawal.GetByID(actx, tx, requestID)
			if err != nil {
				return err
			}
			wrRef = wr.ProviderReference
			return nil
		}); err != nil {
			return err
		}
		if wrRef != nil && *wrRef != "" {
			ref = wrRef
		}
	}

	if attempt.ProviderID == nil || ref == nil || *ref == "" {
		return pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
			// P-C3 (FH-6 ledger-finance ruling): take the same withdrawal
			// lock the other call site uses and re-read the attempt under
			// it, for the SAME reason - this branch's staff-audit "before"
			// must not come from the caller's own pre-transaction snapshot.
			wrBefore, err := withdrawal.LockForPayoutEvidence(actx, tx, requestID)
			if err != nil {
				return err
			}
			lockedBefore, err := GetAttemptByID(actx, tx, attempt.ID)
			if err != nil {
				return fmt.Errorf("payments: poll payout status (no-reference fallback): re-read attempt under lock: %w", err)
			}
			if attempt.State == AttemptSubmitting {
				if err := payoutMarkAmbiguousFromSubmittingIfLeaseExpired(actx, tx, attempt.ID, EvidencePlatform, nextPoll); err != nil {
					return err
				}
			} else if err := RescheduleNonTerminal(actx, tx, attempt.ID, nextPoll); err != nil {
				return err
			}
			// N3: same transaction as the state change above.
			return payoutResolveAudit(actx, tx, tenantID, requestID, lockedBefore, wrBefore.State, "no_reference_reschedule", actor)
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
	gr := callProvider(dispatchCtx, pool, credResolver, in, payoutStatusQuery(provider, *ref))
	return applyPayoutStatusEvidence(ctx, pool, tenantID, requestID, attempt, gr, EvidenceQueryStatus, nextPoll, actor)
}
