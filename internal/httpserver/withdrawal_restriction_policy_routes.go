// lic11 (migration 0182): the withdrawal restriction policy routes. A thin, authenticating, error-mapping layer over
// internal/withdrawalpolicy, which owns every rule (closed cause registry, widening / narrowing classification computed by
// the DATABASE, two approvals by distinct staff AND distinct Persons for a loosening, approvals at most 24 h old, signed
// actor proofs, audit). Nothing here decides anything.
//
// Authentication and tenant binding: every route is the staff chain + RequirePermission(<static withdrawal_restriction_policy:*>).
// The actor comes from the verified token, never from a body. Writes are platform_admin only (platform scope); a tenant
// principal (tenant_admin, compliance) reads ITS OWN tenant's effective policy and versions only (a foreign tenant is a
// 404); change requests are platform records. Reads answer the UNCONFIGURED notice wherever the interim default is in force.
package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawalpolicy"
)

func registerWithdrawalRestrictionPolicyRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/admin/tenants/{tenantID}/withdrawal-restriction-policy",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyRead)(newWRPEffectiveHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/withdrawal-restriction-policy/versions",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyRead)(newWRPVersionsHandler(deps))))
	mux.Handle("GET /v1/admin/withdrawal-restriction-policy-changes",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyRead)(newWRPListHandler(deps))))
	mux.Handle("GET /v1/admin/withdrawal-restriction-policy-changes/{changeID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyRead)(newWRPGetHandler(deps))))
	mux.Handle("POST /v1/admin/withdrawal-restriction-policy-changes",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyChange)(newWRPCreateHandler(deps))))
	mux.Handle("POST /v1/admin/withdrawal-restriction-policy-changes/{changeID}/approve",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyChange)(newWRPApproveHandler(deps))))
	mux.Handle("POST /v1/admin/withdrawal-restriction-policy-changes/{changeID}/reject",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyChange)(newWRPRejectHandler(deps))))
	mux.Handle("POST /v1/admin/withdrawal-restriction-policy-changes/{changeID}/cancel",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalRestrictionPolicyChange)(newWRPCancelHandler(deps))))
}

type wrpCall struct {
	requestID string
	logger    observabilityLogger
	actor     withdrawalpolicy.Actor
	svc       *withdrawalpolicy.Service
	viewer    staffViewer
}

func beginWRPCall(deps Deps, w http.ResponseWriter, r *http.Request) (wrpCall, bool) {
	c := wrpCall{
		requestID: observability.RequestIDFromContext(r.Context()),
		logger:    observability.LoggerFromContext(r.Context(), deps.Logger),
		svc:       deps.WithdrawalRestrictionPolicy,
	}
	if c.svc == nil {
		apierror.Write(w, c.requestID, apierror.CodeWRPUnavailable, "withdrawal restriction policies are not available in this process")
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
	c.actor = withdrawalpolicy.Actor{StaffID: staffID, Scope: withdrawalpolicy.ScopePlatform, Role: tc.Role,
		IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID}
	if tc.TenantID != uuid.Nil {
		c.actor.Scope, c.actor.TenantID = withdrawalpolicy.ScopeTenant, tc.TenantID
	}
	c.viewer = staffViewerOf(tc)
	return c, true
}

func wrpPathUUID(w http.ResponseWriter, r *http.Request, requestID, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil || id == uuid.Nil {
		apierror.Write(w, requestID, apierror.CodeValidation, name+" must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// wrpErrorCode maps a service error to the closed API code and a fixed message (never the error text). Pure.
func wrpErrorCode(err error) (apierror.Code, string) {
	switch {
	case errors.Is(err, withdrawalpolicy.ErrNotFound):
		return apierror.CodeNotFound, "not found"
	case errors.Is(err, withdrawalpolicy.ErrNoPerson):
		return apierror.CodeWRPNoPerson, "this account cannot take part in a four-eyes decision"
	case errors.Is(err, withdrawalpolicy.ErrForbidden):
		return apierror.CodeForbidden, "forbidden"
	case errors.Is(err, withdrawalpolicy.ErrInvalidInput):
		return apierror.CodeValidation, "invalid request"
	case errors.Is(err, withdrawalpolicy.ErrNotConfigurable):
		return apierror.CodeWRPNotConfigurable, "this cause is not configurable (governed by an owner decision or not evaluable)"
	case errors.Is(err, withdrawalpolicy.ErrSelfApproval):
		return apierror.CodeWRPSelfApproval, "the requester may not decide their own change"
	case errors.Is(err, withdrawalpolicy.ErrNotPending):
		return apierror.CodeWRPNotPending, "the change is not open"
	case errors.Is(err, withdrawalpolicy.ErrAlreadyDecided):
		return apierror.CodeWRPAlreadyDecided, "this person already approved the change"
	case errors.Is(err, withdrawalpolicy.ErrDuplicatePending):
		return apierror.CodeWRPDuplicatePending, "an open change for this tenant and cause already exists"
	case errors.Is(err, withdrawalpolicy.ErrApprovalsStale):
		return apierror.CodeWRPApprovalsStale, "approvals are older than the approval window; file a new change"
	case errors.Is(err, withdrawalpolicy.ErrRetryableLockTimeout):
		return apierror.CodeWRPRetryableLockWait, "the tenant is busy; retry"
	}
	if code := withdrawalpolicy.GuardCode(err); code != "" {
		if code[:2] == "AP" {
			return apierror.CodeWRPProofRefused, "the actor proof was refused"
		}
		return apierror.CodeWRPGuardRefused, "a database guard refused the change"
	}
	return apierror.CodeInternal, "internal error"
}

func writeWRPError(w http.ResponseWriter, c wrpCall, op string, err error) {
	code, msg := wrpErrorCode(err)
	if code == apierror.CodeInternal {
		// A missing actor-proof issuer or a step-up backstop failure lands here too: fail closed, nothing was written.
		c.logger.Error("withdrawal restriction policy: refused with an internal error (fail closed)", "op", op, "error", err)
	}
	apierror.Write(w, c.requestID, code, msg)
}

// ---- response shapes ------------------------------------------------------------------------------------------------

type wrpStageJSON struct {
	Stage      string  `json:"stage"`
	Action     string  `json:"action"`
	Source     string  `json:"source"`
	PolicyID   *string `json:"policy_id"`
	SetVersion *int64  `json:"set_version"`
}

type wrpCauseJSON struct {
	Cause              string         `json:"cause"`
	Class              string         `json:"class"`
	Configurable       bool           `json:"configurable"`
	Evaluated          bool           `json:"evaluated"`
	GoverningReference string         `json:"governing_reference"`
	CurrentBehaviour   string         `json:"current_behaviour"`
	Stages             []wrpStageJSON `json:"stages"`
	Unconfigured       bool           `json:"unconfigured"`
	Notice             string         `json:"notice,omitempty"`
}

type wrpActionSemanticJSON struct {
	Action  string `json:"action"`
	Meaning string `json:"meaning"`
}

type wrpParkedJSON struct {
	Visible           bool    `json:"visible"`
	Count             int64   `json:"count"`
	OldestParkedSince *string `json:"oldest_parked_since"`
	OldestAgeSeconds  *int64  `json:"oldest_age_seconds"`
	Note              string  `json:"note"`
}

type wrpEffectiveResponse struct {
	TenantID        string                  `json:"tenant_id"`
	SetVersion      int64                   `json:"set_version"`
	Configured      bool                    `json:"configured"`
	Causes          []wrpCauseJSON          `json:"causes"`
	ActionSemantics []wrpActionSemanticJSON `json:"action_semantics"`
	Parked          wrpParkedJSON           `json:"parked"`
	Disclaimer      string                  `json:"disclaimer"`
}

type wrpVersionJSON struct {
	ID         string    `json:"id"`
	Cause      string    `json:"cause"`
	Action     string    `json:"action"`
	SetVersion int64     `json:"set_version"`
	ChangeID   *string   `json:"change_id"`
	ReasonCode string    `json:"reason_code"`
	CreatedBy  *staffRef `json:"created_by"`
	CreatedAt  string    `json:"created_at"`
}

type wrpVersionsResponse struct {
	Items      []wrpVersionJSON `json:"items"`
	Disclaimer string           `json:"disclaimer"`
}

type wrpApprovalJSON struct {
	ApproverID staffRef `json:"approver_id"`
	ReasonCode string   `json:"reason_code"`
	DecidedAt  string   `json:"decided_at"`
}

type wrpChangeJSON struct {
	ID                 string            `json:"id"`
	TenantID           string            `json:"tenant_id"`
	Cause              string            `json:"cause"`
	Action             string            `json:"action"`
	ReasonCode         string            `json:"reason_code"`
	Note               *string           `json:"note"`
	PayloadHash        string            `json:"payload_hash"`
	Widening           bool              `json:"widening"`
	RequiredApprovals  int               `json:"required_approvals"`
	BaseSetVersion     int64             `json:"base_set_version"`
	RequestedBy        staffRef          `json:"requested_by"`
	Status             string            `json:"status"`
	CreatedAt          string            `json:"created_at"`
	ExpiresAt          string            `json:"expires_at"`
	DecidedBy          *staffRef         `json:"decided_by"`
	DecidedAt          *string           `json:"decided_at"`
	DecisionReasonCode *string           `json:"decision_reason_code"`
	ExecutedBy         *staffRef         `json:"executed_by"`
	ExecutedAt         *string           `json:"executed_at"`
	Approvals          []wrpApprovalJSON `json:"approvals"`
}

type wrpChangeResponse struct {
	Change     wrpChangeJSON   `json:"change"`
	Executed   bool            `json:"executed"`
	Policy     *wrpVersionJSON `json:"policy,omitempty"`
	Disclaimer string          `json:"disclaimer"`
}

type wrpChangeListResponse struct {
	Items      []wrpChangeJSON `json:"items"`
	Disclaimer string          `json:"disclaimer"`
}

func wrpTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func toWRPVersionJSON(p withdrawalpolicy.Policy, v staffViewer) wrpVersionJSON {
	platform := staffScopePlatform
	return wrpVersionJSON{ID: p.ID.String(), Cause: p.Cause, Action: p.Action, SetVersion: p.SetVersion, ChangeID: uuidPtrString(p.ChangeID),
		ReasonCode: p.ReasonCode, CreatedBy: v.optRef(&platform, p.CreatedBy), CreatedAt: wrpTime(p.CreatedAt)}
}

func toWRPChangeJSON(c withdrawalpolicy.Change, v staffViewer) wrpChangeJSON {
	platform := staffScopePlatform
	out := wrpChangeJSON{ID: c.ID.String(), TenantID: c.TenantID.String(), Cause: c.Cause, Action: c.Action, ReasonCode: c.ReasonCode,
		Note: c.Note, PayloadHash: c.PayloadHash, Widening: c.Widening, RequiredApprovals: c.RequiredApprovals, BaseSetVersion: c.BaseSetVersion,
		RequestedBy: v.ref(staffScopePlatform, c.RequestedBy), Status: c.Status, CreatedAt: wrpTime(c.CreatedAt), ExpiresAt: wrpTime(c.ExpiresAt),
		DecidedBy: v.optRef(&platform, c.DecidedBy), DecidedAt: timePtrString(c.DecidedAt), DecisionReasonCode: c.DecisionReasonCode,
		ExecutedBy: v.optRef(&platform, c.ExecutedBy), ExecutedAt: timePtrString(c.ExecutedAt), Approvals: []wrpApprovalJSON{}}
	for _, ap := range c.Approvals {
		out.Approvals = append(out.Approvals, wrpApprovalJSON{ApproverID: v.ref(staffScopePlatform, ap.ApproverID), ReasonCode: ap.ReasonCode,
			DecidedAt: wrpTime(ap.DecidedAt)})
	}
	return out
}

func toWRPChangeResponse(res *withdrawalpolicy.Result, v staffViewer) wrpChangeResponse {
	out := wrpChangeResponse{Change: toWRPChangeJSON(res.Change, v), Executed: res.Executed, Disclaimer: withdrawalpolicy.Disclaimer}
	if res.Policy != nil {
		p := toWRPVersionJSON(*res.Policy, v)
		out.Policy = &p
	}
	return out
}

// ---- handlers -------------------------------------------------------------------------------------------------------

func newWRPEffectiveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		tenantID, ok := wrpPathUUID(w, r, c.requestID, "tenantID")
		if !ok {
			return
		}
		view, err := c.svc.Effective(r.Context(), c.actor, tenantID)
		if err != nil {
			writeWRPError(w, c, "effective", err)
			return
		}
		out := wrpEffectiveResponse{TenantID: view.TenantID.String(), SetVersion: view.SetVersion, Configured: view.Configured,
			Causes: []wrpCauseJSON{}, ActionSemantics: []wrpActionSemanticJSON{}, Disclaimer: view.Disclaimer,
			Parked: wrpParkedJSON{Visible: view.Parked.Visible, Count: view.Parked.Count, OldestParkedSince: timePtrString(view.Parked.OldestParkedSince),
				OldestAgeSeconds: view.Parked.OldestAgeSeconds, Note: view.Parked.Note}}
		for _, as := range view.ActionSemantics {
			out.ActionSemantics = append(out.ActionSemantics, wrpActionSemanticJSON{Action: as.Action, Meaning: as.Meaning})
		}
		for _, cv := range view.Causes {
			cj := wrpCauseJSON{Cause: cv.Cause, Class: cv.Class, Configurable: cv.Configurable, Evaluated: cv.Evaluated,
				GoverningReference: cv.GoverningReference, CurrentBehaviour: cv.CurrentBehaviour, Stages: []wrpStageJSON{},
				Unconfigured: cv.Unconfigured, Notice: cv.Notice}
			for _, st := range cv.Stages {
				cj.Stages = append(cj.Stages, wrpStageJSON{Stage: st.Stage, Action: st.Action, Source: st.Source,
					PolicyID: uuidPtrString(st.PolicyID), SetVersion: st.SetVersion})
			}
			out.Causes = append(out.Causes, cj)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func newWRPVersionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		tenantID, ok := wrpPathUUID(w, r, c.requestID, "tenantID")
		if !ok {
			return
		}
		list, err := c.svc.History(r.Context(), c.actor, tenantID)
		if err != nil {
			writeWRPError(w, c, "versions", err)
			return
		}
		out := wrpVersionsResponse{Items: []wrpVersionJSON{}, Disclaimer: withdrawalpolicy.Disclaimer}
		for _, p := range list {
			out.Items = append(out.Items, toWRPVersionJSON(p, c.viewer))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type wrpCreateRequest struct {
	TenantID   string `json:"tenant_id"`
	Cause      string `json:"cause"`
	Action     string `json:"action"`
	ReasonCode string `json:"reason_code"`
	Note       string `json:"note"`
}

func newWRPCreateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		var body wrpCreateRequest
		if err := decodeJSONStrict(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		tenantID, err := uuid.Parse(body.TenantID)
		if err != nil || tenantID == uuid.Nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "tenant_id must be a valid UUID")
			return
		}
		res, err := c.svc.Request(r.Context(), c.actor, withdrawalpolicy.RequestInput{
			TenantID: tenantID, Cause: body.Cause, Action: body.Action, ReasonCode: body.ReasonCode, Note: body.Note,
		})
		if err != nil {
			writeWRPError(w, c, "request", err)
			return
		}
		writeJSON(w, http.StatusCreated, toWRPChangeResponse(res, c.viewer))
	}
}

type wrpDecisionRequest struct {
	ReasonCode string `json:"reason_code"`
}

func newWRPApproveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := wrpPathUUID(w, r, c.requestID, "changeID")
		if !ok {
			return
		}
		var body wrpDecisionRequest
		if err := decodeJSONStrict(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		res, err := c.svc.Approve(r.Context(), c.actor, id, body.ReasonCode)
		if err != nil {
			writeWRPError(w, c, "approve", err)
			return
		}
		writeJSON(w, http.StatusOK, toWRPChangeResponse(res, c.viewer))
	}
}

func newWRPRejectHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := wrpPathUUID(w, r, c.requestID, "changeID")
		if !ok {
			return
		}
		var body wrpDecisionRequest
		if err := decodeJSONStrict(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		ch, err := c.svc.Reject(r.Context(), c.actor, id, body.ReasonCode)
		if err != nil {
			writeWRPError(w, c, "reject", err)
			return
		}
		writeJSON(w, http.StatusOK, wrpChangeResponse{Change: toWRPChangeJSON(*ch, c.viewer), Disclaimer: withdrawalpolicy.Disclaimer})
	}
}

func newWRPCancelHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := wrpPathUUID(w, r, c.requestID, "changeID")
		if !ok {
			return
		}
		ch, err := c.svc.Cancel(r.Context(), c.actor, id)
		if err != nil {
			writeWRPError(w, c, "cancel", err)
			return
		}
		writeJSON(w, http.StatusOK, wrpChangeResponse{Change: toWRPChangeJSON(*ch, c.viewer), Disclaimer: withdrawalpolicy.Disclaimer})
	}
}

func newWRPGetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := wrpPathUUID(w, r, c.requestID, "changeID")
		if !ok {
			return
		}
		ch, err := c.svc.GetChange(r.Context(), c.actor, id)
		if err != nil {
			writeWRPError(w, c, "get", err)
			return
		}
		writeJSON(w, http.StatusOK, wrpChangeResponse{Change: toWRPChangeJSON(*ch, c.viewer), Executed: ch.Status == withdrawalpolicy.StatusExecuted,
			Disclaimer: withdrawalpolicy.Disclaimer})
	}
}

func newWRPListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginWRPCall(deps, w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		f := withdrawalpolicy.ListFilter{Status: q.Get("status")}
		if v := q.Get("tenant_id"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "tenant_id must be a valid UUID")
				return
			}
			f.TenantID = id
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "limit must be 1..200")
				return
			}
			f.Limit = n
		}
		list, err := c.svc.ListChanges(r.Context(), c.actor, f)
		if err != nil {
			writeWRPError(w, c, "list", err)
			return
		}
		out := wrpChangeListResponse{Items: []wrpChangeJSON{}, Disclaimer: withdrawalpolicy.Disclaimer}
		for _, ch := range list {
			out.Items = append(out.Items, toWRPChangeJSON(ch, c.viewer))
		}
		writeJSON(w, http.StatusOK, out)
	}
}
