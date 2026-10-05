// PRH-2 K3 (ADR 0101 24.9 / R-9): the payment force-resolution staff API.
//
// Route shape mirrors manual_adjustment_routes.go (K2): the target tenant
// always comes from the {tenantID} path value (a body tenant_id is accepted
// and IGNORED), canActOnTenant refuses a tenant caller naming another tenant
// (403, no data), and a platform caller may name any tenant - but then acts
// ONLY through payments.ManualResolutionService's runSession, i.e. the ADR 0099
// 6.1 acting session, which the database refuses (CG020) unless the principal
// holds an in-force grant for exactly that tenant. This file never opens a
// session itself for these routes and never calls the acting setter.
//
// Two layers, both required (ADR 0099 2): the static permission below is only
// the route gate (finance and platform_admin request/approve, compliance reads,
// never tenant_admin); the authority (the in-force payment_force_resolve
// capability grant, the distinct-Person floor, the beneficiary exclusion,
// S-2(iii), the closed-tenant actor scope, the pinned factual basis, the
// DB-side recount) is migration 0115's, decided in the action's own
// transaction. Errors are classified by SQLSTATE only, never message text, and
// every error body is one of the CLOSED token set of ADR 0101 24.9
// (force_resolve_*). Every refusal writes a payment.manual_resolution_denied
// audit row.
package httpserver

import (
	"context"
	"net/http"
	"strconv"
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

func registerPaymentForceResolutionRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	svc := payments.NewManualResolutionService(deps.DB, payments.DefaultStatementSources)
	// Literal patterns only: the webhook route guard (security C3/L8) fails
	// closed on any non-literal Handle pattern.
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payment-force-resolutions",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveRequest)(newRequestResolutionHandler(deps, svc))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/payment-force-resolutions",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveRead)(newListResolutionsHandler(deps, svc))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/payment-force-resolutions/{resolutionID}",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveRead)(newGetResolutionHandler(deps, svc))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payment-force-resolutions/{resolutionID}/approve",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveApprove)(newDecideResolutionHandler(deps, svc, payments.ResolutionApprove))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payment-force-resolutions/{resolutionID}/reject",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveApprove)(newDecideResolutionHandler(deps, svc, payments.ResolutionReject))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/payment-force-resolutions/{resolutionID}/cancel",
		staff(auth.RequirePermission(auth.PermPaymentForceResolveRequest)(newCancelResolutionHandler(deps, svc))))
}

// resolutionCall is the per-request context, mirroring adjustmentCall.
type resolutionCall struct {
	requestID string
	logger    observabilityLogger
	tc        tenant.Context
	subject   uuid.UUID
	target    uuid.UUID
	ipAddress string
	userAgent string
}

func (c resolutionCall) meta() payments.ResolutionMeta {
	return payments.ResolutionMeta{IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID}
}

// beginResolutionCall resolves the verified caller and the
// canActOnTenant-validated {tenantID} path value.
func beginResolutionCall(deps Deps, w http.ResponseWriter, r *http.Request, op string) (resolutionCall, bool) {
	c := resolutionCall{
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
	c.tc, c.subject = tc, subject
	target, err := uuid.Parse(r.PathValue("tenantID"))
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
		return c, false
	}
	c.target = target
	if !canActOnTenant(tc, target) {
		c.logger.Warn("payment_force_resolution_denied", "op", op, "reason", "foreign_tenant")
		recordResolutionDenied(r.Context(), deps, c, op, payments.TokenForceResolveNotPermitted, "")
		apierror.Write(w, c.requestID, apierror.CodeForbidden, payments.TokenForceResolveNotPermitted)
		return c, false
	}
	return c, true
}

func resolutionTarget(w http.ResponseWriter, c resolutionCall) (payments.ResolutionTarget, bool) {
	tg, err := payments.NewResolutionTarget(c.tc, c.target)
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeForbidden, payments.TokenForceResolveNotPermitted)
		return payments.ResolutionTarget{}, false
	}
	return tg, true
}

// recordResolutionDenied writes a denied audit row outside the refused
// transaction: a tenant caller's own tenant, or - for a platform caller, who
// may have no valid acting session at all - a platform row with the route
// target as subject tenant (ADR 0104 / migration 0109). The row carries the
// closed token, the actor, the tenant, the resolution or attempt id (when the
// route names one), IP, user agent and request id.
func recordResolutionDenied(ctx context.Context, deps Deps, c resolutionCall, op, token, sqlstate string, ids ...string) {
	meta := map[string]any{"denied_token": token, "target_tenant_id": c.target.String()}
	if sqlstate != "" {
		// The SQLSTATE CODE only, never message text (PM-S5): the audit row tells an
		// operator which guard refused without carrying database wording.
		meta["sqlstate"] = sqlstate
	}
	if len(ids) > 0 && ids[0] != "" {
		meta["resolution_id"] = ids[0]
	}
	if len(ids) > 1 && ids[1] != "" {
		meta["attempt_id"] = ids[1]
	}
	entry := audit.Entry{
		ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "payment.manual_resolution_denied", TargetType: "payment_manual_resolution",
		Outcome: audit.OutcomeDenied, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
		Metadata: meta,
	}
	meta["op"] = op
	if len(ids) > 0 && ids[0] != "" {
		entry.TargetID = ids[0]
	}
	dctx, cancel := deniedAuditCtx(ctx)
	defer cancel()
	var err error
	if c.tc.TenantID == uuid.Nil {
		entry.SubjectTenantID = c.target
		err = deps.DB.WithPlatformAdmin(dctx, c.subject, func(ctx context.Context, tx pgx.Tx) error { return audit.Record(ctx, tx, entry) })
	} else {
		entry.TenantID = c.tc.TenantID
		err = deps.DB.WithTenant(dctx, c.tc.TenantID, func(ctx context.Context, tx pgx.Tx) error { return audit.Record(ctx, tx, entry) })
	}
	if err != nil {
		c.logger.Error("payment_force_resolution_denied_audit_failed", "op", op)
	}
}

// writeResolutionError maps payments.ClassifyResolutionError to one fixed
// closed-token body per class; every refusal also writes a denied audit row.
func writeResolutionError(ctx context.Context, deps Deps, w http.ResponseWriter, c resolutionCall, op string, err error, ids ...string) {
	class := payments.ClassifyResolutionError(err)
	token := payments.ResolutionToken(class)
	switch class {
	case payments.ResolutionErrNotFound:
		apierror.Write(w, c.requestID, apierror.CodeNotFound, token)
	case payments.ResolutionErrInvalid:
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
	case payments.ResolutionErrForbidden, payments.ResolutionErrSession:
		c.logger.Warn("payment_force_resolution_refused", "op", op, "class", string(class), "sqlstate", payments.ResolutionSQLState(err))
		recordResolutionDenied(ctx, deps, c, op, token, payments.ResolutionSQLState(err), ids...)
		apierror.Write(w, c.requestID, apierror.CodeForbidden, token)
	case payments.ResolutionErrDisabled, payments.ResolutionErrPrecondition, payments.ResolutionErrNotResolvable,
		payments.ResolutionErrExpired, payments.ResolutionErrConflict, payments.ResolutionErrRetryable:
		c.logger.Warn("payment_force_resolution_refused", "op", op, "class", string(class), "sqlstate", payments.ResolutionSQLState(err))
		recordResolutionDenied(ctx, deps, c, op, token, payments.ResolutionSQLState(err), ids...)
		apierror.Write(w, c.requestID, apierror.CodeConflict, token)
	default:
		c.logger.Error("payment_force_resolution_failed", "op", op, "err", err.Error())
		apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	}
}

// --- DTOs ---

type requestResolutionBody struct {
	// TenantID is accepted and IGNORED: the tenant is the canActOnTenant-
	// validated path value, never a body field (ADR 0101 24.9).
	TenantID        *string `json:"tenant_id,omitempty"`
	AttemptID       string  `json:"attempt_id"`
	Kind            string  `json:"kind"`
	FindingCode     string  `json:"finding_code,omitempty"`
	BasisCode       string  `json:"basis_code,omitempty"`
	ContextCode     string  `json:"context_code,omitempty"`
	EvidenceRefHash string  `json:"evidence_ref_hash,omitempty"`
	ReasonCode      string  `json:"reason_code"`
	Note            string  `json:"note,omitempty"`
}

type resolutionDTO struct {
	ID                  string  `json:"id"`
	TenantID            string  `json:"tenant_id"`
	AttemptID           string  `json:"attempt_id"`
	Operation           string  `json:"operation"`
	Kind                string  `json:"kind"`
	TargetState         *string `json:"target_state,omitempty"`
	FindingCode         *string `json:"finding_code,omitempty"`
	BasisCode           *string `json:"basis_code,omitempty"`
	ContextCode         *string `json:"context_code,omitempty"`
	EvidenceRefHash     *string `json:"evidence_ref_hash,omitempty"`
	AmountMinorUnits    string  `json:"amount_minor_units"`
	AssetCode           string  `json:"asset_code"`
	ReasonCode          string  `json:"reason_code"`
	PayloadHash         string  `json:"payload_hash"`
	RequestedBy         string  `json:"requested_by"`
	RequestedByScope    string  `json:"requested_by_scope"`
	RequiredAtSubmit    int     `json:"required_at_submission"`
	TenantStatus        string  `json:"tenant_status_at_submission"`
	State               string  `json:"state"`
	RefusalCode         *string `json:"refusal_code,omitempty"`
	LedgerTransactionID *string `json:"ledger_transaction_id,omitempty"`
	CreatedAt           string  `json:"created_at"`
	ExpiresAt           string  `json:"expires_at"`
}

func toResolutionDTO(r payments.ManualResolution) resolutionDTO {
	dto := resolutionDTO{
		ID: r.ID.String(), TenantID: r.TenantID.String(), AttemptID: r.AttemptID.String(), Operation: r.Operation,
		Kind: string(r.Kind), TargetState: r.TargetState, FindingCode: r.FindingCode, BasisCode: r.BasisCode,
		ContextCode: r.ContextCode, EvidenceRefHash: r.EvidenceRefHash, AmountMinorUnits: strconv.FormatInt(r.Amount, 10),
		AssetCode: r.AssetCode, ReasonCode: r.ReasonCode, PayloadHash: r.PayloadHash, RequestedBy: r.RequestedBy.String(),
		RequestedByScope: r.RequestedByScope, RequiredAtSubmit: r.RequiredAtSubmission, TenantStatus: r.TenantStatusAtSubmission,
		State: string(r.State), RefusalCode: r.RefusalCode,
		CreatedAt: r.CreatedAt.Format(time.RFC3339), ExpiresAt: r.ExpiresAt.Format(time.RFC3339),
	}
	if r.LedgerTransactionID != nil {
		s := r.LedgerTransactionID.String()
		dto.LedgerTransactionID = &s
	}
	return dto
}

// --- handlers ---

func newRequestResolutionHandler(deps Deps, svc *payments.ManualResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResolutionCall(deps, w, r, "request")
		if !ok {
			return
		}
		tg, ok := resolutionTarget(w, c)
		if !ok {
			return
		}
		var body requestResolutionBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		attemptID, err := uuid.Parse(body.AttemptID)
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
			return
		}
		res, err := svc.Request(r.Context(), tg, payments.ResolutionRequestInput{
			AttemptID: attemptID, Kind: payments.ResolutionKind(body.Kind), FindingCode: body.FindingCode,
			BasisCode: body.BasisCode, ContextCode: body.ContextCode, EvidenceRefHash: body.EvidenceRefHash,
			ReasonCode: body.ReasonCode, Note: body.Note,
		}, c.meta())
		if err != nil {
			writeResolutionError(r.Context(), deps, w, c, "request", err, "", attemptID.String())
			return
		}
		writeJSON(w, http.StatusCreated, toResolutionDTO(res))
	}
}

func newListResolutionsHandler(deps Deps, svc *payments.ManualResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResolutionCall(deps, w, r, "list")
		if !ok {
			return
		}
		tg, ok := resolutionTarget(w, c)
		if !ok {
			return
		}
		rs, err := svc.List(r.Context(), tg, 100, c.meta())
		if err != nil {
			writeResolutionError(r.Context(), deps, w, c, "list", err)
			return
		}
		out := make([]resolutionDTO, 0, len(rs))
		for _, x := range rs {
			out = append(out, toResolutionDTO(x))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func newGetResolutionHandler(deps Deps, svc *payments.ManualResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResolutionCall(deps, w, r, "get")
		if !ok {
			return
		}
		tg, ok := resolutionTarget(w, c)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("resolutionID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		res, err := svc.Get(r.Context(), tg, id, c.meta())
		if err != nil {
			writeResolutionError(r.Context(), deps, w, c, "get", err, id.String())
			return
		}
		writeJSON(w, http.StatusOK, toResolutionDTO(res))
	}
}

type decideResolutionBody struct {
	TenantID    *string `json:"tenant_id,omitempty"` // accepted and IGNORED
	PayloadHash string  `json:"payload_hash"`
	ReasonCode  string  `json:"reason_code"`
}

type resolutionDecisionResponse struct {
	Resolution resolutionDTO `json:"resolution"`
	Executed   bool          `json:"executed"`
	Refused    bool          `json:"refused"`
	Counted    int           `json:"counted_approvals"`
	Required   int           `json:"required_approvals"`
}

func newDecideResolutionHandler(deps Deps, svc *payments.ManualResolutionService, decision payments.ResolutionDecision) http.HandlerFunc {
	op := string(decision)
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResolutionCall(deps, w, r, op)
		if !ok {
			return
		}
		tg, ok := resolutionTarget(w, c)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("resolutionID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body decideResolutionBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		out, err := svc.Decide(r.Context(), tg, id, payments.ResolutionDecisionInput{Decision: decision, PayloadHash: body.PayloadHash, ReasonCode: body.ReasonCode}, c.meta())
		if err != nil {
			writeResolutionError(r.Context(), deps, w, c, op, err, id.String())
			return
		}
		writeJSON(w, http.StatusOK, resolutionDecisionResponse{Resolution: toResolutionDTO(out.Resolution), Executed: out.Executed,
			Refused: out.Refused, Counted: out.Counted, Required: out.Required})
	}
}

func newCancelResolutionHandler(deps Deps, svc *payments.ManualResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginResolutionCall(deps, w, r, "cancel")
		if !ok {
			return
		}
		tg, ok := resolutionTarget(w, c)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("resolutionID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		res, err := svc.Cancel(r.Context(), tg, id, c.meta())
		if err != nil {
			writeResolutionError(r.Context(), deps, w, c, "cancel", err, id.String())
			return
		}
		writeJSON(w, http.StatusOK, toResolutionDTO(res))
	}
}
