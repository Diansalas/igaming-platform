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
// [deleted by E2, 2026-09-28] InitiateDeposit/attemptDeposit are gone
// (PROV-OUTBOUND-CRED-1-LEGACY-PATH). InitiateDepositAttempt (this file)
// is now the ONLY deposit-creation entry point in production; the
// httpserver deposit handler calls it exclusively. The paragraph above is
// left as a historical record of this step's own additive framing at the
// time it was written, not a description of the current call graph.
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
	"errors"
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
	// RedirectURL/HostedFieldToken are phase B's DepositResult redirect
	// detail for THIS synchronous call only (whichever attempt - the
	// first, or the last cascaded one - ended up interactive/pending) -
	// transient, never persisted to deposit_intents, mirroring the
	// pre-cutover InitiateDeposit/attemptDeposit path's identical
	// in-memory-only fields ([deleted by E2] - both are gone).
	RedirectURL      string
	HostedFieldToken string
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

	// --- Phase A0: load routing candidates (read-only tx), then rank ---
	// OUTSIDE any transaction (ADR 0095 §9.6/§5.1: "health filter outside
	// the tx"). RankRoutingCandidates takes no tx parameter at all, so
	// this is a structural guarantee, not a comment - see breaker.go and
	// RankRoutingCandidates' own doc comment.
	var candidates []ProviderCapability
	if err := pool.WithTenantReadOnly(ctx, params.Scope.TenantID, func(actx context.Context, tx pgx.Tx) error {
		var err error
		candidates, err = ListRoutingCandidates(actx, tx, params.Scope.TenantID, params.Scope.BrandID)
		return err
	}); err != nil {
		return InitiateDepositAttemptResult{}, fmt.Errorf("payments: load routing candidates: %w", err)
	}
	routedProvider, routedCapability, routeErr := RankRoutingCandidates(ctx, candidates, o.providers, o.breaker, RoutingRequest{
		TenantID: params.Scope.TenantID, BrandID: params.Scope.BrandID, AssetCode: params.AssetCode,
		PaymentMethod: params.PaymentMethod, Amount: params.Amount, Operation: OperationDeposit,
	})

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

		// RG, exactly as the now-deleted InitiateDeposit used to
		// ([deleted by E2]) (ADR 0095 §4.3 T1+T2 runs RG "after RG" -
		// the ADR's own ordering).
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
		allowed, denyReason, err := kycGate.EvaluateDeposit(actx, tx, intent.TenantID, intent.BrandID, intent.PlayerAccountID, eligibility.PersonID, intent.Amount, intent.AssetCode)
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

		// Routing was already decided in phase A0, entirely outside any
		// transaction - this tx only ever reads the RESULT.
		if routeErr != nil {
			intent, err = o.finalizeDeclined(actx, tx, intent, nil, nil, "no_routable_provider")
			return err
		}
		provider, capability = routedProvider, routedCapability

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
			if errors.Is(err, ErrKillSwitchEngaged) {
				// Phase 2 orchestrator wiring (ADR 0095 §10.3/§10.5): a
				// kill-switch refusal at T1+T2 is never a caller-visible
				// 500 - it finalizes the intent as a clean, terminal
				// decline (T3) with reason "kill_switch", in this SAME
				// transaction (no attempt row exists to roll back; the
				// INSERT...SELECT's own WHERE predicate simply matched
				// zero rows, an ordinary result, not a Postgres error -
				// see InsertSubmittingAttempt's own doc comment). The
				// player-facing response stays generic: depositIntentResponse
				// never surfaces decline_reason, only status="declined" -
				// the "kill_switch" label lives only in
				// deposit.declined's own audit metadata, for operators.
				//
				// C5 (RV-PRH-I1 kill-switch phase 2 code review): the
				// provider capability we had just routed to (BEFORE the
				// kill-switch claim refused it) is passed as providerID,
				// exactly like every other post-routing decline
				// (drive.go/orchestrator.go/receipt.go/sweeper.go's own
				// finalizeDeclined calls that already had a resolved
				// capability) - the operator can see WHICH provider's
				// switch fired, wildcard and provider-scoped alike, both
				// in deposit_intents.provider_id and in the audit row's
				// own metadata.provider_id. No provider_reference exists
				// (no call was ever made), so that stays nil.
				intent, err = o.finalizeDeclined(actx, tx, intent, &capability.ProviderID, nil, "kill_switch")
				return err
			}
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
	gr := callProvider(ctx, pool, credResolver, in, depositAdapterCall(provider, attempt, manifest))

	// Feed the breaker (§9.6) - plain in-memory, no tx, no I/O - but only
	// for a result that actually reached the adapter's own transport
	// (gr.Attempted), never for a gate-level pre-flight refusal.
	if gr.Attempted {
		o.breaker.RecordResult(attempt.TenantID, capability.ProviderID, gr.Class)
	}
	redirectURL, hostedFieldToken := gr.Value.RedirectURL, gr.Value.HostedFieldToken

	// --- Phase C: apply the evidence, CAS, in a fresh tx ---------------
	var cascadeChild *PaymentAttempt
	err = pool.WithTenant(ctx, attempt.TenantID, func(actx context.Context, tx pgx.Tx) error {
		// Lock parent before attempt (ADR 0095 §14).
		if _, err := tx.Exec(actx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intent.ID); err != nil {
			return fmt.Errorf("payments: lock deposit intent: %w", err)
		}
		// Ledger-finance review F3 (rv-fh3-ledger.md, 076e42e): re-read
		// under the now-held lock for consistency with driveCreatedAttempt's
		// and the sweeper's own identical fix, even though the race window
		// this closes is narrower here - this is the FIRST attempt on a
		// brand-new intent, so no external evidence could target it before
		// phase B (just above) ever returns its provider_reference.
		current, err := GetAttemptByID(actx, tx, attempt.ID)
		if err != nil {
			return err
		}
		updated, child, err := o.applyDepositCallResult(actx, tx, intent, current, capability, claimToken, gr, EvidenceSync, false)
		intent = updated
		cascadeChild = child
		return err
	})
	if err != nil {
		return InitiateDepositAttemptResult{}, err
	}

	updatedAttempt, err := getAttemptInTenant(ctx, pool, attempt.TenantID, attempt.ID)
	if err != nil {
		return InitiateDepositAttemptResult{}, err
	}

	// §4.6 case (a): a cascade-eligible SYNCHRONOUS decline is driven
	// immediately by this same request's own driver - a bounded loop
	// across cascade attempts, each its own A0/T2/B/C cycle, never an
	// inline call inside the tx that just committed the previous
	// decline.
	for cascadeChild != nil {
		var driven *PaymentAttempt
		intent, updatedAttempt, driven, redirectURL, hostedFieldToken, err = o.driveCreatedAttempt(ctx, pool, kycGate, credResolver, intent, *cascadeChild, false)
		if err != nil {
			return InitiateDepositAttemptResult{}, err
		}
		cascadeChild = driven
	}

	return InitiateDepositAttemptResult{
		Intent: intent, Attempt: updatedAttempt, AttemptCreated: true,
		RedirectURL: redirectURL, HostedFieldToken: hostedFieldToken,
	}, nil
}
