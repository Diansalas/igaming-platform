// Stage 9.2 Part B2 / Wave 3 (ADR 0083 §6.2.3/§9.2): the TENANT-scoped
// HTTP admin surface for sb_exposure_limits - create/disable/list.
// Modelled on risk_handlers.go's shape exactly (see internal/sportsbook/
// exposure_admin.go's own doc comment for the full precedent rationale) -
// UNLIKE sportsbook_jurisdiction_admin_handlers.go's platform-admin-only,
// WithPlatformAdmin/WithoutTenant shape, this table IS tenant-owned
// configuration, so every handler here runs under
// deps.DB.WithTenant(tc.TenantID, ...) and is RequireTenantScope-wrapped,
// exactly like registerRiskRoutes.
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

type exposureLimitResponse struct {
	ID                     string `json:"id"`
	TenantID               string `json:"tenant_id"`
	BrandID                string `json:"brand_id,omitempty"`
	ScopeKind              string `json:"scope_kind"`
	AssetCode              string `json:"asset_code"`
	MaxOpenPotentialPayout int64  `json:"max_open_potential_payout"`
	// Measure states, on the record, which figure
	// max_open_potential_payout is denominated in (ADR 0083 §6.2.2's own
	// disclosure requirement: "the admin surface must state on the record
	// which measure the configured number is denominated in, so a human
	// setting it is not guessing") - always "gross_potential_payout" today
	// (stake included, never netted), a constant rather than a
	// configurable field, since §6.2.2 defines exactly one measure this
	// stage.
	Measure                string `json:"measure"`
	Status                 string `json:"status"`
	AuthorizationReference string `json:"authorization_reference"`
	ReasonCode             string `json:"reason_code"`
	CreatedByActorID       string `json:"created_by_actor_id"`
	CreatedAt              string `json:"created_at"`
	UpdatedAt              string `json:"updated_at"`
}

func toExposureLimitResponse(l sportsbook.ExposureLimit) exposureLimitResponse {
	resp := exposureLimitResponse{
		ID: l.ID.String(), TenantID: l.TenantID.String(), ScopeKind: l.ScopeKind, AssetCode: l.AssetCode,
		MaxOpenPotentialPayout: l.MaxOpenPotentialPayout, Measure: "gross_potential_payout", Status: l.Status,
		AuthorizationReference: l.AuthorizationReference, ReasonCode: l.ReasonCode, CreatedByActorID: l.CreatedByActorID.String(),
		CreatedAt: l.CreatedAt.Format(rfc3339), UpdatedAt: l.UpdatedAt.Format(rfc3339),
	}
	if l.BrandID != nil {
		resp.BrandID = l.BrandID.String()
	}
	return resp
}

type createExposureLimitRequest struct {
	BrandID                string `json:"brand_id,omitempty"`
	ScopeKind              string `json:"scope_kind"`
	AssetCode              string `json:"asset_code"`
	MaxOpenPotentialPayout int64  `json:"max_open_potential_payout"`
	AuthorizationReference string `json:"authorization_reference"`
	ReasonCode             string `json:"reason_code"`
}

// newCreateSportsbookExposureLimitHandler arms a new exposure ceiling for
// this tenant. Requires sportsbook_exposure_limit:manage (RoleRiskManager
// only) and RequireTenantScope.
func newCreateSportsbookExposureLimitHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

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

		var req createExposureLimitRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("scope_kind", req.ScopeKind, "event", "market", "selection")
		v.RequireNonEmpty("asset_code", req.AssetCode)
		v.RequireNonEmpty("authorization_reference", req.AuthorizationReference)
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		if req.MaxOpenPotentialPayout <= 0 {
			v.Add("max_open_potential_payout", "must be a positive integer (minor units, gross potential payout)")
		}
		var brandID *uuid.UUID
		if req.BrandID != "" {
			parsed, err := uuid.Parse(req.BrandID)
			if err != nil {
				v.Add("brand_id", "must be a valid UUID")
			} else {
				brandID = &parsed
			}
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var created sportsbook.ExposureLimit
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = sportsbook.CreateExposureLimit(ctx, tx, sportsbook.CreateExposureLimitParams{
				TenantID: tc.TenantID, BrandID: brandID, ScopeKind: req.ScopeKind, AssetCode: req.AssetCode,
				MaxOpenPotentialPayout: req.MaxOpenPotentialPayout,
				AuthorizationReference: req.AuthorizationReference, ReasonCode: req.ReasonCode,
				CreatedByActorID: actorID, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if errors.Is(err, sportsbook.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if db.IsUniqueViolation(err) {
			apierror.Write(w, requestID, apierror.CodeConflict, "an active exposure limit already exists for this (brand, scope_kind, asset_code) - disable it first")
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "unknown asset_code or brand_id")
			return
		}
		if err != nil {
			logger.Error("create_sportsbook_exposure_limit_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create exposure limit")
			return
		}
		writeJSON(w, http.StatusCreated, toExposureLimitResponse(created))
	}
}

type disableExposureLimitRequest struct {
	ReasonCode string `json:"reason_code"`
}

// newDisableSportsbookExposureLimitHandler disables (never edits or
// deletes) an active exposure limit.
func newDisableSportsbookExposureLimitHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

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
		limitID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid exposure limit id")
			return
		}

		var req disableExposureLimitRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var disabled sportsbook.ExposureLimit
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			disabled, err = sportsbook.DisableExposureLimit(ctx, tx, sportsbook.DisableExposureLimitParams{
				ID: limitID, ReasonCode: req.ReasonCode, ActorID: actorID,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if errors.Is(err, sportsbook.ErrExposureLimitNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "exposure limit not found or already disabled")
			return
		}
		if errors.Is(err, sportsbook.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("disable_sportsbook_exposure_limit_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to disable exposure limit")
			return
		}
		writeJSON(w, http.StatusOK, toExposureLimitResponse(disabled))
	}
}

// newListSportsbookExposureLimitsHandler lists every exposure limit
// (active and disabled) for the caller's own tenant - migration 0088's
// tenant_staff_scope RLS policy restricts the read; there is no player
// policy at all (a limit has no player-facing read path).
func newListSportsbookExposureLimitsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var items []exposureLimitResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			limits, err := sportsbook.ListExposureLimits(ctx, tx)
			if err != nil {
				return err
			}
			items = make([]exposureLimitResponse, 0, len(limits))
			for _, l := range limits {
				items = append(items, toExposureLimitResponse(l))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_sportsbook_exposure_limits_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list exposure limits")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}
