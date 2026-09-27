// PRH-I1 (ADR 0095 §10.5): the payment kill-switch admin API.
//
// IMPLEMENTATION NOTE (deviation from the ADR's literal §10.5 route table,
// recorded here for architect/security review): the ADR text describes TWO
// separate route surfaces (a tenant staff admin API and a
// `/platform/tenants/{tenantId}/...` platform admin API) with two disjoint
// permission-name families. This file instead reuses this codebase's own
// existing, reviewed convention for exactly this shape of dual-scope admin
// resource - see registerProviderCredentialRoutes/canActOnTenant
// (provider_credential_handlers.go, admin_routes.go): ONE route family
// under /v1/admin/tenants/{tenantID}/payments/..., where the target tenant
// always comes from the path, canActOnTenant refuses (403, no data) a
// tenant-scoped caller naming any tenant but its own, and a platform-scoped
// caller (tc.TenantID == uuid.Nil) may name any tenant. This achieves every
// functional requirement §10.5 lists (tenant isolation, platform reach,
// cross-tenant 404/403, audit with actor+target tenant) through a pattern
// this codebase already has three independent reviewed instances of,
// rather than introducing a second, parallel route tree and permission
// family. The actual scope-dependent authority difference the ADR cares
// about (a platform-engaged switch is untouchable from a tenant session, a
// release needs a distinct principal) is enforced by the DATABASE
// (migration 0105), not by which URL prefix was used - so this
// simplification changes no security property, only the URL/permission
// surface shape.
//
// Every write dispatches to db.WithPrincipalScope (tenant-scoped caller) or
// db.WithPlatformAdmin (platform-scoped caller) - never db.WithTenant alone
// - because migration 0105's actor-forcing trigger requires app.principal_id
// (tenant) or app.platform_admin_principal_id (platform) to be set, not
// merely app.tenant_id. Every mutation writes audit.Record in the SAME
// transaction. A successful engage additionally emits the S95-C7 alert log
// line (logKillSwitchEngagedAlert) - this codebase has no dedicated
// alerting/notification subsystem (see sportsbook_settlement_handlers.go's
// identical logSettlementIntegrityAlert precedent); this is a structured,
// allow-listed Error-level log line, not new infrastructure.
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerPaymentsKillSwitchRoutes(mux *http.ServeMux, deps Deps) {
	if deps.PaymentOrchestrator == nil {
		return
	}
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	mux.Handle("GET /v1/admin/tenants/{tenantID}/payments/kill-switches",
		staff(auth.RequirePermission(auth.PermPaymentsKillSwitchRead)(newListKillSwitchesHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/payments/kill-switches/{killSwitchID}",
		staff(auth.RequirePermission(auth.PermPaymentsKillSwitchRead)(newGetKillSwitchHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payments/kill-switches",
		staff(auth.RequirePermission(auth.PermPaymentsKillSwitchEngage)(newEngageKillSwitchHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}",
		staff(auth.RequirePermission(auth.PermPaymentsKillSwitchRead)(newGetKillSwitchReleaseRequestHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payments/kill-switches/{killSwitchID}/release-requests",
		staff(auth.RequirePermission(auth.PermPaymentsKillSwitchRelease)(newRequestKillSwitchReleaseHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}/approve",
		staff(auth.RequirePermission(auth.PermPaymentsKillSwitchRelease)(newApproveKillSwitchReleaseHandler(deps))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}/cancel",
		staff(auth.RequirePermission(auth.PermPaymentsKillSwitchRelease)(newCancelKillSwitchReleaseHandler(deps))))
}

// killSwitchCall carries what every handler resolves first - mirrors
// providerCredentialCall (provider_credential_handlers.go) exactly.
type killSwitchCall struct {
	requestID string
	logger    observabilityLogger
	tc        tenant.Context
	subject   uuid.UUID
	target    uuid.UUID
}

// observabilityLogger is the minimal logging surface these handlers need,
// named locally so this file does not have to import slog just to spell
// the parameter type of beginKillSwitchCall's return value.
type observabilityLogger = interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// beginKillSwitchCall authenticates the subject, parses the path tenant,
// and applies canActOnTenant. Writes the response and returns false on any
// failure; a foreign-tenant attempt is 403 with no data and a denied audit
// row in the CALLER's own scope (never the named foreign tenant's).
func beginKillSwitchCall(deps Deps, w http.ResponseWriter, r *http.Request, op string) (killSwitchCall, bool) {
	c := killSwitchCall{
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
	if !canActOnTenant(tc, target) {
		c.logger.Warn("payments_kill_switch_denied", "op", op, "reason", "foreign_tenant")
		recordKillSwitchDenied(r.Context(), deps, c, op)
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return c, false
	}
	return c, true
}

func recordKillSwitchDenied(ctx context.Context, deps Deps, c killSwitchCall, op string) {
	entry := audit.Entry{
		TenantID: c.tc.TenantID, ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "payments_kill_switch." + op, TargetType: "payment_kill_switch",
		Outcome: audit.OutcomeDenied, RequestID: c.requestID,
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
		c.logger.Error("payments_kill_switch_denied_audit_failed", "op", op)
	}
}

// runKillSwitchTx dispatches to the correct DB-session shape for c's
// caller: WithPrincipalScope for a tenant-scoped caller (sets
// app.tenant_id AND app.principal_id, what migration 0105's actor-forcing
// trigger requires for the 'tenant' scope) or WithPlatformAdmin for a
// platform-scoped caller (sets app.platform_admin_principal_id, what the
// trigger requires for the 'platform' scope). Never db.WithTenant alone -
// it sets no app.principal_id, which the trigger's session resolver would
// reject as unresolvable.
func runKillSwitchTx(ctx context.Context, deps Deps, c killSwitchCall, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if c.tc.TenantID == uuid.Nil {
		return deps.DB.WithPlatformAdmin(ctx, c.subject, fn)
	}
	return deps.DB.WithPrincipalScope(ctx, c.target, c.subject, fn)
}

// killSwitchSessionScope is c.tc's scope as a string, for audit metadata
// only (never trusted as the DB actor - that is forced by the trigger).
func (c killSwitchCall) sessionScopeLabel() string {
	if c.tc.TenantID == uuid.Nil {
		return "platform"
	}
	return "tenant"
}

// auditTenantID is the tenant_id value a mutation's own audit.Entry must
// carry. audit_log's RLS (migration 0014) requires tenant_id = app.tenant_id
// when non-NULL and tenant_id IS NULL when app.tenant_id is unset - there is
// no "platform writes into any tenant's audit scope" policy family (unlike
// migration 0105's two-family design). A WithPlatformAdmin transaction never
// sets app.tenant_id, so a platform-scoped mutation's audit row is written
// as a platform-level event (TenantID: uuid.Nil) with the actual target
// tenant carried in Metadata instead - mirrors recordKillSwitchDenied's
// (and provider_credential_handlers.go's identical recordProviderCredentialDenied)
// already-established handling of this exact RLS shape.
func (c killSwitchCall) auditTenantID() uuid.UUID {
	if c.tc.TenantID == uuid.Nil {
		return uuid.Nil
	}
	return c.target
}

// writeKillSwitchError maps a killswitch.go/DB error to one fixed response
// per class, mirroring provider_credential_handlers.go's
// writeProviderCredentialError convention: never the raw trigger/Postgres
// error text (which could carry no secret here, but the convention is
// still "one fixed body per class", not an ad hoc passthrough).
func writeKillSwitchError(w http.ResponseWriter, c killSwitchCall, op string, err error) {
	switch {
	case errors.Is(err, payments.ErrAttemptStateConflict):
		apierror.Write(w, c.requestID, apierror.CodeConflict, "conflict")
	default:
		// Every other failure this package can produce here is a
		// migration-0105 trigger RAISE EXCEPTION (self-approval, a
		// platform-engaged row touched by a tenant session, a stale
		// version, an expired or already-decided request) or a genuine
		// database error. All are reported as 409 conflict - the request
		// was syntactically valid but the current state of the switch/
		// request does not allow it - never the raw exception text, which
		// is logged server-side only for diagnosis.
		c.logger.Error("payments_kill_switch_failed", "op", op)
		apierror.Write(w, c.requestID, apierror.CodeConflict, "conflict")
	}
}

// logKillSwitchEngagedAlert is S95-C7's "alert on every engage": a
// structured, allow-listed Error-level log line (this codebase has no
// dedicated alert/notification subsystem - see
// sportsbook_settlement_handlers.go's logSettlementIntegrityAlert, the same
// pattern applied here). Never the request body, headers or token.
func logKillSwitchEngagedAlert(logger observabilityLogger, ks payments.KillSwitch, requestID string) {
	logger.Error("payments_kill_switch_engaged_alert",
		"tenant_id", ks.TenantID.String(),
		"provider_scope", ks.ProviderScope,
		"operation_scope", string(ks.OperationScope),
		"reason_code", ks.ReasonCode,
		"changed_by", ks.ChangedBy.String(),
		"changed_by_scope", string(ks.ChangedByScope),
		"request_id", requestID,
	)
}

// --- DTOs -------------------------------------------------------------

type killSwitchDTO struct {
	ID               string  `json:"id"`
	TenantID         string  `json:"tenant_id"`
	ProviderScope    string  `json:"provider_scope"`
	OperationScope   string  `json:"operation_scope"`
	Engaged          bool    `json:"engaged"`
	EngagedByScope   *string `json:"engaged_by_scope,omitempty"`
	ReleaseRequestID *string `json:"release_request_id,omitempty"`
	ReasonCode       string  `json:"reason_code"`
	ChangedBy        string  `json:"changed_by"`
	ChangedByScope   string  `json:"changed_by_scope"`
	ChangedAt        string  `json:"changed_at"`
	Version          int64   `json:"version"`
	CreatedAt        string  `json:"created_at"`
}

func toKillSwitchDTO(ks payments.KillSwitch) killSwitchDTO {
	var engagedByScope *string
	if ks.EngagedByScope != nil {
		s := string(*ks.EngagedByScope)
		engagedByScope = &s
	}
	return killSwitchDTO{
		ID: ks.ID.String(), TenantID: ks.TenantID.String(), ProviderScope: ks.ProviderScope,
		OperationScope: string(ks.OperationScope), Engaged: ks.Engaged, EngagedByScope: engagedByScope,
		ReleaseRequestID: uuidPtr(ks.ReleaseRequestID), ReasonCode: ks.ReasonCode,
		ChangedBy: ks.ChangedBy.String(), ChangedByScope: string(ks.ChangedByScope),
		ChangedAt: ks.ChangedAt.UTC().Format(time.RFC3339Nano), Version: ks.Version,
		CreatedAt: ks.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

type killSwitchReleaseRequestDTO struct {
	ID               string  `json:"id"`
	TenantID         string  `json:"tenant_id"`
	KillSwitchID     string  `json:"kill_switch_id"`
	ExpectedVersion  int64   `json:"expected_version"`
	ReasonCode       string  `json:"reason_code"`
	RequestedBy      string  `json:"requested_by"`
	RequestedByScope string  `json:"requested_by_scope"`
	CreatedAt        string  `json:"created_at"`
	ExpiresAt        string  `json:"expires_at"`
	Status           string  `json:"status"`
	ApprovedBy       *string `json:"approved_by,omitempty"`
	ApprovedByScope  *string `json:"approved_by_scope,omitempty"`
	DecidedAt        *string `json:"decided_at,omitempty"`
}

func toKillSwitchReleaseRequestDTO(req payments.KillSwitchReleaseRequest) killSwitchReleaseRequestDTO {
	var approvedByScope *string
	if req.ApprovedByScope != nil {
		s := string(*req.ApprovedByScope)
		approvedByScope = &s
	}
	return killSwitchReleaseRequestDTO{
		ID: req.ID.String(), TenantID: req.TenantID.String(), KillSwitchID: req.KillSwitchID.String(),
		ExpectedVersion: req.ExpectedVersion, ReasonCode: req.ReasonCode,
		RequestedBy: req.RequestedBy.String(), RequestedByScope: string(req.RequestedByScope),
		CreatedAt: req.CreatedAt.UTC().Format(time.RFC3339Nano), ExpiresAt: req.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Status: req.Status, ApprovedBy: uuidPtr(req.ApprovedBy), ApprovedByScope: approvedByScope,
		DecidedAt: rfc3339Ptr(req.DecidedAt),
	}
}

type engageKillSwitchRequest struct {
	ProviderScope  string `json:"provider_scope"`
	OperationScope string `json:"operation_scope"`
	ReasonCode     string `json:"reason_code"`
}

type killSwitchReleaseRequestBody struct {
	ReasonCode string `json:"reason_code"`
}

// --- handlers -----------------------------------------------------------

func newListKillSwitchesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKillSwitchCall(deps, w, r, "list")
		if !ok {
			return
		}
		var out []payments.KillSwitch
		err := runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			list, err := payments.ListKillSwitches(ctx, tx, c.target)
			out = list
			return err
		})
		if err != nil {
			writeKillSwitchError(w, c, "list", err)
			return
		}
		dtos := make([]killSwitchDTO, 0, len(out))
		for _, ks := range out {
			dtos = append(dtos, toKillSwitchDTO(ks))
		}
		writeJSON(w, http.StatusOK, map[string]any{"kill_switches": dtos})
	}
}

func newGetKillSwitchHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKillSwitchCall(deps, w, r, "get")
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("killSwitchID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var ks payments.KillSwitch
		var found bool
		err = runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			ks, found, err = payments.GetKillSwitchByID(ctx, tx, c.target, id)
			return err
		})
		if err != nil {
			writeKillSwitchError(w, c, "get", err)
			return
		}
		if !found {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, toKillSwitchDTO(ks))
	}
}

func newGetKillSwitchReleaseRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKillSwitchCall(deps, w, r, "get_release_request")
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var req payments.KillSwitchReleaseRequest
		var found bool
		err = runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			req, found, err = payments.GetKillSwitchReleaseRequestByID(ctx, tx, c.target, id)
			return err
		})
		if err != nil {
			writeKillSwitchError(w, c, "get_release_request", err)
			return
		}
		if !found {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, toKillSwitchReleaseRequestDTO(req))
	}
}

// newEngageKillSwitchHandler is the single-actor engage (§10.4). Idempotent
// on (tenant, provider_scope, operation_scope): engaging an already-engaged
// switch re-engages it with the new reason_code (EngageKillSwitch's own doc
// comment). Emits the S95-C7 alert on success.
func newEngageKillSwitchHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKillSwitchCall(deps, w, r, "engage")
		if !ok {
			return
		}
		var body engageKillSwitchRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		if body.ProviderScope == "" || body.OperationScope == "" || body.ReasonCode == "" {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "provider_scope, operation_scope and reason_code are required")
			return
		}
		switch body.OperationScope {
		case string(payments.KillSwitchOperationDeposit), string(payments.KillSwitchOperationPayout), string(payments.KillSwitchOperationAny):
		default:
			apierror.Write(w, c.requestID, apierror.CodeValidation, "operation_scope must be deposit, payout or *")
			return
		}
		var ks payments.KillSwitch
		err := runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ks, err = payments.EngageKillSwitch(ctx, tx, c.target, body.ProviderScope, payments.KillSwitchOperationScope(body.OperationScope), body.ReasonCode)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "payments_kill_switch.engage", TargetType: "payment_kill_switch", TargetID: ks.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID,
				Metadata: map[string]any{
					"reason_code": body.ReasonCode, "provider_scope": body.ProviderScope, "operation_scope": body.OperationScope,
					"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
				},
			})
		})
		if err != nil {
			writeKillSwitchError(w, c, "engage", err)
			return
		}
		logKillSwitchEngagedAlert(c.logger, ks, c.requestID)
		writeJSON(w, http.StatusOK, toKillSwitchDTO(ks))
	}
}

// newRequestKillSwitchReleaseHandler files the release-request half of
// four-eyes release. expected_version is read from the switch's OWN
// current row inside this same transaction, never accepted from the
// client - a client-supplied version could otherwise be stale by
// construction, defeating the version check's purpose.
func newRequestKillSwitchReleaseHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKillSwitchCall(deps, w, r, "request_release")
		if !ok {
			return
		}
		killSwitchID, err := uuid.Parse(r.PathValue("killSwitchID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body killSwitchReleaseRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		if body.ReasonCode == "" {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "reason_code is required")
			return
		}
		var req payments.KillSwitchReleaseRequest
		var notFound, notEngaged bool
		err = runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			ks, found, err := payments.GetKillSwitchByID(ctx, tx, c.target, killSwitchID)
			if err != nil {
				return err
			}
			if !found {
				notFound = true
				return nil
			}
			if !ks.Engaged {
				notEngaged = true
				return nil
			}
			req, err = payments.RequestKillSwitchRelease(ctx, tx, c.target, killSwitchID, ks.Version, body.ReasonCode, 0)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "payments_kill_switch.request_release", TargetType: "payment_kill_switch_release_request", TargetID: req.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID,
				Metadata: map[string]any{"reason_code": body.ReasonCode, "kill_switch_id": killSwitchID.String(), "actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String()},
			})
		})
		if notFound {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		if notEngaged {
			apierror.Write(w, c.requestID, apierror.CodeConflict, "the switch is not currently engaged")
			return
		}
		if err != nil {
			writeKillSwitchError(w, c, "request_release", err)
			return
		}
		writeJSON(w, http.StatusCreated, toKillSwitchReleaseRequestDTO(req))
	}
}

// newApproveKillSwitchReleaseHandler performs the approval and the release
// UPDATE in the SAME transaction (ApproveAndReleaseKillSwitch's own doc
// comment) - the database itself refuses a same-principal approval
// (four-eyes) and a stale/expired/already-decided request; this handler
// does not re-check either.
func newApproveKillSwitchReleaseHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKillSwitchCall(deps, w, r, "approve_release")
		if !ok {
			return
		}
		requestID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var notFound bool
		var ks payments.KillSwitch
		err = runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			req, found, err := payments.GetKillSwitchReleaseRequestByID(ctx, tx, c.target, requestID)
			if err != nil {
				return err
			}
			if !found {
				notFound = true
				return nil
			}
			ks, err = payments.ApproveAndReleaseKillSwitch(ctx, tx, c.target, req.KillSwitchID, requestID)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "payments_kill_switch.approve_release", TargetType: "payment_kill_switch", TargetID: ks.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID,
				Metadata: map[string]any{"release_request_id": requestID.String(), "actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String()},
			})
		})
		if notFound {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		if err != nil {
			writeKillSwitchError(w, c, "approve_release", err)
			return
		}
		writeJSON(w, http.StatusOK, toKillSwitchDTO(ks))
	}
}

func newCancelKillSwitchReleaseHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginKillSwitchCall(deps, w, r, "cancel_release")
		if !ok {
			return
		}
		requestID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var notFound bool
		err = runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			req, found, err := payments.GetKillSwitchReleaseRequestByID(ctx, tx, c.target, requestID)
			if err != nil {
				return err
			}
			if !found {
				notFound = true
				return nil
			}
			if err := payments.CancelKillSwitchRelease(ctx, tx, c.target, requestID); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "payments_kill_switch.cancel_release", TargetType: "payment_kill_switch_release_request", TargetID: req.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID,
				Metadata: map[string]any{"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String()},
			})
		})
		if notFound {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		if err != nil {
			writeKillSwitchError(w, c, "cancel_release", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
