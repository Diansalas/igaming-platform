// Stage 4H-B1 Wave 3 Phase 3 (item E): application-level four-eyes
// wiring for four of the reconnaissance's own seven un-consumed
// ChangeOperation values (docs/governance/wave-3-reconnaissance.md
// gap-list item 32 - "only held_disposition_resolve actually calls
// ConsumeApprovedChangeRequest in application code today"). Uses the
// EXISTING governance model exactly (change_governance.go's
// FileChangeRequest/RecordChangeApproval/ConsumeApprovedChangeRequest,
// migration 0063's DB-enforced dual-control/SEP-1 triggers) - never a
// parallel model, mirroring held_disposition_ops.go's own
// ResolveHeldDispositionAction as this file's own template exactly.
//
// The three REMAINING operations (bonus_adjustment_write,
// grant_forced_conversion, grant_cancel_completed) are DELIBERATELY NOT
// wired here - each requires genuinely NEW business logic (no existing
// function performs a manual balance adjustment, a forced conversion
// override, or cancellation of an already-completed Grant) with an
// undetermined posting shape / design boundary the reconnaissance's own
// dependency map explicitly routes to ledger-finance (posting shape) and
// architect (the forced-conversion AOE-override scope question) BEFORE
// bonus-engine implements - not merely missing HTTP wiring. Wiring a
// four-eyes gate around a function that does not yet exist, or whose
// posting shape is undetermined, would either be a no-op or would force
// an undisclosed design decision into this file. Per the dispatch's own
// instruction ("if you find you need a cross-domain contract change...
// STOP, document it precisely, and do not implement it yourself"), these
// three are named here as NOT IMPLEMENTED / BLOCKED, not silently
// narrowed or half-built.
package bonus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/economicop"
)

// resolveRequiredApprovals is the one place every wrapper in this file
// resolves its own four-eyes threshold, via the EXISTING
// ResolveApprovalPolicy (change_governance.go) - never a caller-supplied
// value (a caller cannot be trusted to supply its own threshold; this
// mirrors resolveJurisdictionID/resolveLicensingMode's own "resolved by
// this package, not the caller" posture for T.1's gate parameters).
func resolveRequiredApprovals(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, operation ChangeOperation, brandID *uuid.UUID, assetCode *string) (int32, error) {
	policy, err := ResolveApprovalPolicy(ctx, tx, tenantID, operation, brandID, assetCode)
	if err != nil {
		return 0, err
	}
	return policy.RequiredApprovals, nil
}

// --- campaign_activate ---

// ActivateCampaign is doc 10's own "activate a Campaign" transition,
// FOUR-EYES GATED (security-architecture.md §B1.2's campaign_activate
// operation) - the reconnaissance's own finding that "UpdateCampaignStatus
// itself has no such gate today" (gap-list item 1) is closed here, via a
// NEW wrapper (UpdateCampaignStatus itself is left unconditional, exactly
// as Phase 2 built it - this wrapper is the only caller that should ever
// reach it for an activate transition in production).
//
// payloadMatch is the fixed `{"action":"activate"}` object - a Campaign
// activation carries no other parameters, so this is the entire
// identifying payload a filed bonus_change_requests row for this
// Campaign must contain (JSONB containment, migration 0063's own
// consume function - `payload @> payload_match`).
var campaignActivatePayloadMatch = []byte(`{"action":"activate"}`)

func ActivateCampaign(ctx context.Context, tx pgx.Tx, tenantID, campaignID, actorID uuid.UUID) (Campaign, error) {
	c, err := GetCampaignByID(ctx, tx, campaignID)
	if err != nil {
		return Campaign{}, err
	}
	requiredApprovals, err := resolveRequiredApprovals(ctx, tx, tenantID, ChangeOpCampaignActivate, c.BrandID, nil)
	if err != nil {
		return Campaign{}, err
	}
	if _, err := ConsumeApprovedChangeRequest(ctx, tx, tenantID, ChangeOpCampaignActivate, campaignID, campaignActivatePayloadMatch, requiredApprovals, actorID); err != nil {
		return Campaign{}, err
	}
	activated, err := UpdateCampaignStatus(ctx, tx, tenantID, campaignID, CampaignActive)
	if err != nil {
		return Campaign{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actorID,
		Action: "bonus_campaign.activated", TargetType: "bonus_campaign", TargetID: campaignID.String(), Outcome: audit.OutcomeSuccess,
	}); err != nil {
		return Campaign{}, err
	}
	return activated, nil
}

// --- offer_publish ---

// PublishOfferVersion is doc 10's own "publish an Offer/OfferVersion"
// transition, FOUR-EYES GATED (security-architecture.md §B1.2's
// offer_publish operation) - closes gap-list item 2's "offer_publish's
// own ChangeOperation is declared... but has zero Go call site consuming
// it". Pins the Offer's own current_version_id and flips it Active in
// the SAME transaction as the four-eyes consume, atomically - a Grant
// issued against this Offer immediately afterward is guaranteed to see
// the newly-published version, never a stale one (no window where the
// consume has succeeded but the version pin has not yet happened).
func PublishOfferVersion(ctx context.Context, tx pgx.Tx, tenantID, offerID, offerVersionID, actorID uuid.UUID) (Offer, error) {
	o, err := GetOfferByID(ctx, tx, offerID)
	if err != nil {
		return Offer{}, err
	}
	ov, err := GetOfferVersionByID(ctx, tx, offerVersionID)
	if err != nil {
		return Offer{}, err
	}
	if ov.OfferID != offerID {
		return Offer{}, fmt.Errorf("bonus: offer version %s does not belong to offer %s", offerVersionID, offerID)
	}
	assetCode := ov.RewardAssetCode
	requiredApprovals, err := resolveRequiredApprovals(ctx, tx, tenantID, ChangeOpOfferPublish, o.BrandID, &assetCode)
	if err != nil {
		return Offer{}, err
	}
	payloadMatch := []byte(fmt.Sprintf(`{"offer_version_id":%q}`, offerVersionID))
	if _, err := ConsumeApprovedChangeRequest(ctx, tx, tenantID, ChangeOpOfferPublish, offerID, payloadMatch, requiredApprovals, actorID); err != nil {
		return Offer{}, err
	}
	if _, err := SetOfferCurrentVersion(ctx, tx, tenantID, offerID, offerVersionID); err != nil {
		return Offer{}, err
	}
	published, err := UpdateOfferStatus(ctx, tx, tenantID, offerID, OfferActive)
	if err != nil {
		return Offer{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actorID,
		Action: "bonus_offer.published", TargetType: "bonus_offer", TargetID: offerID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{"offer_version_id": offerVersionID.String()},
	}); err != nil {
		return Offer{}, err
	}
	return published, nil
}

// --- manual_grant_issue ---

// manualGrantIssuePayloadMatch is the deterministic subset of a manual
// grant's own defining fields a filed bonus_change_requests row
// (operation=manual_grant_issue) must carry, and this wrapper's own
// payloadMatch is built from identically - so a caller cannot request
// approval for one Grant shape and then apply it against a DIFFERENT one
// (the exact "actor/subject-substitution"-class attack SEC-W15-02 names,
// closed here the same way held_disposition_ops.go's own
// `{"action":%q}` payload match closes it for ACTION_REFORFEIT/
// ACTION_ROUTE_TO_CASH).
func manualGrantIssuePayloadMatch(playerAccountID, offerVersionID uuid.UUID, assetCode string, amount *big.Int) []byte {
	return []byte(fmt.Sprintf(`{"player_account_id":%q,"offer_version_id":%q,"asset_code":%q,"amount":%q}`,
		playerAccountID.String(), offerVersionID.String(), assetCode, amount.String()))
}

// IssueManualGrantRequest performs doc 10's "(none) -> issued" transition
// for a manual grant - deliberately WITHOUT any four-eyes/EOI-budget
// consumption (T.2: "issued is a decision, not a movement" - no value has
// moved yet, so there is nothing to four-eyes-gate at this step).
//
// NAMED, LOAD-BEARING DESIGN CONSTRAINT this split exists to satisfy
// (found while wiring this, not assumed): migration 0063's own SEP-1
// trigger (bonus_change_approvals_enforce_separation) resolves a
// manual_grant_issue request's beneficiary by joining bonus_grants ON
// g.id = bonus_change_requests.target_id - it REQUIRES a real,
// already-existing bonus_grants row at the moment an APPROVAL is
// recorded (RecordChangeApproval), not merely at consume time. A
// single-call "file the request against a not-yet-existent Grant" design
// (this file's own first, INCORRECT attempt) fails this trigger outright
// (SEP-1 refuses with "beneficiary Person could not be resolved").
// Issuing the Grant FIRST - a decision-only, non-value-moving step T.2
// already permits unconditionally - gives the four-eyes request a real
// target_id to name before anyone ever approves it. Callers MUST file
// the bonus_change_requests row (operation=manual_grant_issue,
// target_type="bonus_grants", target_id=this function's own returned
// Grant.ID, payload built via manualGrantIssuePayloadMatch) and have it
// approved AFTER calling this function and BEFORE calling
// ActivateManualGrantWithApproval below.
func IssueManualGrantRequest(ctx context.Context, tx pgx.Tx, g Grant, parentOperationID uuid.UUID, actorID uuid.UUID) (Grant, GateOutcome, error) {
	if _, err := economicop.CheckEntry(ctx, tx, g.TenantID, parentOperationID, g.AssetCode); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	g.ParentOperationID = &parentOperationID
	g.CreatedByActorType = ActorStaff
	g.CreatedByActorID = actorID
	return issueIdempotent(ctx, tx, IssueGrantParams{Grant: g})
}

// ActivateManualGrantWithApproval performs doc 10's "issued -> activated"
// transition for a manual grant, gated by BOTH controls in series -
// neither replacing the other, exactly doc 34 §5.3's own composition
// rule ("a NEW gate is added in series, never by loosening or reordering
// an existing one"):
//  1. The four-eyes consume THIS Wave adds (ConsumeApprovedChangeRequest,
//     operation=manual_grant_issue, target_id=grantID) - closing gap-list
//     item 32's finding that "manual grant issuance above threshold
//     requires four-eyes" was not enforced by any code path reachable
//     today.
//  2. The EOI locking budget consume that ALREADY existed
//     (economicop.ConsumeRootBudget, unchanged) - a distinct control from
//     four-eyes governance (doc 34's own lineage/budget mechanism).
//
// Both run inside ActivateGrant's own PostGateHook seam - strictly after
// the T.1 gate chain, strictly before the effecting ledger write (doc 34
// §5.3 rule 3/doc 10 N2.4a, DR-4HB1W2-01's own fix, unchanged and
// respected here exactly as targeting.go's IssueSingleManualGrant already
// does for the EOI half alone).
func ActivateManualGrantWithApproval(ctx context.Context, tx pgx.Tx, tenantID, grantID, parentOperationID uuid.UUID, actorID uuid.UUID, amount *big.Int) (Grant, GateOutcome, error) {
	g, err := GetGrantByID(ctx, tx, grantID)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	requiredApprovals, err := resolveRequiredApprovals(ctx, tx, tenantID, ChangeOpManualGrantIssue, &g.BrandID, &g.AssetCode)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	payloadMatch := manualGrantIssuePayloadMatch(g.PlayerAccountID, g.OfferVersionID, g.AssetCode, amount)
	playerAccountID := g.PlayerAccountID

	postGateHook := func(hookCtx context.Context, hookTx pgx.Tx) error {
		if _, err := ConsumeApprovedChangeRequest(hookCtx, hookTx, tenantID, ChangeOpManualGrantIssue, grantID, payloadMatch, requiredApprovals, actorID); err != nil {
			return err
		}
		op, getErr := economicop.GetByID(hookCtx, hookTx, parentOperationID)
		if getErr != nil {
			return getErr
		}
		return economicop.ConsumeRootBudget(hookCtx, hookTx, tenantID, op.RootOperationID, economicop.OperationBonusManualGrant, playerAccountID, amount)
	}

	return ActivateGrant(ctx, tx, tenantID, grantID, ActivateGrantParams{
		ActorType: ActorStaff, ActorID: actorID, Amount: amount,
		PostGateHook: postGateHook,
	})
}

// --- bulk_job_execute ---

// ExecuteBulkGrantJobResult reports what RunStaticBulkGrantJob's own
// per-item outcomes rolled up to, for the caller's own status write.
type ExecuteBulkGrantJobResult struct {
	Status BulkGrantJobStatus
	Job    BulkGrantJob
}

// BulkJobExecutePayloadMatch is the deterministic, ECONOMIC payload a
// filed bonus_change_requests row (operation=bulk_job_execute) must carry
// for ExecuteBulkGrantJobWithApproval to consume it.
//
// FIX (Stage 4H-B1 Wave 3 Phase 6, `security`): this replaces the fixed
// literal `{"action":"execute"}` this wrapper previously used, which
// pinned NOTHING about what was actually being approved. The
// per-recipient `amount` is chosen by the EXECUTING caller (the HTTP
// request body), never by the approver, so an approval obtained for a
// modest bulk grant authorized an arbitrarily larger one - the
// payload-substitution shape of SEC-W15-02 - and an approval obtained for
// one recipient list authorized execution against a different, larger one
// (SEC-W15-02's pagination-laundering shape; `bulk_grant_jobs` carries no
// DB-level immutability trigger on `target_player_list`, migration 0060,
// so the job row alone does not bind the approval either). Binding all
// four of (offer version, asset, per-recipient amount, recipient set) into
// the approval payload is exactly what migration 0063's own header already
// required of this operation ("for a bulk job it pins the recipient-set
// hash") and what manualGrantIssuePayloadMatch already does for the
// single-grant surface - this is that same, existing pattern applied to
// the surface that was missing it, never a new governance model.
//
// recipient_set_hash is SHA-256 over the recipient player_account_ids,
// lowercased, de-duplicated, sorted ascending and joined with "\n" - a
// stable, order-independent content hash a filing client can reproduce
// exactly (doc 34 §2.2's `subject_set_hash`: "a content hash pinning the
// authorized subject set").
func BulkJobExecutePayloadMatch(offerVersionID uuid.UUID, assetCode string, amountPerRecipient *big.Int, playerAccountIDs []uuid.UUID) []byte {
	amount := "0"
	if amountPerRecipient != nil {
		amount = amountPerRecipient.String()
	}
	unique := make(map[string]struct{}, len(playerAccountIDs))
	for _, id := range playerAccountIDs {
		unique[strings.ToLower(id.String())] = struct{}{}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))

	payload, err := json.Marshal(struct {
		Action             string `json:"action"`
		OfferVersionID     string `json:"offer_version_id"`
		AssetCode          string `json:"asset_code"`
		AmountPerRecipient string `json:"amount_per_recipient"`
		RecipientCount     int    `json:"recipient_count"`
		RecipientSetHash   string `json:"recipient_set_hash"`
	}{
		Action: "execute", OfferVersionID: offerVersionID.String(), AssetCode: assetCode,
		AmountPerRecipient: amount, RecipientCount: len(ids), RecipientSetHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		// Unreachable (every field is a plain string/int). Returning a
		// payload that can never match is the fail-closed outcome if it
		// somehow were reached, never a permissive one.
		return []byte(`{"action":"unmatchable"}`)
	}
	return payload
}

// ExecuteBulkGrantJobWithApproval wraps RunStaticBulkGrantJob
// (targeting.go, unchanged) with the four-eyes consume this Wave adds -
// closing gap-list item 32's finding that "bulk job execution always
// requires four-eyes" was not enforced by any code path reachable today.
//
// NAMED, DISCLOSED ENGINEERING DECISION (ordinary, reversible - CLAUDE.md's
// own "make the call yourself" latitude, not a Human Decision Register
// item): this function executes the job SYNCHRONOUSLY, inline, in the
// caller's own transaction - the reconnaissance's own dependency map
// flagged "inline vs. background-worker execution for potentially large
// jobs" as needing architect's sign-off on the worker-lifecycle SHAPE,
// but RunStaticBulkGrantJob's own resumability (per-item idempotency via
// bulk_grant_job_items' unique constraint) already makes a synchronous
// caller safe to retry after a timeout/crash - a future background-worker
// invocation mechanism can call the IDENTICAL RunStaticBulkGrantJob
// unchanged, this function's own four-eyes-consume-then-execute
// sequencing needs no rework when that lands. Named here, not silently
// presented as the final production shape for an arbitrarily large job.
func ExecuteBulkGrantJobWithApproval(ctx context.Context, tx pgx.Tx, tenantID, jobID, actorID uuid.UUID, target StaticPlayerListTarget, grantTemplate Grant, systemActorID uuid.UUID, amount *big.Int) (ExecuteBulkGrantJobResult, error) {
	job, err := GetBulkGrantJobByID(ctx, tx, jobID)
	if err != nil {
		return ExecuteBulkGrantJobResult{}, err
	}
	assetCode := grantTemplate.AssetCode
	requiredApprovals, err := resolveRequiredApprovals(ctx, tx, tenantID, ChangeOpBulkJobExecute, &job.BrandID, &assetCode)
	if err != nil {
		return ExecuteBulkGrantJobResult{}, err
	}
	payloadMatch := BulkJobExecutePayloadMatch(job.OfferVersionID, assetCode, amount, target.PlayerAccountIDs)
	if _, err := ConsumeApprovedChangeRequest(ctx, tx, tenantID, ChangeOpBulkJobExecute, jobID, payloadMatch, requiredApprovals, actorID); err != nil {
		return ExecuteBulkGrantJobResult{}, err
	}

	running, err := UpdateBulkGrantJobStatus(ctx, tx, tenantID, jobID, BulkJobRunning)
	if err != nil {
		return ExecuteBulkGrantJobResult{}, err
	}

	if err := RunStaticBulkGrantJob(ctx, tx, running, target, grantTemplate, systemActorID, amount); err != nil {
		if _, failErr := UpdateBulkGrantJobStatus(ctx, tx, tenantID, jobID, BulkJobFailed); failErr != nil {
			return ExecuteBulkGrantJobResult{}, fmt.Errorf("bonus: record bulk job failure (job=%s): %w (original error: %v)", jobID, failErr, err)
		}
		return ExecuteBulkGrantJobResult{}, err
	}

	items, err := ListBulkGrantJobItems(ctx, tx, tenantID, jobID)
	if err != nil {
		return ExecuteBulkGrantJobResult{}, err
	}
	finalStatus := BulkJobCompleted
	for _, it := range items {
		if it.Outcome == ItemDenied || it.Outcome == ItemError {
			finalStatus = BulkJobPartiallyCompleted
			break
		}
	}
	final, err := UpdateBulkGrantJobStatus(ctx, tx, tenantID, jobID, finalStatus)
	if err != nil {
		return ExecuteBulkGrantJobResult{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actorID,
		Action: "bulk_grant_job.executed", TargetType: "bulk_grant_job", TargetID: jobID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{"final_status": string(finalStatus), "item_count": len(items)},
	}); err != nil {
		return ExecuteBulkGrantJobResult{}, err
	}
	return ExecuteBulkGrantJobResult{Status: final.Status, Job: final}, nil
}
