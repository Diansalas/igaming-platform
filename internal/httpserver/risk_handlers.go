// Stage 4G: the minimum admin API for Risk & Limits rule management.
// Gated by PermRiskConfigRead (read) / PermRiskConfigManage
// (create/disable) - see internal/auth/permission.go's own doc comments
// for the separation-of-duties rationale (RoleRiskManager only for
// manage; RoleRiskManager + RoleTenantAdmin for read).
//
// Every rule created through THIS API is tenant-scoped (tx runs under
// db.WithTenant) - a genuinely platform-wide rule (tenant_id NULL,
// enforced across every tenant, e.g. a jurisdiction-wide legal ceiling)
// is schema/evaluator-supported (migration 0041, internal/risk) but has
// no HTTP write path this stage, mirroring internal/rg.
// CreateStaffRestriction's identical, already-established precedent:
// "no role/permission grants a platform-scoped write path for this kind
// of row yet" - see that function's own doc comment. Recorded as an open
// decision in docs/decisions/0031 §8, not silently assumed unreachable.
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

type riskRuleResponse struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id,omitempty"`
	BrandID          string `json:"brand_id,omitempty"`
	JurisdictionCode string `json:"jurisdiction_code,omitempty"`
	LicensingMode    string `json:"licensing_mode,omitempty"`
	PlayerAccountID  string `json:"player_account_id,omitempty"`
	Product          string `json:"product,omitempty"`
	Operation        string `json:"operation"`
	ProviderID       string `json:"provider_id,omitempty"`
	GameID           string `json:"game_id,omitempty"`
	AssetCode        string `json:"asset_code,omitempty"`
	PaymentMethod    string `json:"payment_method,omitempty"`
	LimitKind        string `json:"limit_kind"`
	TimeWindow       string `json:"time_window"`
	Threshold        int64  `json:"threshold"`
	// Which asset exponent's minor units Threshold is expressed in, for
	// an asset-agnostic amount rule; omitted for an asset-scoped rule,
	// whose denomination is its own asset_code (ADR 0031 §35).
	ThresholdExponent *int16 `json:"threshold_exponent,omitempty"`
	RuleKind          string `json:"rule_kind"`
	Action            string `json:"action"`
	Status            string `json:"status"`
	EffectiveFrom     string `json:"effective_from"`
	EffectiveUntil    string `json:"effective_until,omitempty"`
	Description       string `json:"description,omitempty"`
	CreatedAt         string `json:"created_at"`
}

func toRiskRuleResponse(r risk.Rule) riskRuleResponse {
	resp := riskRuleResponse{
		ID: r.ID.String(), JurisdictionCode: r.JurisdictionCode, LicensingMode: r.LicensingMode, Product: r.Product, Operation: string(r.Operation),
		ProviderID: r.ProviderID, AssetCode: r.AssetCode, PaymentMethod: r.PaymentMethod,
		LimitKind: string(r.LimitKind), TimeWindow: string(r.TimeWindow), Threshold: r.Threshold, ThresholdExponent: r.ThresholdExponent,
		RuleKind: string(r.RuleKind), Action: string(r.Action), Status: string(r.Status),
		EffectiveFrom: r.EffectiveFrom.Format(rfc3339), Description: r.Description, CreatedAt: r.CreatedAt.Format(rfc3339),
	}
	if r.TenantID != nil {
		resp.TenantID = r.TenantID.String()
	}
	if r.BrandID != nil {
		resp.BrandID = r.BrandID.String()
	}
	if r.PlayerAccountID != nil {
		resp.PlayerAccountID = r.PlayerAccountID.String()
	}
	if r.GameID != nil {
		resp.GameID = r.GameID.String()
	}
	if r.EffectiveUntil != nil {
		resp.EffectiveUntil = r.EffectiveUntil.Format(rfc3339)
	}
	return resp
}

func newListRiskRulesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		operation := r.URL.Query().Get("operation")
		v := validation.New()
		v.RequireNonEmpty("operation", operation)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var rules []risk.Rule
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rules, err = risk.ListRulesForOperation(ctx, tx, risk.Operation(operation))
			return err
		})
		if err != nil {
			logger.Error("list_risk_rules_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list risk rules")
			return
		}
		resp := make([]riskRuleResponse, 0, len(rules))
		for _, rule := range rules {
			resp = append(resp, toRiskRuleResponse(rule))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type createRiskRuleRequest struct {
	BrandID          string `json:"brand_id"`
	JurisdictionCode string `json:"jurisdiction_code"`
	LicensingMode    string `json:"licensing_mode"`
	PlayerAccountID  string `json:"player_account_id"`
	Product          string `json:"product"`
	Operation        string `json:"operation"`
	ProviderID       string `json:"provider_id"`
	GameID           string `json:"game_id"`
	AssetCode        string `json:"asset_code"`
	PaymentMethod    string `json:"payment_method"`
	LimitKind        string `json:"limit_kind"`
	TimeWindow       string `json:"time_window"`
	Threshold        int64  `json:"threshold"`
	// REQUIRED for an amount-shaped rule that leaves asset_code empty:
	// which asset exponent's minor units `threshold` is expressed in.
	// Must be OMITTED when asset_code is set, since the asset itself is
	// then the threshold's denomination, resolved from the `assets`
	// registry at evaluation time. A wildcard-asset rule authored at one
	// exponent fails CLOSED for an asset of any other exponent rather
	// than being silently re-denominated (ADR 0031 §35, closing §32(d)).
	ThresholdExponent *int16 `json:"threshold_exponent"`
	RuleKind          string `json:"rule_kind"`
	Action            string `json:"action"`
	Description       string `json:"description"`
}

func newCreateRiskRuleHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		actorID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var req createRiskRuleRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("operation", req.Operation,
			string(risk.OperationCasinoLaunch), string(risk.OperationCasinoBet), string(risk.OperationDeposit),
			string(risk.OperationWithdrawal), string(risk.OperationSportsbookBet), string(risk.OperationBonusGrant),
			string(risk.OperationBonusConversion))
		v.RequireOneOf("limit_kind", req.LimitKind, string(risk.LimitMinAmount), string(risk.LimitMaxAmount), string(risk.LimitCumulativeAmount))
		v.RequireOneOf("time_window", req.TimeWindow,
			string(risk.WindowTransaction), string(risk.WindowRollingHour), string(risk.WindowRollingDay),
			string(risk.WindowRollingWeek), string(risk.WindowRollingMonth))
		if req.RuleKind != "" {
			v.RequireOneOf("rule_kind", req.RuleKind, string(risk.RuleHardLimit), string(risk.RuleConfigurableLimit), string(risk.RuleRiskSignal))
		}
		if req.Action != "" {
			v.RequireOneOf("action", req.Action, string(risk.ActionDeny), string(risk.ActionReview))
		}
		if req.Product != "" {
			v.RequireOneOf("product", req.Product, "casino", "sportsbook", "payments", "bonus")
		}
		if req.LicensingMode != "" {
			v.RequireOneOf("licensing_mode", req.LicensingMode, "under_platform_licence", "own_licence")
		}
		if req.Threshold < 0 {
			v.Add("threshold", "must be non-negative")
		}
		// Denomination validation mirrors risk.CreateRule's own (which
		// mirrors migration 0046's CHECK) so an operator gets a field-level
		// 400 rather than a generic one. Amount-shaped limit kinds are the
		// only ones that carry a threshold denomination at all.
		if req.ThresholdExponent != nil && (*req.ThresholdExponent < 0 || *req.ThresholdExponent > 18) {
			v.Add("threshold_exponent", "must be between 0 and 18")
		}
		if req.AssetCode == "" && req.ThresholdExponent == nil {
			v.Add("threshold_exponent", "required when asset_code is omitted: state which asset exponent the threshold's minor units are expressed in")
		}
		if req.AssetCode != "" && req.ThresholdExponent != nil {
			v.Add("threshold_exponent", "must be omitted when asset_code is set - the asset is already the threshold's denomination")
		}
		var brandID, playerAccountID, gameID *uuid.UUID
		if req.BrandID != "" {
			parsed, err := uuid.Parse(req.BrandID)
			if err != nil {
				v.Add("brand_id", "must be a valid UUID")
			} else {
				brandID = &parsed
			}
		}
		if req.PlayerAccountID != "" {
			parsed, err := uuid.Parse(req.PlayerAccountID)
			if err != nil {
				v.Add("player_account_id", "must be a valid UUID")
			} else {
				playerAccountID = &parsed
			}
		}
		if req.GameID != "" {
			parsed, err := uuid.Parse(req.GameID)
			if err != nil {
				v.Add("game_id", "must be a valid UUID")
			} else {
				gameID = &parsed
			}
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var created risk.Rule
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = risk.CreateRule(ctx, tx, risk.CreateRuleParams{
				TenantID: &tc.TenantID, BrandID: brandID, JurisdictionCode: req.JurisdictionCode, LicensingMode: req.LicensingMode,
				PlayerAccountID: playerAccountID, Product: req.Product, Operation: risk.Operation(req.Operation),
				ProviderID: req.ProviderID, GameID: gameID, AssetCode: req.AssetCode, PaymentMethod: req.PaymentMethod,
				LimitKind: risk.LimitKind(req.LimitKind), TimeWindow: risk.TimeWindow(req.TimeWindow), Threshold: req.Threshold,
				ThresholdExponent: req.ThresholdExponent,
				RuleKind:          risk.RuleKind(req.RuleKind), Action: risk.RuleAction(req.Action), Description: req.Description,
				CreatedByActorType: "staff", CreatedByActorID: actorID,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if errors.Is(err, risk.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("create_risk_rule_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create risk rule")
			return
		}
		writeJSON(w, http.StatusCreated, toRiskRuleResponse(created))
	}
}

func newDisableRiskRuleHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		actorID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}
		ruleID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid rule id")
			return
		}

		var disabled risk.Rule
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			disabled, err = risk.DisableRule(ctx, tx, risk.DisableRuleParams{
				RuleID: ruleID, ActorType: "staff", ActorID: actorID,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if errors.Is(err, risk.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "risk rule not found or already disabled")
			return
		}
		if err != nil {
			logger.Error("disable_risk_rule_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to disable risk rule")
			return
		}
		writeJSON(w, http.StatusOK, toRiskRuleResponse(disabled))
	}
}
