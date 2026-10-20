package httpserver

// lic10 (L-20): the governed admin surface for KYC enforcement policies (ADR 0096; migrations 0100/0103). Library-only until
// now. PLATFORM-ONLY (PermKYCEnforcementPolicyManage / Read: platform_admin), no RequireTenantScope (a platform token carries
// no tenant), step-up `critical` on every write, reason code required on every write, request evidence (IP, user agent,
// request id) on every audit record.
//
// WHAT THE ROUTES DO NOT DO, ON PURPOSE:
//   - they ship and suggest NO threshold, asset, tier or playable-operation value: every value comes from the request;
//   - there is NO create-and-activate: a policy is created as a DRAFT and activated by a DIFFERENT platform-admin principal
//     (migration 0100 four-eyes, enforced by the database trigger, not by this file);
//   - there is NO way to switch an active policy off without a successor (migration 0103, security F3): "deactivate" an active
//     policy means SUPERSEDE it with an activated draft of the same enforcement key; a never-activated draft can be withdrawn;
//   - only the evaluator-wired trigger types (cumulative_deposit, play) are accepted: edd_amount and registration_tier stay
//     NOT IMPLEMENTED (the database also refuses to activate them).
//
// The effective rule is whatever the ACTIVE rows say; a jurisdiction with no active row is "not required" (ADR 0096: dormant
// by default). The launch readiness item that flags a brand with zero policies (lic7) is unchanged.

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

type kycEnforcementPolicyResponse struct {
	ID                      string    `json:"id"`
	LicensingJurisdictionID string    `json:"licensing_jurisdiction_id"`
	TriggerType             string    `json:"trigger_type"`
	Status                  string    `json:"status"`
	ThresholdMinorUnits     *string   `json:"threshold_minor_units"`
	AssetCode               *string   `json:"asset_code"`
	RequiredTier            *string   `json:"required_tier"`
	PlayOperation           *string   `json:"play_operation"`
	EffectiveFrom           string    `json:"effective_from"`
	LegalReviewReference    *string   `json:"legal_review_reference"`
	ReasonCode              string    `json:"reason_code"`
	CreatedAt               string    `json:"created_at"`
	CreatedBy               *staffRef `json:"created_by"`
	SuccessorPolicyID       *string   `json:"successor_policy_id"`
}

func toKYCEnforcementPolicyResponse(p kyc.EnforcementPolicy) kycEnforcementPolicyResponse {
	platform := staffScopePlatform
	by := p.CreatedByActorID
	r := kycEnforcementPolicyResponse{
		ID: p.ID.String(), LicensingJurisdictionID: p.LicensingJurisdictionID.String(), TriggerType: p.TriggerType, Status: p.Status,
		ThresholdMinorUnits: p.ThresholdMinorUnits, AssetCode: p.AssetCode, RequiredTier: p.RequiredTier, PlayOperation: p.PlayOperation,
		EffectiveFrom: p.EffectiveFrom.UTC().Format(rfc3339), LegalReviewReference: p.LegalReviewReference, ReasonCode: p.ReasonCode,
		CreatedAt: p.CreatedAt.UTC().Format(rfc3339), CreatedBy: platformStaffViewer().optRef(&platform, &by),
	}
	if p.SupersededByPolicyID != nil {
		s := p.SupersededByPolicyID.String()
		r.SuccessorPolicyID = &s
	}
	return r
}

// kycEnfReq is the shared preamble: the authenticated platform principal (server-side), request id, logger.
type kycEnfReq struct {
	requestID string
	logger    interface{ Error(msg string, args ...any) }
	staffID   uuid.UUID
}

func beginKYCEnfReq(deps Deps, w http.ResponseWriter, r *http.Request) (kycEnfReq, bool) {
	c := kycEnfReq{requestID: observability.RequestIDFromContext(r.Context()), logger: observability.LoggerFromContext(r.Context(), deps.Logger)}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	id, err := uuid.Parse(tc.Subject)
	if err != nil || id == uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	c.staffID = id
	return c, true
}

// writeKYCPolicyError maps a library/database refusal to a closed code and a fixed sentence (never the database text).
func writeKYCPolicyError(w http.ResponseWriter, c kycEnfReq, op string, err error) {
	switch {
	case errors.Is(err, kyc.ErrNotFound):
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "enforcement policy not found")
		return
	case errors.Is(err, kyc.ErrPolicyPersonNotDistinct):
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "four-eyes: a different platform administrator than the policy's author must make this change")
		return
	case errors.Is(err, kyc.ErrPolicyInvalidInput):
		apierror.Write(w, c.requestID, apierror.CodeConflict, "the policy is not in the state this action needs")
		return
	}
	switch kyc.ClassifyPolicyError(err) {
	case kyc.PolicyFailureFourEyes:
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "four-eyes: a different platform administrator than the policy's author must make this change")
	case kyc.PolicyFailureNotWired:
		apierror.Write(w, c.requestID, apierror.CodeConflict, "this trigger type is not evaluator-wired and cannot be activated")
	case kyc.PolicyFailureNeedsSuccess:
		apierror.Write(w, c.requestID, apierror.CodeConflict, "an active policy is replaced by an activated successor of the same enforcement key; it cannot be switched off")
	case kyc.PolicyFailureIllegalMove:
		apierror.Write(w, c.requestID, apierror.CodeConflict, "the policy is not in the state this action needs")
	case kyc.PolicyFailureDuplicate:
		apierror.Write(w, c.requestID, apierror.CodeConflict, "an active policy already exists for this enforcement key; supersede it instead")
	case kyc.PolicyFailureUnknownRef:
		apierror.Write(w, c.requestID, apierror.CodeValidation, "unknown licensing_jurisdiction_id or asset_code")
	case kyc.PolicyFailureInvalid:
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid policy")
	default:
		c.logger.Error("kyc enforcement policy write failed", "op", op, "error", err)
		apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to change the enforcement policy")
	}
}

func pathPolicyID(w http.ResponseWriter, r *http.Request, requestID string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("policyID"))
	if err != nil || id == uuid.Nil {
		apierror.Write(w, requestID, apierror.CodeValidation, "policyID must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// newListKYCEnforcementPoliciesHandler is GET /v1/admin/kyc/enforcement-policies[?licensing_jurisdiction_id=&status=].
func newListKYCEnforcementPoliciesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKYCEnfReq(deps, w, r)
		if !ok {
			return
		}
		var f kyc.ListEnforcementPoliciesFilter
		if raw := r.URL.Query().Get("licensing_jurisdiction_id"); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil || id == uuid.Nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "licensing_jurisdiction_id must be a valid UUID")
				return
			}
			f.LicensingJurisdictionID = id
		}
		f.Status = r.URL.Query().Get("status")
		if f.Status != "" && f.Status != "draft" && f.Status != "active" && f.Status != "withdrawn" {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "status must be draft, active or withdrawn")
			return
		}
		var list []kyc.EnforcementPolicy
		err := deps.DB.WithPlatformAdmin(r.Context(), c.staffID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			list, err = kyc.ListEnforcementPolicies(ctx, tx, f)
			return err
		})
		if err != nil {
			c.logger.Error("list kyc enforcement policies failed", "error", err)
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list enforcement policies")
			return
		}
		out := make([]kycEnforcementPolicyResponse, 0, len(list))
		for _, p := range list {
			out = append(out, toKYCEnforcementPolicyResponse(p))
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	}
}

type createKYCEnforcementPolicyRequest struct {
	LicensingJurisdictionID string  `json:"licensing_jurisdiction_id"`
	TriggerType             string  `json:"trigger_type"`
	ThresholdMinorUnits     *string `json:"threshold_minor_units"`
	AssetCode               *string `json:"asset_code"`
	PlayOperation           *string `json:"play_operation"`
	LegalReviewReference    string  `json:"legal_review_reference"`
	ReasonCode              string  `json:"reason_code"`
}

// newCreateKYCEnforcementPolicyHandler is POST /v1/admin/kyc/enforcement-policies: authors a DRAFT.
func newCreateKYCEnforcementPolicyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKYCEnfReq(deps, w, r)
		if !ok {
			return
		}
		var req createKYCEnforcementPolicyRequest
		if err := decodeJSONStrict(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		jur, err := uuid.Parse(req.LicensingJurisdictionID)
		if err != nil || jur == uuid.Nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "licensing_jurisdiction_id must be a valid UUID")
			return
		}
		params := kyc.CreateEnforcementPolicyParams{
			LicensingJurisdictionID: jur, TriggerType: req.TriggerType, ThresholdMinorUnits: req.ThresholdMinorUnits, AssetCode: req.AssetCode,
			PlayOperation: req.PlayOperation, LegalReviewReference: req.LegalReviewReference, ReasonCode: req.ReasonCode,
			Audit: &kyc.AuditMeta{ReasonCode: req.ReasonCode, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID},
		}
		if err := kyc.ValidatePolicyInput(params); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, err.Error())
			return
		}
		var created kyc.EnforcementPolicy
		err = deps.DB.WithPlatformAdmin(r.Context(), c.staffID, func(ctx context.Context, tx pgx.Tx) error {
			id, err := kyc.CreateEnforcementPolicy(ctx, tx, params)
			if err != nil {
				return err
			}
			created, err = kyc.GetEnforcementPolicy(ctx, tx, id)
			return err
		})
		if err != nil {
			writeKYCPolicyError(w, c, "create", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, toKYCEnforcementPolicyResponse(created))
	}
}

type kycEnforcementPolicyReasonRequest struct {
	ReasonCode string `json:"reason_code"`
}

type kycEnforcementPolicySupersedeRequest struct {
	SuccessorPolicyID string `json:"successor_policy_id"`
	ReasonCode        string `json:"reason_code"`
}

// runKYCPolicyTransition runs one transition in a platform-admin transaction and answers with the read-back policy.
func runKYCPolicyTransition(deps Deps, w http.ResponseWriter, r *http.Request, c kycEnfReq, op string, id uuid.UUID, meta *kyc.AuditMeta,
	run func(ctx context.Context, tx pgx.Tx) error) {
	var out kyc.EnforcementPolicy
	err := deps.DB.WithPlatformAdmin(r.Context(), c.staffID, func(ctx context.Context, tx pgx.Tx) error {
		if err := run(ctx, tx); err != nil {
			return err
		}
		var err error
		out, err = kyc.GetEnforcementPolicy(ctx, tx, id)
		return err
	})
	if err != nil {
		writeKYCPolicyError(w, c, op, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, toKYCEnforcementPolicyResponse(out))
}

func decodeReason(w http.ResponseWriter, r *http.Request, c kycEnfReq) (string, bool) {
	var req kycEnforcementPolicyReasonRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
		return "", false
	}
	if !kycPolicyReasonOK(req.ReasonCode) {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]")
		return "", false
	}
	return req.ReasonCode, true
}

// kycPolicyReasonOK is the reason-code format shared with the other governed admin writes.
func kycPolicyReasonOK(s string) bool { return kyc.ValidPolicyReasonCode(s) }

// newActivateKYCEnforcementPolicyHandler is POST /v1/admin/kyc/enforcement-policies/{policyID}/activate. The database refuses the
// policy's own author (four-eyes) and a trigger type the evaluator does not consult.
func newActivateKYCEnforcementPolicyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKYCEnfReq(deps, w, r)
		if !ok {
			return
		}
		id, ok := pathPolicyID(w, r, c.requestID)
		if !ok {
			return
		}
		reason, ok := decodeReason(w, r, c)
		if !ok {
			return
		}
		meta := &kyc.AuditMeta{ReasonCode: reason, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID}
		runKYCPolicyTransition(deps, w, r, c, "activate", id, meta, func(ctx context.Context, tx pgx.Tx) error {
			return kyc.ActivateEnforcementPolicyAudited(ctx, tx, id, meta)
		})
	}
}

// newWithdrawKYCEnforcementPolicyHandler is POST /v1/admin/kyc/enforcement-policies/{policyID}/withdraw: a DRAFT only. An
// active policy is never switched off without a successor (use .../supersede).
func newWithdrawKYCEnforcementPolicyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKYCEnfReq(deps, w, r)
		if !ok {
			return
		}
		id, ok := pathPolicyID(w, r, c.requestID)
		if !ok {
			return
		}
		reason, ok := decodeReason(w, r, c)
		if !ok {
			return
		}
		meta := &kyc.AuditMeta{ReasonCode: reason, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID}
		runKYCPolicyTransition(deps, w, r, c, "withdraw", id, meta, func(ctx context.Context, tx pgx.Tx) error {
			return kyc.WithdrawDraftEnforcementPolicyAudited(ctx, tx, id, meta)
		})
	}
}

// newSupersedeKYCEnforcementPolicyHandler is POST /v1/admin/kyc/enforcement-policies/{policyID}/supersede: replaces the ACTIVE
// policy {policyID} by the DRAFT successor_policy_id, atomically. The acting administrator must differ from both authors.
func newSupersedeKYCEnforcementPolicyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKYCEnfReq(deps, w, r)
		if !ok {
			return
		}
		id, ok := pathPolicyID(w, r, c.requestID)
		if !ok {
			return
		}
		var req kycEnforcementPolicySupersedeRequest
		if err := decodeJSONStrict(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		succ, err := uuid.Parse(req.SuccessorPolicyID)
		if err != nil || succ == uuid.Nil || succ == id {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "successor_policy_id must be a valid UUID of a different policy")
			return
		}
		if !kycPolicyReasonOK(req.ReasonCode) {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]")
			return
		}
		meta := &kyc.AuditMeta{ReasonCode: req.ReasonCode, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID}
		runKYCPolicyTransition(deps, w, r, c, "supersede", succ, meta, func(ctx context.Context, tx pgx.Tx) error {
			return kyc.SupersedeEnforcementPolicyWithDraft(ctx, tx, id, succ, meta)
		})
	}
}
