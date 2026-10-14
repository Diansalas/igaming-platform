package httpserver

// ADR 0119 slices S5 + S6 (section 14.2): the staff lifecycle API. Every /v1/admin route is a staffRoute (bearer,
// staff principal, liveness, aal2, step-up class, then the route's own permission gate). The domain is
// internal/stafflifecycle; this file maps the verified token to a stafflifecycle.Actor, the path to the target, and
// each domain error to ONE closed apierror code. Contract: docs/plans/adr0119-s5s6-api-contract.md and the OpenAPI
// tag staff-lifecycle. No response ever carries a token, password or factor.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/stafflifecycle"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerStaffLifecycleRoutes(mux *http.ServeMux, deps Deps) {
	// Tenant staff (section 14.2). Reads: staff:read, class none. Mutations: staff:manage, class critical.
	mux.Handle("GET /v1/admin/tenants/{tenantID}/staff",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermStaffRead)(newListTenantStaffHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/staff/{staffID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermStaffRead)(newGetTenantStaffHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/staff/{staffID}/lifecycle-events",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermStaffRead)(newStaffEventsHandler(deps, false))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/invitations",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newInviteStaffHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/invitation/resend",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, false, lcResend))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/invitation/revoke",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, false, lcRevokeInvite))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/suspend",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, false, lcSuspend))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/reactivate",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, false, lcReactivate))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/offboard",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, false, lcOffboard))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/role",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, false, lcRole))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/mfa-reset",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, false, lcMFAReset))))

	// Platform staff: platform callers only (a tenant caller is 403 inside the handler).
	mux.Handle("GET /v1/admin/platform-staff",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermStaffRead)(newListPlatformStaffHandler(deps))))
	mux.Handle("GET /v1/admin/platform-staff/{staffID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermStaffRead)(newGetPlatformStaffHandler(deps))))
	mux.Handle("GET /v1/admin/platform-staff/{staffID}/lifecycle-events",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermStaffRead)(newStaffEventsHandler(deps, true))))
	mux.Handle("POST /v1/admin/platform-staff/{staffID}/suspend",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, true, lcSuspend))))
	mux.Handle("POST /v1/admin/platform-staff/{staffID}/offboard",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, true, lcOffboard))))
	mux.Handle("POST /v1/admin/platform-staff/{staffID}/invitation/resend",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, true, lcResend))))
	mux.Handle("POST /v1/admin/platform-staff/{staffID}/invitation/revoke",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLifecycleActionHandler(deps, true, lcRevokeInvite))))

	// Platform four-eyes (section 10.4): platform_staff:govern (platform_admin only).
	mux.Handle("POST /v1/admin/platform/staff-lifecycle-requests",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermPlatformStaffGovern)(newCreateLifecycleRequestHandler(deps))))
	mux.Handle("GET /v1/admin/platform/staff-lifecycle-requests",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermPlatformStaffGovern)(newListLifecycleRequestsHandler(deps))))
	mux.Handle("GET /v1/admin/platform/staff-lifecycle-requests/{requestID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermPlatformStaffGovern)(newGetLifecycleRequestHandler(deps))))
	mux.Handle("POST /v1/admin/platform/staff-lifecycle-requests/{requestID}/approve",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermPlatformStaffGovern)(newDecideLifecycleRequestHandler(deps, "approve"))))
	mux.Handle("POST /v1/admin/platform/staff-lifecycle-requests/{requestID}/reject",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermPlatformStaffGovern)(newDecideLifecycleRequestHandler(deps, "reject"))))
	mux.Handle("POST /v1/admin/platform/staff-lifecycle-requests/{requestID}/cancel",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermPlatformStaffGovern)(newDecideLifecycleRequestHandler(deps, "cancel"))))

	// Invitation acceptance (section 6.3): unauthenticated, credential bucket.
	mux.Handle("POST /v1/staff/invitations/accept",
		rateLimitFunc(deps.authLimiter, rateBucketCredential, rateLimitCredentialPerMin, newInvitationAcceptHandler(deps)))
}

// ---- plumbing ----------------------------------------------------------------------------------------------------

type lifecycleCall struct {
	requestID string
	logger    interface {
		Error(string, ...any)
		Warn(string, ...any)
	}
	tc    tenant.Context
	actor stafflifecycle.Actor
	svc   *stafflifecycle.Service
}

// lifecycleService builds the domain service from the process dependencies (no extra Deps field: the staff MFA
// keys key the invite / reset tokens, the email provider delivers them, the actor-proof issuer is the process
// default). nil when a dependency is missing (the routes answer 503).
func lifecycleService(deps Deps) *stafflifecycle.Service {
	if deps.DB == nil || deps.StaffMFA == nil {
		return nil
	}
	svc, err := stafflifecycle.New(deps.DB, deps.StaffMFA.keys, deps.EmailProvider, deps.StaffBackofficeBaseURL)
	if err != nil {
		return nil
	}
	return svc.WithLogger(deps.Logger)
}

func beginLifecycleCall(deps Deps, w http.ResponseWriter, r *http.Request) (lifecycleCall, bool) {
	requestID := observability.RequestIDFromContext(r.Context())
	c := lifecycleCall{requestID: requestID, logger: observability.LoggerFromContext(r.Context(), deps.Logger)}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil || subject == uuid.Nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
		return c, false
	}
	c.tc = tc
	c.actor = stafflifecycle.Actor{StaffID: subject, TenantID: tc.TenantID, Role: tc.Role, StaffVersion: tc.StaffVersion,
		Meta: stafflifecycle.Meta{RequestID: requestID, IP: clientIP(r), UserAgent: r.UserAgent()}}
	c.svc = lifecycleService(deps)
	if c.svc == nil {
		apierror.Write(w, requestID, apierror.CodeUnavailable, "staff administration is temporarily unavailable")
		return c, false
	}
	return c, true
}

// pathTenant parses {tenantID}; a tenant caller naming another tenant is 404 (never data).
func (c lifecycleCall) pathTenant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("tenantID"))
	if err != nil || id == uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid tenant id")
		return uuid.Nil, false
	}
	if !canActOnTenant(c.tc, id) {
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (c lifecycleCall) pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil || id == uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid "+strings.ReplaceAll(name, "ID", " id"))
		return uuid.Nil, false
	}
	return id, true
}

// requirePlatformCaller refuses a tenant-scoped caller on a platform route (403, never data).
func (c lifecycleCall) requirePlatformCaller(w http.ResponseWriter) bool {
	if c.tc.TenantID != uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "only a platform-scoped caller may act on platform staff")
		return false
	}
	return true
}

// decodeOptional decodes a JSON body that may be absent (an empty body is the empty object).
func decodeOptional(r *http.Request, dst any) error {
	if err := decodeJSON(r, dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// writeLifecycleError maps a domain error to ONE closed code.
func (c lifecycleCall) writeError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrActorStaffNotLive):
		observability.RecordStaffReassertionRefusal(ctx, observability.StaffReassertionFlowStaffLifecycle)
		c.logger.Warn("staff_lifecycle_actor_not_live")
		writeSessionRevoked(w, c.requestID)
	case errors.Is(err, stafflifecycle.ErrInvalidInput):
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
	case errors.Is(err, stafflifecycle.ErrPayloadHashMismatch):
		apierror.Write(w, c.requestID, apierror.CodeValidation, "payload_hash does not match the request")
	case errors.Is(err, stafflifecycle.ErrNotImplemented):
		apierror.Write(w, c.requestID, apierror.CodeValidation, "this request kind is not implemented")
	case errors.Is(err, stafflifecycle.ErrPersonNotFound):
		apierror.Write(w, c.requestID, apierror.CodeValidation, "person_id does not reference an existing person")
	case errors.Is(err, stafflifecycle.ErrNotFound):
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
	case errors.Is(err, stafflifecycle.ErrForbidden):
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "forbidden")
	case errors.Is(err, stafflifecycle.ErrSelfTarget):
		apierror.Write(w, c.requestID, apierror.CodeSelfTargetForbidden, "a staff member may not act on their own account")
	case errors.Is(err, stafflifecycle.ErrRoleNotGrantable):
		apierror.Write(w, c.requestID, apierror.CodeRoleNotGrantable, "the caller may not grant or act on this role")
	case errors.Is(err, stafflifecycle.ErrFourEyes):
		apierror.Write(w, c.requestID, apierror.CodeFourEyesViolation, "four-eyes: the decider must be another staff member and another Person, with a linked Person")
	case errors.Is(err, stafflifecycle.ErrPersonRequired):
		apierror.Write(w, c.requestID, apierror.CodePersonRequired, "this role requires a linked Person")
	case errors.Is(err, stafflifecycle.ErrTransition):
		apierror.Write(w, c.requestID, apierror.CodeLifecycleTransitionInvalid, "the staff member is not in a state this action applies to")
	case errors.Is(err, stafflifecycle.ErrLastAdmin):
		apierror.Write(w, c.requestID, apierror.CodeLastAdmin, "the last active, enrolled administrator cannot be removed")
	case errors.Is(err, stafflifecycle.ErrBusy):
		apierror.Write(w, c.requestID, apierror.CodeLifecycleBusy, "the staff record is busy; the sessions are already revoked, retry")
	case errors.Is(err, stafflifecycle.ErrConflict):
		apierror.Write(w, c.requestID, apierror.CodeConflict, "a staff user with this email already exists in this scope")
	case errors.Is(err, stafflifecycle.ErrUnavailable):
		apierror.Write(w, c.requestID, apierror.CodeUnavailable, "staff administration is temporarily unavailable")
	default:
		c.logger.Error("staff_lifecycle_failed", "error", err)
		apierror.Write(w, c.requestID, apierror.CodeUnavailable, "staff administration is temporarily unavailable")
	}
}

// ---- DTOs --------------------------------------------------------------------------------------------------------

type staffMfaSummaryDTO struct {
	Enrolled   bool `json:"enrolled"`
	HardLocked bool `json:"hard_locked"`
}

type staffDirectoryEntryDTO struct {
	ID              string             `json:"id"`
	TenantID        string             `json:"tenant_id,omitempty"`
	Email           string             `json:"email"`
	DisplayName     *string            `json:"display_name"`
	Role            string             `json:"role"`
	Status          string             `json:"status"`
	MFA             staffMfaSummaryDTO `json:"mfa"`
	PersonLinked    bool               `json:"person_linked"`
	LastLoginAt     *string            `json:"last_login_at"`
	CreatedAt       string             `json:"created_at"`
	StatusChangedAt *string            `json:"status_changed_at"`
}

type staffDirectoryListDTO struct {
	Staff     []staffDirectoryEntryDTO `json:"staff"`
	Truncated bool                     `json:"truncated"`
}

type staffLifecycleResultDTO struct {
	Staff         staffDirectoryEntryDTO `json:"staff"`
	EmailDelivery string                 `json:"email_delivery,omitempty"`
}

type staffLifecycleEventActorDTO struct {
	Type    string   `json:"type"`
	StaffID staffRef `json:"staff_id"`
}

type staffLifecycleEventDTO struct {
	ID                 string                      `json:"id"`
	StaffUserID        string                      `json:"staff_user_id"`
	Event              string                      `json:"event"`
	Actor              staffLifecycleEventActorDTO `json:"actor"`
	Before             stafflifecycle.State        `json:"before"`
	After              stafflifecycle.State        `json:"after"`
	ReasonCode         *string                     `json:"reason_code"`
	Note               *string                     `json:"note"`
	LifecycleRequestID *string                     `json:"lifecycle_request_id"`
	CreatedAt          string                      `json:"created_at"`
}

type staffLifecycleEventListDTO struct {
	Events []staffLifecycleEventDTO `json:"events"`
}

type platformStaffLifecycleRequestDTO struct {
	ID                string  `json:"id"`
	Kind              string  `json:"kind"`
	State             string  `json:"state"`
	TargetStaffID     *string `json:"target_staff_id"`
	InviteEmail       *string `json:"invite_email"`
	ReasonCode        *string `json:"reason_code"`
	Note              *string `json:"note"`
	RequestedBy       string  `json:"requested_by"`
	RequiredApprovals int     `json:"required_approvals"`
	PayloadHash       string  `json:"payload_hash"`
	ExpiresAt         string  `json:"expires_at"`
	CreatedAt         string  `json:"created_at"`
	DecidedAt         *string `json:"decided_at"`
}

type platformStaffLifecycleRequestListDTO struct {
	Requests []platformStaffLifecycleRequestDTO `json:"requests"`
}

func lcTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func optTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := lcTime(*t)
	return &s
}

func lcOptUUID(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func toStaffDirectoryEntryDTO(s stafflifecycle.Staff) staffDirectoryEntryDTO {
	d := staffDirectoryEntryDTO{ID: s.ID.String(), Email: s.Email, DisplayName: s.DisplayName, Role: s.Role, Status: s.Status,
		MFA: staffMfaSummaryDTO{Enrolled: s.MFAEnrolled, HardLocked: s.MFAHardLocked}, PersonLinked: s.PersonLinked,
		LastLoginAt: optTime(s.LastLoginAt), CreatedAt: lcTime(s.CreatedAt), StatusChangedAt: optTime(s.StatusChangedAt)}
	if s.TenantID != uuid.Nil {
		d.TenantID = s.TenantID.String()
	}
	return d
}

func toLifecycleResultDTO(res stafflifecycle.Result) staffLifecycleResultDTO {
	return staffLifecycleResultDTO{Staff: toStaffDirectoryEntryDTO(res.Staff), EmailDelivery: string(res.Delivery)}
}

func toStaffLifecycleEventDTO(v staffViewer, e stafflifecycle.Event) staffLifecycleEventDTO {
	scope := staffScopePlatform
	if e.ActorIsTenantStaff {
		scope = staffScopeTenant
	}
	var actorID uuid.UUID
	if e.ActorStaffID != nil {
		actorID = *e.ActorStaffID
	}
	return staffLifecycleEventDTO{ID: e.ID.String(), StaffUserID: e.StaffUserID.String(), Event: e.Event,
		Actor:  staffLifecycleEventActorDTO{Type: e.ActorType, StaffID: v.ref(scope, actorID)},
		Before: e.Before, After: e.After, ReasonCode: e.ReasonCode, Note: e.Note,
		LifecycleRequestID: lcOptUUID(e.LifecycleRequestID), CreatedAt: lcTime(e.CreatedAt)}
}

func toLifecycleRequestDTO(r stafflifecycle.Request) platformStaffLifecycleRequestDTO {
	return platformStaffLifecycleRequestDTO{ID: r.ID.String(), Kind: r.Kind, State: r.State, TargetStaffID: lcOptUUID(r.TargetStaffID),
		InviteEmail: r.InviteEmail, ReasonCode: r.ReasonCode, Note: r.Note, RequestedBy: r.RequestedBy.String(),
		RequiredApprovals: r.RequiredApprovals, PayloadHash: r.PayloadHash, ExpiresAt: lcTime(r.ExpiresAt),
		CreatedAt: lcTime(r.CreatedAt), DecidedAt: optTime(r.DecidedAt)}
}

// ---- reads -------------------------------------------------------------------------------------------------------

var (
	staffStatusFilter = map[string]bool{"invited": true, "active": true, "suspended": true, "offboarded": true}
)

func directoryFilter(w http.ResponseWriter, r *http.Request, c lifecycleCall, allowRole bool) (stafflifecycle.DirectoryFilter, bool) {
	f := stafflifecycle.DirectoryFilter{Status: r.URL.Query().Get("status"), Role: r.URL.Query().Get("role")}
	if (f.Status != "" && !staffStatusFilter[f.Status]) || (f.Role != "" && (!allowRole || !stafflifecycle.IsTenantRole(f.Role))) {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid filter")
		return f, false
	}
	return f, true
}

func writeDirectory(w http.ResponseWriter, list []stafflifecycle.Staff, truncated bool) {
	out := staffDirectoryListDTO{Staff: make([]staffDirectoryEntryDTO, 0, len(list)), Truncated: truncated}
	for _, s := range list {
		out.Staff = append(out.Staff, toStaffDirectoryEntryDTO(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func newListTenantStaffHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok {
			return
		}
		tid, ok := c.pathTenant(w, r)
		if !ok {
			return
		}
		f, ok := directoryFilter(w, r, c, true)
		if !ok {
			return
		}
		list, truncated, err := c.svc.ListStaff(r.Context(), c.actor, tid, f)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeDirectory(w, list, truncated)
	}
}

func newListPlatformStaffHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok || !c.requirePlatformCaller(w) {
			return
		}
		f, ok := directoryFilter(w, r, c, false)
		if !ok {
			return
		}
		list, truncated, err := c.svc.ListStaff(r.Context(), c.actor, uuid.Nil, f)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeDirectory(w, list, truncated)
	}
}

func newGetTenantStaffHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok {
			return
		}
		tid, ok := c.pathTenant(w, r)
		if !ok {
			return
		}
		sid, ok := c.pathUUID(w, r, "staffID")
		if !ok {
			return
		}
		st, err := c.svc.GetStaff(r.Context(), c.actor, tid, sid)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeJSON(w, http.StatusOK, toStaffDirectoryEntryDTO(st))
	}
}

func newGetPlatformStaffHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok || !c.requirePlatformCaller(w) {
			return
		}
		sid, ok := c.pathUUID(w, r, "staffID")
		if !ok {
			return
		}
		st, err := c.svc.GetStaff(r.Context(), c.actor, uuid.Nil, sid)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeJSON(w, http.StatusOK, toStaffDirectoryEntryDTO(st))
	}
}

func newStaffEventsHandler(deps Deps, platform bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok {
			return
		}
		tid := uuid.Nil
		if platform {
			if !c.requirePlatformCaller(w) {
				return
			}
		} else if tid, ok = c.pathTenant(w, r); !ok {
			return
		}
		sid, ok := c.pathUUID(w, r, "staffID")
		if !ok {
			return
		}
		events, err := c.svc.ListEvents(r.Context(), c.actor, tid, sid)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		v := staffViewerOf(c.tc)
		out := staffLifecycleEventListDTO{Events: make([]staffLifecycleEventDTO, 0, len(events))}
		for _, e := range events {
			out.Events = append(out.Events, toStaffLifecycleEventDTO(v, e))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// ---- mutations ---------------------------------------------------------------------------------------------------

type staffInvitationRequest struct {
	Email    string `json:"email"`
	Role     string `json:"role"`
	PersonID string `json:"person_id,omitempty"`
}

func newInviteStaffHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok {
			return
		}
		tid, ok := c.pathTenant(w, r)
		if !ok {
			return
		}
		var req staffInvitationRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		in := stafflifecycle.InviteInput{Email: req.Email, Role: req.Role}
		if req.PersonID != "" {
			p, err := uuid.Parse(req.PersonID)
			if err != nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "person_id must be a valid UUID")
				return
			}
			in.PersonID = &p
		}
		res, err := c.svc.Invite(r.Context(), c.actor, tid, in)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toLifecycleResultDTO(res))
	}
}

type lifecycleOp int

const (
	lcResend lifecycleOp = iota + 1
	lcRevokeInvite
	lcSuspend
	lcReactivate
	lcOffboard
	lcRole
	lcMFAReset
)

type staffLifecycleActionRequest struct {
	ReasonCode string `json:"reason_code"`
	Note       string `json:"note,omitempty"`
	Role       string `json:"role,omitempty"`
}

func newLifecycleActionHandler(deps Deps, platform bool, op lifecycleOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok {
			return
		}
		tid := uuid.Nil
		if platform {
			if !c.requirePlatformCaller(w) {
				return
			}
		} else if tid, ok = c.pathTenant(w, r); !ok {
			return
		}
		sid, ok := c.pathUUID(w, r, "staffID")
		if !ok {
			return
		}
		var req staffLifecycleActionRequest
		if err := decodeOptional(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if (op != lcRole && req.Role != "") || (op == lcResend && (req.ReasonCode != "" || req.Note != "")) {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		reason := stafflifecycle.Reason{Code: req.ReasonCode, Note: req.Note}
		var res stafflifecycle.Result
		var err error
		ctx := r.Context()
		switch op {
		case lcResend:
			res, err = c.svc.ResendInvite(ctx, c.actor, tid, sid)
		case lcRevokeInvite:
			res, err = c.svc.RevokeInvite(ctx, c.actor, tid, sid, reason)
		case lcSuspend:
			res, err = c.svc.Suspend(ctx, c.actor, tid, sid, reason)
		case lcOffboard:
			res, err = c.svc.Offboard(ctx, c.actor, tid, sid, reason)
		case lcReactivate:
			res, err = c.svc.Reactivate(ctx, c.actor, tid, sid, reason)
		case lcRole:
			res, err = c.svc.ChangeRole(ctx, c.actor, tid, sid, req.Role, reason)
		case lcMFAReset:
			res, err = c.svc.ResetMFA(ctx, c.actor, tid, sid, reason)
		}
		if err != nil {
			c.writeError(ctx, w, err)
			return
		}
		writeJSON(w, http.StatusOK, toLifecycleResultDTO(res))
	}
}

// ---- platform four-eyes ------------------------------------------------------------------------------------------

type platformStaffLifecycleRequestCreate struct {
	Kind          string `json:"kind"`
	TargetStaffID string `json:"target_staff_id,omitempty"`
	Email         string `json:"email,omitempty"`
	PersonID      string `json:"person_id,omitempty"`
	ReasonCode    string `json:"reason_code,omitempty"`
	Note          string `json:"note,omitempty"`
}

func newCreateLifecycleRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok || !c.requirePlatformCaller(w) {
			return
		}
		var req platformStaffLifecycleRequestCreate
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		in := stafflifecycle.RequestInput{Kind: req.Kind, Email: req.Email, Reason: stafflifecycle.Reason{Code: req.ReasonCode, Note: req.Note}}
		for _, f := range []struct {
			raw string
			dst **uuid.UUID
		}{{req.TargetStaffID, &in.TargetStaffID}, {req.PersonID, &in.PersonID}} {
			if f.raw == "" {
				continue
			}
			id, err := uuid.Parse(f.raw)
			if err != nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid id")
				return
			}
			*f.dst = &id
		}
		out, err := c.svc.CreateRequest(r.Context(), c.actor, in)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toLifecycleRequestDTO(out))
	}
}

func newListLifecycleRequestsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok || !c.requirePlatformCaller(w) {
			return
		}
		list, err := c.svc.ListRequests(r.Context(), c.actor)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		out := platformStaffLifecycleRequestListDTO{Requests: make([]platformStaffLifecycleRequestDTO, 0, len(list))}
		for _, q := range list {
			out.Requests = append(out.Requests, toLifecycleRequestDTO(q))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func newGetLifecycleRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok || !c.requirePlatformCaller(w) {
			return
		}
		id, ok := c.pathUUID(w, r, "requestID")
		if !ok {
			return
		}
		q, err := c.svc.GetRequest(r.Context(), c.actor, id)
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeJSON(w, http.StatusOK, toLifecycleRequestDTO(q))
	}
}

type platformStaffLifecycleDecision struct {
	PayloadHash string `json:"payload_hash"`
}

func newDecideLifecycleRequestHandler(deps Deps, decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLifecycleCall(deps, w, r)
		if !ok || !c.requirePlatformCaller(w) {
			return
		}
		id, ok := c.pathUUID(w, r, "requestID")
		if !ok {
			return
		}
		var body platformStaffLifecycleDecision
		if err := decodeOptional(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		var out stafflifecycle.Request
		var err error
		switch decision {
		case "approve":
			if !isHex64(body.PayloadHash) {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "payload_hash is required")
				return
			}
			out, _, err = c.svc.Approve(r.Context(), c.actor, id, body.PayloadHash)
		case "reject":
			if body.PayloadHash != "" {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
				return
			}
			out, err = c.svc.Reject(r.Context(), c.actor, id)
		default:
			if body.PayloadHash != "" {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
				return
			}
			out, err = c.svc.Cancel(r.Context(), c.actor, id)
		}
		if err != nil {
			c.writeError(r.Context(), w, err)
			return
		}
		writeJSON(w, http.StatusOK, toLifecycleRequestDTO(out))
	}
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
