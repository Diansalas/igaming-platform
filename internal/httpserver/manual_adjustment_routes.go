// PRH-2 K2 (ADR 0100): the governed manual adjustment and financial
// approval policy admin API.
//
// Route shape mirrors capability_routes.go (K1): the target tenant always
// comes from the {tenantID} path value, canActOnTenant refuses a tenant
// caller naming another tenant (403, no data), and a platform caller may
// name any tenant - but then acts ONLY through internal/adjustment's
// runSession, i.e. the ADR 0099 §6.1 acting session, which the database
// refuses (CG020) unless the principal holds an in-force grant for exactly
// that tenant. This file never opens a session itself for the adjustment
// routes and never calls the acting setter (static test K2-P3 pins both).
//
// Two layers, both required (ADR 0099 §2): the static permission below is
// only the route gate; the authority (the in-force ledger_adjustment
// capability grant, the distinct-Person floor, the beneficiary exclusion,
// S-2(iii), the reason-code catalogue, the exposure refusal, the policy
// evaluation) is migration 0113's, decided in the action's own
// transaction. Errors are classified by SQLSTATE only, never message text,
// and every response body is a fixed string.
package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerManualAdjustmentRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	svc := adjustment.NewService(deps.DB)
	base := "/v1/admin/tenants/{tenantID}/manual-adjustments"
	mux.Handle("POST "+base,
		staff(auth.RequirePermission(auth.PermLedgerAdjustmentInitiate)(newSubmitAdjustmentHandler(deps, svc))))
	mux.Handle("GET "+base,
		staff(auth.RequirePermission(auth.PermLedgerAdjustmentRead)(newListAdjustmentsHandler(deps, svc))))
	mux.Handle("GET "+base+"/{requestID}",
		staff(auth.RequirePermission(auth.PermLedgerAdjustmentRead)(newGetAdjustmentHandler(deps, svc))))
	mux.Handle("POST "+base+"/{requestID}/approve",
		staff(auth.RequirePermission(auth.PermLedgerAdjustmentApprove)(newDecideAdjustmentHandler(deps, svc, adjustment.DecisionApprove))))
	mux.Handle("POST "+base+"/{requestID}/reject",
		staff(auth.RequirePermission(auth.PermLedgerAdjustmentApprove)(newDecideAdjustmentHandler(deps, svc, adjustment.DecisionReject))))
	mux.Handle("POST "+base+"/{requestID}/cancel",
		staff(auth.RequirePermission(auth.PermLedgerAdjustmentInitiate)(newCancelAdjustmentHandler(deps, svc))))

	// Financial approval policy changes (HD-PRH2-7): platform-family rows
	// (platform/jurisdiction/profile) on the platform route; tenant/brand
	// rows and profile assignments on the tenant route. Tenants may only
	// tighten - enforced by migration 0113, not here.
	mux.Handle("POST /v1/admin/financial-policy-changes",
		staff(auth.RequirePermission(auth.PermFinancialPolicyAuthor)(newProposePolicyChangeHandler(deps, svc, false))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/financial-policy-changes",
		staff(auth.RequireAnyPermission(auth.PermFinancialPolicyAuthor, auth.PermFinancialPolicyTighten)(newProposePolicyChangeHandler(deps, svc, true))))
	mux.Handle("POST /v1/admin/financial-policy-changes/{changeID}/approve",
		staff(auth.RequireAnyPermission(auth.PermFinancialPolicyAuthor, auth.PermFinancialPolicyTighten)(newDecidePolicyChangeHandler(deps, svc, adjustment.DecisionApprove))))
	mux.Handle("POST /v1/admin/financial-policy-changes/{changeID}/reject",
		staff(auth.RequireAnyPermission(auth.PermFinancialPolicyAuthor, auth.PermFinancialPolicyTighten)(newDecidePolicyChangeHandler(deps, svc, adjustment.DecisionReject))))
	mux.Handle("POST /v1/admin/financial-policy-changes/{changeID}/cancel",
		staff(auth.RequireAnyPermission(auth.PermFinancialPolicyAuthor, auth.PermFinancialPolicyTighten)(newCancelPolicyChangeHandler(deps, svc))))
	mux.Handle("GET /v1/admin/financial-policies",
		staff(auth.RequirePermission(auth.PermFinancialPolicyRead)(newListPoliciesHandler(deps, svc))))
}

// adjustmentCall is the per-request context, mirroring capabilityCall.
type adjustmentCall struct {
	requestID string
	logger    observabilityLogger
	tc        tenant.Context
	subject   uuid.UUID
	target    uuid.UUID
	ipAddress string
	userAgent string
}

func (c adjustmentCall) meta() adjustment.Meta {
	return adjustment.Meta{IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID}
}

// beginAdjustmentCall resolves the verified caller and, when withTarget,
// the canActOnTenant-validated {tenantID} path value.
func beginAdjustmentCall(deps Deps, w http.ResponseWriter, r *http.Request, op string, withTarget bool) (adjustmentCall, bool) {
	c := adjustmentCall{
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
	if !withTarget {
		return c, true
	}
	target, err := uuid.Parse(r.PathValue("tenantID"))
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
		return c, false
	}
	c.target = target
	if !canActOnTenant(tc, target) {
		c.logger.Warn("ledger_adjustment_denied", "op", op, "reason", "foreign_tenant")
		recordAdjustmentDenied(r.Context(), deps, c, op, "foreign_tenant")
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return c, false
	}
	return c, true
}

// adjustmentTarget builds the adjustment.Target from the validated call.
func adjustmentTarget(w http.ResponseWriter, c adjustmentCall) (adjustment.Target, bool) {
	tg, err := adjustment.NewTarget(c.tc, c.target)
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return adjustment.Target{}, false
	}
	return tg, true
}

// recordAdjustmentDenied writes a denied audit row outside the refused
// transaction: a tenant caller's own tenant, or - for a platform caller,
// who may have no valid acting session at all - a platform row with the
// route target as subject tenant (ADR 0104 / migration 0109).
func recordAdjustmentDenied(ctx context.Context, deps Deps, c adjustmentCall, op, class string) {
	entry := audit.Entry{
		ActorType: audit.ActorStaff, ActorID: c.subject,
		Action: "ledger_adjustment." + op, TargetType: "ledger_adjustment_request",
		Outcome: audit.OutcomeDenied, IPAddress: c.ipAddress, UserAgent: c.userAgent, RequestID: c.requestID,
		Metadata: map[string]any{"denied_class": class, "target_tenant_id": c.target.String()},
	}
	record := func(ctx context.Context, tx pgx.Tx) error { return audit.Record(ctx, tx, entry) }
	dctx, cancel := deniedAuditCtx(ctx)
	defer cancel()
	var err error
	if c.tc.TenantID == uuid.Nil {
		entry.SubjectTenantID = c.target
		if c.target == uuid.Nil {
			entry.SubjectTenantID = uuid.Nil
		}
		err = deps.DB.WithPlatformAdmin(dctx, c.subject, func(ctx context.Context, tx pgx.Tx) error { return audit.Record(ctx, tx, entry) })
	} else {
		entry.TenantID = c.tc.TenantID
		err = deps.DB.WithTenant(dctx, c.tc.TenantID, record)
	}
	if err != nil {
		c.logger.Error("ledger_adjustment_denied_audit_failed", "op", op)
	}
}

// writeAdjustmentError maps adjustment.ClassifyError to one fixed body per
// class; a refusal also writes a denied audit row.
func writeAdjustmentError(ctx context.Context, deps Deps, w http.ResponseWriter, c adjustmentCall, op string, err error) {
	class := adjustment.ClassifyError(err)
	switch class {
	case adjustment.ErrClassNotFound:
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
	case adjustment.ErrClassInvalid:
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
	case adjustment.ErrClassForbidden:
		c.logger.Warn("ledger_adjustment_refused", "op", op, "class", string(class), "sqlstate", adjustment.SQLState(err))
		recordAdjustmentDenied(ctx, deps, c, op, string(class))
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
	case adjustment.ErrClassDisabled:
		recordAdjustmentDenied(ctx, deps, c, op, string(class))
		apierror.Write(w, c.requestID, apierror.CodeConflict, "manual adjustments are not enabled for this tenant")
	case adjustment.ErrClassExposure:
		recordAdjustmentDenied(ctx, deps, c, op, string(class))
		apierror.Write(w, c.requestID, apierror.CodeConflict, "open_payment_exposure")
	case adjustment.ErrClassConflict:
		c.logger.Warn("ledger_adjustment_refused", "op", op, "class", string(class), "sqlstate", adjustment.SQLState(err))
		recordAdjustmentDenied(ctx, deps, c, op, string(class)+":"+adjustment.SQLState(err))
		apierror.Write(w, c.requestID, apierror.CodeConflict, "conflict")
	case adjustment.ErrClassSessionInvalid:
		// CG020: a platform caller without an in-force grant for the
		// target tenant (or a suspended principal) - refused, not a 500.
		c.logger.Warn("ledger_adjustment_session_refused", "op", op, "sqlstate", adjustment.SQLState(err))
		recordAdjustmentDenied(ctx, deps, c, op, string(class))
		apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
	default:
		c.logger.Error("ledger_adjustment_failed", "op", op, "err", err.Error())
		apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	}
}

// --- DTOs ---

type submitAdjustmentBody struct {
	WalletID               string  `json:"wallet_id"`
	AssetCode              string  `json:"asset_code"`
	Direction              string  `json:"direction"`
	AmountMinorUnits       string  `json:"amount_minor_units"`
	ReasonCode             string  `json:"reason_code"`
	CausationTransactionID *string `json:"causation_transaction_id,omitempty"`
	EvidenceRefHash        *string `json:"evidence_ref_hash,omitempty"`
	Note                   string  `json:"note"`
}

type adjustmentDTO struct {
	ID                     string   `json:"id"`
	TenantID               string   `json:"tenant_id"`
	WalletID               string   `json:"wallet_id"`
	PlayerAccountID        string   `json:"player_account_id"`
	AssetCode              string   `json:"asset_code"`
	Direction              string   `json:"direction"`
	AmountMinorUnits       string   `json:"amount_minor_units"`
	ReasonCode             string   `json:"reason_code"`
	CausationTransactionID *string  `json:"causation_transaction_id,omitempty"`
	EvidenceRefHash        *string  `json:"evidence_ref_hash,omitempty"`
	PayloadHash            string   `json:"payload_hash"`
	InitiatedBy            string   `json:"initiated_by"`
	InitiatedByScope       string   `json:"initiated_by_scope"`
	RequiredAtSubmission   int      `json:"required_at_submission"`
	ContributingPolicyIDs  []string `json:"contributing_policy_ids"`
	TenantStatus           string   `json:"tenant_status_at_submission"`
	State                  string   `json:"state"`
	RefusalCode            *string  `json:"refusal_code,omitempty"`
	LedgerTransactionID    *string  `json:"ledger_transaction_id,omitempty"`
	CreatedAt              string   `json:"created_at"`
	ExpiresAt              string   `json:"expires_at"`
}

func toAdjustmentDTO(r adjustment.Request) adjustmentDTO {
	dto := adjustmentDTO{
		ID: r.ID.String(), TenantID: r.TenantID.String(), WalletID: r.WalletID.String(), PlayerAccountID: r.PlayerAccountID.String(),
		AssetCode: r.AssetCode, Direction: string(r.Direction), AmountMinorUnits: strconv.FormatInt(r.Amount, 10),
		ReasonCode: string(r.ReasonCode), EvidenceRefHash: r.EvidenceRefHash, PayloadHash: r.PayloadHash,
		InitiatedBy: r.InitiatedBy.String(), InitiatedByScope: r.InitiatedByScope, RequiredAtSubmission: r.RequiredAtSubmission,
		TenantStatus: r.TenantStatusAtSubmit, State: string(r.State), RefusalCode: r.RefusalCode,
		CreatedAt: r.CreatedAt.Format(time.RFC3339), ExpiresAt: r.ExpiresAt.Format(time.RFC3339),
	}
	for _, id := range r.ContributingPolicyIDs {
		dto.ContributingPolicyIDs = append(dto.ContributingPolicyIDs, id.String())
	}
	if r.CausationTransactionID != nil {
		s := r.CausationTransactionID.String()
		dto.CausationTransactionID = &s
	}
	if r.LedgerTransactionID != nil {
		s := r.LedgerTransactionID.String()
		dto.LedgerTransactionID = &s
	}
	return dto
}

// --- adjustment handlers ---

func newSubmitAdjustmentHandler(deps Deps, svc *adjustment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, "submit", true)
		if !ok {
			return
		}
		tg, ok := adjustmentTarget(w, c)
		if !ok {
			return
		}
		var body submitAdjustmentBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		walletID, err := uuid.Parse(body.WalletID)
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "wallet_id is required")
			return
		}
		// Integer minor units only, as a decimal string (never a float).
		amount, err := strconv.ParseInt(body.AmountMinorUnits, 10, 64)
		if err != nil || amount <= 0 {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "amount_minor_units must be a positive integer (int64)")
			return
		}
		in := adjustment.SubmitInput{
			WalletID: walletID, AssetCode: body.AssetCode, Direction: adjustment.Direction(body.Direction), Amount: amount,
			ReasonCode: adjustment.ReasonCode(body.ReasonCode), EvidenceRefHash: body.EvidenceRefHash, Note: body.Note,
		}
		if body.CausationTransactionID != nil {
			id, err := uuid.Parse(*body.CausationTransactionID)
			if err != nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid causation_transaction_id")
				return
			}
			in.CausationTransactionID = &id
		}
		req, err := svc.Submit(r.Context(), tg, in, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, "submit", err)
			return
		}
		writeJSON(w, http.StatusCreated, toAdjustmentDTO(req))
	}
}

func newListAdjustmentsHandler(deps Deps, svc *adjustment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, "list", true)
		if !ok {
			return
		}
		tg, ok := adjustmentTarget(w, c)
		if !ok {
			return
		}
		reqs, err := svc.List(r.Context(), tg, 100, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, "list", err)
			return
		}
		out := make([]adjustmentDTO, 0, len(reqs))
		for _, x := range reqs {
			out = append(out, toAdjustmentDTO(x))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func newGetAdjustmentHandler(deps Deps, svc *adjustment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, "get", true)
		if !ok {
			return
		}
		tg, ok := adjustmentTarget(w, c)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		req, err := svc.Get(r.Context(), tg, id, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, "get", err)
			return
		}
		writeJSON(w, http.StatusOK, toAdjustmentDTO(req))
	}
}

type decideAdjustmentBody struct {
	PayloadHash string `json:"payload_hash"`
	ReasonCode  string `json:"reason_code"`
}

type decisionResponse struct {
	Request  adjustmentDTO `json:"request"`
	Executed bool          `json:"executed"`
	Counted  int           `json:"counted_approvals"`
	Required int           `json:"required_approvals"`
}

func newDecideAdjustmentHandler(deps Deps, svc *adjustment.Service, decision adjustment.Decision) http.HandlerFunc {
	op := string(decision)
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, op, true)
		if !ok {
			return
		}
		tg, ok := adjustmentTarget(w, c)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body decideAdjustmentBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		out, err := svc.Decide(r.Context(), tg, id, adjustment.DecisionInput{Decision: decision, PayloadHash: body.PayloadHash, ReasonCode: body.ReasonCode}, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, op, err)
			return
		}
		writeJSON(w, http.StatusOK, decisionResponse{Request: toAdjustmentDTO(out.Request), Executed: out.Executed, Counted: out.Counted, Required: out.Required})
	}
}

func newCancelAdjustmentHandler(deps Deps, svc *adjustment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, "cancel", true)
		if !ok {
			return
		}
		tg, ok := adjustmentTarget(w, c)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		req, err := svc.Cancel(r.Context(), tg, id, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, "cancel", err)
			return
		}
		writeJSON(w, http.StatusOK, toAdjustmentDTO(req))
	}
}

// --- policy change handlers ---

type proposePolicyChangeBody struct {
	ChangeKind                      string     `json:"change_kind"`
	OperationKind                   string     `json:"operation_kind"`
	Level                           string     `json:"level"`
	BrandID                         *string    `json:"brand_id,omitempty"`
	JurisdictionID                  *string    `json:"jurisdiction_id,omitempty"`
	ProfileCode                     *string    `json:"profile_code,omitempty"`
	AssetCode                       *string    `json:"asset_code,omitempty"`
	BaseRequiredApprovals           *int       `json:"base_required_approvals,omitempty"`
	ThresholdMinorUnits             *string    `json:"threshold_minor_units,omitempty"`
	RequiredApprovalsAboveThreshold *int       `json:"required_approvals_above_threshold,omitempty"`
	EffectiveFrom                   *time.Time `json:"effective_from,omitempty"`
	LegalReviewReference            *string    `json:"legal_review_reference,omitempty"`
}

type policyChangeDTO struct {
	ID          string `json:"id"`
	ChangeKind  string `json:"change_kind"`
	ContentHash string `json:"content_hash"`
	Status      string `json:"status"`
	ExpiresAt   string `json:"expires_at"`
}

func toPolicyChangeDTO(c adjustment.PolicyChange) policyChangeDTO {
	return policyChangeDTO{ID: c.ID.String(), ChangeKind: c.ChangeKind, ContentHash: c.ContentHash, Status: c.Status, ExpiresAt: c.ExpiresAt.Format(time.RFC3339)}
}

func parseOptUUID(s *string) (*uuid.UUID, bool) {
	if s == nil {
		return nil, true
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return nil, false
	}
	return &id, true
}

func newProposePolicyChangeHandler(deps Deps, svc *adjustment.Service, tenantRoute bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, "policy_change.propose", tenantRoute)
		if !ok {
			return
		}
		var body proposePolicyChangeBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		brand, ok1 := parseOptUUID(body.BrandID)
		jur, ok2 := parseOptUUID(body.JurisdictionID)
		if !ok1 || !ok2 {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid id")
			return
		}
		in := adjustment.PolicyChangeInput{
			ChangeKind: body.ChangeKind, OperationKind: body.OperationKind, Level: body.Level, BrandID: brand, JurisdictionID: jur,
			ProfileCode: body.ProfileCode, AssetCode: body.AssetCode, BaseRequiredApprovals: body.BaseRequiredApprovals,
			ThresholdMinorUnits: body.ThresholdMinorUnits, RequiredApprovalsAboveThreshold: body.RequiredApprovalsAboveThreshold,
			EffectiveFrom: body.EffectiveFrom, LegalReviewReference: body.LegalReviewReference,
		}
		if tenantRoute {
			// The tenant is the canActOnTenant-validated path value, never
			// a body field (a tenant caller's is re-forced in the service).
			target := c.target
			in.TenantID = &target
		} else if body.Level == adjustment.LevelTenant || body.Level == adjustment.LevelBrand || body.ChangeKind == adjustment.ChangeKindProfileAssignment {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "tenant, brand and profile-assignment changes use the tenant route")
			return
		}
		ch, err := svc.ProposePolicyChange(r.Context(), in, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, "policy_change.propose", err)
			return
		}
		writeJSON(w, http.StatusCreated, toPolicyChangeDTO(ch))
	}
}

type decidePolicyChangeBody struct {
	ContentHash string `json:"content_hash"`
	ReasonCode  string `json:"reason_code"`
}

func newDecidePolicyChangeHandler(deps Deps, svc *adjustment.Service, decision adjustment.Decision) http.HandlerFunc {
	op := "policy_change." + string(decision)
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, op, false)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("changeID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		var body decidePolicyChangeBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		ch, err := svc.DecidePolicyChange(r.Context(), id, decision, body.ContentHash, body.ReasonCode, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, op, err)
			return
		}
		writeJSON(w, http.StatusOK, toPolicyChangeDTO(ch))
	}
}

func newCancelPolicyChangeHandler(deps Deps, svc *adjustment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, "policy_change.cancel", false)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("changeID"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		ch, err := svc.CancelPolicyChange(r.Context(), id, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, "policy_change.cancel", err)
			return
		}
		writeJSON(w, http.StatusOK, toPolicyChangeDTO(ch))
	}
}

type policyDTO struct {
	ID                              string  `json:"id"`
	OperationKind                   string  `json:"operation_kind"`
	Level                           string  `json:"level"`
	TenantID                        *string `json:"tenant_id,omitempty"`
	AssetCode                       *string `json:"asset_code,omitempty"`
	BaseRequiredApprovals           int     `json:"base_required_approvals"`
	ThresholdMinorUnits             *string `json:"threshold_minor_units,omitempty"`
	RequiredApprovalsAboveThreshold *int    `json:"required_approvals_above_threshold,omitempty"`
	EffectiveFrom                   string  `json:"effective_from"`
}

func newListPoliciesHandler(deps Deps, svc *adjustment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginAdjustmentCall(deps, w, r, "policy.list", false)
		if !ok {
			return
		}
		op := r.URL.Query().Get("operation_kind")
		if op == "" {
			op = adjustment.OperationKind
		}
		ps, err := svc.ListPolicies(r.Context(), op, c.meta())
		if err != nil {
			writeAdjustmentError(r.Context(), deps, w, c, "policy.list", err)
			return
		}
		out := make([]policyDTO, 0, len(ps))
		for _, p := range ps {
			d := policyDTO{ID: p.ID.String(), OperationKind: p.OperationKind, Level: p.Level, AssetCode: p.AssetCode,
				BaseRequiredApprovals: p.BaseRequiredApprovals, ThresholdMinorUnits: p.ThresholdMinorUnits,
				RequiredApprovalsAboveThreshold: p.RequiredApprovalsAboveThreshold, EffectiveFrom: p.EffectiveFrom.Format(time.RFC3339)}
			if p.TenantID != nil {
				s := p.TenantID.String()
				d.TenantID = &s
			}
			out = append(out, d)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}
