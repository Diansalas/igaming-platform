// Stage 9.2 (ADR 0083 §5.2/§9.2): the platform-admin HTTP surface for
// sb_jurisdiction_restrictions - create/withdraw/list. Modelled on
// risk_handlers.go's identical shape (see internal/sportsbook/
// jurisdiction_admin.go's own doc comment for the full precedent
// rationale), adapted for a platform-admin-only, non-tenant-scoped
// table (mirrors newUpsertCasinoGameHandler's WithPlatformAdmin usage
// exactly).
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

type jurisdictionRestrictionResponse struct {
	ID                     string `json:"id"`
	ScopeKind              string `json:"scope_kind"`
	EventID                string `json:"event_id,omitempty"`
	MarketID               string `json:"market_id,omitempty"`
	SelectionID            string `json:"selection_id,omitempty"`
	JurisdictionCode       string `json:"jurisdiction_code"`
	RestrictionKind        string `json:"restriction_kind"`
	Status                 string `json:"status"`
	AuthorizationReference string `json:"authorization_reference"`
	ReasonCode             string `json:"reason_code"`
	CreatedByActorID       string `json:"created_by_actor_id"`
	CreatedAt              string `json:"created_at"`
	UpdatedAt              string `json:"updated_at"`
}

func toJurisdictionRestrictionResponse(r sportsbook.JurisdictionRestriction) jurisdictionRestrictionResponse {
	resp := jurisdictionRestrictionResponse{
		ID: r.ID.String(), ScopeKind: r.ScopeKind, JurisdictionCode: r.JurisdictionCode,
		RestrictionKind: r.RestrictionKind, Status: r.Status, AuthorizationReference: r.AuthorizationReference,
		ReasonCode: r.ReasonCode, CreatedByActorID: r.CreatedByActorID.String(),
		CreatedAt: r.CreatedAt.Format(rfc3339), UpdatedAt: r.UpdatedAt.Format(rfc3339),
	}
	if r.EventID != nil {
		resp.EventID = r.EventID.String()
	}
	if r.MarketID != nil {
		resp.MarketID = r.MarketID.String()
	}
	if r.SelectionID != nil {
		resp.SelectionID = r.SelectionID.String()
	}
	return resp
}

type createJurisdictionRestrictionRequest struct {
	ScopeKind              string `json:"scope_kind"`
	EventID                string `json:"event_id,omitempty"`
	MarketID               string `json:"market_id,omitempty"`
	SelectionID            string `json:"selection_id,omitempty"`
	JurisdictionCode       string `json:"jurisdiction_code"`
	AuthorizationReference string `json:"authorization_reference"`
	ReasonCode             string `json:"reason_code"`
}

// newCreateSportsbookJurisdictionRestrictionHandler arms a new deny-only
// restriction. Platform-admin only (never RequireTenantScope-wrapped -
// same reasoning as PUT /v1/admin/casino/games) - a platform-admin
// token's tenant_id is nil, which RequireTenantScope would reject before
// this handler ever ran.
func newCreateSportsbookJurisdictionRestrictionHandler(deps Deps) http.HandlerFunc {
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
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var req createJurisdictionRestrictionRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("scope_kind", req.ScopeKind, "event", "market", "selection")
		v.RequireNonEmpty("jurisdiction_code", req.JurisdictionCode)
		v.RequireNonEmpty("authorization_reference", req.AuthorizationReference)
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		var eventID, marketID, selectionID *uuid.UUID
		switch req.ScopeKind {
		case "event":
			v.RequireNonEmpty("event_id", req.EventID)
		case "market":
			v.RequireNonEmpty("market_id", req.MarketID)
		case "selection":
			v.RequireNonEmpty("selection_id", req.SelectionID)
		}
		if req.EventID != "" {
			parsed, err := uuid.Parse(req.EventID)
			if err != nil {
				v.Add("event_id", "must be a valid UUID")
			} else {
				eventID = &parsed
			}
		}
		if req.MarketID != "" {
			parsed, err := uuid.Parse(req.MarketID)
			if err != nil {
				v.Add("market_id", "must be a valid UUID")
			} else {
				marketID = &parsed
			}
		}
		if req.SelectionID != "" {
			parsed, err := uuid.Parse(req.SelectionID)
			if err != nil {
				v.Add("selection_id", "must be a valid UUID")
			} else {
				selectionID = &parsed
			}
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var created sportsbook.JurisdictionRestriction
		err = deps.DB.WithPlatformAdmin(r.Context(), subjectID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = sportsbook.CreateJurisdictionRestriction(ctx, tx, sportsbook.CreateJurisdictionRestrictionParams{
				ScopeKind: req.ScopeKind, EventID: eventID, MarketID: marketID, SelectionID: selectionID,
				JurisdictionCode: req.JurisdictionCode, AuthorizationReference: req.AuthorizationReference, ReasonCode: req.ReasonCode,
				CreatedByActorID: subjectID, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if errors.Is(err, sportsbook.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "unknown jurisdiction_code, event_id, market_id, or selection_id")
			return
		}
		if err != nil {
			logger.Error("create_sportsbook_jurisdiction_restriction_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create jurisdiction restriction")
			return
		}
		writeJSON(w, http.StatusCreated, toJurisdictionRestrictionResponse(created))
	}
}

type withdrawJurisdictionRestrictionRequest struct {
	ReasonCode string `json:"reason_code"`
}

// newWithdrawSportsbookJurisdictionRestrictionHandler withdraws (never
// deletes) an active restriction.
func newWithdrawSportsbookJurisdictionRestrictionHandler(deps Deps) http.HandlerFunc {
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
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		restrictionID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid restriction id")
			return
		}

		var req withdrawJurisdictionRestrictionRequest
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

		var withdrawn sportsbook.JurisdictionRestriction
		err = deps.DB.WithPlatformAdmin(r.Context(), subjectID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			withdrawn, err = sportsbook.WithdrawJurisdictionRestriction(ctx, tx, sportsbook.WithdrawJurisdictionRestrictionParams{
				ID: restrictionID, ReasonCode: req.ReasonCode, ActorID: subjectID,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if errors.Is(err, sportsbook.ErrRestrictionNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "jurisdiction restriction not found or already withdrawn")
			return
		}
		if errors.Is(err, sportsbook.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("withdraw_sportsbook_jurisdiction_restriction_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to withdraw jurisdiction restriction")
			return
		}
		writeJSON(w, http.StatusOK, toJurisdictionRestrictionResponse(withdrawn))
	}
}

// newListSportsbookJurisdictionRestrictionsHandler lists every restriction
// (active and withdrawn) - the underlying table's own RLS read policy is
// platform-uniform read-open (migration 0087), so this runs under
// WithoutTenant; PermSportsbookJurisdictionRestrictionRead is what
// actually restricts who reaches this handler.
func newListSportsbookJurisdictionRestrictionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

		var items []jurisdictionRestrictionResponse
		err := deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			restrictions, err := sportsbook.ListJurisdictionRestrictions(ctx, tx)
			if err != nil {
				return err
			}
			items = make([]jurisdictionRestrictionResponse, 0, len(restrictions))
			for _, res := range restrictions {
				items = append(items, toJurisdictionRestrictionResponse(res))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_sportsbook_jurisdiction_restrictions_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list jurisdiction restrictions")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}
