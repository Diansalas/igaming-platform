// Stage 10.3 W2a: the provider-credential admin API (ADR 0093 §3 and its
// W2a design-review amendment; security review
// docs/plans/stage-10.3-planning/07-w2a-design-review-security.md §6).
//
// Six routes under /v1/admin/tenants/{tenantID}/. The target tenant always
// comes from the path through canActOnTenant (ADR 0011): a tenant-scoped
// caller may only name its own tenant; the body never carries a tenant and
// unknown fields are refused. Enabling a key takes two people (file ->
// decide -> apply, platform only); disabling takes one (transitions,
// platform or tenant admin). Errors are classified by SQLSTATE in
// internal/providercred and mapped here to one fixed response per class;
// trigger text, store text, the secret and the confirmation value are
// never returned, logged or audited. Request bodies are never logged.
package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// registerProviderCredentialRoutes mounts the admin API. The read and
// transition routes are always mounted (revocation must work even when the
// real credential subsystem is not constructed); the request, decision
// and apply routes are mounted only when it is (security review §3:
// absent fingerprint key -> routes not mounted).
func registerProviderCredentialRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	mux.Handle("GET /v1/admin/tenants/{tenantID}/provider-credentials",
		staff(auth.RequirePermission(auth.PermProviderCredentialRead)(newListProviderCredentialsHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/provider-credentials/{handleID}/transitions",
		staff(auth.RequirePermission(auth.PermProviderCredentialRevoke)(newTransitionProviderCredentialHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/provider-credential-requests",
		staff(auth.RequireAnyPermission(auth.PermProviderCredentialRequest, auth.PermProviderCredentialApprove)(newListProviderCredentialRequestsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/provider-credential-requests/{requestID}",
		staff(auth.RequireAnyPermission(auth.PermProviderCredentialRequest, auth.PermProviderCredentialApprove)(newGetProviderCredentialRequestHandler(deps))))
	if deps.ProviderCredentials == nil {
		return
	}
	mux.Handle("POST /v1/admin/tenants/{tenantID}/provider-credential-requests",
		staff(auth.RequirePermission(auth.PermProviderCredentialRequest)(newFileProviderCredentialRequestHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/provider-credential-requests/{requestID}/decisions",
		staff(auth.RequirePermission(auth.PermProviderCredentialApprove)(newDecideProviderCredentialRequestHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/provider-credential-requests/{requestID}/apply",
		staff(auth.RequirePermission(auth.PermProviderCredentialRequest)(newApplyProviderCredentialRequestHandler(deps))))
}

// providerCredentialCall carries what every handler resolves first.
type providerCredentialCall struct {
	requestID string
	logger    interface {
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
	tc      tenant.Context
	subject uuid.UUID
	target  uuid.UUID
	ac      providercred.AuditContext
}

// beginProviderCredentialCall authenticates the subject, parses the path
// tenant and applies canActOnTenant. It writes the response and returns
// false on any failure; a foreign-tenant attempt is 403 with no data, and
// a denied audit row in the caller's own scope.
func beginProviderCredentialCall(deps Deps, w http.ResponseWriter, r *http.Request, op string) (providerCredentialCall, bool) {
	c := providerCredentialCall{
		requestID: observability.RequestIDFromContext(r.Context()),
		logger:    observability.LoggerFromContext(r.Context(), deps.Logger),
	}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	target, err := uuid.Parse(r.PathValue("tenantID"))
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
		return c, false
	}
	c.tc, c.subject, c.target = tc, subject, target
	c.ac = providercred.AuditContext{ActorID: subject, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID}
	if !canActOnTenant(tc, target) {
		c.logger.Warn("provider_credential_denied", "op", op, "reason", "foreign_tenant")
		recordProviderCredentialDenied(r.Context(), deps, c, op)
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return c, false
	}
	return c, true
}

// recordProviderCredentialDenied writes the denied audit row in the
// caller's OWN scope (never the named foreign tenant's).
func recordProviderCredentialDenied(ctx context.Context, deps Deps, c providerCredentialCall, op string) {
	entry := audit.Entry{
		TenantID:  c.tc.TenantID,
		ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "provider_credential." + op, TargetType: "provider_credential_handle",
		Outcome: audit.OutcomeDenied, IPAddress: c.ac.IPAddress, UserAgent: c.ac.UserAgent, RequestID: c.requestID,
		Metadata: map[string]any{"denied": "foreign_tenant"},
	}
	record := func(ctx context.Context, tx pgx.Tx) error { return audit.Record(ctx, tx, entry) }
	var err error
	if c.tc.TenantID == uuid.Nil {
		err = deps.DB.WithPlatformAdmin(ctx, c.subject, record)
	} else {
		err = deps.DB.WithTenant(ctx, c.tc.TenantID, record)
	}
	if err != nil {
		c.logger.Error("provider_credential_denied_audit_failed", "op", op)
	}
}

// writeProviderCredentialError is the review §6 error mapping. The class
// goes to the log only; each response body is fixed per Kind (and so
// byte-identical apart from request_id).
func writeProviderCredentialError(w http.ResponseWriter, c providerCredentialCall, op string, err error) {
	kind := providercred.KindOf(err)
	switch kind {
	case providercred.KindInvalid:
		c.logger.Warn("provider_credential_rejected", "op", op, "kind", kind.String(), "class", providercred.ClassOf(err))
		apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
	case providercred.KindNotFound:
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
	case providercred.KindRegistrationRejected:
		c.logger.Warn("provider_credential_rejected", "op", op, "kind", kind.String(), "class", providercred.ClassOf(err))
		apierror.Write(w, c.requestID, apierror.CodeCredentialRegistrationRejected, "credential registration rejected")
	case providercred.KindApprovalRejected:
		c.logger.Warn("provider_credential_rejected", "op", op, "kind", kind.String(), "class", providercred.ClassOf(err))
		apierror.Write(w, c.requestID, apierror.CodeApprovalRejected, "approval rejected")
	case providercred.KindActivationRejected:
		c.logger.Warn("provider_credential_rejected", "op", op, "kind", kind.String(), "class", providercred.ClassOf(err))
		apierror.Write(w, c.requestID, apierror.CodeCredentialActivationRejected, "credential activation rejected")
	case providercred.KindTransitionRejected:
		c.logger.Warn("provider_credential_rejected", "op", op, "kind", kind.String(), "class", providercred.ClassOf(err))
		apierror.Write(w, c.requestID, apierror.CodeCredentialTransitionRejected, "credential transition rejected")
	default:
		c.logger.Error("provider_credential_failed", "op", op)
		apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	}
}

// --- DTOs (allow-listed fields only; no store result ever flows here) ---

type providerCredentialHandleDTO struct {
	ID                  string  `json:"id"`
	Domain              string  `json:"domain"`
	ProviderID          string  `json:"provider_id"`
	Purpose             string  `json:"purpose"`
	KeyID               string  `json:"key_id"`
	Status              string  `json:"status"`
	NotBefore           string  `json:"not_before"`
	NotAfter            *string `json:"not_after"`
	StatusChangedAt     string  `json:"status_changed_at"`
	Fingerprint         string  `json:"fingerprint"`
	SecretRef           string  `json:"secret_ref"`
	VendorAccountID     *string `json:"vendor_account_id"`
	ActivationRequestID string  `json:"activation_request_id"`
	RevokedAt           *string `json:"revoked_at,omitempty"`
	RevokedBy           *string `json:"revoked_by,omitempty"`
	RevokeReason        *string `json:"revoke_reason,omitempty"`
}

func rfc3339Ptr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func uuidPtr(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func toHandleDTO(h providercred.Handle) providerCredentialHandleDTO {
	return providerCredentialHandleDTO{
		ID: h.ID.String(), Domain: h.Domain, ProviderID: h.ProviderID, Purpose: h.Purpose, KeyID: h.KeyID,
		Status: h.Status, NotBefore: h.NotBefore.UTC().Format(time.RFC3339Nano), NotAfter: rfc3339Ptr(h.NotAfter),
		StatusChangedAt: h.StatusChangedAt.UTC().Format(time.RFC3339Nano), Fingerprint: h.Fingerprint,
		SecretRef: h.SecretRef, VendorAccountID: h.VendorAccountID, ActivationRequestID: h.ActivationRequestID.String(),
		RevokedAt: rfc3339Ptr(h.RevokedAt), RevokedBy: uuidPtr(h.RevokedBy), RevokeReason: h.RevokeReason,
	}
}

type providerCredentialApprovalDTO struct {
	ID                  string  `json:"id"`
	ApproverPrincipalID string  `json:"approver_principal_id"`
	Decision            string  `json:"decision"`
	ContentHash         string  `json:"content_hash"`
	ReasonCode          *string `json:"reason_code"`
	DecidedAt           string  `json:"decided_at"`
}

type providerCredentialRequestDTO struct {
	ID                     string                          `json:"id"`
	Domain                 string                          `json:"domain"`
	ProviderID             string                          `json:"provider_id"`
	Purpose                string                          `json:"purpose"`
	KeyID                  string                          `json:"key_id"`
	SecretRef              string                          `json:"secret_ref"`
	Fingerprint            string                          `json:"fingerprint"`
	VendorAccountID        *string                         `json:"vendor_account_id"`
	NotBefore              string                          `json:"not_before"`
	NotAfter               *string                         `json:"not_after"`
	PredecessorHandleID    *string                         `json:"predecessor_handle_id"`
	PredecessorDisposition string                          `json:"predecessor_disposition"`
	PredecessorNotAfter    *string                         `json:"predecessor_not_after"`
	ContentHash            string                          `json:"content_hash"`
	ReasonCode             string                          `json:"reason_code"`
	RequestedByPrincipalID string                          `json:"requested_by_principal_id"`
	RequestedAt            string                          `json:"requested_at"`
	State                  string                          `json:"state"`
	AppliedAt              *string                         `json:"applied_at"`
	AppliedByPrincipalID   *string                         `json:"applied_by_principal_id"`
	AppliedHandleID        *string                         `json:"applied_handle_id"`
	Approvals              []providerCredentialApprovalDTO `json:"approvals"`
}

func toApprovalDTO(a providercred.Approval) providerCredentialApprovalDTO {
	return providerCredentialApprovalDTO{
		ID: a.ID.String(), ApproverPrincipalID: a.ApproverPrincipalID.String(), Decision: a.Decision,
		ContentHash: a.ContentHash, ReasonCode: a.ReasonCode, DecidedAt: a.DecidedAt.UTC().Format(time.RFC3339Nano),
	}
}

func toRequestDTO(r providercred.Request) providerCredentialRequestDTO {
	d := providerCredentialRequestDTO{
		ID: r.ID.String(), Domain: r.Domain, ProviderID: r.ProviderID, Purpose: r.Purpose, KeyID: r.KeyID,
		SecretRef: r.SecretRef, Fingerprint: r.Fingerprint, VendorAccountID: r.VendorAccountID,
		NotBefore: r.NotBefore.UTC().Format(time.RFC3339Nano), NotAfter: rfc3339Ptr(r.NotAfter),
		PredecessorHandleID: uuidPtr(r.PredecessorHandleID), PredecessorDisposition: r.PredecessorDisposition,
		PredecessorNotAfter: rfc3339Ptr(r.PredecessorNotAfter), ContentHash: r.ContentHash, ReasonCode: r.ReasonCode,
		RequestedByPrincipalID: r.RequestedBy.String(), RequestedAt: r.RequestedAt.UTC().Format(time.RFC3339Nano),
		State: r.State, AppliedAt: rfc3339Ptr(r.AppliedAt), AppliedByPrincipalID: uuidPtr(r.AppliedBy),
		AppliedHandleID: uuidPtr(r.AppliedHandleID), Approvals: []providerCredentialApprovalDTO{},
	}
	for _, a := range r.Approvals {
		d.Approvals = append(d.Approvals, toApprovalDTO(a))
	}
	return d
}

// --- request bodies (no tenant field; DisallowUnknownFields) ---

type fileProviderCredentialRequestBody struct {
	Domain                 string  `json:"domain"`
	ProviderID             string  `json:"provider_id"`
	Purpose                string  `json:"purpose"`
	KeyID                  string  `json:"key_id"`
	SecretRef              string  `json:"secret_ref"`
	Confirmation           string  `json:"confirmation"`
	VendorAccountID        *string `json:"vendor_account_id"`
	NotBefore              string  `json:"not_before"`
	NotAfter               *string `json:"not_after"`
	PredecessorHandleID    *string `json:"predecessor_handle_id"`
	PredecessorDisposition string  `json:"predecessor_disposition"`
	PredecessorNotAfter    *string `json:"predecessor_not_after"`
	ReasonCode             string  `json:"reason_code"`
}

type decideProviderCredentialRequestBody struct {
	Decision    string `json:"decision"`
	ContentHash string `json:"content_hash"`
	ReasonCode  string `json:"reason_code"`
}

type transitionProviderCredentialBody struct {
	Action     string  `json:"action"`
	NotAfter   *string `json:"not_after"`
	ReasonCode string  `json:"reason_code"`
}

var errInvalidRequestBody = errors.New("invalid request body")

func parseTimePtr(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil, errInvalidRequestBody
	}
	return &t, nil
}

// --- handlers ---

func newListProviderCredentialsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginProviderCredentialCall(deps, w, r, "list")
		if !ok {
			return
		}
		var handles []providercred.Handle
		err := deps.DB.WithTenant(r.Context(), c.target, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			handles, err = providercred.ListHandles(ctx, tx, c.target)
			return err
		})
		if err != nil {
			writeProviderCredentialError(w, c, "list", err)
			return
		}
		out := make([]providerCredentialHandleDTO, 0, len(handles))
		for _, h := range handles {
			out = append(out, toHandleDTO(h))
		}
		writeJSON(w, http.StatusOK, map[string]any{"handles": out})
	}
}

func newListProviderCredentialRequestsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginProviderCredentialCall(deps, w, r, "list_requests")
		if !ok {
			return
		}
		var reqs []providercred.Request
		err := deps.DB.WithPlatformAdmin(r.Context(), c.subject, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			reqs, err = providercred.ListRequests(ctx, tx, c.target)
			return err
		})
		if err != nil {
			writeProviderCredentialError(w, c, "list_requests", err)
			return
		}
		out := make([]providerCredentialRequestDTO, 0, len(reqs))
		for _, q := range reqs {
			out = append(out, toRequestDTO(q))
		}
		writeJSON(w, http.StatusOK, map[string]any{"requests": out})
	}
}

func newGetProviderCredentialRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginProviderCredentialCall(deps, w, r, "get_request")
		if !ok {
			return
		}
		reqID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var q providercred.Request
		err = deps.DB.WithPlatformAdmin(r.Context(), c.subject, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			q, err = providercred.GetRequest(ctx, tx, c.target, reqID)
			return err
		})
		if err != nil {
			writeProviderCredentialError(w, c, "get_request", err)
			return
		}
		writeJSON(w, http.StatusOK, toRequestDTO(q))
	}
}

func newFileProviderCredentialRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginProviderCredentialCall(deps, w, r, "request_filed")
		if !ok {
			return
		}
		var body fileProviderCredentialRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		notBefore, err := time.Parse(time.RFC3339Nano, body.NotBefore)
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		notAfter, err1 := parseTimePtr(body.NotAfter)
		predNotAfter, err2 := parseTimePtr(body.PredecessorNotAfter)
		var predID *uuid.UUID
		var err3 error
		if body.PredecessorHandleID != nil {
			id, perr := uuid.Parse(*body.PredecessorHandleID)
			if perr != nil {
				err3 = errInvalidRequestBody
			}
			predID = &id
		}
		if err1 != nil || err2 != nil || err3 != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		req, err := deps.ProviderCredentials.FileRequest(r.Context(), deps.DB, providercred.FileRequestParams{
			TargetTenantID: c.target, Domain: body.Domain, ProviderID: body.ProviderID, Purpose: body.Purpose,
			KeyID: body.KeyID, SecretRef: body.SecretRef, Confirmation: body.Confirmation,
			VendorAccountID: body.VendorAccountID, NotBefore: notBefore, NotAfter: notAfter,
			PredecessorHandleID: predID, PredecessorDisposition: body.PredecessorDisposition,
			PredecessorNotAfter: predNotAfter, ReasonCode: body.ReasonCode, RequestedBy: c.subject,
		}, c.ac)
		body.Confirmation = ""
		if err != nil {
			writeProviderCredentialError(w, c, "request_filed", err)
			return
		}
		writeJSON(w, http.StatusCreated, toRequestDTO(req))
	}
}

func newDecideProviderCredentialRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginProviderCredentialCall(deps, w, r, "request_decided")
		if !ok {
			return
		}
		reqID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body decideProviderCredentialRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		ap, err := deps.ProviderCredentials.DecideRequest(r.Context(), deps.DB, providercred.DecideParams{
			TargetTenantID: c.target, RequestID: reqID, Approver: c.subject,
			Decision: body.Decision, ContentHash: body.ContentHash, ReasonCode: body.ReasonCode,
		}, c.ac)
		if err != nil {
			writeProviderCredentialError(w, c, "request_decided", err)
			return
		}
		writeJSON(w, http.StatusCreated, toApprovalDTO(ap))
	}
}

func newApplyProviderCredentialRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginProviderCredentialCall(deps, w, r, "activated")
		if !ok {
			return
		}
		reqID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		// The apply body is empty or {} (no field is accepted).
		var body struct{}
		if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		h, err := deps.ProviderCredentials.ApplyRequest(r.Context(), deps.DB, providercred.ApplyParams{
			TargetTenantID: c.target, RequestID: reqID, Applier: c.subject,
		}, c.ac)
		if err != nil {
			writeProviderCredentialError(w, c, "activated", err)
			return
		}
		writeJSON(w, http.StatusCreated, toHandleDTO(h))
	}
}

func newTransitionProviderCredentialHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginProviderCredentialCall(deps, w, r, "transitioned")
		if !ok {
			return
		}
		handleID, err := uuid.Parse(r.PathValue("handleID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body transitionProviderCredentialBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		notAfter, err := parseTimePtr(body.NotAfter)
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var h providercred.Handle
		err = deps.DB.WithTenant(r.Context(), c.target, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			h, err = providercred.TransitionHandle(ctx, tx, handleID, body.Action, notAfter, body.ReasonCode, c.subject, c.ac)
			return err
		})
		if err != nil {
			if providercred.KindOf(err) == providercred.KindTransitionRejected {
				providercred.RecordTransitionFailure(r.Context(), deps.DB, c.target, handleID, c.ac)
			}
			writeProviderCredentialError(w, c, "transitioned", err)
			return
		}
		writeJSON(w, http.StatusOK, toHandleDTO(h))
	}
}
