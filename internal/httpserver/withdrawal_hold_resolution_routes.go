// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 revision 2 section 6, migration 0124): the
// withdrawal hold-resolution staff API.
//
// Route shape mirrors payment_force_resolution_routes.go (K3): the target tenant
// always comes from the {tenantID} path value (a body tenant_id is accepted and
// IGNORED), canActOnTenant refuses a tenant caller naming another tenant (403, no
// data), and the service acts ONLY through the ADR 0099 6.1 acting-session setter in
// internal/db, which the database refuses (CG020) unless the platform principal holds an
// in-force grant for exactly that tenant. This file never opens a session itself and
// never calls the acting setter.
//
// Two layers, both required (ADR 0099 2): the static permission below is only the
// route gate - withdrawal_hold_resolution:request|approve|read are held by
// RolePlatformAdmin ONLY (no tenant role, ADR 0111 security H-4; a tenant caller is
// refused 403 at the middleware and, were it ever to reach the service, by the
// service and the database); the authority (the in-force
// withdrawal_hold_resolution:request / :approve platform grant, the distinct-Person
// floor, the beneficiary exclusion, S-2(iii), the pinned preconditions, the DB-side
// recount and the signed actor proof) is migration 0124's, decided in the action's
// own transaction. Errors are classified by SQLSTATE only and every error body is one
// of the CLOSED token set (hold_resolution_*). Every refusal writes a
// withdrawal.hold_resolution_denied audit row. There is no generic override route,
// and no route that triggers a release without the counted approvals.
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerWithdrawalHoldResolutionRoutes(mux *http.ServeMux, deps Deps) {
	svc := payments.NewWithdrawalHoldResolutionService(deps.DB)
	// Literal patterns only: the webhook route guard (security C3/L8) fails closed on
	// any non-literal Handle pattern.
	mux.Handle("POST /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions",
		staffRoute(deps, stepUpFinancial, auth.RequirePermission(auth.PermWithdrawalHoldResolutionRequest)(staffIdempotent(deps, newRequestHoldResolutionHandler(deps, svc)))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalHoldResolutionRead)(newListHoldResolutionsHandler(deps, svc))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions/{resolutionID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalHoldResolutionRead)(newGetHoldResolutionHandler(deps, svc))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions/{resolutionID}/approve",
		staffRoute(deps, stepUpFinancial, auth.RequirePermission(auth.PermWithdrawalHoldResolutionApprove)(staffIdempotent(deps, newDecideHoldResolutionHandler(deps, svc, payments.ResolutionApprove)))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions/{resolutionID}/reject",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalHoldResolutionApprove)(staffIdempotent(deps, newDecideHoldResolutionHandler(deps, svc, payments.ResolutionReject)))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions/{resolutionID}/cancel",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalHoldResolutionRequest)(staffIdempotent(deps, newCancelHoldResolutionHandler(deps, svc)))))
}

// holdCall is the per-request context, mirroring resolutionCall.
type holdCall struct {
	requestID string
	logger    observabilityLogger
	tc        tenant.Context
	subject   uuid.UUID
	target    uuid.UUID
	ipAddress string
	userAgent string
}

func (c holdCall) meta() payments.ResolutionMeta {
	return payments.ResolutionMeta{IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID}
}

// beginHoldCall resolves the verified caller and the canActOnTenant-validated
// {tenantID} path value.
func beginHoldCall(deps Deps, w http.ResponseWriter, r *http.Request, op string) (holdCall, bool) {
	c := holdCall{
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
		c.logger.Warn("withdrawal_hold_resolution_denied", "op", op, "reason", "foreign_tenant")
		recordHoldResolutionDenied(r.Context(), deps, c, op, payments.TokenHoldResolveNotPermitted, "")
		apierror.Write(w, c.requestID, apierror.CodeForbidden, payments.TokenHoldResolveNotPermitted)
		return c, false
	}
	return c, true
}

func holdResolutionTarget(w http.ResponseWriter, c holdCall) (payments.ResolutionTarget, bool) {
	tg, err := payments.NewResolutionTarget(c.tc, c.target)
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeForbidden, payments.TokenHoldResolveNotPermitted)
		return payments.ResolutionTarget{}, false
	}
	return tg, true
}

// recordHoldResolutionDenied writes a denied audit row outside the refused
// transaction: a platform row with the route target as subject tenant (ADR 0104 /
// migration 0109), or the caller's own tenant for a (defence-in-depth) tenant caller.
func recordHoldResolutionDenied(ctx context.Context, deps Deps, c holdCall, op, token, sqlstate string, ids ...string) {
	meta := map[string]any{"denied_token": token, "target_tenant_id": c.target.String(), "op": op}
	if sqlstate != "" {
		// The SQLSTATE CODE only, never message text.
		meta["sqlstate"] = sqlstate
	}
	if len(ids) > 0 && ids[0] != "" {
		meta["resolution_id"] = ids[0]
	}
	if len(ids) > 1 && ids[1] != "" {
		meta["withdrawal_request_id"] = ids[1]
	}
	entry := audit.Entry{
		ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "withdrawal.hold_resolution_denied", TargetType: "withdrawal_hold_resolution",
		Outcome: audit.OutcomeDenied, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
		Metadata: meta,
	}
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
		c.logger.Error("withdrawal_hold_resolution_denied_audit_failed", "op", op)
	}
}

// writeHoldResolutionError maps payments.ClassifyHoldResolutionError to one fixed
// closed-token body per class; every refusal also writes a denied audit row.
func writeHoldResolutionError(ctx context.Context, deps Deps, w http.ResponseWriter, c holdCall, op string, err error, ids ...string) {
	if errors.Is(err, db.ErrActorStaffNotLive) {
		// ADR 0119 S4b section 9.5: the actor's own row was suspended or changed role before the A8 re-assertion;
		// nothing was written.
		c.logger.Warn("withdrawal_hold_resolution_actor_not_live", "op", op)
		observability.RecordStaffReassertionRefusal(ctx, observability.StaffReassertionFlowHoldResolution)
		recordHoldResolutionDenied(ctx, deps, c, op, "actor_not_live", "", ids...)
		writeSessionRevoked(w, c.requestID)
		return
	}
	class := payments.ClassifyHoldResolutionError(err)
	token := payments.HoldResolutionToken(class)
	switch class {
	case payments.ResolutionErrNotFound:
		apierror.Write(w, c.requestID, apierror.CodeNotFound, token)
	case payments.ResolutionErrInvalid:
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
	case payments.ResolutionErrForbidden, payments.ResolutionErrSession:
		c.logger.Warn("withdrawal_hold_resolution_refused", "op", op, "class", string(class), "sqlstate", payments.ResolutionSQLState(err))
		recordHoldResolutionDenied(ctx, deps, c, op, token, payments.ResolutionSQLState(err), ids...)
		apierror.Write(w, c.requestID, apierror.CodeForbidden, token)
	case payments.ResolutionErrDisabled, payments.ResolutionErrPrecondition, payments.ResolutionErrNotResolvable,
		payments.ResolutionErrExpired, payments.ResolutionErrConflict, payments.ResolutionErrRetryable:
		c.logger.Warn("withdrawal_hold_resolution_refused", "op", op, "class", string(class), "sqlstate", payments.ResolutionSQLState(err))
		recordHoldResolutionDenied(ctx, deps, c, op, token, payments.ResolutionSQLState(err), ids...)
		apierror.Write(w, c.requestID, apierror.CodeConflict, token)
	default:
		c.logger.Error("withdrawal_hold_resolution_failed", "op", op, "err", err.Error())
		apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	}
}

// --- DTOs ---

type requestHoldResolutionBody struct {
	// TenantID is accepted and IGNORED: the tenant is the canActOnTenant-validated path
	// value, never a body field.
	TenantID            *string `json:"tenant_id,omitempty"`
	WithdrawalRequestID string  `json:"withdrawal_request_id"`
	Kind                string  `json:"kind"`
	ReasonCode          string  `json:"reason_code"`
	EvidenceRefHash     string  `json:"evidence_ref_hash"`
	Note                string  `json:"note,omitempty"`
}

type holdResolutionDTO struct {
	ID                  string   `json:"id"`
	TenantID            string   `json:"tenant_id"`
	WithdrawalRequestID string   `json:"withdrawal_request_id"`
	Kind                string   `json:"kind"`
	AmountMinorUnits    string   `json:"amount_minor_units"`
	AssetCode           string   `json:"asset_code"`
	Exponent            *int16   `json:"exponent"` // asset registry decimal exponent (ADR 0007); null when it could not be resolved
	ReasonCode          string   `json:"reason_code"`
	EvidenceRefHash     string   `json:"evidence_ref_hash"`
	PayloadHash         string   `json:"payload_hash"`
	RequestedBy         staffRef `json:"requested_by"` // null for a non-platform caller when the requester is platform staff (ADR 0114 H1)
	RequestedByScope    string   `json:"requested_by_scope"`
	RequiredAtSubmit    int      `json:"required_at_submission"`
	TenantStatusSubmit  string   `json:"tenant_status_at_submission"`
	BrandStatusSubmit   string   `json:"brand_status_at_submission"`
	State               string   `json:"state"`
	RefusalCode         *string  `json:"refusal_code,omitempty"`
	LedgerTransactionID *string  `json:"ledger_transaction_id,omitempty"`
	CreatedAt           string   `json:"created_at"`
	ExpiresAt           string   `json:"expires_at"`
}

func toHoldResolutionDTO(v staffViewer, r payments.HoldResolution, exps map[string]int16) holdResolutionDTO {
	dto := holdResolutionDTO{
		ID: r.ID.String(), TenantID: r.TenantID.String(), WithdrawalRequestID: r.WithdrawalRequestID.String(), Kind: string(r.Kind),
		AmountMinorUnits: strconv.FormatInt(r.Amount, 10), AssetCode: r.AssetCode, Exponent: exponentRef(exps, r.AssetCode), ReasonCode: r.ReasonCode,
		EvidenceRefHash: r.EvidenceRefHash, PayloadHash: r.PayloadHash, RequestedBy: v.ref(r.RequestedByScope, r.RequestedBy), RequestedByScope: r.RequestedByScope,
		RequiredAtSubmit: r.RequiredAtSubmission, TenantStatusSubmit: r.TenantStatusAtSubmission, BrandStatusSubmit: r.BrandStatusAtSubmission,
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

func newRequestHoldResolutionHandler(deps Deps, svc *payments.WithdrawalHoldResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginHoldCall(deps, w, r, "request")
		if !ok {
			return
		}
		tg, ok := holdResolutionTarget(w, c)
		if !ok {
			return
		}
		var body requestHoldResolutionBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		wrID, err := uuid.Parse(body.WithdrawalRequestID)
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
			return
		}
		res, err := svc.Request(r.Context(), tg, payments.HoldResolutionRequestInput{
			WithdrawalRequestID: wrID, Kind: payments.HoldResolutionKind(body.Kind), ReasonCode: body.ReasonCode,
			EvidenceRefHash: body.EvidenceRefHash, Note: body.Note,
		}, c.meta())
		if err != nil {
			writeHoldResolutionError(r.Context(), deps, w, c, "request", err, "", wrID.String())
			return
		}
		exps := resolutionExponentsBestEffort(r.Context(), deps, c.logger, res.AssetCode)
		writeJSON(w, http.StatusCreated, toHoldResolutionDTO(staffViewerOf(c.tc), res, exps))
	}
}

func newListHoldResolutionsHandler(deps Deps, svc *payments.WithdrawalHoldResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginHoldCall(deps, w, r, "list")
		if !ok {
			return
		}
		tg, ok := holdResolutionTarget(w, c)
		if !ok {
			return
		}
		rs, err := svc.List(r.Context(), tg, 100, c.meta())
		if err != nil {
			writeHoldResolutionError(r.Context(), deps, w, c, "list", err)
			return
		}
		codes := make([]string, 0, len(rs))
		for _, x := range rs {
			codes = append(codes, x.AssetCode)
		}
		exps, ok := resolutionExponentsOrFail(r.Context(), deps, w, c.logger, c.requestID, codes...)
		if !ok {
			return
		}
		out := make([]holdResolutionDTO, 0, len(rs))
		for _, x := range rs {
			out = append(out, toHoldResolutionDTO(staffViewerOf(c.tc), x, exps))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func newGetHoldResolutionHandler(deps Deps, svc *payments.WithdrawalHoldResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginHoldCall(deps, w, r, "get")
		if !ok {
			return
		}
		tg, ok := holdResolutionTarget(w, c)
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
			writeHoldResolutionError(r.Context(), deps, w, c, "get", err, id.String())
			return
		}
		exps, ok := resolutionExponentsOrFail(r.Context(), deps, w, c.logger, c.requestID, res.AssetCode)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, toHoldResolutionDTO(staffViewerOf(c.tc), res, exps))
	}
}

type decideHoldResolutionBody struct {
	TenantID    *string `json:"tenant_id,omitempty"` // accepted and IGNORED
	PayloadHash string  `json:"payload_hash"`
	ReasonCode  string  `json:"reason_code"`
}

type holdResolutionDecisionResponse struct {
	Resolution holdResolutionDTO `json:"resolution"`
	Executed   bool              `json:"executed"`
	Refused    bool              `json:"refused"`
	Counted    int               `json:"counted_approvals"`
	Required   int               `json:"required_approvals"`
}

func newDecideHoldResolutionHandler(deps Deps, svc *payments.WithdrawalHoldResolutionService, decision payments.ResolutionDecision) http.HandlerFunc {
	op := string(decision)
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginHoldCall(deps, w, r, op)
		if !ok {
			return
		}
		tg, ok := holdResolutionTarget(w, c)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("resolutionID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body decideHoldResolutionBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		out, err := svc.Decide(r.Context(), tg, id, payments.HoldResolutionDecisionInput{Decision: decision, PayloadHash: body.PayloadHash, ReasonCode: body.ReasonCode}, c.meta())
		if err != nil {
			writeHoldResolutionError(r.Context(), deps, w, c, op, err, id.String())
			return
		}
		exps := resolutionExponentsBestEffort(r.Context(), deps, c.logger, out.Resolution.AssetCode)
		writeJSON(w, http.StatusOK, holdResolutionDecisionResponse{Resolution: toHoldResolutionDTO(staffViewerOf(c.tc), out.Resolution, exps), Executed: out.Executed,
			Refused: out.Refused, Counted: out.Counted, Required: out.Required})
	}
}

func newCancelHoldResolutionHandler(deps Deps, svc *payments.WithdrawalHoldResolutionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginHoldCall(deps, w, r, "cancel")
		if !ok {
			return
		}
		tg, ok := holdResolutionTarget(w, c)
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
			writeHoldResolutionError(r.Context(), deps, w, c, "cancel", err, id.String())
			return
		}
		exps := resolutionExponentsBestEffort(r.Context(), deps, c.logger, res.AssetCode)
		writeJSON(w, http.StatusOK, toHoldResolutionDTO(staffViewerOf(c.tc), res, exps))
	}
}
