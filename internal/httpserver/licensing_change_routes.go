// Migration 0177 (MKT-DUAL-1; licensing audit G-1 / G-2 / G-9): the licensing / jurisdiction change routes.
//
// A thin, authenticating, error-mapping layer over internal/licensingchange, which owns every rule (the closed kind
// registry, the widening / narrowing classification the DATABASE computes, four-eyes by distinct staff AND distinct Person,
// approvals at most 24 h old, signed actor proofs, audit). Nothing here decides anything.
//
// Authentication and tenant binding: every route is the staff chain + RequirePermission(<static licensing_change:*>). The
// actor (staff id, scope, role) comes from the verified token, never from a body. A tenant-scoped caller may only file
// tenant-requestable kinds about its OWN tenant (the payload's tenant must equal the token's; a foreign one is a 404) and read
// the requests about itself (RLS); it never approves, rejects or executes. A platform caller is platform_admin (the interim
// answer to HDR-M-1).
//
// Idempotency is by state: an identical open request is LICENSING_CHANGE_DUPLICATE_PENDING, a replayed approval by the same
// Person LICENSING_CHANGE_ALREADY_DECIDED, a replayed decision or cancel LICENSING_CHANGE_NOT_PENDING; each is a 409 that
// changes nothing. A process with no actor-proof issuer answers 500 and writes nothing.
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/licensingchange"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerLicensingChangeRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("POST /v1/admin/licensing-changes",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermLicensingChangeRequest)(newLicensingChangeCreateHandler(deps))))
	mux.Handle("GET /v1/admin/licensing-changes",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermLicensingChangeRead)(newLicensingChangeListHandler(deps))))
	mux.Handle("GET /v1/admin/licensing-changes/{changeID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermLicensingChangeRead)(newLicensingChangeGetHandler(deps))))
	mux.Handle("POST /v1/admin/licensing-changes/{changeID}/approve",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermLicensingChangeApprove)(newLicensingChangeApproveHandler(deps))))
	mux.Handle("POST /v1/admin/licensing-changes/{changeID}/reject",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermLicensingChangeApprove)(newLicensingChangeRejectHandler(deps))))
	mux.Handle("POST /v1/admin/licensing-changes/{changeID}/cancel",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermLicensingChangeRequest)(newLicensingChangeCancelHandler(deps))))
}

// licensingChangeActorFrom builds the service actor from the verified token.
func licensingChangeActorFrom(tc tenant.Context, r *http.Request, requestID string) (licensingchange.Actor, bool) {
	staffID, err := uuid.Parse(tc.Subject)
	if err != nil || staffID == uuid.Nil {
		return licensingchange.Actor{}, false
	}
	a := licensingchange.Actor{StaffID: staffID, Scope: licensingchange.ScopePlatform, Role: tc.Role,
		IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID}
	if tc.TenantID != uuid.Nil {
		a.Scope, a.TenantID = licensingchange.ScopeTenant, tc.TenantID
	}
	return a, true
}

type licensingCall struct {
	requestID string
	logger    observabilityLogger
	actor     licensingchange.Actor
	svc       *licensingchange.Service
	viewer    staffViewer
}

func beginLicensingCall(deps Deps, w http.ResponseWriter, r *http.Request) (licensingCall, bool) {
	c := licensingCall{
		requestID: observability.RequestIDFromContext(r.Context()),
		logger:    observability.LoggerFromContext(r.Context(), deps.Logger),
		svc:       deps.LicensingChanges,
	}
	if c.svc == nil {
		apierror.Write(w, c.requestID, apierror.CodeLicensingChangeUnavailable, "licensing changes are not available in this process")
		return c, false
	}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	a, ok := licensingChangeActorFrom(tc, r, c.requestID)
	if !ok {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	c.actor = a
	c.viewer = staffViewerOf(tc)
	return c, true
}

func pathChangeID(w http.ResponseWriter, r *http.Request, requestID string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("changeID"))
	if err != nil || id == uuid.Nil {
		apierror.Write(w, requestID, apierror.CodeValidation, "changeID must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// licensingChangeErrorCode maps a service error to the closed API code and a fixed message (never the error text). Pure, so
// the whole mapping is unit-tested without a database. Specific sentinels before broad ones.
func licensingChangeErrorCode(err error) (apierror.Code, string) {
	var domain *licensingchange.DomainError
	switch {
	case errors.Is(err, licensingchange.ErrNotFound), errors.Is(err, jurisdiction.ErrNotFound):
		return apierror.CodeNotFound, "not found"
	case errors.Is(err, licensingchange.ErrNoPerson):
		return apierror.CodeLicensingChangeNoPerson, "this account cannot take part in a four-eyes decision"
	case errors.Is(err, licensingchange.ErrActingSession), errors.Is(err, licensingchange.ErrForbidden):
		return apierror.CodeForbidden, "forbidden"
	case errors.Is(err, licensingchange.ErrInvalidInput), errors.Is(err, jurisdiction.ErrInvalidInput), errors.Is(err, operatingmarket.ErrInvalidInput):
		return apierror.CodeValidation, "invalid request"
	case errors.Is(err, licensingchange.ErrSelfApproval):
		return apierror.CodeLicensingChangeSelfApproval, "the requester may not decide their own request"
	case errors.Is(err, licensingchange.ErrNotPending):
		return apierror.CodeLicensingChangeNotPending, "the request is not open"
	case errors.Is(err, licensingchange.ErrAlreadyDecided):
		return apierror.CodeLicensingChangeAlreadyDecided, "this person already approved the request"
	case errors.Is(err, licensingchange.ErrDuplicatePending):
		return apierror.CodeLicensingChangeDuplicatePending, "an identical request is already open"
	case errors.Is(err, licensingchange.ErrApprovalsStale):
		return apierror.CodeLicensingChangeApprovalsStale, "approvals are older than the approval window; file a new request"
	case errors.Is(err, licensingchange.ErrRetryableLockTimeout):
		return apierror.CodeLicensingChangeRetryableLockWait, "the subject is busy; retry"
	case errors.As(err, &domain):
		return apierror.CodeLicensingChangeRefused, "the change was refused by the domain rules"
	}
	if code := licensingchange.GuardCode(err); code != "" {
		if code[:2] == "AP" {
			return apierror.CodeLicensingChangeProofRefused, "the actor proof was refused"
		}
		return apierror.CodeLicensingChangeGuardRefused, "a database guard refused the change"
	}
	return apierror.CodeInternal, "internal error"
}

func writeLicensingChangeError(w http.ResponseWriter, c licensingCall, op string, err error) {
	code, msg := licensingChangeErrorCode(err)
	if code == apierror.CodeInternal {
		if errors.Is(err, licensingchange.ErrProofUnavailable) || errors.Is(err, actorproof.ErrNoIssuer) || errors.Is(err, actorproof.ErrStepUpMissing) {
			c.logger.Error("licensing change: governed write refused (fail closed)", "op", op, "error", err)
		} else {
			c.logger.Error("licensing change: unexpected error", "op", op, "error", err)
		}
	}
	apierror.Write(w, c.requestID, code, msg)
}

// ---- response shapes ------------------------------------------------------------------------------------------------

type licensingApprovalJSON struct {
	ApproverID staffRef `json:"approver_id"`
	ReasonCode string   `json:"reason_code"`
	DecidedAt  string   `json:"decided_at"`
}

type licensingChangeJSON struct {
	ID                 string                  `json:"id"`
	Kind               string                  `json:"kind"`
	Payload            json.RawMessage         `json:"payload"`
	PayloadHash        string                  `json:"payload_hash"`
	ReasonCode         string                  `json:"reason_code"`
	Note               *string                 `json:"note"`
	Widening           bool                    `json:"widening"`
	RequiredApprovals  int                     `json:"required_approvals"`
	SubjectTenantID    *string                 `json:"subject_tenant_id"`
	SubjectLicenceID   *string                 `json:"subject_licence_id"`
	RequestedBy        staffRef                `json:"requested_by"`
	RequestedScope     string                  `json:"requested_scope"`
	Status             string                  `json:"status"`
	CreatedAt          string                  `json:"created_at"`
	ExpiresAt          string                  `json:"expires_at"`
	DecidedBy          *staffRef               `json:"decided_by"`
	DecidedAt          *string                 `json:"decided_at"`
	DecisionReasonCode *string                 `json:"decision_reason_code"`
	ExecutedBy         *staffRef               `json:"executed_by"`
	ExecutedAt         *string                 `json:"executed_at"`
	Approvals          []licensingApprovalJSON `json:"approvals"`
}

type licensingChangeResponse struct {
	ChangeRequest licensingChangeJSON `json:"change_request"`
	Executed      bool                `json:"executed"`
	Outcome       map[string]any      `json:"outcome,omitempty"`
	Disclaimer    string              `json:"disclaimer"`
}

func uuidPtrString(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func timePtrString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// toLicensingChangeJSON renders a request for viewer v (ADR 0114 H1): a tenant viewer sees the staff id of a tenant-scope
// actor (its own staff) only; every platform actor (approver, rejecter, executor, a platform requester) is a null ref.
func toLicensingChangeJSON(r licensingchange.Request, v staffViewer) licensingChangeJSON {
	decidedScope := staffScopePlatform // reject / expire are platform; a cancel is by the requester
	if r.Status == licensingchange.StatusCancelled {
		decidedScope = r.RequestedScope
	}
	platform := staffScopePlatform
	out := licensingChangeJSON{
		ID: r.ID.String(), Kind: string(r.Kind), Payload: r.Payload, PayloadHash: r.PayloadHash, ReasonCode: r.ReasonCode,
		Note: r.Note, Widening: r.Widening, RequiredApprovals: r.RequiredApprovals,
		SubjectTenantID: uuidPtrString(r.SubjectTenantID), SubjectLicenceID: uuidPtrString(r.SubjectLicenceID),
		RequestedBy: v.ref(r.RequestedScope, r.RequestedBy), RequestedScope: r.RequestedScope, Status: r.Status,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano), ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339Nano),
		DecidedBy: v.optRef(&decidedScope, r.DecidedBy), DecidedAt: timePtrString(r.DecidedAt), DecisionReasonCode: r.DecisionReasonCode,
		ExecutedBy: v.optRef(&platform, r.ExecutedBy), ExecutedAt: timePtrString(r.ExecutedAt),
		Approvals: []licensingApprovalJSON{},
	}
	for _, ap := range r.Approvals {
		out.Approvals = append(out.Approvals, licensingApprovalJSON{ApproverID: v.ref(staffScopePlatform, ap.ApproverID), ReasonCode: ap.ReasonCode,
			DecidedAt: ap.DecidedAt.UTC().Format(time.RFC3339Nano)})
	}
	return out
}

func toLicensingChangeResponse(res *licensingchange.Result, v staffViewer) licensingChangeResponse {
	return licensingChangeResponse{ChangeRequest: toLicensingChangeJSON(res.Request, v), Executed: res.Executed, Outcome: res.Outcome,
		Disclaimer: licensingchange.Disclaimer}
}

// ---- handlers -------------------------------------------------------------------------------------------------------

type licensingChangeCreateRequest struct {
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	ReasonCode string          `json:"reason_code"`
	Note       string          `json:"note"`
}

func newLicensingChangeCreateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLicensingCall(deps, w, r)
		if !ok {
			return
		}
		var body licensingChangeCreateRequest
		if err := decodeJSONStrict(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		res, err := c.svc.Request(r.Context(), c.actor, licensingchange.RequestInput{
			Kind: licensingchange.Kind(body.Kind), Payload: body.Payload, ReasonCode: body.ReasonCode, Note: body.Note,
		})
		if err != nil {
			writeLicensingChangeError(w, c, "request", err)
			return
		}
		writeJSON(w, http.StatusCreated, toLicensingChangeResponse(res, c.viewer))
	}
}

type licensingDecisionRequest struct {
	ReasonCode string `json:"reason_code"`
}

func newLicensingChangeApproveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLicensingCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := pathChangeID(w, r, c.requestID)
		if !ok {
			return
		}
		var body licensingDecisionRequest
		if err := decodeJSONStrict(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		res, err := c.svc.Approve(r.Context(), c.actor, id, body.ReasonCode)
		if err != nil {
			writeLicensingChangeError(w, c, "approve", err)
			return
		}
		writeJSON(w, http.StatusOK, toLicensingChangeResponse(res, c.viewer))
	}
}

func newLicensingChangeRejectHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLicensingCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := pathChangeID(w, r, c.requestID)
		if !ok {
			return
		}
		var body licensingDecisionRequest
		if err := decodeJSONStrict(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		req, err := c.svc.Reject(r.Context(), c.actor, id, body.ReasonCode)
		if err != nil {
			writeLicensingChangeError(w, c, "reject", err)
			return
		}
		writeJSON(w, http.StatusOK, licensingChangeResponse{ChangeRequest: toLicensingChangeJSON(*req, c.viewer), Disclaimer: licensingchange.Disclaimer})
	}
}

func newLicensingChangeCancelHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLicensingCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := pathChangeID(w, r, c.requestID)
		if !ok {
			return
		}
		req, err := c.svc.Cancel(r.Context(), c.actor, id)
		if err != nil {
			writeLicensingChangeError(w, c, "cancel", err)
			return
		}
		writeJSON(w, http.StatusOK, licensingChangeResponse{ChangeRequest: toLicensingChangeJSON(*req, c.viewer), Disclaimer: licensingchange.Disclaimer})
	}
}

func newLicensingChangeGetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLicensingCall(deps, w, r)
		if !ok {
			return
		}
		id, ok := pathChangeID(w, r, c.requestID)
		if !ok {
			return
		}
		req, err := c.svc.Get(r.Context(), c.actor, id)
		if err != nil {
			writeLicensingChangeError(w, c, "get", err)
			return
		}
		writeJSON(w, http.StatusOK, licensingChangeResponse{ChangeRequest: toLicensingChangeJSON(*req, c.viewer), Executed: req.Status == licensingchange.StatusExecuted,
			Disclaimer: licensingchange.Disclaimer})
	}
}

type licensingChangeListResponse struct {
	Items      []licensingChangeJSON `json:"items"`
	Disclaimer string                `json:"disclaimer"`
}

func newLicensingChangeListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLicensingCall(deps, w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		f := licensingchange.ListFilter{Status: q.Get("status"), Kind: licensingchange.Kind(q.Get("kind"))}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "limit must be 1..200")
				return
			}
			f.Limit = n
		}
		list, err := c.svc.List(r.Context(), c.actor, f)
		if err != nil {
			writeLicensingChangeError(w, c, "list", err)
			return
		}
		out := licensingChangeListResponse{Items: []licensingChangeJSON{}, Disclaimer: licensingchange.Disclaimer}
		for _, req := range list {
			out.Items = append(out.Items, toLicensingChangeJSON(req, c.viewer))
		}
		writeJSON(w, http.StatusOK, out)
	}
}
