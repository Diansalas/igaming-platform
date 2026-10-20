package httpserver

// lic10 (migration 0181; audit G8, owner question Q21): the residence determination maker-checker routes.
//
//	GET  /v1/admin/tenants/{tenantID}/residence-approval-policy                 mode in force and history (none)
//	POST /v1/admin/tenants/{tenantID}/residence-approval-policy                 file a change: stricter is active at once, less strict is pending (none)
//	POST /v1/admin/tenants/{tenantID}/residence-approval-policy/{policyID}/approvals   one of the two approvals of a less strict change (critical)
//	GET  /v1/admin/kyc/residence-determinations/pending                         determinations waiting for a second Person (none)
//	POST /v1/admin/kyc/verifications/{id}/residence-approval                    the second Person's approval of one determination (critical)
//
// Step-up (ADR 0119 8.1): filing a change only tightens or files a request that changes nothing until approved (protective:
// none); both approvals loosen a control (critical). The signer backstop (actorproof.StepUpOperations) registers exactly the
// two approval operations. Every route is TENANT-scoped (a platform token carries no tenant and is refused 403; there is no
// platform_acting path, which would widen the ADR 0099 acting-session allow-list). The target tenant of the policy routes is the
// path value and must be the caller's own: a foreign tenant is 404. The static permissions are only the route gate: the DATABASE re-checks the role, the Person and the signed proof, and enforces
// every four-eyes rule. Responses are Cache-Control: no-store; error bodies are fixed sentences.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/residenceapproval"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ResidenceApprovalAPI is the maker-checker surface the routes need (satisfied by *residenceapproval.Service).
type ResidenceApprovalAPI interface {
	SetPolicy(ctx context.Context, a residenceapproval.Actor, mode residenceapproval.Mode, reasonCode string) (residenceapproval.PolicyChange, error)
	ApprovePolicy(ctx context.Context, a residenceapproval.Actor, policyID uuid.UUID, reasonCode string) (residenceapproval.PolicyApproval, error)
	ReadPolicy(ctx context.Context, a residenceapproval.Actor) (residenceapproval.Policy, error)
	ApproveDetermination(ctx context.Context, a residenceapproval.Actor, verificationID uuid.UUID, determinedAt time.Time, reasonCode string) error
}

var _ ResidenceApprovalAPI = (*residenceapproval.Service)(nil)

func registerResidenceApprovalRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/admin/tenants/{tenantID}/residence-approval-policy",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermResidenceApprovalPolicyRead)(newResidencePolicyReadHandler(deps)))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/residence-approval-policy",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermResidenceApprovalPolicyManage)(newResidencePolicySetHandler(deps)))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/residence-approval-policy/{policyID}/approvals",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermResidenceApprovalPolicyManage)(newResidencePolicyApproveHandler(deps)))))
	mux.Handle("GET /v1/admin/kyc/residence-determinations/pending",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newPendingResidenceApprovalsHandler(deps)))))
	mux.Handle("POST /v1/admin/kyc/verifications/{id}/residence-approval",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationReview)(newApproveResidenceDeterminationHandler(deps)))))
}

type residenceCall struct {
	requestID string
	logger    observabilityLogger
	actor     residenceapproval.Actor
	viewer    staffViewer
}

// beginResidencePolicyCall builds the actor from the verified token and the path tenant ONLY.
func beginResidencePolicyCall(deps Deps, w http.ResponseWriter, r *http.Request) (residenceCall, bool) {
	w.Header().Set("Cache-Control", "no-store")
	c := residenceCall{requestID: observability.RequestIDFromContext(r.Context()), logger: observability.LoggerFromContext(r.Context(), deps.Logger)}
	if deps.ResidenceApproval == nil {
		apierror.Write(w, c.requestID, apierror.CodeUnavailable, "residence approval is not available in this process")
		return c, false
	}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	staffID, err := uuid.Parse(tc.Subject)
	if err != nil || staffID == uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	target, err := uuid.Parse(r.PathValue("tenantID"))
	if err != nil || target == uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
		return c, false
	}
	if tc.TenantID == uuid.Nil || tc.TenantID != target {
		// Only the tenant's own staff (a platform token carries no tenant and is refused by RequireTenantScope before this point);
		// a foreign tenant is indistinguishable from an unknown one.
		c.logger.Warn("residence_policy_denied", "reason", "foreign_tenant")
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
		return c, false
	}
	c.actor = residenceapproval.Actor{StaffID: staffID, Scope: residenceapproval.ScopeTenant, TenantID: target, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID}
	c.viewer = staffViewerOf(tc)
	return c, true
}

// residenceErrorCode maps a service error to the closed API code and a fixed sentence (never the database text). Pure.
func residenceErrorCode(err error) (apierror.Code, string) {
	switch {
	case errors.Is(err, residenceapproval.ErrNotFound):
		return apierror.CodeNotFound, "not found"
	case errors.Is(err, residenceapproval.ErrProofUnavailable):
		return apierror.CodeUnavailable, "residence approval is not available in this process"
	case errors.Is(err, residenceapproval.ErrInvalidInput):
		return apierror.CodeValidation, "invalid request"
	case errors.Is(err, residenceapproval.ErrFourEyes):
		return apierror.CodeForbidden, "four-eyes: a different staff member and a different Person must make this decision"
	case errors.Is(err, residenceapproval.ErrNoPerson):
		return apierror.CodeForbidden, "this account is not linked to a Person and cannot take part in a four-eyes decision"
	case errors.Is(err, residenceapproval.ErrForbidden), errors.Is(err, residenceapproval.ErrActorScope):
		return apierror.CodeForbidden, "insufficient permissions for this operation"
	case errors.Is(err, residenceapproval.ErrNotPending):
		return apierror.CodeConflict, "nothing to do: not pending, already in force, or no longer current"
	case errors.Is(err, residenceapproval.ErrDuplicate):
		return apierror.CodeConflict, "already recorded"
	}
	return apierror.CodeInternal, "failed to complete the residence approval operation"
}

func (c residenceCall) fail(w http.ResponseWriter, op string, err error) {
	code, msg := residenceErrorCode(err)
	if code == apierror.CodeInternal {
		c.logger.Error("residence approval failed", "op", op, "error", err)
	}
	apierror.Write(w, c.requestID, code, msg)
}

type residencePolicyVersionResponse struct {
	ID                string   `json:"id"`
	Mode              string   `json:"mode"`
	PreviousMode      string   `json:"previous_mode"`
	Status            string   `json:"status"`
	RequiredApprovals int      `json:"required_approvals"`
	Approvals         int      `json:"approvals"`
	ReasonCode        string   `json:"reason_code"`
	RequestedBy       staffRef `json:"requested_by"`
	RequestedAt       string   `json:"requested_at"`
	ActivatedAt       *string  `json:"activated_at"`
}

type residencePolicyResponse struct {
	EffectiveMode string                           `json:"effective_mode"`
	Versions      []residencePolicyVersionResponse `json:"versions"`
}

func newResidencePolicyReadHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResidencePolicyCall(deps, w, r)
		if !ok {
			return
		}
		p, err := deps.ResidenceApproval.ReadPolicy(r.Context(), c.actor)
		if err != nil {
			c.fail(w, "read", err)
			return
		}
		resp := residencePolicyResponse{EffectiveMode: string(p.EffectiveMode), Versions: make([]residencePolicyVersionResponse, 0, len(p.Versions))}
		for _, v := range p.Versions {
			item := residencePolicyVersionResponse{ID: v.ID.String(), Mode: string(v.Mode), PreviousMode: string(v.PreviousMode), Status: v.Status,
				RequiredApprovals: v.RequiredApprovals, Approvals: v.Approvals, ReasonCode: v.ReasonCode,
				RequestedBy: c.viewer.ref(v.RequestedByScope, v.RequestedBy), RequestedAt: v.RequestedAt.UTC().Format(rfc3339)}
			if v.ActivatedAt != nil {
				s := v.ActivatedAt.UTC().Format(rfc3339)
				item.ActivatedAt = &s
			}
			resp.Versions = append(resp.Versions, item)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type residencePolicySetRequest struct {
	Mode       string `json:"mode"`
	ReasonCode string `json:"reason_code"`
}

type residencePolicyChangeResponse struct {
	PolicyID          string `json:"policy_id"`
	Status            string `json:"status"`
	RequiredApprovals int    `json:"required_approvals"`
	PreviousMode      string `json:"previous_mode"`
	RequestedMode     string `json:"requested_mode"`
}

// newResidencePolicySetHandler: 200 when a STRICTER mode was executed at once, 202 when a LESS strict mode was filed pending.
func newResidencePolicySetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResidencePolicyCall(deps, w, r)
		if !ok {
			return
		}
		var req residencePolicySetRequest
		if err := decodeJSONStrict(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		res, err := deps.ResidenceApproval.SetPolicy(r.Context(), c.actor, residenceapproval.Mode(req.Mode), req.ReasonCode)
		if err != nil {
			c.fail(w, "set", err)
			return
		}
		status := http.StatusAccepted
		if res.ExecutedImmediately {
			status = http.StatusOK
		}
		writeJSON(w, status, residencePolicyChangeResponse{PolicyID: res.PolicyID.String(), Status: res.Status, RequiredApprovals: res.RequiredApprovals,
			PreviousMode: string(res.PreviousMode), RequestedMode: string(res.RequestedMode)})
	}
}

type residenceReasonRequest struct {
	ReasonCode string `json:"reason_code"`
}

type residencePolicyApprovalResponse struct {
	Status            string `json:"status"`
	Approvals         int    `json:"approvals"`
	RequiredApprovals int    `json:"required_approvals"`
}

func newResidencePolicyApproveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResidencePolicyCall(deps, w, r)
		if !ok {
			return
		}
		pid, err := uuid.Parse(r.PathValue("policyID"))
		if err != nil || pid == uuid.Nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "policyID must be a valid UUID")
			return
		}
		var req residenceReasonRequest
		if err := decodeJSONStrict(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		res, err := deps.ResidenceApproval.ApprovePolicy(r.Context(), c.actor, pid, req.ReasonCode)
		if err != nil {
			c.fail(w, "approve_policy", err)
			return
		}
		writeJSON(w, http.StatusOK, residencePolicyApprovalResponse{Status: res.Status, Approvals: res.Approvals, RequiredApprovals: res.RequiredApprovals})
	}
}

// ---- determination approval (tenant-scoped) -----------------------------------------------------------------------------

type pendingResidenceApprovalResponse struct {
	VerificationID  string   `json:"verification_id"`
	PlayerAccountID string   `json:"player_account_id"`
	BrandID         string   `json:"brand_id"`
	DeterminedAt    string   `json:"determined_at"`
	DeterminedBy    staffRef `json:"determined_by"`
	PolicyMode      string   `json:"policy_mode"`
}

// residenceTimeFormat carries the full microsecond precision the proof binds (the approval must name the exact determination).
const residenceTimeFormat = "2006-01-02T15:04:05.000000Z07:00"

func newPendingResidenceApprovalsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var list []kyc.PendingResidenceApproval
		err = deps.DB.WithTenantReadOnly(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			list, err = kyc.ListPendingResidenceApprovals(ctx, tx, tc.TenantID, 0)
			return err
		})
		if err != nil {
			logger.Error("list pending residence approvals failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list pending residence approvals")
			return
		}
		viewer := staffViewerOf(tc)
		out := make([]pendingResidenceApprovalResponse, 0, len(list))
		for _, p := range list {
			out = append(out, pendingResidenceApprovalResponse{VerificationID: p.VerificationID.String(), PlayerAccountID: p.PlayerAccountID.String(),
				BrandID: p.BrandID.String(), DeterminedAt: p.DeterminedAt.UTC().Format(residenceTimeFormat),
				DeterminedBy: viewer.ref(staffScopeTenant, p.DeterminedBy), PolicyMode: p.PolicyMode})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type approveResidenceDeterminationRequest struct {
	DeterminedAt string `json:"determined_at"`
	ReasonCode   string `json:"reason_code"`
}

func newApproveResidenceDeterminationHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		if deps.ResidenceApproval == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "residence approval is not available in this process")
			return
		}
		tc, err := tenant.FromContext(r.Context())
		if err != nil || tc.TenantID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil || staffID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		vid, err := uuid.Parse(r.PathValue("id"))
		if err != nil || vid == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid verification id")
			return
		}
		var req approveResidenceDeterminationRequest
		if err := decodeJSONStrict(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		at, err := time.Parse(time.RFC3339Nano, req.DeterminedAt)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "determined_at must be the RFC 3339 time listed for the determination")
			return
		}
		actor := residenceapproval.Actor{StaffID: staffID, Scope: residenceapproval.ScopeTenant, TenantID: tc.TenantID, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID}
		if err := deps.ResidenceApproval.ApproveDetermination(r.Context(), actor, vid, at, req.ReasonCode); err != nil {
			code, msg := residenceErrorCode(err)
			if code == apierror.CodeInternal {
				logger.Error("residence determination approval failed", "error", err)
			}
			apierror.Write(w, requestID, code, msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"verification_id": vid.String(), "status": "approved"})
	}
}
