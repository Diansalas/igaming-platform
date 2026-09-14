// Stage 4D-RG: player self-service self-exclusion + status, and the
// minimal staff/admin API for creating and reading Responsible Gaming
// restrictions (ADR 0026 §13). No full back-office UI, no "end
// restriction early" endpoint (see internal/rg's own doc comments for
// why), no cross-tenant staff-initiated restriction (see
// internal/rg.CreateStaffRestriction's doc comment and the recorded OPEN
// DECISION in ADR 0026 §12).
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

type restrictionResponse struct {
	ID              string  `json:"id"`
	RestrictionType string  `json:"restriction_type"`
	Scope           string  `json:"scope"`
	StartsAt        string  `json:"starts_at"`
	EndsAt          *string `json:"ends_at,omitempty"`
	Indefinite      bool    `json:"indefinite"`
	Source          string  `json:"source"`
	ReasonCode      string  `json:"reason_code,omitempty"`
	CreatedAt       string  `json:"created_at"`
	Active          bool    `json:"active"`
}

func toRestrictionResponse(r rg.Restriction) restrictionResponse {
	resp := restrictionResponse{
		ID: r.ID.String(), RestrictionType: string(r.RestrictionType), Scope: string(r.Scope()),
		StartsAt: r.StartsAt.Format(rfc3339), Indefinite: r.EndsAt == nil, Source: string(r.Source),
		ReasonCode: r.ReasonCode, CreatedAt: r.CreatedAt.Format(rfc3339), Active: r.IsActiveAt(time.Now().UTC()),
	}
	if r.EndsAt != nil {
		s := r.EndsAt.Format(rfc3339)
		resp.EndsAt = &s
	}
	return resp
}

// --- Player self-service ---

type createSelfExclusionRequest struct {
	// DurationDays is optional - omitted or zero means indefinite (ADR
	// 0026 §4). When present it must be positive.
	DurationDays *int `json:"duration_days,omitempty"`
}

// newCreateSelfExclusionHandler lets an authenticated player record their
// own, platform-wide self-exclusion (ADR 0026 §4). Runs under
// db.Pool.WithPlayerScope, NOT WithTenant - migration 0037's
// player_self_insert RLS policy requires the app.player_account_id GUC
// only that call sets, and independently re-derives every identity field
// from it, never trusting anything in the request body beyond the
// optional duration.
func newCreateSelfExclusionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}

		var req createSelfExclusionRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.DurationDays != nil && *req.DurationDays <= 0 {
			apierror.Write(w, requestID, apierror.CodeValidation, "duration_days must be positive when set")
			return
		}

		var restriction rg.Restriction
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			restriction, err = rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{
				TenantID: tc.TenantID, PlayerAccountID: playerAccountID, DurationDays: req.DurationDays,
			})
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if errors.Is(err, rg.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("create_self_exclusion_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to record self-exclusion")
			return
		}
		writeJSON(w, http.StatusCreated, toRestrictionResponse(restriction))
	}
}

// newGetMyRGStatusHandler is a player's own "am I restricted" self-check.
// Runs under db.Pool.WithPlayerScope - migration 0037's player_self_read
// RLS policy is what actually limits the result to the caller's own
// person, not this handler's own query text.
func newGetMyRGStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}

		var restrictions []rg.Restriction
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			restrictions, err = rg.ListRestrictionsForAccount(ctx, tx, playerAccountID)
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if err != nil {
			logger.Error("get_rg_status_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to read restriction status")
			return
		}
		writeJSON(w, http.StatusOK, toRestrictionListResponse(restrictions))
	}
}

// --- Staff/admin ---

type createStaffRestrictionRequest struct {
	PlayerAccountID string `json:"player_account_id"`
	// Scope must be "tenant" or "brand" - never "platform" (see
	// internal/rg.CreateStaffRestriction's doc comment).
	Scope        string `json:"scope"`
	DurationDays *int   `json:"duration_days,omitempty"`
	ReasonCode   string `json:"reason_code"`
}

// newCreateStaffRestrictionHandler is the staff-initiated counterpart to
// player self-service. Gated by PermRGRestrictionWrite, held only by
// RoleCompliance (a tenant-scoped role) - the route is NOT wrapped in
// RequireTenantScope for consistency with the rest of this admin surface,
// but every current PermRGRestrictionWrite grantee already carries a
// tenant-scoped token, so tc.TenantID is always real here in practice; a
// hypothetical future platform-scoped grantee would still be correctly
// rejected by db.WithTenant itself refusing a nil tenant id.
func newCreateStaffRestrictionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}

		var req createStaffRestrictionRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		targetID, err := uuid.Parse(req.PlayerAccountID)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid player_account_id")
			return
		}
		v := validation.New()
		v.RequireOneOf("scope", req.Scope, "tenant", "brand")
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		if req.DurationDays != nil && *req.DurationDays <= 0 {
			v.Add("duration_days", "must be positive when set")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var restriction rg.Restriction
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			restriction, err = rg.CreateStaffRestriction(ctx, tx, rg.CreateStaffRestrictionParams{
				ActorStaffID: staffID, TargetPlayerAccountID: targetID, Scope: rg.Scope(req.Scope),
				DurationDays: req.DurationDays, ReasonCode: req.ReasonCode,
			})
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if errors.Is(err, rg.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("create_staff_rg_restriction_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to record restriction")
			return
		}
		writeJSON(w, http.StatusCreated, toRestrictionResponse(restriction))
	}
}

// newListRestrictionsForAccountHandler is the staff read counterpart -
// gated by PermRGRestrictionRead (RoleTenantAdmin/RoleCompliance). Runs
// under db.Pool.WithTenant; migration 0037's staff_and_system_read RLS
// policy limits the result to this tenant's own rows plus every
// platform-wide row, regardless of which tenant/player originally created
// it - a tenant admin is legitimately entitled to see that one of their
// OWN players is platform-wide self-excluded, even though they cannot see
// who else that same restriction affects at another tenant (this query is
// always scoped to one already-named player_account_id, never a bare
// "list everyone").
func newListRestrictionsForAccountHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		targetID, err := uuid.Parse(r.URL.Query().Get("player_account_id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "player_account_id query parameter is required")
			return
		}

		var restrictions []rg.Restriction
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			restrictions, err = rg.ListRestrictionsForAccount(ctx, tx, targetID)
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if err != nil {
			logger.Error("list_rg_restrictions_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list restrictions")
			return
		}
		writeJSON(w, http.StatusOK, toRestrictionListResponse(restrictions))
	}
}

func toRestrictionListResponse(restrictions []rg.Restriction) []restrictionResponse {
	resp := make([]restrictionResponse, 0, len(restrictions))
	for _, r := range restrictions {
		resp = append(resp, toRestrictionResponse(r))
	}
	return resp
}
