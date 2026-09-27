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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
	// L6 (RV-PRH-I1 security review): §10.5 says "the handler verifies that
	// the tenant exists" - only reachable for a platform-scoped caller
	// (tc.TenantID == uuid.Nil), since a tenant-scoped caller can only ever
	// name its own, already-real tenant (canActOnTenant, above). `tenants`
	// carries no RLS, so a plain existence check needs no tenant/platform
	// GUC context.
	if tc.TenantID == uuid.Nil {
		var exists bool
		if err := deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tenants WHERE id=$1)`, target).Scan(&exists)
		}); err != nil {
			c.logger.Error("payments_kill_switch_tenant_lookup_failed", "op", op)
			apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
			return c, false
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return c, false
		}
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

// killSwitchErrorClass classifies err into the class both
// writeKillSwitchError and recordKillSwitchRefusalAudit use: "cas_conflict"
// and "trigger_refusal" are genuine administrative refusals (409, and -
// RV-PRH-I1 security review L5 - a denied-audit row); anything else is
// "internal_error" (500, no denied-audit row - a transient DB/infra
// failure is not a considered refusal and must never be recorded as one).
func killSwitchErrorClass(err error) string {
	switch {
	case errors.Is(err, payments.ErrAttemptStateConflict):
		return "cas_conflict"
	case isKillSwitchTriggerRefusal(err):
		return "trigger_refusal"
	default:
		return "internal_error"
	}
}

// writeKillSwitchError maps a killswitch.go/DB error to one fixed response
// per class, mirroring provider_credential_handlers.go's
// writeProviderCredentialError convention: never the raw trigger/Postgres
// error text (which could carry no secret here, but the convention is
// still "one fixed body per class", not an ad hoc passthrough).
// writeKillSwitchError maps an error to a response class and logs an
// allow-listed error CLASS (RV-PRH-I1 security review L5): a trigger
// refusal (self-approval, a platform-engaged row touched by a tenant
// session, a stale version, an expired or already-decided request - always
// Postgres SQLSTATE P0001, raised by a plain RAISE EXCEPTION with no other
// code path in this schema using that SQLSTATE) is a 409 conflict; any
// other error is a genuine internal/database failure and is a 500, never
// mis-reported as "conflict" (an operator retrying an engage that failed on
// a transient DB error must not be told "already engaged"). The raw
// message is logged server-side for diagnosis - these are internal
// database errors, never user-controlled input, so logging them carries no
// secret-leak risk (S95-C8(a)'s redaction concern is specific to vendor
// HTTP transport errors, not this package).
//
// It also writes the RV-PRH-I1 security review L5 denied-audit row for the
// two refusal classes, in a transaction SEPARATE from the mutation attempt
// that failed - that transaction already rolled back, discarding anything
// audit.Record would have written inside it, so a refused self-approval,
// platform-lock violation, stale-version or expired-request release
// attempt would otherwise leave no audit trail at all. A genuine internal
// error gets no denied-audit row: it is not a considered refusal, and
// mis-recording infrastructure failures as denials would corrupt the
// audit trail's meaning.
func writeKillSwitchError(ctx context.Context, deps Deps, w http.ResponseWriter, c killSwitchCall, op, targetType, targetID string, err error) {
	class := killSwitchErrorClass(err)
	switch class {
	case "cas_conflict":
		c.logger.Warn("payments_kill_switch_refused", "op", op, "class", class)
		recordKillSwitchRefusalAudit(ctx, deps, c, op, class, targetType, targetID)
		apierror.Write(w, c.requestID, apierror.CodeConflict, "conflict")
	case "trigger_refusal":
		c.logger.Warn("payments_kill_switch_refused", "op", op, "class", class, "err", err.Error())
		recordKillSwitchRefusalAudit(ctx, deps, c, op, class, targetType, targetID)
		apierror.Write(w, c.requestID, apierror.CodeConflict, "conflict")
	default:
		c.logger.Error("payments_kill_switch_failed", "op", op, "class", class, "err", err.Error())
		apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	}
}

// recordKillSwitchRefusalAudit writes the OutcomeDenied audit row for a
// kill-switch mutation refused by a database trigger or a CAS conflict
// (RV-PRH-I1 security review L5). Uses runKillSwitchTx - a fresh
// transaction dispatched exactly like the mutation itself - so this write
// commits independently of the failed attempt. A failure to write the
// audit row itself is logged, never surfaced to the caller: the original
// refusal response (409) already told the caller what happened, and the
// caller must not receive a DIFFERENT status because audit logging had a
// problem.
func recordKillSwitchRefusalAudit(ctx context.Context, deps Deps, c killSwitchCall, op, class, targetType, targetID string) {
	entry := audit.Entry{
		TenantID: c.auditTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "payments_kill_switch." + op, TargetType: targetType, TargetID: targetID,
		Outcome: audit.OutcomeDenied, RequestID: c.requestID,
		Metadata: map[string]any{"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(), "denied_class": class},
	}
	if err := runKillSwitchTx(ctx, deps, c, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, entry)
	}); err != nil {
		c.logger.Error("payments_kill_switch_denied_audit_failed", "op", op, "class", class)
	}
}

// isKillSwitchTriggerRefusal reports whether err is a plain RAISE EXCEPTION
// from one of migration 0105/0106's own trigger functions (SQLSTATE
// P0001) - a semantic refusal, not an infrastructure failure.
func isKillSwitchTriggerRefusal(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "P0001"
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

// scopeOrNil renders a *payments.KillSwitchSessionScope as a plain string
// pointer for JSON metadata (nil stays nil, never "").
func scopeOrNil(s *payments.KillSwitchSessionScope) any {
	if s == nil {
		return nil
	}
	return string(*s)
}

// engageAuditBeforeState renders the pre-engage state for M3's audit
// before/after requirement. found=false (first-ever engage of this
// provider/operation pair) is itself meaningful and recorded explicitly,
// never silently omitted.
func engageAuditBeforeState(before payments.KillSwitch, found bool) map[string]any {
	if !found {
		return map[string]any{"existed": false}
	}
	return map[string]any{
		"existed": true, "engaged": before.Engaged, "engaged_by_scope": scopeOrNil(before.EngagedByScope),
		"version": before.Version, "reason_code": before.ReasonCode,
	}
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
			writeKillSwitchError(r.Context(), deps, w, c, "list", "payment_kill_switch", "", err)
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
			writeKillSwitchError(r.Context(), deps, w, c, "get", "payment_kill_switch", id.String(), err)
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
			writeKillSwitchError(r.Context(), deps, w, c, "get_release_request", "payment_kill_switch_release_request", id.String(), err)
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
		// M2 (RV-PRH-I1 security review): provider_scope is validated
		// against the CODE-level provider registry (never tenant-editable
		// capability config, which a tenant could otherwise widen) so a
		// typo or trailing whitespace can never engage a switch that
		// matches nothing - a false-assurance containment that returns 200
		// but blocks no real traffic.
		if body.ProviderScope != strings.TrimSpace(body.ProviderScope) {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "provider_scope must not have leading or trailing whitespace")
			return
		}
		if body.ProviderScope != "*" {
			if _, registered := deps.PaymentOrchestrator.Provider(body.ProviderScope); !registered {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "provider_scope must be '*' or a provider id registered in this deployment")
				return
			}
		}
		var ks payments.KillSwitch
		err := runKillSwitchTx(r.Context(), deps, c, func(ctx context.Context, tx pgx.Tx) error {
			// M3: capture the prior state (natural-key lookup - engage has
			// no id yet on a first-ever engage of this provider/operation
			// pair) BEFORE mutating, so the audit row carries a real
			// before/after, not just the after-state reason_code.
			before, foundBefore, err := payments.GetKillSwitch(ctx, tx, c.target, body.ProviderScope, payments.KillSwitchOperationScope(body.OperationScope))
			if err != nil {
				return err
			}
			// M3: KS-L6 detection. At most one request can be open per
			// switch (the DB's own partial unique index), so this is the
			// one KS-L6 could cancel if this engage turns out to be a
			// platform takeover.
			var preexistingOpenRequestID *string
			if foundBefore {
				var openID uuid.UUID
				scanErr := tx.QueryRow(ctx,
					`SELECT id FROM payment_kill_switch_release_requests WHERE kill_switch_id=$1 AND status='open'`, before.ID,
				).Scan(&openID)
				if scanErr == nil {
					s := openID.String()
					preexistingOpenRequestID = &s
				} else if !errors.Is(scanErr, pgx.ErrNoRows) {
					return scanErr
				}
			}

			ks, err = payments.EngageKillSwitch(ctx, tx, c.target, body.ProviderScope, payments.KillSwitchOperationScope(body.OperationScope), body.ReasonCode)
			if err != nil {
				return err
			}

			isTakeover := foundBefore && before.Engaged && before.EngagedByScope != nil && *before.EngagedByScope == payments.KillSwitchScopeTenant &&
				ks.EngagedByScope != nil && *ks.EngagedByScope == payments.KillSwitchScopePlatform

			metadata := map[string]any{
				"reason_code": body.ReasonCode, "provider_scope": body.ProviderScope, "operation_scope": body.OperationScope,
				"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
				"before":               engageAuditBeforeState(before, foundBefore),
				"after":                map[string]any{"engaged": ks.Engaged, "engaged_by_scope": scopeOrNil(ks.EngagedByScope), "version": ks.Version, "reason_code": ks.ReasonCode},
				"is_platform_takeover": isTakeover,
			}
			if isTakeover && preexistingOpenRequestID != nil {
				metadata["ks_l6_cancelled_request_id"] = *preexistingOpenRequestID
			}

			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "payments_kill_switch.engage", TargetType: "payment_kill_switch", TargetID: ks.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID,
				Metadata: metadata,
			})
		})
		if err != nil {
			writeKillSwitchError(r.Context(), deps, w, c, "engage", "payment_kill_switch", "", err)
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
				Metadata: map[string]any{
					"reason_code": body.ReasonCode, "kill_switch_id": killSwitchID.String(), "actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
					"before": map[string]any{"engaged": ks.Engaged, "version": ks.Version},
					"after":  map[string]any{"status": req.Status, "expected_version": req.ExpectedVersion},
				},
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
			writeKillSwitchError(r.Context(), deps, w, c, "request_release", "payment_kill_switch", killSwitchID.String(), err)
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
			// M3: capture the switch's state before release.
			before, foundBefore, err := payments.GetKillSwitchByID(ctx, tx, c.target, req.KillSwitchID)
			if err != nil {
				return err
			}
			ks, err = payments.ApproveAndReleaseKillSwitch(ctx, tx, c.target, req.KillSwitchID, requestID)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: c.auditTenantID(), ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "payments_kill_switch.approve_release", TargetType: "payment_kill_switch", TargetID: ks.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID,
				Metadata: map[string]any{
					"release_request_id": requestID.String(), "actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
					"before": engageAuditBeforeState(before, foundBefore),
					"after":  map[string]any{"engaged": ks.Engaged, "engaged_by_scope": scopeOrNil(ks.EngagedByScope), "version": ks.Version},
				},
			})
		})
		if notFound {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		if err != nil {
			writeKillSwitchError(r.Context(), deps, w, c, "approve_release", "payment_kill_switch_release_request", requestID.String(), err)
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
				Metadata: map[string]any{
					"actor_scope": c.sessionScopeLabel(), "target_tenant_id": c.target.String(),
					"before": map[string]any{"status": req.Status},
					"after":  map[string]any{"status": "cancelled"},
				},
			})
		})
		if notFound {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		if err != nil {
			writeKillSwitchError(r.Context(), deps, w, c, "cancel_release", "payment_kill_switch_release_request", requestID.String(), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
