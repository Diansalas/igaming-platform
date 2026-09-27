// PRH-I1 step (b): InitiateDepositAttempt is a NEW, ADDITIVE deposit
// entry point that runs the ADR 0095 §3.1 three-phase pattern
// (commit-intent -> call-without-tx -> commit-evidence) through
// migration 0101's payment_attempts state machine (attempt.go) and the
// provider-call gate (gate.go), for the SYNCHRONOUS player-request path
// only (§5.1's "Flow (player request)" row).
//
// THIS DOES NOT REPLACE InitiateDeposit. The existing InitiateDeposit/
// attemptDeposit/ReceiveVerifiedCallback path (orchestrator.go) is
// UNTOUCHED and remains the platform's actual deposit entry point -
// httpserver's deposit handler still calls it, and every existing test
// still exercises it. Cutting the real entry point over to this
// two-phase pattern (replacing InitiateDeposit, the webhook path's
// inline cascade, and the withdrawal dispatch/resolve handlers) is
// PRH-I1 steps (c) onward: the sweeper (which this step's deposits with
// no synchronous resolution silently depend on but which does not exist
// yet - see the NextActionAt residual note below), cascade-via-committed-
// attempt-row, and callback resolution via payment_attempts all need to
// exist first, or the cutover would silently orphan every non-
// synchronous outcome.
//
// What IS real and tested here: phase A commits the intent+attempt in
// one tx before any call; phase B makes the provider call with NO
// transaction and NO pooled connection held (proven by the INV-IO-1
// capture test in deposit_v2_integration_test.go); phase C applies the
// evidence via the SAME CAS functions migration 0101/attempt.go expose,
// under the same lock order phase A used.
//
// NextActionAt residual (explicit, not silently accepted): a pending or
// ambiguous outcome here sets payment_attempts.next_action_at so a future
// sweeper (step (c)) WILL be able to pick it up, but no sweeper exists in
// this step, so today such an attempt converges only via a later manual
// T17 touch plus a repeat of phase C's evidence application (there is no
// automatic re-poll yet). This is recorded, not hidden.
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

// depositAttemptClaimLease is how long a synchronous player-request claim
// holds its lease before a future sweeper would consider it abandoned.
// There is no sweeper yet (see package doc comment); this value only
// bounds what a later step's sweeper will see once it exists.
const depositAttemptClaimLease = 2 * time.Minute

// InitiateDepositAttemptResult is InitiateDepositAttempt's return value.
// Attempt is the zero value when phase A denied the request before any
// attempt was created (RG or KYC deny) - callers must check
// AttemptCreated, never assume Attempt.ID is valid.
type InitiateDepositAttemptResult struct {
	Intent         DepositIntent
	Attempt        PaymentAttempt
	AttemptCreated bool
}

// InitiateDepositAttempt runs phase A (commit-intent), phase B
// (call-without-tx) and phase C (commit-evidence) for one synchronous
// deposit request. pool.WithTenant is used for phases A and C; phase B
// runs with the ORIGINAL, unmarked ctx (never one obtained from inside a
// WithTenant callback), so txscope.Held(ctx) is false when the gate
// checks it - this is what INV-IO-1 actually depends on, not a
// convention comment.
func (o *Orchestrator) InitiateDepositAttempt(
	ctx context.Context,
	pool *db.Pool,
	kycGate DepositKYCGate,
	credResolver OutboundCredentialResolver,
	params InitiateDepositParams,
) (InitiateDepositAttemptResult, error) {
	if err := validateInitiateDepositParams(params); err != nil {
		return InitiateDepositAttemptResult{}, err
	}

	// --- Phase A: commit the intent to call (or a pre-call denial) ----
	var (
		intent         DepositIntent
		attempt        PaymentAttempt
		attemptCreated bool
		provider       PaymentProvider
		capability     ProviderCapability
		manifest       OperationManifest
		claimToken     uuid.UUID
	)
	err := pool.WithTenant(ctx, params.Scope.TenantID, func(actx context.Context, tx pgx.Tx) error {
		intentID := uuid.New()
		conflict, _, err := db.IdempotentInsert(actx, tx, func(spTx pgx.Tx) error {
			_, err := spTx.Exec(actx,
				`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				intentID, params.Scope.TenantID, params.Scope.BrandID, params.Scope.PlayerAccountID, params.Scope.WalletID,
				params.AssetCode, params.Amount, params.PaymentMethod, params.IdempotencyKey,
			)
			return err
		})
		if err != nil {
			return fmt.Errorf("payments: create deposit intent: %w", err)
		}
		if conflict {
			existing, found, err := loadDepositIntentByIdempotencyKey(actx, tx, params.Scope.TenantID, params.Scope.PlayerAccountID, params.IdempotencyKey)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("payments: idempotency conflict on deposit intent but no existing row found")
			}
			if existing.BrandID != params.Scope.BrandID || existing.WalletID != params.Scope.WalletID ||
				existing.AssetCode != params.AssetCode || existing.Amount != params.Amount || existing.PaymentMethod != params.PaymentMethod {
				return fmt.Errorf("%w: existing intent %s", ErrIdempotencyKeyReused, existing.ID)
			}
			// A retry against an existing intent resumes only when the
			// live attempt is still 'created' (§5.1); this step does not
			// yet implement resume-by-claiming a cascade row (no
			// cascade exists yet), so a retry against a live
			// submitting/pending/ambiguous attempt, or a terminal
			// intent, simply returns the current state with no new
			// phase B/C - the caller sees AttemptCreated=false and the
			// existing intent.
			intent = existing
			live, ok, err := ListLiveAttemptForDepositIntent(actx, tx, existing.ID)
			if err != nil {
				return err
			}
			if ok {
				attempt = live
			}
			return nil
		}

		intent = DepositIntent{
			ID: intentID, TenantID: params.Scope.TenantID, BrandID: params.Scope.BrandID,
			PlayerAccountID: params.Scope.PlayerAccountID, WalletID: params.Scope.WalletID,
			AssetCode: params.AssetCode, Amount: params.Amount, PaymentMethod: params.PaymentMethod,
			IdempotencyKey: params.IdempotencyKey, Status: DepositIntentPending,
		}

		if err := audit.Record(actx, tx, audit.Entry{
			TenantID: params.Scope.TenantID, ActorType: audit.ActorPlayer, ActorID: params.Scope.PlayerAccountID,
			Action: "deposit.requested", TargetType: "deposit_intent", TargetID: intentID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{"amount": params.Amount, "asset_code": params.AssetCode, "payment_method": params.PaymentMethod},
		}); err != nil {
			return fmt.Errorf("payments: audit deposit request: %w", err)
		}

		// RG, exactly as InitiateDeposit does today (ADR 0095 §4.3 T1+T2
		// runs RG "after RG" - the ADR's own ordering).
		eligibility, err := rg.EvaluateEligibility(actx, tx, rg.EligibilityParams{
			TenantID: intent.TenantID, BrandID: intent.BrandID, PlayerAccountID: intent.PlayerAccountID, WalletID: intent.WalletID,
		})
		if err != nil {
			return fmt.Errorf("payments: evaluate rg eligibility: %w", err)
		}
		if !eligibility.Allowed {
			if err := audit.Record(actx, tx, audit.Entry{
				TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "payments.deposit_denied_by_rg",
				TargetType: "player_account", TargetID: intent.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{"reason_code": eligibility.Code, "person_id": eligibility.PersonID.String(), "deposit_intent_id": intent.ID.String()},
			}); err != nil {
				return fmt.Errorf("payments: audit rg denial: %w", err)
			}
			intent, err = o.finalizeDeclined(actx, tx, intent, nil, nil, "rg_ineligible:"+eligibility.Code)
			return err
		}

		// KYC deposit gate (ADR 0096 seam - kycgate.go).
		allowed, denyReason, err := kycGate.EvaluateDeposit(actx, tx, intent.TenantID, intent.PlayerAccountID, intent.Amount, intent.AssetCode)
		if err != nil {
			return fmt.Errorf("payments: evaluate deposit kyc gate: %w", err)
		}
		if !allowed {
			if err := audit.Record(actx, tx, audit.Entry{
				TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "payments.deposit_denied_by_kyc",
				TargetType: "player_account", TargetID: intent.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{"reason_code": denyReason, "deposit_intent_id": intent.ID.String()},
			}); err != nil {
				return fmt.Errorf("payments: audit kyc denial: %w", err)
			}
			intent, err = o.finalizeDeclined(actx, tx, intent, nil, nil, "kyc_required:"+denyReason)
			return err
		}

		// Route. NOTE (residual, explicitly named): RouteProvider still
		// runs its health filter INSIDE this tx, which is a known,
		// pre-existing gap against §9.6's "outside a tx" requirement -
		// splitting ListRoutingCandidates/Rank fully is not repeated here
		// and stays open for the step that rebuilds routing/breaker
		// (§9.6) on top of this one.
		p, cap2, err := o.RouteProvider(actx, tx, RoutingRequest{
			TenantID: intent.TenantID, BrandID: intent.BrandID, AssetCode: intent.AssetCode,
			PaymentMethod: intent.PaymentMethod, Amount: intent.Amount, Operation: OperationDeposit,
		})
		if err != nil {
			intent, err = o.finalizeDeclined(actx, tx, intent, nil, nil, "no_routable_provider")
			return err
		}
		provider, capability = p, cap2

		claimToken = uuid.New()
		// Manifest is read from the ADAPTER's own declared capability
		// (ADR 0095 §10.1: "adapter-declared, code only, never tenant-
		// editable"), never from the tenant-configured provider_capabilities
		// row RouteProvider returns - that row has no manifest column yet
		// (capability persistence is a later PRH-I1 step).
		manifest = provider.Capabilities().Manifest
		a, err := InsertSubmittingAttempt(actx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: intent.TenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &intent.ID, ProviderID: capability.ProviderID,
			PaymentMethod: intent.PaymentMethod, AssetCode: intent.AssetCode, Amount: intent.Amount,
			Interactive: manifest.Interactive,
			ClaimToken:  claimToken, LeaseOwner: "player-request", LeaseUntil: time.Now().Add(depositAttemptClaimLease),
		})
		if err != nil {
			return err
		}
		attempt = a
		attemptCreated = true
		return nil
	})
	if err != nil {
		return InitiateDepositAttemptResult{}, err
	}
	if !attemptCreated {
		return InitiateDepositAttemptResult{Intent: intent, Attempt: attempt, AttemptCreated: false}, nil
	}

	// --- Phase B: the provider call, with NO transaction held ---------
	in := callProviderInput{
		TenantID: attempt.TenantID, ProviderID: capability.ProviderID,
		AttemptState: attempt.State, ClaimToken: claimToken, ExpectedClaim: claimToken,
		IdempotencyKey: attempt.ExternalIdempotencyKey, Domain: "payments", Manifest: manifest,
	}
	gr := callProvider(ctx, credResolver, in, func(callCtx context.Context, cc CallContext) (DepositResult, ErrorClass, error) {
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
				// §4.4 precondition 3 / §8: a manifest without
				// SyncSuccessPossible, or a success with no reference,
				// is never trusted as a definite synchronous success.
				return res, ErrorClassAmbiguous, nil
			}
			return res, ErrorClassSucceeded, nil
		case OutcomeDeclined:
			return res, ErrorClassDefiniteDecline, nil
		default:
			return res, ErrorClassAmbiguous, nil
		}
	})

	// --- Phase C: apply the evidence, CAS, in a fresh tx ---------------
	err = pool.WithTenant(ctx, attempt.TenantID, func(actx context.Context, tx pgx.Tx) error {
		// Lock parent before attempt (ADR 0095 §14).
		if _, err := tx.Exec(actx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intent.ID); err != nil {
			return fmt.Errorf("payments: lock deposit intent: %w", err)
		}

		nextPoll := time.Now().Add(30 * time.Second)
		res := gr.Value
		switch gr.Class {
		case ErrorClassNotSent:
			return MarkNotSent(actx, tx, attempt.ID, claimToken, nextPoll)

		case ErrorClassPending:
			if err := MarkAccepted(actx, tx, attempt.ID, EvidenceSync, res.ProviderReference, nextPoll); err != nil {
				return err
			}
			return setIntentAttempt(actx, tx, intent.ID, &capability.ProviderID, &res.ProviderReference, DepositIntentPending)

		case ErrorClassSucceeded:
			updated, err := o.postDepositSuccess(actx, tx, intent, capability.ProviderID, res.ProviderReference, attempt.Amount, attempt.AssetCode)
			if err != nil {
				return err
			}
			intent = updated
			return ApplySuccess(actx, tx, attempt.ID, SuccessEvidence{
				Evidence: EvidenceSync, ProviderReference: res.ProviderReference, LedgerTransactionID: intent.LedgerTransactionID,
			})

		case ErrorClassDefiniteDecline:
			var refPtr *string
			if res.ProviderReference != "" {
				refPtr = &res.ProviderReference
			}
			updated, err := o.finalizeDeclined(actx, tx, intent, &capability.ProviderID, refPtr, res.DeclineReason)
			if err != nil {
				return err
			}
			intent = updated
			cascadable := res.Cascadable
			return ApplyDecline(actx, tx, attempt.ID, DeclineEvidence{
				Evidence: EvidenceSync, Reason: res.DeclineReason, Stage: DeclineAtSubmission,
				Cascadable: &cascadable, ProviderRef: refPtr,
			})

		default: // ErrorClassAmbiguous and anything unclassified
			var refPtr *string
			if res.ProviderReference != "" {
				refPtr = &res.ProviderReference
			}
			updated, err := o.finalizeAmbiguous(actx, tx, intent, &capability.ProviderID, refPtr, "sync_ambiguous")
			if err != nil {
				return err
			}
			intent = updated
			return MarkAmbiguousFromSubmitting(actx, tx, attempt.ID, EvidenceSync, nextPoll)
		}
	})
	if err != nil {
		return InitiateDepositAttemptResult{}, err
	}

	updatedAttempt, err := func() (PaymentAttempt, error) {
		var a PaymentAttempt
		err := pool.WithTenant(ctx, attempt.TenantID, func(actx context.Context, tx pgx.Tx) error {
			var err error
			a, err = GetAttemptByID(actx, tx, attempt.ID)
			return err
		})
		return a, err
	}()
	if err != nil {
		return InitiateDepositAttemptResult{}, err
	}

	return InitiateDepositAttemptResult{Intent: intent, Attempt: updatedAttempt, AttemptCreated: true}, nil
}
