//go:build integration

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
	"github.com/Diansalas/igaming-platform/internal/rg"
)

// initiateDepositWithAttempt is the TEST-ONLY bridge (PRH-payments-
// callback-cutover; PROV-OUTBOUND-CRED-1-LEGACY-PATH/E2) for domain tests
// written against the pre-ADR-0095 legacy deposit-creation shape
// (Orchestrator.InitiateDeposit/attemptDeposit/handleDecline/
// resolveAmbiguous - all deleted from orchestrator.go; InitiateDepositAttempt
// is the only live production entry point now, and it needs a *db.Pool,
// not a caller-held pgx.Tx, since its own phase B runs deliberately
// OUTSIDE any transaction (INV-IO-1) - it cannot be driven from inside a
// caller's already-open tx the way this bridge's many callers need).
//
// legacyShapeInitiateDeposit below (this file, TEST-ONLY, never reachable
// from non-test code, so the txscope static guard - which only scans
// non-test .go files - does not apply to it) reproduces the exact
// intent-creation-then-route-then-call-then-cascade shape the deleted
// chain implemented, using ONLY still-live, still-exported/package-
// private building blocks (RouteProvider, setIntentAttempt,
// finalizeDeclined, finalizeAmbiguous, rg.EvaluateEligibility) - never a
// resurrected copy of the deleted functions themselves. It exists ONLY so
// these tests keep asserting the domain behaviour (ledger, idempotency,
// locks, audit, concurrency, cascade) they were written for, against the
// receipt path's attempt-based resolution requirement.
//
// A retried call (same idempotency key) returns the SAME intent as
// before - this function is idempotent with respect to that: it only
// backfills an attempt if the intent does not already have one.
func initiateDepositWithAttempt(ctx context.Context, tx pgx.Tx, orch *Orchestrator, params InitiateDepositParams) (DepositIntent, error) {
	intent, err := legacyShapeInitiateDeposit(ctx, tx, orch, params)
	if err != nil {
		return intent, err
	}
	var existing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1`, intent.ID).Scan(&existing); err != nil {
		return intent, fmt.Errorf("payments test bridge: count existing attempts: %w", err)
	}
	if existing > 0 {
		return intent, nil
	}
	return intent, backfillAttemptForIntent(ctx, tx, intent)
}

// legacyShapeInitiateDeposit is the TEST-ONLY reproduction of the deleted
// InitiateDeposit/attemptDeposit/handleDecline/resolveAmbiguous chain's
// observable behaviour (intent creation, idempotent retry, the RG gate,
// routing, one provider call, in-process cascade on a cascadable decline
// up to MaxCascadeDepth, and ambiguous resolution via QueryStatus) -
// see initiateDepositWithAttempt's own doc comment for why this exists
// instead of a pool-based call to the live InitiateDepositAttempt.
func legacyShapeInitiateDeposit(ctx context.Context, tx pgx.Tx, orch *Orchestrator, params InitiateDepositParams) (DepositIntent, error) {
	if err := validateInitiateDepositParams(params); err != nil {
		return DepositIntent{}, err
	}

	intentID := uuid.New()
	conflict, _, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
		_, err := spTx.Exec(ctx,
			`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			intentID, params.Scope.TenantID, params.Scope.BrandID, params.Scope.PlayerAccountID, params.Scope.WalletID,
			params.AssetCode, params.Amount, params.PaymentMethod, params.IdempotencyKey,
		)
		return err
	})
	if err != nil {
		return DepositIntent{}, fmt.Errorf("payments test bridge: create deposit intent: %w", err)
	}
	if conflict {
		existing, found, err := loadDepositIntentByIdempotencyKey(ctx, tx, params.Scope.TenantID, params.Scope.PlayerAccountID, params.IdempotencyKey)
		if err != nil {
			return DepositIntent{}, err
		}
		if !found {
			return DepositIntent{}, fmt.Errorf("payments test bridge: idempotency conflict on deposit intent but no existing row found")
		}
		if existing.BrandID != params.Scope.BrandID || existing.WalletID != params.Scope.WalletID ||
			existing.AssetCode != params.AssetCode || existing.Amount != params.Amount || existing.PaymentMethod != params.PaymentMethod {
			return DepositIntent{}, fmt.Errorf("%w: existing intent %s", ErrIdempotencyKeyReused, existing.ID)
		}
		return existing, nil
	}

	intent := DepositIntent{
		ID: intentID, TenantID: params.Scope.TenantID, BrandID: params.Scope.BrandID,
		PlayerAccountID: params.Scope.PlayerAccountID, WalletID: params.Scope.WalletID,
		AssetCode: params.AssetCode, Amount: params.Amount, PaymentMethod: params.PaymentMethod,
		IdempotencyKey: params.IdempotencyKey, Status: DepositIntentPending,
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.Scope.TenantID, ActorType: audit.ActorPlayer, ActorID: params.Scope.PlayerAccountID,
		Action: "deposit.requested", TargetType: "deposit_intent", TargetID: intentID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{"amount": params.Amount, "asset_code": params.AssetCode, "payment_method": params.PaymentMethod},
	}); err != nil {
		return DepositIntent{}, fmt.Errorf("payments test bridge: audit deposit request: %w", err)
	}

	eligibility, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{
		TenantID: intent.TenantID, BrandID: intent.BrandID, PlayerAccountID: intent.PlayerAccountID, WalletID: intent.WalletID,
	})
	if err != nil {
		return DepositIntent{}, fmt.Errorf("payments test bridge: evaluate rg eligibility: %w", err)
	}
	if !eligibility.Allowed {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "payments.deposit_denied_by_rg",
			TargetType: "player_account", TargetID: intent.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
			Metadata: map[string]any{
				"reason_code": eligibility.Code, "person_id": eligibility.PersonID.String(),
				"deposit_intent_id": intent.ID.String(), "brand_id": intent.BrandID.String(),
			},
		}); err != nil {
			return DepositIntent{}, fmt.Errorf("payments test bridge: audit rg denial: %w", err)
		}
		return orch.finalizeDeclined(ctx, tx, intent, nil, nil, "rg_ineligible:"+eligibility.Code)
	}

	return legacyShapeAttemptDeposit(ctx, tx, orch, intent, nil)
}

// legacyShapeAttemptDeposit is legacyShapeInitiateDeposit's per-attempt
// loop - the TEST-ONLY equivalent of the deleted attemptDeposit/
// handleDecline/resolveAmbiguous trio, built only from still-live
// building blocks (see legacyShapeInitiateDeposit's own doc comment).
func legacyShapeAttemptDeposit(ctx context.Context, tx pgx.Tx, orch *Orchestrator, intent DepositIntent, excluded []string) (DepositIntent, error) {
	provider, capability, err := orch.RouteProvider(ctx, tx, RoutingRequest{
		TenantID: intent.TenantID, BrandID: intent.BrandID, AssetCode: intent.AssetCode,
		PaymentMethod: intent.PaymentMethod, Amount: intent.Amount, Operation: OperationDeposit,
		ExcludeProviderIDs: excluded,
	})
	if errors.Is(err, ErrNoRoutableProvider) {
		return orch.finalizeDeclined(ctx, tx, intent, nil, nil, "no_routable_provider")
	}
	if err != nil {
		return intent, fmt.Errorf("payments test bridge: route provider: %w", err)
	}
	providerID := capability.ProviderID

	result, err := provider.Deposit(ctx, DepositRequest{
		MerchantReference: intent.ID.String(), Amount: intent.Amount, AssetCode: intent.AssetCode, PaymentMethod: intent.PaymentMethod,
	})
	if err != nil {
		return orch.finalizeAmbiguous(ctx, tx, intent, &providerID, nil, "initiation_transport_error")
	}

	switch result.Outcome {
	case OutcomePending:
		if result.ProviderReference == "" {
			return intent, fmt.Errorf("payments test bridge: adapter %s returned OutcomePending with no provider reference", providerID)
		}
		ref := result.ProviderReference
		if _, err := setIntentAttempt(ctx, tx, intent.ID, &providerID, &ref, DepositIntentPending); err != nil {
			return intent, err
		}
		intent.ProviderID, intent.ProviderReference, intent.Status = &providerID, &ref, DepositIntentPending
		intent.RedirectURL, intent.HostedFieldToken = result.RedirectURL, result.HostedFieldToken
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "deposit.initiated",
			TargetType: "deposit_intent", TargetID: intent.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{"provider_id": providerID, "provider_reference": ref},
		}); err != nil {
			return intent, fmt.Errorf("payments test bridge: audit deposit initiated: %w", err)
		}
		return intent, nil

	case OutcomeDeclined:
		return legacyShapeHandleDecline(ctx, tx, orch, intent, providerID, result.ProviderReference, result.Cascadable, result.DeclineReason, excluded)

	case OutcomeAmbiguous:
		return legacyShapeResolveAmbiguous(ctx, tx, orch, intent, providerID, provider, result.ProviderReference, excluded)

	default:
		return intent, fmt.Errorf("payments test bridge: adapter %s returned invalid synchronous Deposit outcome %q", providerID, result.Outcome)
	}
}

func legacyShapeHandleDecline(ctx context.Context, tx pgx.Tx, orch *Orchestrator, intent DepositIntent, providerID, providerReference string, cascadable bool, reason string, excluded []string) (DepositIntent, error) {
	var refPtr *string
	if providerReference != "" {
		refPtr = &providerReference
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "deposit.attempt_declined",
		TargetType: "deposit_intent", TargetID: intent.ID.String(), Outcome: audit.OutcomeFailure,
		Metadata: map[string]any{"provider_id": providerID, "provider_reference": providerReference, "decline_reason": reason, "cascadable": cascadable},
	}); err != nil {
		return intent, fmt.Errorf("payments test bridge: audit attempt decline: %w", err)
	}

	attemptsSoFar := len(excluded) + 1
	if cascadable && attemptsSoFar < orch.maxCascadeDepth() {
		nextExcluded := make([]string, 0, len(excluded)+1)
		nextExcluded = append(nextExcluded, excluded...)
		nextExcluded = append(nextExcluded, providerID)
		return legacyShapeAttemptDeposit(ctx, tx, orch, intent, nextExcluded)
	}
	return orch.finalizeDeclined(ctx, tx, intent, &providerID, refPtr, reason)
}

func legacyShapeResolveAmbiguous(ctx context.Context, tx pgx.Tx, orch *Orchestrator, intent DepositIntent, providerID string, provider PaymentProvider, providerReference string, excluded []string) (DepositIntent, error) {
	if providerReference == "" {
		return orch.finalizeAmbiguous(ctx, tx, intent, &providerID, nil, "no_provider_reference_to_query")
	}
	status, err := provider.QueryStatus(ctx, providerReference)
	if err != nil {
		return orch.finalizeAmbiguous(ctx, tx, intent, &providerID, &providerReference, "query_status_failed")
	}

	switch status.Outcome {
	case OutcomeSucceeded:
		updated, _, err := orch.postDepositSuccess(ctx, tx, intent, nil, providerID, providerReference, status.Amount, status.AssetCode)
		return updated, err
	case OutcomeDeclined:
		return legacyShapeHandleDecline(ctx, tx, orch, intent, providerID, providerReference, status.Cascadable, status.DeclineReason, excluded)
	default: // OutcomeAmbiguous or OutcomePending - still unresolved
		return orch.finalizeAmbiguous(ctx, tx, intent, &providerID, &providerReference, "still_unresolved_after_query_status")
	}
}

// backfillAttemptForIntent drives one payment_attempts row to match
// intent's CURRENT state (as InitiateDeposit's synchronous flow, possibly
// including an in-process cascade, already decided it) - see
// initiateDepositWithAttempt's doc comment.
func backfillAttemptForIntent(ctx context.Context, tx pgx.Tx, intent DepositIntent) error {
	if intent.ProviderID == nil {
		// A phase-A pre-call refusal (RG/KYC deny, no routable provider):
		// no provider was ever tried, so no attempt row exists for this
		// case even on the real InitiateDepositAttempt path (ADR 0095 §4.3
		// T3's "no attempt" outcome) - nothing to backfill.
		return nil
	}
	attempt, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
		ID: uuid.New(), TenantID: intent.TenantID, Operation: AttemptOperationDeposit,
		DepositIntentID: &intent.ID, ProviderID: *intent.ProviderID, PaymentMethod: intent.PaymentMethod,
		AssetCode: intent.AssetCode, Amount: intent.Amount, Interactive: false,
		ClaimToken: uuid.New(), LeaseOwner: "test-backfill", LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		return fmt.Errorf("payments test bridge: backfill attempt: %w", err)
	}
	nextAction := time.Now().Add(time.Minute)

	switch intent.Status {
	case DepositIntentSucceeded:
		var ref string
		if intent.ProviderReference != nil {
			ref = *intent.ProviderReference
		}
		return ApplySuccess(ctx, tx, attempt.ID, SuccessEvidence{
			Evidence: EvidenceSync, ProviderReference: ref, LedgerTransactionID: intent.LedgerTransactionID,
		})
	case DepositIntentDeclined:
		cascadable := false
		return ApplyDecline(ctx, tx, attempt.ID, DeclineEvidence{
			Evidence: EvidenceSync, Reason: "test_backfill_decline", Stage: DeclineAfterAcceptance,
			Cascadable: &cascadable, ProviderRef: intent.ProviderReference,
		})
	case DepositIntentAmbiguous:
		if intent.ProviderReference != nil {
			// The provider accepted, THEN a status check came back
			// ambiguous (T9 then T11) - the reference is already known.
			if err := MarkAccepted(ctx, tx, attempt.ID, EvidenceSync, *intent.ProviderReference, nextAction); err != nil {
				return err
			}
			return MarkAmbiguousFromPending(ctx, tx, attempt.ID, EvidenceSync, nextAction)
		}
		// The call itself timed out/errored with no reference at all (T6).
		return MarkAmbiguousFromSubmitting(ctx, tx, attempt.ID, EvidenceSync, nextAction)
	case DepositIntentPending, DepositIntentFailed:
		if intent.ProviderReference == nil {
			return nil // still awaiting acceptance - stays 'submitting'
		}
		return MarkAccepted(ctx, tx, attempt.ID, EvidenceSync, *intent.ProviderReference, nextAction)
	default:
		return fmt.Errorf("payments test bridge: unhandled deposit intent status %q for attempt backfill", intent.Status)
	}
}

// backfillCascadeAttemptsForIntent is L2's bridge extension (RV-PRH-I1
// ledger review): backfillAttemptForIntent above only ever builds ONE
// attempt per intent, so no test using it could exercise a callback
// naming an EARLIER cascade provider's own reference, or a genuine T13
// second capture. This builds a real two-attempt cascade shape (attempt
// 1: created -> submitting -> declined, cascadable; attempt 2: the
// cascade child, inserted via cascade.go's own insertCascadeAttempt in
// 'created' state, left UNCLAIMED) through the SAME exported T1/T2/T4/T8
// transitions the live cascade path uses - never through the legacy
// InitiateDeposit's own in-process cascade recursion, which has no way
// to report which intermediate provider/reference it tried and declined
// (this is exactly why the single-attempt bridge above cannot be
// extended to a cascade shape by reading intent alone).
func backfillCascadeAttemptsForIntent(ctx context.Context, tx pgx.Tx, intent DepositIntent, firstProviderID string) (first PaymentAttempt, second PaymentAttempt, err error) {
	first, err = InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
		ID: uuid.New(), TenantID: intent.TenantID, Operation: AttemptOperationDeposit,
		DepositIntentID: &intent.ID, AttemptNo: 1, ExcludedProviderIDs: []string{},
		PaymentMethod: intent.PaymentMethod, AssetCode: intent.AssetCode, Amount: intent.Amount,
	})
	if err != nil {
		return first, second, fmt.Errorf("payments test bridge: insert first cascade attempt: %w", err)
	}
	if err = ClaimCreatedForSubmission(ctx, tx, first.ID, firstProviderID, uuid.New(), "test-bridge", time.Now().Add(time.Minute)); err != nil {
		return first, second, fmt.Errorf("payments test bridge: claim first cascade attempt: %w", err)
	}
	firstRef := "bridge-cascade-first-" + first.ID.String()
	if err = MarkAccepted(ctx, tx, first.ID, EvidenceSync, firstRef, time.Now().Add(time.Minute)); err != nil {
		return first, second, fmt.Errorf("payments test bridge: mark first cascade attempt accepted: %w", err)
	}
	cascadable := true
	if err = ApplyDecline(ctx, tx, first.ID, DeclineEvidence{
		Evidence: EvidenceSync, Reason: "test_backfill_decline", Stage: DeclineAfterAcceptance,
		Cascadable: &cascadable, ProviderRef: &firstRef,
	}); err != nil {
		return first, second, fmt.Errorf("payments test bridge: decline first cascade attempt: %w", err)
	}
	first, err = GetAttemptByID(ctx, tx, first.ID)
	if err != nil {
		return first, second, fmt.Errorf("payments test bridge: reread first cascade attempt: %w", err)
	}
	second, err = insertCascadeAttempt(ctx, tx, first)
	if err != nil {
		return first, second, fmt.Errorf("payments test bridge: insert cascade child: %w", err)
	}
	return first, second, nil
}

// RouteProvider is a TEST-ONLY (E2-3/F-2, code-review and security review
// of PROV-OUTBOUND-CRED-1-LEGACY-PATH/E2) convenience wrapper: it used to
// be exported production code (orchestrator.go), kept only for the
// deleted legacy InitiateDeposit/attemptDeposit call sites, which - per
// ADR 0095 §9.6's own gap this wrapper's old doc comment named - called
// provider.HealthStatus (via RankRoutingCandidates) while still holding a
// tx. With the legacy chain gone, nothing in production calls this shape
// any more (InitiateDepositAttempt's own phase A0 calls
// ListRoutingCandidates and RankRoutingCandidates directly, exactly as
// this wrapper does, but with ListRoutingCandidates' own tx committed and
// closed BEFORE RankRoutingCandidates/HealthStatus ever run - see
// deposit_v2.go's own phase A0 comment). Defining this method in a
// _test.go file (rather than keeping it exported in orchestrator.go)
// makes it structurally impossible to reach from non-test/production code
// - a stronger guarantee than a doc comment alone - while letting the
// existing test call sites that still name it keep passing an unchanged
// tx-shaped call, unaffected by this relocation.
func (o *Orchestrator) RouteProvider(ctx context.Context, tx pgx.Tx, req RoutingRequest) (PaymentProvider, ProviderCapability, error) {
	candidates, err := ListRoutingCandidates(ctx, tx, req.TenantID, req.BrandID)
	if err != nil {
		return nil, ProviderCapability{}, err
	}
	return RankRoutingCandidates(ctx, candidates, o.providers, o.breaker, req)
}
