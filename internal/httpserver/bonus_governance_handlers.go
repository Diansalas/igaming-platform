// Stage 4H-B1 Wave 3 Phase 3 (item F): the HTTP admin surface for
// four-eyes filing/approval, EOI minting, and the per-domain activate/
// publish/execute endpoints item E's application-level wiring needs a
// caller for. Closes reconnaissance §3.1's own named finding ("Missing
// HTTP admin surfaces for four-eyes filing and campaign activation").
// API only, per the dispatch's own explicit scope - no Back Office UI.
//
// Every endpoint here follows this codebase's own established
// convention exactly (bonus_handlers.go/withdrawal_handlers.go):
// authn via auth.Middleware, tenant scoping via auth.RequireTenantScope,
// RBAC via auth.RequirePermission (static) or a dynamic per-operation
// permission check (RoleHasPermission, for the generic change-request
// surface whose required permission depends on a body field), actor/
// subject separation left to the underlying migration-0063 DB triggers
// (never re-implemented here), input validation via internal/validation,
// audit logging (either via the domain function's own audit.Record, or
// added here where the domain function does not audit itself), and a
// curated JSON response shape - never leaking a Go error's raw string
// where a caller could confuse "denied" with "internal error".
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/economicop"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// changeOperationPermission maps each of the security doc's eight
// dual-controlled bonus operations to the ONE permission whose holder
// may file OR approve a change request naming it - deliberately the
// SAME domain-authority permission a caller would need to perform the
// underlying operation directly (a promotions_manager may file/approve
// campaign_activate/offer_publish; a bonus_operations principal may
// file/approve manual_grant_issue/bulk_job_execute/bonus_adjustment_write/
// grant_forced_conversion/grant_cancel_completed/held_disposition_resolve) -
// never a separate, broader "approver" permission that would let staff
// approve an operation class they could not themselves perform, which
// would quietly reopen the hard-constraint separations security-
// architecture.md §B1.1 already established between RolePromotionsManager
// and RoleBonusOperations. The migration-0063 DB triggers separately
// enforce requester != approver (distinct Person) - this map only
// decides WHO may participate at all, never who specifically must.
var changeOperationPermission = map[bonus.ChangeOperation]auth.Permission{
	bonus.ChangeOpManualGrantIssue:       auth.PermBonusGrantIssue,
	bonus.ChangeOpBulkJobExecute:         auth.PermBonusBulkExecute,
	bonus.ChangeOpBonusAdjustmentWrite:   auth.PermBonusAdjustmentWrite,
	bonus.ChangeOpGrantForcedConversion:  auth.PermBonusGrantReview,
	bonus.ChangeOpCampaignActivate:       auth.PermBonusCampaignActivate,
	bonus.ChangeOpOfferPublish:           auth.PermBonusOfferManage,
	bonus.ChangeOpGrantCancelCompleted:   auth.PermBonusGrantCancel,
	bonus.ChangeOpHeldDispositionResolve: auth.PermBonusHeldDispositionResolve,
}

func requirePermissionForOperation(w http.ResponseWriter, requestID string, tc tenant.Context, operation bonus.ChangeOperation) bool {
	perm, ok := changeOperationPermission[operation]
	if !ok {
		apierror.Write(w, requestID, apierror.CodeValidation, "unrecognized operation")
		return false
	}
	if !auth.RoleHasPermission(auth.Role(tc.Role), perm) {
		apierror.Write(w, requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return false
	}
	return true
}

// --- generic four-eyes: file / approve|reject a change request ---

type fileChangeRequestRequest struct {
	Operation       string          `json:"operation"`
	TargetType      string          `json:"target_type"`
	TargetID        string          `json:"target_id"`
	Payload         json.RawMessage `json:"payload"`
	AmountAtRequest string          `json:"amount_at_request,omitempty"`
	AssetCode       string          `json:"asset_code,omitempty"`
	ReasonCode      string          `json:"reason_code"`
}

type changeRequestResponse struct {
	ID         string `json:"id"`
	Operation  string `json:"operation"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	State      string `json:"state"`
}

func toChangeRequestResponse(r bonus.ChangeRequest) changeRequestResponse {
	return changeRequestResponse{ID: r.ID.String(), Operation: string(r.Operation), TargetType: r.TargetType, TargetID: r.TargetID.String(), State: string(r.State)}
}

// newFileChangeRequestHandler is the ONE surface every one of the eight
// dual-controlled bonus operations files a request through - the DB
// layer (migration 0063) is already operation-agnostic, so a single
// generic endpoint covers all eight, per reconnaissance §3.1's own
// recommendation, never a per-operation filing endpoint.
func newFileChangeRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		var req fileChangeRequestRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("operation", req.Operation)
		v.RequireNonEmpty("target_type", req.TargetType)
		v.RequireUUID("target_id", req.TargetID)
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		operation := bonus.ChangeOperation(req.Operation)
		if !requirePermissionForOperation(w, requestID, tc, operation) {
			return
		}
		targetID, _ := uuid.Parse(req.TargetID)
		var amount *big.Int
		if req.AmountAtRequest != "" {
			amt, ok := new(big.Int).SetString(req.AmountAtRequest, 10)
			if !ok || amt.Sign() < 0 {
				apierror.Write(w, requestID, apierror.CodeValidation, "amount_at_request must be a non-negative decimal integer string")
				return
			}
			amount = amt
		}
		var assetCode *string
		if req.AssetCode != "" {
			assetCode = &req.AssetCode
		}
		payload := []byte(req.Payload)
		if len(payload) == 0 {
			payload = []byte("{}")
		} else if !json.Valid(payload) {
			apierror.Write(w, requestID, apierror.CodeValidation, "payload must be valid JSON")
			return
		}

		var resp changeRequestResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			created, err := bonus.FileChangeRequest(ctx, tx, bonus.ChangeRequest{
				TenantID: tc.TenantID, Operation: operation, TargetType: req.TargetType, TargetID: targetID,
				Payload: payload, AmountAtRequest: amount, AssetCode: assetCode,
				ReasonCode: req.ReasonCode, RequestedByPrincipalID: staffID,
			})
			if err != nil {
				return err
			}
			resp = toChangeRequestResponse(created)
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bonus_change_request.filed", TargetType: "bonus_change_request", TargetID: created.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"operation": req.Operation, "target_type": req.TargetType, "target_id": req.TargetID, "reason_code": req.ReasonCode},
			})
		})
		if err != nil {
			deps.Logger.Error("file_change_request_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to file change request")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

type decideChangeRequestRequest struct {
	Decision string `json:"decision"` // "approve" | "reject"
	// ReasonCode is MANDATORY for decision="reject" (migration 0063's own
	// CHECK (decision <> 'reject' OR reason_code IS NOT NULL)) and
	// optional for an approve.
	ReasonCode string `json:"reason_code,omitempty"`
	// NOTE (Stage 4H-B1 Wave 3 Phase 6, `security`): there are
	// deliberately NO threshold_at_decision/amount_at_decision fields
	// here - see newDecideChangeRequestHandler's own doc comment.
}

// newDecideChangeRequestHandler records an approve/reject decision - the
// migration-0063 governance trigger (requester != approver) and SEP-1
// trigger (beneficiary separation) fire on THIS write and are the real
// enforcement; this handler adds only authn/authz/audit around it, per
// this file's own top-of-file doc comment.
//
// FIX (Stage 4H-B1 Wave 3 Phase 6, `security`):
// bonus_change_approvals.threshold_at_decision/amount_at_decision are now
// resolved SERVER-SIDE, inside this same transaction, from
// bonus.ResolveApprovalPolicy and from the locked request's own
// amount_at_request - they are no longer read from the request body.
//
// Migration 0063 created those two columns for exactly one purpose, in its
// own words (quoting withdrawal_approvals' precedent): "what stops a later
// threshold change from retroactively making a past decision look
// compliant... when the record is read during a dispute." A client-supplied
// value defeats that purpose completely: the approving principal could
// POST threshold_at_decision=999999999, amount_at_decision=1 alongside a
// genuinely large, genuinely above-threshold approval, and the resulting
// bonus_change_approvals row - append-only, protected by deny-update/
// deny-delete triggers, and therefore trusted precisely BECAUSE it cannot
// be edited afterwards - would permanently assert that the decision was a
// routine below-threshold one. The forensic record that exists to be
// trustworthy in a dispute was authored by the party it is meant to hold
// to account. Both fields were also optional, so an approver could simply
// omit them and leave the record blank.
//
// internal/withdrawal.Approve (the precedent migration 0063 says it is
// modeled on) already does exactly what this fix does - "The ApprovalPolicy
// in force is resolved internally, from ResolveApprovalPolicy... never
// passed in by the caller... recorded verbatim on the WithdrawalApproval
// row (threshold_amount_at_decision) specifically so a later policy
// mutation is detectable after the fact". This is that existing pattern
// applied to the surface that was missing it, not a new mechanism.
func newDecideChangeRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		changeRequestID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request id")
			return
		}
		var req decideChangeRequestRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("decision", req.Decision, "approve", "reject")
		if req.Decision == "reject" {
			// Surfaced as a 400 here rather than letting migration 0063's
			// own CHECK constraint raise it as an opaque Postgres error a
			// caller would see as a bare "decision refused".
			v.RequireNonEmpty("reason_code", req.ReasonCode)
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var reasonCode *string
		if req.ReasonCode != "" {
			reasonCode = &req.ReasonCode
		}

		var resp changeRequestResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			existing, err := bonus.GetChangeRequestByID(ctx, tx, changeRequestID)
			if err != nil {
				return err
			}
			if !requirePermissionForOperationTx(existing.Operation, tc) {
				return errInsufficientPermission
			}
			// The policy in force AT THIS DECISION, resolved here from the
			// request's own (tenant, operation, brand, asset) - never from
			// the caller. ResolveApprovalPolicy fails closed to
			// threshold 0 / 2 approvals when no row resolves.
			policy, err := bonus.ResolveApprovalPolicy(ctx, tx, tc.TenantID, existing.Operation, existing.BrandID, existing.AssetCode)
			if err != nil {
				return err
			}
			if err := bonus.RecordChangeApproval(ctx, tx, tc.TenantID, changeRequestID, staffID, req.Decision, reasonCode,
				policy.ApprovalThresholdMinor, existing.AmountAtRequest); err != nil {
				return err
			}
			resp = toChangeRequestResponse(existing)
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bonus_change_request." + req.Decision + "d", TargetType: "bonus_change_request", TargetID: changeRequestID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{
					"operation":          string(existing.Operation),
					"reason_code":        req.ReasonCode,
					"threshold_minor":    policy.ApprovalThresholdMinor.String(),
					"required_approvals": policy.RequiredApprovals,
				},
			})
		})
		if errors.Is(err, errInsufficientPermission) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
			return
		}
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "change request not found")
			return
		}
		// migration 0063's own governance/SEP-1 triggers (requester !=
		// approver, beneficiary separation) refuse a policy-violating
		// approval by raising a Postgres exception (SQLSTATE P0001) on
		// this very INSERT - surfaced here as a 403 (a refused policy
		// decision), never a 500 (which would incorrectly suggest an
		// application bug rather than a working control).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "change request decision refused: "+pgErr.Message)
			return
		}
		if err != nil {
			deps.Logger.Error("decide_change_request_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to record change request decision: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

var errInsufficientPermission = errors.New("insufficient permissions")

// requirePermissionForOperationTx is requirePermissionForOperation's
// non-HTTP-writing twin, for use INSIDE a WithTenant closure (where a
// direct apierror.Write would run before the transaction outcome is
// known) - the caller maps the returned false to a Forbidden response
// itself.
func requirePermissionForOperationTx(operation bonus.ChangeOperation, tc tenant.Context) bool {
	perm, ok := changeOperationPermission[operation]
	if !ok {
		return false
	}
	return auth.RoleHasPermission(auth.Role(tc.Role), perm)
}

// --- EOI minting ---

type mintEconomicOperationRequest struct {
	OperationType string `json:"operation_type"` // "bonus_manual_grant" | "bonus_bulk_grant"
	SubjectScope  string `json:"subject_scope"`  // "single_subject" | "enumerated_set"
	// SubjectRef is REQUIRED for subject_scope=single_subject (doc 34
	// §2.2: "the beneficiary's player_account_id") and SubjectSetCount is
	// REQUIRED for subject_scope=enumerated_set.
	SubjectRef      string `json:"subject_ref,omitempty"`
	SubjectSetCount int32  `json:"subject_set_count,omitempty"`
	// AssetCode, IntendedAggregateValue and RecipientCeiling are all
	// MANDATORY - see newMintEconomicOperationHandler's own doc comment
	// for why an optional ceiling is not a ceiling.
	AssetCode              string `json:"asset_code"`
	IntendedAggregateValue string `json:"intended_aggregate_value"`
	RecipientCeiling       int32  `json:"recipient_ceiling"`
	IdempotencyKey         string `json:"idempotency_key"`
	ExpiresInSeconds       int64  `json:"expires_in_seconds"`
}

// mintEconomicOperationDefaultTTLSeconds/MaxTTLSeconds bound doc 34
// §2.2's "expires_at ... Mandatory; a platform maximum bounds the
// configured value" - an already-approved EOI root minted on one actor's
// authority must not be long-lived (24h ceiling; 1h if the caller says
// nothing).
const (
	mintEconomicOperationDefaultTTLSeconds = 3600
	mintEconomicOperationMaxTTLSeconds     = 24 * 3600
)

type economicOperationResponse struct {
	OperationID     string `json:"operation_id"`
	RootOperationID string `json:"root_operation_id"`
	OperationType   string `json:"operation_type"`
	ApprovalState   string `json:"approval_state"`
	Status          string `json:"status"`
}

// newMintEconomicOperationHandler mints doc 34 §3.1's own two exactly-
// two mint points for Bonus Engine: a staff single-Grant authorization
// (bonus_manual_grant) or a BulkGrantJob's own authorization
// (bonus_bulk_grant). ApprovalState is set to 'approved' by THIS
// endpoint's own caller authority (the caller must already hold the
// SAME permission the eventual grant/bulk-execute action itself
// requires - PermBonusGrantIssue/PermBonusBulkExecute respectively) -
// doc 34's own ApprovalState enum explicitly allows an EOI to be minted
// already-approved (never a separate approval workflow ON the EOI object
// itself for this Wave's two consumer types), mirroring every existing
// test's own MintRootOperationParams{ApprovalState: ApprovalApproved}
// usage exactly - this endpoint does not invent a new EOI-approval
// mechanism, it is the first HTTP-reachable caller of an ALREADY-
// existing, already-tested minting shape.
//
// FIX (Stage 4H-B1 Wave 3 Phase 6, `security`): the budget-bounding
// fields are MANDATORY here, not optional. Before this fix
// intended_aggregate_value, recipient_ceiling and asset_code were all
// optional request fields, and economicop.ConsumeRootBudget treats an
// absent recipient_ceiling as "no recipient check at all" and an absent
// intended_aggregate_value (with a non-null asset_code) as "no value
// check at all" - so a SINGLE actor, on their own authority, could mint an
// already-approved EOI root with an UNBOUNDED budget and then use it to
// authorize an arbitrarily large decomposed grant campaign underneath it.
// That is precisely the SEC-W15-02 decomposition vector the EOI mechanism
// exists to close, reopened at the mechanism's own entry point: a ceiling
// that is optional to declare is not a ceiling. Since this endpoint mints
// on one actor's authority, the bound must be explicit and finite, and
// expires_at must be bounded by a platform maximum (doc 34 §2.2:
// "Mandatory; a platform maximum bounds the configured value" - "an
// authorization that can be executed forever is not an authorization").
// The four-eyes requirement that separately governs each operation
// UNDERNEATH this root (four_eyes_ops.go) is unchanged and is not a
// substitute for this bound - the two controls compose in series (doc 34
// §5.3), they do not replace one another.
func newMintEconomicOperationHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		var req mintEconomicOperationRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("operation_type", req.OperationType, string(economicop.OperationBonusManualGrant), string(economicop.OperationBonusBulkGrant))
		v.RequireOneOf("subject_scope", req.SubjectScope, string(economicop.SubjectScopeSingle), string(economicop.SubjectScopeEnumeratedSet))
		v.RequireNonEmpty("idempotency_key", req.IdempotencyKey)
		v.RequireNonEmpty("asset_code", req.AssetCode)
		v.RequireNonEmpty("intended_aggregate_value", req.IntendedAggregateValue)
		if req.RecipientCeiling <= 0 {
			v.Add("recipient_ceiling", "must be a positive integer - an EOI root minted on a single actor's authority must declare a finite recipient ceiling")
		}
		switch req.SubjectScope {
		case string(economicop.SubjectScopeSingle):
			v.RequireUUID("subject_ref", req.SubjectRef)
			if req.RecipientCeiling > 1 {
				v.Add("recipient_ceiling", "must be 1 for subject_scope=single_subject")
			}
		case string(economicop.SubjectScopeEnumeratedSet):
			if req.SubjectSetCount <= 0 {
				v.Add("subject_set_count", "must be a positive integer for subject_scope=enumerated_set")
			}
			if req.SubjectSetCount > 0 && req.RecipientCeiling > req.SubjectSetCount {
				v.Add("recipient_ceiling", "must not exceed subject_set_count")
			}
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		operationType := economicop.OperationType(req.OperationType)
		var requiredPerm auth.Permission
		switch operationType {
		case economicop.OperationBonusManualGrant:
			requiredPerm = auth.PermBonusGrantIssue
		case economicop.OperationBonusBulkGrant:
			requiredPerm = auth.PermBonusBulkExecute
		}
		if !auth.RoleHasPermission(auth.Role(tc.Role), requiredPerm) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "insufficient permissions to mint this operation type")
			return
		}
		var subjectRef *uuid.UUID
		if req.SubjectRef != "" {
			parsed, perr := uuid.Parse(req.SubjectRef)
			if perr != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "subject_ref must be a valid UUID")
				return
			}
			subjectRef = &parsed
		}
		var subjectSetCount *int32
		if req.SubjectSetCount > 0 {
			subjectSetCount = &req.SubjectSetCount
		}
		var assetCode *string
		if req.AssetCode != "" {
			assetCode = &req.AssetCode
		}
		intendedAggregate, ok := new(big.Int).SetString(req.IntendedAggregateValue, 10)
		if !ok || intendedAggregate.Sign() <= 0 {
			apierror.Write(w, requestID, apierror.CodeValidation, "intended_aggregate_value must be a positive decimal integer string")
			return
		}
		recipientCeiling := &req.RecipientCeiling
		expiresIn := req.ExpiresInSeconds
		if expiresIn <= 0 {
			expiresIn = mintEconomicOperationDefaultTTLSeconds
		}
		if expiresIn > mintEconomicOperationMaxTTLSeconds {
			apierror.Write(w, requestID, apierror.CodeValidation, "expires_in_seconds exceeds the platform maximum for an EOI root")
			return
		}

		var resp economicOperationResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			op, err := bonus.MintRootOperation(ctx, tx, bonus.MintRootOperationParams{
				TenantID: tc.TenantID, OperationType: operationType,
				InitiatingActorType: string(bonus.ActorStaff), InitiatingActorID: staffID, InitiatingPrincipalID: &staffID,
				SubjectScope: economicop.SubjectScope(req.SubjectScope), SubjectRef: subjectRef, SubjectSetCount: subjectSetCount,
				AssetCode: assetCode, IntendedAggregateValue: intendedAggregate, RecipientCeiling: recipientCeiling,
				IdempotencyKey: req.IdempotencyKey, CorrelationID: uuid.New(),
				ExpiresAt:     time.Now().UTC().Add(time.Duration(expiresIn) * time.Second),
				ApprovalState: economicop.ApprovalApproved,
			})
			if err != nil {
				return err
			}
			resp = economicOperationResponse{
				OperationID: op.OperationID.String(), RootOperationID: op.RootOperationID.String(),
				OperationType: string(op.OperationType), ApprovalState: string(op.ApprovalState), Status: string(op.Status),
			}
			return nil
		})
		if err != nil {
			deps.Logger.Error("mint_economic_operation_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to mint economic operation")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}
