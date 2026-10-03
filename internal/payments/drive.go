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
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerref"
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
// like the pre-cutover InitiateDeposit/attemptDeposit path ([deleted by
// E2] - both are gone), they are transient, this-request-only detail,
// never read back from deposit_intents.
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
			// KS-DEP-T2-T3-1 (architect review rv-prh-i1-killswitch-phase2-
			// architect.md): ClaimCreatedForSubmission's CAS predicate
			// folds "kill switch engaged for this provider/operation" and
			// every OTHER zero-row cause (a race on state='created', the
			// sibling-succeeded guard, a provider_id mismatch) into the
			// SAME ErrAttemptStateConflict - the in-statement predicate is
			// what actually enforces the safety property atomically, so
			// this cannot and must not try to distinguish causes by
			// reparsing the CAS itself. KillSwitchEngaged is a SEPARATE,
			// read-only, best-effort classification read (its own doc
			// comment: "for orchestration code that needs to CHOOSE a
			// terminal reason... never a substitute for the in-statement
			// predicate") used ONLY to pick the terminal reason label - a
			// TOCTOU between the CAS and this read can at worst mislabel
			// the decline reason, never bypass the claim predicate itself.
			if !errors.Is(err, ErrAttemptStateConflict) {
				return err
			}
			engaged, kerr := KillSwitchEngaged(actx, tx, intent.TenantID, capability.ProviderID, AttemptOperationDeposit)
			if kerr != nil {
				return kerr
			}
			if !engaged {
				// A genuine, unrelated CAS conflict (a race, or the
				// sibling-succeeded guard) - preserve the pre-existing
				// behaviour: surface it as an error, never silently
				// reinterpret it as a kill-switch decline.
				return err
			}
			if err := audit.Record(actx, tx, audit.Entry{
				TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "payments.cascade_rejected_kill_switch",
				TargetType: "payment_attempt", TargetID: created.ID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{"provider_id": capability.ProviderID, "reason_code": "kill_switch", "deposit_intent_id": intent.ID.String()},
			}); err != nil {
				return err
			}
			if err := RejectCreated(actx, tx, created.ID, EvidencePlatform, "kill_switch"); err != nil {
				return err
			}
			intent, err = o.finalizeDeclined(actx, tx, intent, &capability.ProviderID, nil, "kill_switch")
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
	gr := callProvider(ctx, pool, credResolver, in, depositAdapterCall(provider, attempt, manifest))
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
		// Ledger-finance review F3 (rv-fh3-ledger.md, 076e42e): attempt was
		// read (line ~178) BEFORE phase B's outbound Deposit call and
		// BEFORE the parent lock just above - money-safe (CAS plus
		// INV-DEP-1 catch every stale decision regardless, confirmed by
		// the reviewer's own 25-rep concurrent storm), but a concurrent
		// callback that moved the attempt in the meantime made
		// applyDepositCallResult decide from a stale snapshot the CAS
		// predicates then reject as a plain error - noise today, and a
		// spurious error surfaced to a player whose deposit actually
		// succeeded. Re-read under the now-held lock, exactly as
		// ApplyReceiptEvidence and the sweeper's own identical fix
		// (processViaQueryStatus) already do.
		current, err := GetAttemptByID(actx, tx, attempt.ID)
		if err != nil {
			return err
		}
		updated, child, err := o.applyDepositCallResult(actx, tx, intent, current, capability, claimToken, gr, EvidenceSync, sweeperDriven)
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
	// PRH-2 C security C-1: the redirect/token are copied out of the UNSCRUBBED
	// adapter result before phase C decides, so a T10 park decided there
	// (reference conflict, sync amount mismatch) must not leave them with the
	// caller. Decided by the state phase C committed, not by which branch ran.
	redirectURL, hostedFieldToken = playerFacingRedirect(final, redirectURL, hostedFieldToken)
	return intent, final, cascadeChild, redirectURL, hostedFieldToken, nil
}

// playerFacingRedirect returns the PSP redirect URL / hosted-field token only
// for an attempt phase C left 'pending' (accepted by the provider, awaiting
// the player). For any other state - disputed (parked), declined, ambiguous,
// succeeded - the player must never be sent to a PSP session that the
// platform has refused to bind (a parked attempt's reference may belong to
// another player's deposit).
func playerFacingRedirect(a PaymentAttempt, redirectURL, hostedFieldToken string) (string, string) {
	if a.State != AttemptPending {
		return "", ""
	}
	return redirectURL, hostedFieldToken
}

// depositAdapterCall builds the AdapterCall closure Deposit's phase B
// uses - factored out so driveCreatedAttempt and InitiateDepositAttempt
// share the exact same outcome-classification rule (§4.4 precondition 3
// / §8), never two copies that could silently drift apart.
//
// PRH-2 C (PAY-DEP-REF-VALIDATE-1, PROVIDER-REF-BOUND-1 security condition
// C1, deposit half): the adapter-returned reference is validated with
// providerref.Validate BEFORE the outcome switch, exactly as payoutAdapterCall
// does - an invalid reference on ANY outcome returns ErrorClassProviderRefInvalid
// (phase C parks the attempt, T10) regardless of what outcome the adapter also
// reported, so phase C can never mistake "succeeded, but with a dangerous
// reference" for an ordinary success. A reference is REQUIRED on Pending
// (an empty one parks too, S-9: it would otherwise reach MarkAccepted and fail
// the payment_attempts CHECK as an untyped error); on every other outcome an
// empty reference is allowed here (a decline may carry none, and an empty
// reference on a sync success keeps the existing ambiguous path).
//
// An invalid result is returned SCRUBBED of the reference, redirect URL and
// hosted-field token: nothing from an unvalidated, parked response may be
// persisted, logged, or handed to the player.
//
// LF-5: a sync success also needs the provider's amount/asset echo; absent
// evidence classifies as Ambiguous (the status poll decides). A present but
// different echo is NOT decided here - it needs the transaction - phase C
// compares it (CompareProviderAmount) and takes T10 sync_amount_mismatch.
func depositAdapterCall(provider PaymentProvider, attempt PaymentAttempt, manifest OperationManifest) AdapterCall[DepositResult] {
	return func(callCtx context.Context, cc CallContext) (DepositResult, ErrorClass, error) {
		res, err := provider.Deposit(callCtx, DepositRequest{
			MerchantReference: attempt.MerchantReference, Amount: attempt.Amount,
			AssetCode: attempt.AssetCode, PaymentMethod: attempt.PaymentMethod,
		})
		// The reference is validated BEFORE the error return: an adapter that
		// returns an error together with a (hostile) reference must park like any
		// other outcome, never reach phase C's binding query or the intent write
		// raw (code review F1: it would hit the 0099 CHECK and loop forever).
		var verr error
		if err == nil && res.Outcome == OutcomePending {
			verr = providerref.Validate("deposit.provider_reference", res.ProviderReference)
		} else {
			verr = providerref.ValidateOptional("deposit.provider_reference", res.ProviderReference)
		}
		if verr != nil {
			return DepositResult{Outcome: res.Outcome}, ErrorClassProviderRefInvalid, verr
		}
		if err != nil {
			return res, ErrorClassAmbiguous, err
		}
		switch res.Outcome {
		case OutcomePending:
			return res, ErrorClassPending, nil
		case OutcomeSucceeded:
			if !manifest.SyncSuccessPossible || res.ProviderReference == "" ||
				CompareProviderAmount(attempt.Amount, attempt.AssetCode, res.Amount, res.AssetCode) == AmountEvidenceMissing {
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

// Terminal reasons PRH-2 C writes on a T10 (submitting -> disputed). None is
// one of migration 0107's declined->disputed reasons: those guard T13t/T13d
// only, and C's T10s start from the live 'submitting' state.
const (
	// TerminalReasonInvalidProviderReference: the adapter's reference failed
	// providerref.Validate (a ":<reason>" suffix names the closed reason).
	TerminalReasonInvalidProviderReference = "invalid_provider_reference"
	// TerminalReasonSyncAmountMismatch: a sync success echoed an amount or
	// asset different from the attempt's own record (LF-5). Nothing posted.
	TerminalReasonSyncAmountMismatch = "sync_amount_mismatch"
	// TerminalReasonProviderReferenceConflict: the reference the adapter
	// returned is already bound to another attempt or intent in the same
	// tenant (LF-6). Nothing posted; no error loop.
	TerminalReasonProviderReferenceConflict = "provider_reference_conflict"
)

// parkDepositAttempt is PRH-2 C's shared T10 for a live deposit attempt whose
// provider evidence cannot be trusted or bound: it moves the attempt
// submitting -> disputed with a terminal reason, writes the audit record, and
// recomputes the intent projection (a disputed attempt with no success makes
// the intent 'ambiguous', never 'declined', because funds may be captured -
// recomputeDepositIntentProjection, ADR 0095 §5.1). It NEVER posts to the
// ledger and never returns the failure as an error, so the surrounding
// transaction commits and the attempt can never loop in a retry/escalation
// cycle. Called under the intent lock phase C already holds.
func parkDepositAttempt(
	ctx context.Context, tx pgx.Tx, intent DepositIntent, attempt PaymentAttempt, providerID string,
	evidence EvidenceKind, reason string, adapterOutcome Outcome, bindRef string, extra map[string]any,
) (DepositIntent, error) {
	// bindRef (non-empty only for a validated, binding-checked reference, i.e.
	// the sync_amount_mismatch park) is bound while the attempt is still live,
	// so reconciliation can match the provider's record to this attempt.
	if bindRef != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE payment_attempts SET provider_reference = COALESCE(provider_reference, $2) WHERE id = $1 AND state IN ('submitting','pending','ambiguous')`,
			attempt.ID, bindRef); err != nil {
			return intent, fmt.Errorf("payments: bind reference on parked attempt: %w", err)
		}
	}
	if err := ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, evidence, reason); err != nil {
		return intent, err
	}
	meta := map[string]any{
		"terminal_reason": reason, "last_evidence_kind": string(evidence), "provider_id": providerID,
		"deposit_intent_id": intent.ID.String(), "amount": attempt.Amount, "asset_code": attempt.AssetCode,
		"adapter_outcome": string(adapterOutcome),
	}
	for k, v := range extra {
		meta[k] = v
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payment.attempt_disputed",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: meta,
	}); err != nil {
		return intent, fmt.Errorf("payments: audit parked deposit attempt (%s): %w", reason, err)
	}
	if err := recomputeDepositIntentProjection(ctx, tx, intent.ID); err != nil {
		return intent, err
	}
	updated, err := GetDepositIntentByID(ctx, tx, intent.ID)
	if err != nil {
		return intent, err
	}
	return updated, nil
}

// foreignReferenceBinding is LF-6's in-tx binding pre-check: is
// (tenantID, providerID, reference) already bound to a DIFFERENT payment
// attempt (deposit or payout - payment_attempts holds both operations) or to
// a different deposit intent? Same tenant only: the predicate names the
// tenant explicitly, and the unique indexes it mirrors
// (payment_attempts_tenant_provider_ref, idx_deposit_intents_tenant_provider_ref)
// are per tenant under FORCE RLS - there is no cross-tenant read, so another
// tenant's use of the same string is neither visible nor relevant. Without
// this check the collision surfaces only at the write that binds the
// reference (MarkAccepted, ApplyDecline, the intent projection, or
// ledger.Post), as a unique violation or ErrCallbackPayloadMismatch that rolls
// the transaction back and recurs forever. Must run under the intent lock.
func foreignReferenceBinding(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, reference string, attemptID, intentID uuid.UUID) (bool, string, error) {
	var op string
	err := tx.QueryRow(ctx,
		`SELECT operation FROM payment_attempts
		 WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3 AND id <> $4
		 LIMIT 1`,
		tenantID, providerID, reference, attemptID,
	).Scan(&op)
	if err == nil {
		return true, op, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("payments: check foreign reference binding (attempts): %w", err)
	}
	var one int
	err = tx.QueryRow(ctx,
		`SELECT 1 FROM deposit_intents
		 WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3 AND id <> $4
		 LIMIT 1`,
		tenantID, providerID, reference, intentID,
	).Scan(&one)
	if err == nil {
		return true, "deposit_intent", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("payments: check foreign reference binding (intents): %w", err)
	}
	return false, "", nil
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

	// PRH-2 C (S-9): an invalid or (on Pending) empty reference parks - T10,
	// never an error, never persisted. The scrubbed result carries no reference.
	if gr.Class == ErrorClassProviderRefInvalid {
		reason := TerminalReasonInvalidProviderReference
		extra := map[string]any{}
		if perr, ok := providerref.AsError(gr.Err); ok {
			reason += ":" + string(perr.Reason)
			extra["ref_field"], extra["ref_reason"], extra["ref_len"], extra["ref_sha256_prefix"] = perr.Field, string(perr.Reason), perr.Length, perr.HashPrefix
		}
		updated, err := parkDepositAttempt(ctx, tx, intent, attempt, capability.ProviderID, evidence, reason, res.Outcome, "", extra)
		return updated, nil, err
	}

	// PRH-2 C (LF-6): a reference already bound to another attempt/intent of
	// this tenant parks BEFORE any statement that would bind it (and so before
	// any posting) - T10, not the unique-violation error loop.
	if res.ProviderReference != "" && gr.Class != ErrorClassNotSent {
		conflict, boundOp, err := foreignReferenceBinding(ctx, tx, attempt.TenantID, capability.ProviderID, res.ProviderReference, attempt.ID, intent.ID)
		if err != nil {
			return intent, nil, err
		}
		if conflict {
			updated, err := parkDepositAttempt(ctx, tx, intent, attempt, capability.ProviderID, evidence,
				TerminalReasonProviderReferenceConflict, res.Outcome, "",
				map[string]any{"provider_reference": res.ProviderReference, "bound_to_operation": boundOp})
			return updated, nil, err
		}
	}

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
		// PRH-2 C (LF-5): the provider's own amount/asset echo must equal the
		// attempt's record BEFORE anything else decides to post. Absent evidence
		// never reaches here (depositAdapterCall classifies it Ambiguous); a
		// differing echo is a T10, with no posting and no error.
		if CompareProviderAmount(attempt.Amount, attempt.AssetCode, res.Amount, res.AssetCode) != AmountEvidenceMatch {
			updated, err := parkDepositAttempt(ctx, tx, intent, attempt, capability.ProviderID, evidence,
				TerminalReasonSyncAmountMismatch, res.Outcome, res.ProviderReference,
				map[string]any{"provider_reference": res.ProviderReference, "provider_amount": res.Amount, "provider_asset_code": res.AssetCode})
			return updated, nil, err
		}
		// RV-PRH-I1 ledger-finance N2: a reversal tombstone already
		// occupying (provider_id, provider_reference) must route to T10
		// (ADR 0095 §4.3's own "…T10 instead, with no posting and no
		// error"), never straight into postDepositSuccess - that call's
		// own ledger idempotency-key insert would otherwise hit the
		// tombstone's unique index and surface as an untyped error, which
		// the caller cannot distinguish from a real failure and which
		// retries identically forever. This attempt is always 'submitting'
		// here (phase C dispatches immediately after ClaimCreatedForSubmission),
		// so the non-terminal dispute path applies, exactly as the receipt
		// path's OutcomeSucceeded/{submitting,pending,ambiguous} cell does.
		if res.ProviderReference != "" {
			tombstoned, err := tombstoneExists(ctx, tx, attempt.TenantID, capability.ProviderID, res.ProviderReference)
			if err != nil {
				return intent, nil, err
			}
			if tombstoned {
				// Unified (LF F-C2): same audit and intent recompute as every C park.
				updated, err := parkDepositAttempt(ctx, tx, intent, attempt, capability.ProviderID, evidence,
					TerminalReasonTombstonePrecedesSuccess, res.Outcome, "",
					map[string]any{"provider_reference": res.ProviderReference})
				return updated, nil, err
			}
		}
		// ADR 0095 §28.3: routed through the choke-point wrapper, which
		// checks resolved_for_other(I, A, K) BEFORE ever posting - a
		// success for an intent already financially resolved by ANOTHER
		// attempt or posting takes T10 (this attempt is always
		// 'submitting' here) instead of Flow 1. postedTxID (never
		// updated.LedgerTransactionID - PRH-I5 finding, LF95-C6(a)/T13):
		// a concurrent sibling could have already posted the intent's
		// FIRST capture between this attempt's own phase B and this
		// phase C, in which case updated.LedgerTransactionID still names
		// that first posting while THIS attempt's own posting (the
		// intent's first-ever capture, since resolved_for_other above
		// already refused anything else) got its own, different
		// transaction id - linking the attempt to the wrong one would
		// collide with payment_attempts_tenant_ledger_tx.
		updated, postedTxID, disputed, err := o.postDepositSuccessOrDispute(ctx, tx, intent, attempt, capability.ProviderID, res.ProviderReference, attempt.Amount, attempt.AssetCode, evidence)
		if err != nil {
			return intent, nil, err
		}
		if disputed {
			return intent, nil, nil
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
		if err := rejectCreatedSiblings(ctx, tx, attempt, evidence); err != nil {
			return intent, nil, err
		}
		return updated, nil, nil

	case ErrorClassDefiniteDecline:
		var refPtr *string
		if res.ProviderReference != "" {
			refPtr = &res.ProviderReference
		}
		reason, err := boundedDeclineReasonAudited(ctx, tx, attempt.TenantID, "payment_attempt", attempt.ID.String(), capability.ProviderID, res.DeclineReason)
		if err != nil {
			return intent, nil, err
		}
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
		if err := MarkAmbiguousFromSubmittingBindingRef(ctx, tx, attempt.ID, evidence, res.ProviderReference, nextPoll); err != nil {
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
