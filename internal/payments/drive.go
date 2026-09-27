// PRH-I1 step (c): driveCreatedAttempt runs T2 (route, claim) plus phase
// B/C for one ALREADY-INSERTED 'created' deposit attempt - the shared
// core both the synchronous cascade loop (deposit_v2.go) and the sweeper
// (sweeper.go) use to drive a cascade row or a sweeper-claimed row
// through to its next resolution. It is never called for attempt 1 of a
// player-request deposit (that one is T1+T2 combined, InitiateDepositAttempt's
// own phase A) - only for attempt_no > 1 (cascade) or a sweeper-claimed
// 'created' row.
package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

// driveCreatedAttempt: T2 claim (route outside tx, then RG + the KYC
// deposit gate + the claim CAS inside one per-item tx, per ADR 0095 §4.3
// T2 / LF95-C10(e)), then phase B (no tx), then phase C (evidence, with
// a further cascade insert if the outcome is itself an eligible
// decline). sweeperDriven controls §4.6's interactive-cascade rule (case
// (a) vs (b)) and is also this call's audit/lease-owner label.
// redirectURL/hostedFieldToken carry phase B's DepositResult redirect
// detail back to a SYNCHRONOUS player-request caller (deposit_v2.go) so an
// interactive (redirect/hosted-field) pending outcome from a cascaded
// attempt is still returned to the player who made the original HTTP
// request - the sweeper (sweeperDriven=true) has no synchronous caller and
// simply ignores these two return values. Neither is persisted: exactly
// like the pre-cutover InitiateDeposit/attemptDeposit path, they are
// transient, this-request-only detail, never read back from deposit_intents.
func (o *Orchestrator) driveCreatedAttempt(
	ctx context.Context,
	pool *db.Pool,
	kycGate DepositKYCGate,
	credResolver OutboundCredentialResolver,
	intent DepositIntent,
	created PaymentAttempt,
	sweeperDriven bool,
) (DepositIntent, PaymentAttempt, *PaymentAttempt, string, string, error) {
	leaseOwner := "player-request-cascade"
	if sweeperDriven {
		leaseOwner = "sweeper"
	}

	// A0: route outside any tx, excluding every provider already tried.
	var candidates []ProviderCapability
	if err := pool.WithTenantReadOnly(ctx, intent.TenantID, func(actx context.Context, tx pgx.Tx) error {
		var err error
		candidates, err = ListRoutingCandidates(actx, tx, intent.TenantID, intent.BrandID)
		return err
	}); err != nil {
		return intent, created, nil, "", "", fmt.Errorf("payments: load routing candidates (cascade): %w", err)
	}
	routedProvider, routedCapability, routeErr := RankRoutingCandidates(ctx, candidates, o.providers, o.breaker, RoutingRequest{
		TenantID: intent.TenantID, BrandID: intent.BrandID, AssetCode: created.AssetCode,
		PaymentMethod: created.PaymentMethod, Amount: created.Amount, Operation: OperationDeposit,
		ExcludeProviderIDs: created.ExcludedProviderIDs,
	})

	var (
		attempt    PaymentAttempt
		claimed    bool
		claimToken uuid.UUID
		provider   PaymentProvider
		capability ProviderCapability
		manifest   OperationManifest
	)
	err := pool.WithTenant(ctx, intent.TenantID, func(actx context.Context, tx pgx.Tx) error {
		// RG + KYC deposit gate re-run BEFORE the parent lock (RV-0095
		// ledger N1: deposit gates run before L1, since they take no row
		// lock themselves, only rg's own L0.4 advisory lock).
		eligibility, err := rg.EvaluateEligibility(actx, tx, rg.EligibilityParams{
			TenantID: intent.TenantID, BrandID: intent.BrandID, PlayerAccountID: intent.PlayerAccountID, WalletID: intent.WalletID,
		})
		if err != nil {
			return fmt.Errorf("payments: evaluate rg eligibility (cascade T2): %w", err)
		}
		if !eligibility.Allowed {
			if err := audit.Record(actx, tx, audit.Entry{
				TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "payments.deposit_denied_by_rg",
				TargetType: "player_account", TargetID: intent.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{"reason_code": eligibility.Code, "deposit_intent_id": intent.ID.String(), "attempt_id": created.ID.String()},
			}); err != nil {
				return err
			}
			if err := RejectCreated(actx, tx, created.ID, EvidencePlatform, "rg_ineligible:"+eligibility.Code); err != nil {
				return err
			}
			intent, err = o.finalizeDeclined(actx, tx, intent, nil, nil, "rg_ineligible:"+eligibility.Code)
			return err
		}
		allowed, denyReason, err := kycGate.EvaluateDeposit(actx, tx, intent.TenantID, intent.BrandID, intent.PlayerAccountID, eligibility.PersonID, intent.Amount, intent.AssetCode)
		if err != nil {
			return fmt.Errorf("payments: evaluate deposit kyc gate (cascade T2): %w", err)
		}
		if !allowed {
			if err := audit.Record(actx, tx, audit.Entry{
				TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "payments.deposit_denied_by_kyc",
				TargetType: "player_account", TargetID: intent.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{"reason_code": denyReason, "deposit_intent_id": intent.ID.String(), "attempt_id": created.ID.String()},
			}); err != nil {
				return err
			}
			if err := RejectCreated(actx, tx, created.ID, EvidencePlatform, "kyc_required:"+denyReason); err != nil {
				return err
			}
			intent, err = o.finalizeDeclined(actx, tx, intent, nil, nil, "kyc_required:"+denyReason)
			return err
		}

		// Parent lock (ADR 0095 §14: parent before attempt, everywhere).
		if _, err := tx.Exec(actx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intent.ID); err != nil {
			return fmt.Errorf("payments: lock deposit intent (cascade T2): %w", err)
		}

		if routeErr != nil {
			if err := RejectCreated(actx, tx, created.ID, EvidencePlatform, "no_routable_provider"); err != nil {
				return err
			}
			intent, err = o.finalizeDeclined(actx, tx, intent, nil, nil, "no_routable_provider")
			return err
		}
		provider, capability = routedProvider, routedCapability
		manifest = provider.Capabilities().Manifest
		claimToken = uuid.New()
		if err := ClaimCreatedForSubmission(actx, tx, created.ID, capability.ProviderID, claimToken, leaseOwner, time.Now().Add(depositAttemptClaimLease)); err != nil {
			return err
		}
		claimed = true
		a, err := GetAttemptByID(actx, tx, created.ID)
		attempt = a
		return err
	})
	if err != nil {
		return intent, created, nil, "", "", err
	}
	if !claimed {
		// Rejected pre-call (RG/KYC deny, or no routable provider) - T3,
		// with the intent's projection already updated above in the same
		// tx. Re-read the attempt for the caller's own bookkeeping.
		final, gerr := getAttemptInTenant(ctx, pool, created.TenantID, created.ID)
		if gerr != nil {
			return intent, created, nil, "", "", gerr
		}
		return intent, final, nil, "", "", nil
	}

	// Phase B.
	in := callProviderInput{
		TenantID: attempt.TenantID, ProviderID: capability.ProviderID,
		AttemptState: attempt.State, ClaimToken: claimToken, ExpectedClaim: claimToken,
		IdempotencyKey: attempt.ExternalIdempotencyKey, Domain: "payments", Manifest: manifest,
	}
	gr := callProvider(ctx, credResolver, in, depositAdapterCall(provider, attempt, manifest))
	if gr.Attempted {
		o.breaker.RecordResult(attempt.TenantID, capability.ProviderID, gr.Class)
	}
	redirectURL, hostedFieldToken := gr.Value.RedirectURL, gr.Value.HostedFieldToken

	// Phase C, with a possible further cascade insert.
	var cascadeChild *PaymentAttempt
	err = pool.WithTenant(ctx, attempt.TenantID, func(actx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(actx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intent.ID); err != nil {
			return fmt.Errorf("payments: lock deposit intent: %w", err)
		}
		updated, child, err := o.applyDepositCallResult(actx, tx, intent, attempt, capability, claimToken, gr, EvidenceSync, sweeperDriven)
		intent = updated
		cascadeChild = child
		return err
	})
	if err != nil {
		return intent, attempt, nil, "", "", err
	}
	final, err := getAttemptInTenant(ctx, pool, attempt.TenantID, attempt.ID)
	if err != nil {
		return intent, attempt, nil, "", "", err
	}
	return intent, final, cascadeChild, redirectURL, hostedFieldToken, nil
}

// depositAdapterCall builds the AdapterCall closure Deposit's phase B
// uses - factored out so driveCreatedAttempt and InitiateDepositAttempt
// share the exact same outcome-classification rule (§4.4 precondition 3
// / §8), never two copies that could silently drift apart.
func depositAdapterCall(provider PaymentProvider, attempt PaymentAttempt, manifest OperationManifest) AdapterCall[DepositResult] {
	return func(callCtx context.Context, cc CallContext) (DepositResult, ErrorClass, error) {
		res, err := provider.Deposit(callCtx, DepositRequest{
			MerchantReference: attempt.MerchantReference, Amount: attempt.Amount,
			AssetCode: attempt.AssetCode, PaymentMethod: attempt.PaymentMethod,
		})
		if err != nil {
			return res, ErrorClassAmbiguous, err
		}
		switch res.Outcome {
		case OutcomePending:
			return res, ErrorClassPending, nil
		case OutcomeSucceeded:
			if !manifest.SyncSuccessPossible || res.ProviderReference == "" {
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

// applyDepositCallResult is phase C's evidence-application switch,
// shared by InitiateDepositAttempt's first attempt and
// driveCreatedAttempt's cascade/sweeper-driven attempts. Called with the
// intent's parent lock already held. Returns the updated intent and, on
// an eligible cascade decline, the newly inserted (still 'created')
// child attempt - inserted in THIS SAME tx (§4.6: "the tx that commits
// T8 also inserts attempt n+1"), never driven inline here.
func (o *Orchestrator) applyDepositCallResult(
	ctx context.Context, tx pgx.Tx,
	intent DepositIntent, attempt PaymentAttempt, capability ProviderCapability, claimToken uuid.UUID,
	gr GateResult[DepositResult], evidence EvidenceKind, sweeperDriven bool,
) (DepositIntent, *PaymentAttempt, error) {
	nextPoll := time.Now().Add(30 * time.Second)
	res := gr.Value

	switch gr.Class {
	case ErrorClassNotSent:
		return intent, nil, MarkNotSent(ctx, tx, attempt.ID, claimToken, nextPoll)

	case ErrorClassPending:
		if err := MarkAccepted(ctx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
			return intent, nil, err
		}
		if _, err := setIntentAttempt(ctx, tx, intent.ID, &capability.ProviderID, &res.ProviderReference, DepositIntentPending); err != nil {
			return intent, nil, err
		}
		intent.ProviderID, intent.ProviderReference, intent.Status = &capability.ProviderID, &res.ProviderReference, DepositIntentPending
		// RV-PRH-I1 ledger-finance H2 / S-Q2 (ADR 0095 §6.4): this attempt
		// JUST learned its provider_reference (T4, above) - a verified
		// success callback that raced this same phase C call and arrived
		// first would have been stored as a deferred, unresolved receipt
		// (this attempt had no provider_reference to resolve it against
		// yet). Applying every such deferred receipt for THIS attempt now,
		// under the parent+attempt locks this call already holds, is the
		// backstop that makes that race converge instead of leaving the
		// success permanently unapplied.
		attempt.ProviderID, attempt.ProviderReference = &capability.ProviderID, &res.ProviderReference
		if _, err := ApplyDeferredReceiptsForAttempt(ctx, tx, o, attempt); err != nil {
			return intent, nil, err
		}
		return intent, nil, nil

	case ErrorClassSucceeded:
		// postedTxID (never updated.LedgerTransactionID - PRH-I5 finding,
		// LF95-C6(a)/T13): a concurrent sibling could have already posted
		// the intent's FIRST capture between this attempt's own phase B
		// and this phase C, in which case updated.LedgerTransactionID
		// still names that first posting while THIS attempt's own
		// posting (a genuine T13 second capture) got its own, different
		// transaction id - linking the attempt to the wrong one would
		// collide with payment_attempts_tenant_ledger_tx.
		updated, postedTxID, err := o.postDepositSuccess(ctx, tx, intent, capability.ProviderID, res.ProviderReference, attempt.Amount, attempt.AssetCode)
		if err != nil {
			return intent, nil, err
		}
		if err := ApplySuccess(ctx, tx, attempt.ID, SuccessEvidence{
			Evidence: evidence, ProviderReference: res.ProviderReference, LedgerTransactionID: &postedTxID,
		}); err != nil {
			return intent, nil, err
		}
		// RV-PRH-I1 ledger-finance H4: this success may itself be a T13
		// second capture (a sibling cascade child left 'created' from a
		// PRIOR decline of a different sibling, driven concurrently) - any
		// such leftover 'created' sibling must never reach T2 and place a
		// second real PSP charge now that the intent has succeeded.
		if err := rejectCreatedSiblings(ctx, tx, attempt); err != nil {
			return intent, nil, err
		}
		return updated, nil, nil

	case ErrorClassDefiniteDecline:
		var refPtr *string
		if res.ProviderReference != "" {
			refPtr = &res.ProviderReference
		}
		reason := boundedDeclineReason(res.DeclineReason)
		updated, err := o.finalizeDeclined(ctx, tx, intent, &capability.ProviderID, refPtr, reason)
		if err != nil {
			return intent, nil, err
		}
		cascadable := res.Cascadable
		if err := ApplyDecline(ctx, tx, attempt.ID, DeclineEvidence{
			Evidence: evidence, Reason: reason, Stage: DeclineAtSubmission,
			Cascadable: &cascadable, ProviderRef: refPtr,
		}); err != nil {
			return intent, nil, err
		}
		if cascadeEligible(attempt, updated.Status, cascadable, o.maxCascadeDepth(), sweeperDriven) {
			child, err := insertCascadeAttemptIfEligible(ctx, tx, attempt)
			if err != nil {
				return updated, nil, err
			}
			return updated, child, nil
		}
		return updated, nil, nil

	default: // ErrorClassAmbiguous and anything unclassified
		var refPtr *string
		if res.ProviderReference != "" {
			refPtr = &res.ProviderReference
		}
		updated, err := o.finalizeAmbiguous(ctx, tx, intent, &capability.ProviderID, refPtr, "ambiguous")
		if err != nil {
			return intent, nil, err
		}
		if err := MarkAmbiguousFromSubmitting(ctx, tx, attempt.ID, evidence, nextPoll); err != nil {
			return updated, nil, err
		}
		return updated, nil, nil
	}
}

// getAttemptInTenant re-reads one attempt inside a fresh tenant-scoped
// transaction - a small convenience so callers outside a tx don't repeat
// the WithTenant boilerplate at every call site.
func getAttemptInTenant(ctx context.Context, pool *db.Pool, tenantID, attemptID uuid.UUID) (PaymentAttempt, error) {
	var a PaymentAttempt
	err := pool.WithTenant(ctx, tenantID, func(actx context.Context, tx pgx.Tx) error {
		var err error
		a, err = GetAttemptByID(actx, tx, attemptID)
		return err
	})
	return a, err
}
