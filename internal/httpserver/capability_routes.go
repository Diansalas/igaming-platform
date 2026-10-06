// PRH-2 K1 (ADR 0099): the scoped financial capability grant admin API.
//
// Route shape deliberately mirrors payments_kill_switch_handlers.go's own
// already-reviewed convention for a dual-scope admin resource: ONE route
// family under /v1/admin/tenants/{tenantID}/capability-grants/..., where
// the target tenant always comes from the path, canActOnTenant refuses
// (403, no data) a tenant-scoped caller naming any tenant but its own,
// and a platform-scoped caller (tc.TenantID == uuid.Nil) may name any
// tenant. Every write dispatches to db.WithPrincipalScope (tenant caller)
// or db.WithPlatformAdmin (platform caller) - never db.WithTenant alone -
// because migration 0112's financial_actor_session() requires
// app.principal_id (tenant scope) or app.platform_admin_principal_id
// (platform scope) to be set, exactly like migration 0105's actor
// resolver before it. Every mutation writes audit.Record in the same
// transaction as the mutation; ADR 0099 §2.1's actual authority decision
// (no self-grant, distinct Persons, platform co-approval, no un-revoke,
// R-1..R-13) is made by migration 0112's own triggers, never re-derived
// here - this file's job is routing, request decoding, session dispatch
// and error classification only.
package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerCapabilityRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	mux.Handle("POST /v1/admin/tenants/{tenantID}/capability-grants/requests",
		staff(auth.RequirePermission(auth.PermCapabilityGrantRequest)(newCreateCapabilityGrantRequestHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/capability-grants/requests",
		staff(auth.RequirePermission(auth.PermCapabilityGrantRead)(newListCapabilityGrantRequestsHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/capability-grants/requests/{requestID}/cancel",
		staff(auth.RequirePermission(auth.PermCapabilityGrantRequest)(newCancelCapabilityGrantRequestHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/capability-grants/requests/{requestID}/approve",
		staff(auth.RequirePermission(auth.PermCapabilityGrantApprove)(newDecideCapabilityGrantRequestHandler(deps, "approve"))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/capability-grants/requests/{requestID}/reject",
		staff(auth.RequirePermission(auth.PermCapabilityGrantApprove)(newDecideCapabilityGrantRequestHandler(deps, "reject"))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/capability-grants",
		staff(auth.RequirePermission(auth.PermCapabilityGrantRead)(newListCapabilityGrantsHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/capability-grants/{grantID}/revoke",
		staff(auth.RequirePermission(auth.PermCapabilityGrantRevoke)(newRevokeCapabilityGrantHandler(deps))))
}

// capabilityCall mirrors killSwitchCall exactly - see that type's own doc
// comment for the full rationale of each field.
type capabilityCall struct {
	requestID string
	logger    observabilityLogger
	tc        tenant.Context
	subject   uuid.UUID
	target    uuid.UUID
	ipAddress string
	userAgent string
}

func beginCapabilityCall(deps Deps, w http.ResponseWriter, r *http.Request, op string) (capabilityCall, bool) {
	c := capabilityCall{
		requestID: observability.RequestIDFromContext(r.Context()),
		logger:    observability.LoggerFromContext(r.Context(), deps.Logger),
		ipAddress: clientIP(r),
		userAgent: r.UserAgent(),
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
	if !canActOnTenant(tc, target) {
		c.logger.Warn("capability_grant_denied", "op", op, "reason", "foreign_tenant")
		recordCapabilityDenied(r.Context(), deps, c, op)
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return c, false
	}
	return c, true
}

func recordCapabilityDenied(ctx context.Context, deps Deps, c capabilityCall, op string) {
	entry := audit.Entry{
		TenantID: c.tc.TenantID, ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "capability_grant." + op, TargetType: "capability_grant",
		Outcome: audit.OutcomeDenied, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
		Metadata: map[string]any{"denied": "foreign_tenant"},
	}
	record := func(ctx context.Context, tx pgx.Tx) error { return audit.Record(ctx, tx, entry) }
	dctx, cancel := deniedAuditCtx(ctx)
	defer cancel()
	var err error
	if c.tc.TenantID == uuid.Nil {
		err = deps.DB.WithPlatformAdmin(dctx, c.subject, record)
	} else {
		err = deps.DB.WithTenant(dctx, c.tc.TenantID, record)
	}
	if err != nil {
		c.logger.Error("capability_grant_denied_audit_failed", "op", op)
	}
}

// runCapabilityTx dispatches to the session shape migration 0112's
// financial_actor_session() requires: WithPrincipalScope for a
// tenant-scoped caller, WithPlatformAdmin for a platform-scoped one.
// Never db.WithTenant alone (see runKillSwitchTx's identical rationale).
func runCapabilityTx(ctx context.Context, deps Deps, c capabilityCall, fn func(ctx context.Context, tx pgx.Tx) error) error {
	// SIGNED-ACTOR-PROOF (ADR 0110): the caller is authenticated (verified token
	// subject) and the route's permission check has passed; the K1 functions sign
	// for exactly this principal.
	if c.tc.TenantID == uuid.Nil {
		ctx = capability.WithProofActor(ctx, capability.ProofActor{Actor: c.subject, Scope: capability.ProofScopePlatform})
		return deps.DB.WithPlatformAdmin(ctx, c.subject, fn)
	}
	ctx = capability.WithProofActor(ctx, capability.ProofActor{Actor: c.subject, Scope: capability.ProofScopeTenant, Tenant: c.tc.TenantID})
	return deps.DB.WithPrincipalScope(ctx, c.tc.TenantID, c.subject, fn)
}

func (c capabilityCall) sessionScopeLabel() string {
	if c.tc.TenantID == uuid.Nil {
		return "platform"
	}
	return "tenant"
}

// auditTenantID/auditSubjectTenantID mirror killSwitchCall's own
// identically-named methods and identical RLS rationale (migration 0014
// has no "platform writes into any tenant's audit scope" policy).
func (c capabilityCall) auditTenantID() uuid.UUID {
	if c.tc.TenantID == uuid.Nil {
		return uuid.Nil
	}
	return c.target
}

func (c capabilityCall) auditSubjectTenantID() uuid.UUID {
	if c.tc.TenantID == uuid.Nil {
		return c.target
	}
	return uuid.Nil
}

// writeCapabilityError classifies a capability-grant database error by
// its 'CG' SQLSTATE class (internal/capability.ClassifyError) and writes
// exactly one fixed response body per class - never the raw trigger/
// Postgres error text - mirroring writeKillSwitchError's convention.
func writeCapabilityError(ctx context.Context, deps Deps, w http.ResponseWriter, c capabilityCall, op, targetType, targetID string, err error) {
	if err == pgx.ErrNoRows {
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
		return
	}
	class := capability.ClassifyError(err)
	switch class {
	case capability.ErrClassGuardRefusal:
		c.logger.Warn("capability_grant_refused", "op", op, "class", string(class))
		recordCapabilityRefusalAudit(ctx, deps, c, op, string(class), targetType, targetID)
		// I-1 (architect ruling on G-P1, docs/plans/prh2-hardening-round/
		// reviews/k1-architect-ruling-gp1.md): G-P1 (a platform session
		// requesting a grant naming a tenant-scoped grantee) is DEFERRED,
		// refused fail-closed by migration 0112's own grantee-lookup guard
		// (CG010 "grantee ... does not exist" - a plain platform session
		// is structurally blind to any tenant-scoped staff_users row,
		// migration 0011 dual_scope_isolation). A platform caller cannot
		// be told WHY in more detail without a lookup that would itself
		// require the very visibility that does not exist (there is no
		// way to distinguish "this grantee is real but tenant-scoped"
		// from "this grantee id does not exist at all" from a platform
		// session) - so every guard refusal on the request op, from a
		// platform-scoped caller, gets this one legible message. This is
		// a deliberate, disclosed simplification: it may also fire for a
		// different platform-session refusal reason (e.g. an ineligible
		// G-P2 role or lifetime); the SQLSTATE-derived 409 Conflict class
		// is unchanged either way, only the message text is friendlier.
		if op == "request" && c.tc.TenantID == uuid.Nil {
			apierror.Write(w, c.requestID, apierror.CodeConflict,
				"platform-originated grants for tenant staff are not supported; the tenant administrator requests (G-T)")
			return
		}
		apierror.Write(w, c.requestID, apierror.CodeConflict, "conflict")
	case capability.ErrClassUniqueViolation:
		// F-3 (code review of 0f34d36): a double-submit race against
		// R-11/R-12's partial unique indexes is a legitimate conflict,
		// never a 500 - same 409 Conflict class and refusal-audit
		// treatment as a guard refusal, distinguished only in the log/
		// audit "class" field for diagnosis.
		c.logger.Warn("capability_grant_refused", "op", op, "class", string(class))
		recordCapabilityRefusalAudit(ctx, deps, c, op, string(class), targetType, targetID)
		apierror.Write(w, c.requestID, apierror.CodeConflict, "conflict")
	case capability.ErrClassCheckViolation:
		// F-3: a CHECK-constraint refusal (e.g. R-13 restated at the table
		// level) is a client-supplied-data validation failure, 400 rather
		// than 409 - there is no concurrent second actor to retry against.
		// This codebase's apierror.CodeValidation is its existing generic
		// "bad input" code (no dedicated 422 code exists outside the
		// settlement domain).
		c.logger.Warn("capability_grant_refused", "op", op, "class", string(class))
		recordCapabilityRefusalAudit(ctx, deps, c, op, string(class), targetType, targetID)
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
	case capability.ErrClassSessionInvalid:
		c.logger.Error("capability_grant_session_invalid", "op", op, "err", err.Error())
		apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	default:
		c.logger.Error("capability_grant_failed", "op", op, "err", err.Error())
		apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	}
}

func recordCapabilityRefusalAudit(ctx context.Context, deps Deps, c capabilityCall, op, class, targetType, targetID string) {
	entry := audit.Entry{
		TenantID: c.auditTenantID(), SubjectTenantID: c.auditSubjectTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "capability_grant." + op, TargetType: targetType, TargetID: targetID,
		Outcome: audit.OutcomeDenied, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
		Metadata: map[string]any{"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(), "denied_class": class},
	}
	dctx, cancel := deniedAuditCtx(ctx)
	defer cancel()
	if err := runCapabilityTx(dctx, deps, c, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, entry)
	}); err != nil {
		c.logger.Error("capability_grant_refusal_audit_failed", "op", op)
	}
}

// --- request/response DTOs ---

type createCapabilityGrantRequestBody struct {
	GranteeStaffID string     `json:"grantee_staff_id"`
	Capability     string     `json:"capability"`
	ValidFrom      *time.Time `json:"valid_from,omitempty"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	ReasonCode     string     `json:"reason_code"`
}

type capabilityGrantRequestDTO struct {
	ID               string  `json:"id"`
	TenantID         string  `json:"tenant_id"`
	GranteeStaffID   string  `json:"grantee_staff_id"`
	GranteeScope     string  `json:"grantee_scope"`
	Capability       string  `json:"capability"`
	ValidFrom        string  `json:"valid_from"`
	ValidUntil       *string `json:"valid_until,omitempty"`
	ReasonCode       string  `json:"reason_code"`
	RequestedBy      string  `json:"requested_by"`
	RequestedByScope string  `json:"requested_by_scope"`
	Status           string  `json:"status"`
	CreatedAt        string  `json:"created_at"`
	ExpiresAt        string  `json:"expires_at"`
}

func toCapabilityRequestDTO(r capability.Request) capabilityGrantRequestDTO {
	dto := capabilityGrantRequestDTO{
		ID: r.ID.String(), TenantID: r.TenantID.String(), GranteeStaffID: r.GranteeStaffID.String(),
		GranteeScope: r.GranteeScope, Capability: string(r.Capability), ValidFrom: r.ValidFrom.Format(time.RFC3339),
		ReasonCode: r.ReasonCode, RequestedBy: r.RequestedBy.String(), RequestedByScope: r.RequestedByScope,
		Status: string(r.Status), CreatedAt: r.CreatedAt.Format(time.RFC3339), ExpiresAt: r.ExpiresAt.Format(time.RFC3339),
	}
	if r.ValidUntil != nil {
		s := r.ValidUntil.Format(time.RFC3339)
		dto.ValidUntil = &s
	}
	return dto
}

type capabilityGrantDTO struct {
	ID               string  `json:"id"`
	TenantID         string  `json:"tenant_id"`
	GranteeStaffID   string  `json:"grantee_staff_id"`
	GranteeScope     string  `json:"grantee_scope"`
	Capability       string  `json:"capability"`
	ValidFrom        string  `json:"valid_from"`
	ValidUntil       *string `json:"valid_until,omitempty"`
	GrantedAt        string  `json:"granted_at"`
	RevokedAt        *string `json:"revoked_at,omitempty"`
	RevokeReasonCode *string `json:"revoke_reason_code,omitempty"`
}

func toCapabilityGrantDTO(g capability.Grant) capabilityGrantDTO {
	dto := capabilityGrantDTO{
		ID: g.ID.String(), TenantID: g.TenantID.String(), GranteeStaffID: g.GranteeStaffID.String(),
		GranteeScope: g.GranteeScope, Capability: string(g.Capability), ValidFrom: g.ValidFrom.Format(time.RFC3339),
		GrantedAt: g.GrantedAt.Format(time.RFC3339),
	}
	if g.ValidUntil != nil {
		s := g.ValidUntil.Format(time.RFC3339)
		dto.ValidUntil = &s
	}
	if g.RevokedAt != nil {
		s := g.RevokedAt.Format(time.RFC3339)
		dto.RevokedAt = &s
	}
	dto.RevokeReasonCode = g.RevokeReasonCode
	return dto
}

// --- handlers ---

func newCreateCapabilityGrantRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginCapabilityCall(deps, w, r, "request")
		if !ok {
			return
		}
		var body createCapabilityGrantRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		granteeID, err := uuid.Parse(body.GranteeStaffID)
		if err != nil || body.Capability == "" || body.ReasonCode == "" {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "grantee_staff_id, capability and reason_code are required")
			return
		}
		// R-14 (ADR 0099, k1-architect-ruling-r12.md): no longer defaulted
		// to time.Now() here. When body.ValidFrom is absent, the zero
		// time.Time{} is passed through to capability.CreateRequest, which
		// turns it into SQL NULL; migration 0112's request-guard trigger
		// then defaults it to now() itself. Defaulting it here would just
		// be a second, redundant clock reading that could itself race the
		// trigger's own now() across the 5-minute backdating tolerance.
		var validFrom time.Time
		if body.ValidFrom != nil {
			validFrom = *body.ValidFrom
		}

		var req capability.Request
		err = runCapabilityTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			var txErr error
			req, txErr = capability.CreateRequest(ctx, tx, c.target, capability.NewRequestInput{
				GranteeStaffID: granteeID, Capability: capability.Capability(body.Capability),
				ValidFrom: validFrom, ValidUntil: body.ValidUntil, ReasonCode: body.ReasonCode,
			})
			if txErr != nil {
				return txErr
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), SubjectTenantID: c.auditSubjectTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "capability_grant.requested", TargetType: "capability_grant_request", TargetID: req.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
				Metadata: map[string]any{
					"grantee_staff_id": body.GranteeStaffID, "capability": body.Capability,
					"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
				},
			})
		})
		if err != nil {
			writeCapabilityError(r.Context(), deps, w, c, "request", "capability_grant_request", "", err)
			return
		}
		writeJSON(w, http.StatusCreated, toCapabilityRequestDTO(req))
	}
}

// newCancelCapabilityGrantRequestHandler: ONLY the requester may cancel (migration
// 0120, ADR 0110). Any other caller - including a platform admin cancelling a
// tenant-originated request, or another member of the requester's tenant - gets
// the guard's CG010, which this handler maps to 409 Conflict (never a 5xx).
func newCancelCapabilityGrantRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginCapabilityCall(deps, w, r, "cancel")
		if !ok {
			return
		}
		requestID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		err = runCapabilityTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			before, err := capability.GetRequest(ctx, tx, c.target, requestID)
			if err != nil {
				return err
			}
			if err := capability.CancelRequest(ctx, tx, c.target, requestID); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), SubjectTenantID: c.auditSubjectTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "capability_grant.cancelled", TargetType: "capability_grant_request", TargetID: requestID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
				Metadata: map[string]any{
					"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
					"grantee_staff_id": before.GranteeStaffID.String(), "capability": string(before.Capability), "reason_code": before.ReasonCode,
					"before_status": string(before.Status), "after_status": "cancelled",
				},
			})
		})
		if err != nil {
			writeCapabilityError(r.Context(), deps, w, c, "cancel", "capability_grant_request", requestID.String(), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type decideCapabilityGrantRequestBody struct {
	ReasonCode string `json:"reason_code"`
}

// decisionAuditAction is an explicit map (F-7, code review of 0f34d36):
// decision + "d" produced "capability_grant.rejectd" for the reject path
// (English's own irregular past tense, not a decision-list bug) - always
// spell out the two known actions rather than concatenate.
var decisionAuditAction = map[string]string{
	"approve": "capability_grant.approved",
	"reject":  "capability_grant.rejected",
}

func newDecideCapabilityGrantRequestHandler(deps Deps, decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginCapabilityCall(deps, w, r, decision)
		if !ok {
			return
		}
		requestID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body decideCapabilityGrantRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		if body.ReasonCode == "" {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "reason_code is required")
			return
		}

		var grant *capability.Grant
		err = runCapabilityTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			before, err := capability.GetRequest(ctx, tx, c.target, requestID)
			if err != nil {
				return err
			}
			_, g, txErr := capability.DecideAndGrant(ctx, tx, c.target, requestID, decision, body.ReasonCode)
			if txErr != nil {
				return txErr
			}
			grant = g
			targetID := ""
			if grant != nil {
				targetID = grant.ID.String()
			}
			afterStatus := "rejected"
			if decision == "approve" {
				afterStatus = "approved"
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), SubjectTenantID: c.auditSubjectTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: decisionAuditAction[decision], TargetType: "capability_grant_request", TargetID: requestID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
				Metadata: map[string]any{
					"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(), "grant_id": targetID,
					"grantee_staff_id": before.GranteeStaffID.String(), "capability": string(before.Capability), "reason_code": body.ReasonCode,
					"before_status": string(before.Status), "after_status": afterStatus,
				},
			})
		})
		if err != nil {
			writeCapabilityError(r.Context(), deps, w, c, decision, "capability_grant_request", requestID.String(), err)
			return
		}
		if grant != nil {
			writeJSON(w, http.StatusOK, toCapabilityGrantDTO(*grant))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type revokeCapabilityGrantBody struct {
	ReasonCode string `json:"reason_code"`
}

func newRevokeCapabilityGrantHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginCapabilityCall(deps, w, r, "revoke")
		if !ok {
			return
		}
		grantID, err := uuid.Parse(r.PathValue("grantID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body revokeCapabilityGrantBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		if body.ReasonCode == "" {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "reason_code is required")
			return
		}
		err = runCapabilityTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			before, err := capability.GetGrant(ctx, tx, c.target, grantID)
			if err != nil {
				return err
			}
			if err := capability.RevokeGrant(ctx, tx, c.target, grantID, body.ReasonCode); err != nil {
				return err
			}
			beforeRevokedAt := "null"
			if before.RevokedAt != nil {
				beforeRevokedAt = before.RevokedAt.Format(time.RFC3339)
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), SubjectTenantID: c.auditSubjectTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "capability_grant.revoked", TargetType: "capability_grant", TargetID: grantID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
				Metadata: map[string]any{
					"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
					"grantee_staff_id": before.GranteeStaffID.String(), "capability": string(before.Capability), "reason_code": body.ReasonCode,
					"before_revoked_at": beforeRevokedAt, "after_revoked_at": "set",
				},
			})
		})
		if err != nil {
			writeCapabilityError(r.Context(), deps, w, c, "revoke", "capability_grant", grantID.String(), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func newListCapabilityGrantRequestsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginCapabilityCall(deps, w, r, "list_requests")
		if !ok {
			return
		}
		var reqs []capability.Request
		err := runCapabilityTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			var txErr error
			reqs, txErr = capability.ListRequests(ctx, tx, c.target)
			return txErr
		})
		if err != nil {
			writeCapabilityError(r.Context(), deps, w, c, "list_requests", "capability_grant_request", "", err)
			return
		}
		dtos := make([]capabilityGrantRequestDTO, 0, len(reqs))
		for _, req := range reqs {
			dtos = append(dtos, toCapabilityRequestDTO(req))
		}
		writeJSON(w, http.StatusOK, dtos)
	}
}

func newListCapabilityGrantsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginCapabilityCall(deps, w, r, "list_grants")
		if !ok {
			return
		}
		var grants []capability.Grant
		err := runCapabilityTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			var txErr error
			grants, txErr = capability.ListGrants(ctx, tx, c.target)
			return txErr
		})
		if err != nil {
			writeCapabilityError(r.Context(), deps, w, c, "list_grants", "capability_grant", "", err)
			return
		}
		dtos := make([]capabilityGrantDTO, 0, len(grants))
		for _, g := range grants {
			dtos = append(dtos, toCapabilityGrantDTO(g))
		}
		writeJSON(w, http.StatusOK, dtos)
	}
}
