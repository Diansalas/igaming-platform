// Stage 4H-B0-R6, Workstream A: the admin API for ADR 0037's Asset
// Registry (layers 1-3) and Asset Authorization (layers 4-7).
//
// Two conventions in this file are load-bearing rather than stylistic:
//
//  1. Layer-1-3 handlers run under deps.DB.WithPlatformAdmin, which sets
//     the app.platform_admin_principal_id GUC migration 0044's RLS
//     policies require and leaves app.tenant_id unset. Layer-4-7 handlers
//     run under deps.DB.WithTenant with the tenant from the verified
//     token. Neither ever reads a tenant identifier out of a request
//     body - internal/assetregistry independently re-checks the tenant
//     argument against the transaction's own GUC and fails closed on a
//     mismatch (ErrTenantContextMismatch).
//
//  2. Every mutating handler passes a reason_code through to the service,
//     which requires it (CLAUDE.md: "actor, tenant, entity, before/after
//     state, IP, reason code"). A missing reason_code is a 400, not a
//     silently-empty audit field.
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

type assetResponse struct {
	Code               string `json:"code"`
	AssetType          string `json:"asset_type"`
	DecimalExponent    int16  `json:"decimal_exponent"`
	DisplayName        string `json:"display_name"`
	Network            string `json:"network,omitempty"`
	Active             bool   `json:"active"`
	PlatformAuthorized bool   `json:"platform_authorized"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

func toAssetResponse(a assetregistry.Asset) assetResponse {
	return assetResponse{
		Code: a.Code, AssetType: a.AssetType, DecimalExponent: a.DecimalExponent,
		DisplayName: a.DisplayName, Network: a.Network, Active: a.Active,
		PlatformAuthorized: a.PlatformAuthorized,
		CreatedAt:          a.CreatedAt.Format(rfc3339), UpdatedAt: a.UpdatedAt.Format(rfc3339),
	}
}

// actorFromRequest resolves the acting principal from the VERIFIED token's
// subject - never from a header, query or body field.
func actorFromRequest(r *http.Request, reasonCode string) (assetregistry.ActorContext, error) {
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		return assetregistry.ActorContext{}, err
	}
	actorID, err := uuid.Parse(tc.Subject)
	if err != nil {
		return assetregistry.ActorContext{}, err
	}
	return assetregistry.ActorContext{
		ActorID: actorID, IPAddress: clientIP(r), UserAgent: r.UserAgent(),
		RequestID: observability.RequestIDFromContext(r.Context()), ReasonCode: reasonCode,
	}, nil
}

// writeAssetRegistryError maps this package's sentinels onto HTTP codes so
// a four-eyes refusal or a narrowing violation is never reported as a
// generic 500 (which would make an operator think the platform was
// broken rather than that a control had correctly refused).
func writeAssetRegistryError(w http.ResponseWriter, requestID string, logger interface {
	Error(msg string, args ...any)
}, action string, err error) {
	switch {
	case errors.Is(err, assetregistry.ErrInvalidInput):
		apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
	case errors.Is(err, assetregistry.ErrNotFound):
		apierror.Write(w, requestID, apierror.CodeNotFound, err.Error())
	case errors.Is(err, assetregistry.ErrDualControlRequired),
		errors.Is(err, assetregistry.ErrSelfApproval),
		// A duplicate decision is a 409 for the same reason a
		// self-approval is - the request state already accounts for this
		// principal - but it is a DISTINCT sentinel, so the response body
		// says "you already decided this" rather than accusing a retrying
		// operator of self-dealing (code-reviewer finding F9).
		errors.Is(err, assetregistry.ErrDuplicateDecision):
		apierror.Write(w, requestID, apierror.CodeConflict, err.Error())
	// The principal itself is not eligible to request/decide (not
	// platform-scoped, no confirmed Person linkage, or not active -
	// migration 0047). That is a property of the actor, not of the
	// payload or of the request's state, so 403.
	case errors.Is(err, assetregistry.ErrApproverNotEligible):
		apierror.Write(w, requestID, apierror.CodeForbidden, err.Error())
	case errors.Is(err, assetregistry.ErrWidensPlatformAuthorization):
		apierror.Write(w, requestID, apierror.CodeForbidden, err.Error())
	case errors.Is(err, assetregistry.ErrTenantContextMismatch):
		apierror.Write(w, requestID, apierror.CodeTenantMismatch, err.Error())
	default:
		logger.Error(action+"_failed", "error", err)
		apierror.Write(w, requestID, apierror.CodeInternal, "asset registry operation failed")
	}
}

// --- Layers 1-3 (platform-admin only) ---

func newListAssetsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		actor, err := actorFromRequest(r, "list")
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var assets []assetregistry.Asset
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			assets, err = assetregistry.ListAssets(ctx, tx)
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "list_assets", err)
			return
		}
		resp := make([]assetResponse, 0, len(assets))
		for _, a := range assets {
			resp = append(resp, toAssetResponse(a))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type fileAssetChangeRequestBody struct {
	Operation       string `json:"operation"`
	AssetCode       string `json:"asset_code"`
	AssetType       string `json:"asset_type,omitempty"`
	DecimalExponent int16  `json:"decimal_exponent,omitempty"`
	Network         string `json:"network,omitempty"`
	// Required only for operation = platform_operation_eligibility (ADR
	// 0037 §C.5.1 op 9's GRANT direction, dual-controlled since migration
	// 0047). eligibility_product may be omitted, which means EVERY
	// product - recorded explicitly in the approved payload rather than
	// left absent, so the approver approves the breadth too.
	EligibilityOperation string `json:"eligibility_operation,omitempty"`
	EligibilityProduct   string `json:"eligibility_product,omitempty"`
	ReasonCode           string `json:"reason_code"`
}

func newFileAssetChangeRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var body fileAssetChangeRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("asset_code", body.AssetCode)
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		// Built from assetregistry.ChangeOperations() rather than a
		// hand-listed set, so this allowlist cannot drift from the
		// service's own definition (and from migration 0047's CHECK) the
		// next time one changes.
		ops := assetregistry.ChangeOperations()
		allowedOps := make([]string, 0, len(ops))
		for _, op := range ops {
			allowedOps = append(allowedOps, string(op))
		}
		v.RequireOneOf("operation", body.Operation, allowedOps...)
		if assetregistry.ChangeOperation(body.Operation) == assetregistry.ChangePlatformOperationEligibility {
			eligibilityOps := assetregistry.Operations()
			allowedEligibility := make([]string, 0, len(eligibilityOps))
			for _, op := range eligibilityOps {
				allowedEligibility = append(allowedEligibility, string(op))
			}
			v.RequireOneOf("eligibility_operation", body.EligibilityOperation, allowedEligibility...)
		} else if body.EligibilityOperation != "" || body.EligibilityProduct != "" {
			// Refused rather than ignored: silently dropping fields an
			// approver may have read in the request body is exactly how a
			// four-eyes approval ends up describing something other than
			// what gets applied.
			v.Add("eligibility_operation", "is only valid for operation=platform_operation_eligibility")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var req assetregistry.ChangeRequest
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			req, err = assetregistry.FileChangeRequest(ctx, tx, assetregistry.FileChangeRequestParams{
				Operation: assetregistry.ChangeOperation(body.Operation), AssetCode: body.AssetCode,
				AssetType: body.AssetType, DecimalExponent: body.DecimalExponent, Network: body.Network,
				EligibilityOperation: assetregistry.Operation(body.EligibilityOperation),
				EligibilityProduct:   body.EligibilityProduct,
				Actor:                actor,
			})
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "file_asset_change_request", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": req.ID.String(), "operation": string(req.Operation), "asset_code": req.AssetCode,
			"state": req.State, "requested_at": req.RequestedAt.Format(rfc3339),
		})
	}
}

type decideAssetChangeRequestBody struct {
	Decision   string `json:"decision"`
	ReasonCode string `json:"reason_code"`
}

func newDecideAssetChangeRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		reqID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid change request id")
			return
		}
		var body decideAssetChangeRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("decision", body.Decision, "approve", "reject")
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var ap assetregistry.Approval
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ap, err = assetregistry.DecideChangeRequest(ctx, tx, assetregistry.DecideChangeRequestParams{
				RequestID: reqID, Approve: body.Decision == "approve", Actor: actor,
			})
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "decide_asset_change_request", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": ap.ID.String(), "request_id": ap.RequestID.String(),
			"decision": ap.Decision, "decided_at": ap.DecidedAt.Format(rfc3339),
		})
	}
}

type createAssetBody struct {
	Code            string `json:"code"`
	AssetType       string `json:"asset_type"`
	DecimalExponent int16  `json:"decimal_exponent"`
	DisplayName     string `json:"display_name"`
	Network         string `json:"network,omitempty"`
	ReasonCode      string `json:"reason_code"`
	// There is deliberately NO `active` or `platform_authorized` field
	// here. ADR 0037 §C.5.5: an API that let creation accept either would
	// "silently reopen exactly the gap this whole ADR exists to close",
	// and migration 0044's trigger refuses such an insert regardless.
}

func newCreateAssetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var body createAssetBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("code", body.Code)
		v.RequireNonEmpty("display_name", body.DisplayName)
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		v.RequireOneOf("asset_type", body.AssetType, assetregistry.AssetTypeFiat, assetregistry.AssetTypeCrypto)
		if body.DecimalExponent < 0 || body.DecimalExponent > 18 {
			v.Add("decimal_exponent", "must be between 0 and 18")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var created assetregistry.Asset
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = assetregistry.CreateAsset(ctx, tx, assetregistry.CreateAssetParams{
				Code: body.Code, AssetType: body.AssetType, DecimalExponent: body.DecimalExponent,
				DisplayName: body.DisplayName, Network: body.Network, Actor: actor,
			})
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "create_asset", err)
			return
		}
		writeJSON(w, http.StatusCreated, toAssetResponse(created))
	}
}

type updateAssetMetadataBody struct {
	DisplayName string `json:"display_name"`
	ReasonCode  string `json:"reason_code"`
}

func newUpdateAssetMetadataHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		code := r.PathValue("code")
		var body updateAssetMetadataBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("display_name", body.DisplayName)
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var updated assetregistry.Asset
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = assetregistry.UpdateAssetMetadata(ctx, tx, code, body.DisplayName, actor)
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "update_asset_metadata", err)
			return
		}
		writeJSON(w, http.StatusOK, toAssetResponse(updated))
	}
}

type assetFlagBody struct {
	Value      *bool  `json:"value"`
	ReasonCode string `json:"reason_code"`
}

func newSetAssetActivationHandler(deps Deps) http.HandlerFunc {
	return newAssetFlagHandler(deps, "set_asset_activation", func(ctx context.Context, tx pgx.Tx, code string, value bool, actor assetregistry.ActorContext) (assetregistry.Asset, error) {
		return assetregistry.SetActive(ctx, tx, code, value, actor)
	})
}

func newSetAssetPlatformAuthorizationHandler(deps Deps) http.HandlerFunc {
	return newAssetFlagHandler(deps, "set_asset_platform_authorization", func(ctx context.Context, tx pgx.Tx, code string, value bool, actor assetregistry.ActorContext) (assetregistry.Asset, error) {
		return assetregistry.SetPlatformAuthorized(ctx, tx, code, value, actor)
	})
}

func newAssetFlagHandler(deps Deps, action string,
	apply func(context.Context, pgx.Tx, string, bool, assetregistry.ActorContext) (assetregistry.Asset, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		code := r.PathValue("code")
		var body assetFlagBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if body.Value == nil {
			// Required explicitly rather than defaulting: defaulting a
			// missing boolean would make "grant" reachable by omission,
			// which is precisely the fail-open shape finding S-4 was.
			v.Add("value", "is required (true to grant, false to revoke)")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var updated assetregistry.Asset
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = apply(ctx, tx, code, *body.Value, actor)
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, action, err)
			return
		}
		writeJSON(w, http.StatusOK, toAssetResponse(updated))
	}
}

type operationEligibilityBody struct {
	Product    string `json:"product,omitempty"`
	Operation  string `json:"operation"`
	Eligible   *bool  `json:"eligible"`
	ReasonCode string `json:"reason_code"`
}

func (b operationEligibilityBody) validate() *validation.Errors {
	v := validation.New()
	v.RequireNonEmpty("reason_code", b.ReasonCode)
	ops := assetregistry.Operations()
	allowed := make([]string, 0, len(ops))
	for _, op := range ops {
		allowed = append(allowed, string(op))
	}
	v.RequireOneOf("operation", b.Operation, allowed...)
	if b.Eligible == nil {
		v.Add("eligible", "is required (true or false)")
	}
	return v
}

// newSetPlatformOperationEligibilityHandler writes the PLATFORM-WIDE
// layer-7 default (tenant_id NULL) - platform-admin only, per ADR 0037
// §C.5.1 op 9's split.
//
// GRANTING (eligible = true) additionally requires an approved
// platform_operation_eligibility change request for this exact
// (asset, operation, product), filed and decided through the same two
// change-request endpoints create/activate/platform_authorize use. An
// attempt without one is a 409 (ErrDualControlRequired), not a 500 and
// certainly not a success: this one row is a platform-wide gate for every
// tenant, and until migration 0047 a single credential could flip it.
// Revoking (eligible = false) needs no approval, deliberately.
func newSetPlatformOperationEligibilityHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		code := r.PathValue("code")
		var body operationEligibilityBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if v := body.validate(); v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var row assetregistry.OperationEligibility
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			row, err = assetregistry.ConfigureOperationEligibility(ctx, tx, assetregistry.ConfigureEligibilityParams{
				AssetCode: code, Product: body.Product,
				Operation: assetregistry.Operation(body.Operation), Eligible: *body.Eligible, Actor: actor,
			})
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "set_platform_operation_eligibility", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": row.ID.String(), "asset_code": row.AssetCode, "product": row.Product,
			"operation": string(row.Operation), "eligible": row.Eligible, "scope": "platform",
		})
	}
}

// --- Layers 4-7 (tenant-scoped) ---

func newListAssetAuthorizationsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var rows []assetregistry.Authorization
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rows, err = assetregistry.ListAuthorizations(ctx, tx, r.URL.Query().Get("asset_code"))
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "list_asset_authorizations", err)
			return
		}
		resp := make([]map[string]any, 0, len(rows))
		for _, a := range rows {
			item := map[string]any{
				"id": a.ID.String(), "scope_kind": string(a.ScopeKind), "asset_code": a.AssetCode,
				"product": a.Product, "eligible": a.Eligible, "reason_code": a.ReasonCode,
			}
			if a.BrandID != nil {
				item["brand_id"] = a.BrandID.String()
			}
			if a.JurisdictionID != nil {
				item["jurisdiction_id"] = a.JurisdictionID.String()
			}
			resp = append(resp, item)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type authorizeScopeBody struct {
	AssetCode string `json:"asset_code"`
	// BrandID/JurisdictionID name WHICH brand/jurisdiction is being
	// configured (a configuration target, not caller identity - see
	// assetregistry.AuthorizeScopeParams' own doc comment). The TENANT is
	// never accepted from the body: it comes from the verified token, and
	// the service re-checks it against the transaction's GUC.
	BrandID        string `json:"brand_id,omitempty"`
	JurisdictionID string `json:"jurisdiction_id,omitempty"`
	Product        string `json:"product,omitempty"`
	Eligible       *bool  `json:"eligible"`
	ReasonCode     string `json:"reason_code"`
}

func newAuthorizeAssetScopeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		scopeKind := assetregistry.ScopeKind(r.PathValue("scopeKind"))
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var body authorizeScopeBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("scope_kind", string(scopeKind),
			string(assetregistry.ScopeTenant), string(assetregistry.ScopeBrand), string(assetregistry.ScopeJurisdiction))
		v.RequireNonEmpty("asset_code", body.AssetCode)
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if body.Eligible == nil {
			v.Add("eligible", "is required (true or false)")
		}
		var brandID, jurisdictionID uuid.UUID
		if body.BrandID != "" {
			if parsed, err := uuid.Parse(body.BrandID); err != nil {
				v.Add("brand_id", "must be a valid UUID")
			} else {
				brandID = parsed
			}
		}
		if body.JurisdictionID != "" {
			if parsed, err := uuid.Parse(body.JurisdictionID); err != nil {
				v.Add("jurisdiction_id", "must be a valid UUID")
			} else {
				jurisdictionID = parsed
			}
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var row assetregistry.Authorization
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			row, err = assetregistry.AuthorizeScope(ctx, tx, assetregistry.AuthorizeScopeParams{
				TenantID: tc.TenantID, ScopeKind: scopeKind, BrandID: brandID, JurisdictionID: jurisdictionID,
				AssetCode: body.AssetCode, Product: body.Product, Eligible: *body.Eligible, Actor: actor,
			})
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "authorize_asset_scope", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": row.ID.String(), "scope_kind": string(row.ScopeKind), "asset_code": row.AssetCode,
			"product": row.Product, "eligible": row.Eligible,
		})
	}
}

// newSetTenantOperationEligibilityHandler writes a TENANT override of the
// platform-wide layer-7 default. It can only ever narrow: migration
// 0045's trigger rejects a widening row at write time, which surfaces
// here as 403 rather than 500.
func newSetTenantOperationEligibilityHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		code := r.PathValue("code")
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var body operationEligibilityBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if v := body.validate(); v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := actorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var row assetregistry.OperationEligibility
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			row, err = assetregistry.ConfigureOperationEligibility(ctx, tx, assetregistry.ConfigureEligibilityParams{
				TenantID: tc.TenantID, AssetCode: code, Product: body.Product,
				Operation: assetregistry.Operation(body.Operation), Eligible: *body.Eligible, Actor: actor,
			})
			return err
		})
		if err != nil {
			writeAssetRegistryError(w, requestID, logger, "set_tenant_operation_eligibility", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": row.ID.String(), "asset_code": row.AssetCode, "product": row.Product,
			"operation": string(row.Operation), "eligible": row.Eligible, "scope": "tenant",
		})
	}
}
