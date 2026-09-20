// Stage 5 (Operator Back Office MVP): the bonus admin READ surface.
//
// Stage 4I Exit Triage found bonus campaign administration entirely
// write-only over HTTP - zero GET routes anywhere (no campaign list, no
// offer list, no change-request approval queue, no per-player grant/
// reward view). This file closes exactly the three gaps this dispatch
// names: campaign list, the change-request approval queue, and grant
// list (tenant-wide or per-player). It is purely additive - no existing
// campaign/offer/grant/change-request semantics, state machine, or
// four-eyes mechanism is touched; every function here is read-only.
//
// Conventions followed exactly, matching bonus_handlers.go/
// bonus_governance_handlers.go: writeJSON/decodeJSON, apierror.Write,
// tenant.FromContext, deps.DB.WithTenant, auth.RequirePermission. Every
// list endpoint uses the shared Stage 5 pagination envelope
// (pagination.go's parsePageParams/newPagedResponse) - {"items":[...],
// "limit":,"offset":,"total":}.
//
// Permission: PermBonusRead is reused for all three endpoints - it is
// already the platform's own "general, read-only bonus visibility"
// permission (permission.go's own doc comment: "a tenant admin may see
// every campaign and every player's grant history, and may not author,
// activate, issue, adjust, bulk-assign, or cancel anything"), and is
// already held by every role with a legitimate reason to see this
// surface (RoleTenantAdmin, RoleSupport, RoleCompliance,
// RolePromotionsManager, RoleBonusOperations). No new permission is
// added here (out of scope for this pass, per dispatch).
package httpserver

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// --- GET /v1/admin/bonus/campaigns ---

// newListCampaignsHandler is the paginated campaign list this tenant's
// staff needs to operate the bonus engine at all - reconnaissance's own
// named gap ("no campaign list"). Optional ?status= filters to one of
// bonus_campaigns.status's real closed-set values (never a value this
// handler invents).
func newListCampaignsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		p := parsePageParams(r)

		var statusFilter *bonus.CampaignStatus
		if raw := r.URL.Query().Get("status"); raw != "" {
			v := validation.New()
			v.RequireOneOf("status", raw,
				string(bonus.CampaignDraft), string(bonus.CampaignActive), string(bonus.CampaignPaused),
				string(bonus.CampaignEnded), string(bonus.CampaignArchived))
			if v.HasErrors() {
				apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
				return
			}
			s := bonus.CampaignStatus(raw)
			statusFilter = &s
		}

		var resp pagedResponse[campaignResponse]
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			campaigns, total, err := bonus.ListCampaignsPage(ctx, tx, tc.TenantID, statusFilter, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			items := make([]campaignResponse, 0, len(campaigns))
			for _, c := range campaigns {
				items = append(items, toCampaignResponse(c))
			}
			resp = newPagedResponse(items, p, total)
			return nil
		})
		if err != nil {
			deps.Logger.Error("list_campaigns_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list campaigns")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- GET /v1/admin/bonus/change-requests ---

// newListChangeRequestsHandler is the four-eyes approval queue - the
// reconnaissance's own "no change-request approval queue" gap. Defaults
// to ?status=pending (bonus_change_requests.state's real "awaiting
// decision" value) when no status filter is given, so an operator
// hitting this endpoint with no query string lands on their actual
// queue, per the dispatch's own instruction. The existing
// POST .../decide mutation endpoint is entirely unchanged - this is a
// read-only companion to it, never a new decision path.
func newListChangeRequestsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		p := parsePageParams(r)

		stateRaw := r.URL.Query().Get("status")
		if stateRaw == "" {
			stateRaw = string(bonus.ChangeRequestPending)
		}
		v := validation.New()
		v.RequireOneOf("status", stateRaw,
			string(bonus.ChangeRequestPending), string(bonus.ChangeRequestApplied),
			string(bonus.ChangeRequestRejected), string(bonus.ChangeRequestCancelled))
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		state := bonus.ChangeRequestState(stateRaw)

		var resp pagedResponse[changeRequestResponse]
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			requests, total, err := bonus.ListChangeRequestsPage(ctx, tx, tc.TenantID, &state, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			items := make([]changeRequestResponse, 0, len(requests))
			for _, cr := range requests {
				items = append(items, toChangeRequestResponse(cr))
			}
			resp = newPagedResponse(items, p, total)
			return nil
		})
		if err != nil {
			deps.Logger.Error("list_change_requests_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list change requests")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- GET /v1/admin/bonus/grants ---

// newListGrantsHandler is the reconnaissance's own "no per-player grant/
// reward view" gap, plus a tenant-wide grant list. When
// ?player_account_id= is supplied, this returns that player's own
// grant/reward state (for a Back Office player-detail page) - still
// wrapped in the same paginated envelope every other Stage 5 list
// endpoint uses, per the dispatch's own instruction, even though a
// single player's grant count is typically small. When omitted, this is
// a tenant-wide list, optionally filtered by ?status= to one of
// bonus_grants.status's real closed-set values. Both forms may be
// combined (a per-player list additionally filtered by status).
func newListGrantsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		p := parsePageParams(r)

		var playerAccountID *uuid.UUID
		if raw := r.URL.Query().Get("player_account_id"); raw != "" {
			id, perr := uuid.Parse(raw)
			if perr != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "player_account_id must be a valid UUID")
				return
			}
			playerAccountID = &id
		}

		var statusFilter *bonus.GrantStatus
		if raw := r.URL.Query().Get("status"); raw != "" {
			v := validation.New()
			v.RequireOneOf("status", raw,
				string(bonus.GrantIssued), string(bonus.GrantActivated), string(bonus.GrantInProgress),
				string(bonus.GrantPendingSettlement), string(bonus.GrantCompleted), string(bonus.GrantConverted),
				string(bonus.GrantExpired), string(bonus.GrantCancelled), string(bonus.GrantForfeited), string(bonus.GrantReversed))
			if v.HasErrors() {
				apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
				return
			}
			s := bonus.GrantStatus(raw)
			statusFilter = &s
		}

		var resp pagedResponse[grantResponse]
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			grants, total, err := bonus.ListGrantsPage(ctx, tx, tc.TenantID, playerAccountID, statusFilter, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			items := make([]grantResponse, 0, len(grants))
			for _, g := range grants {
				remaining, err := bonus.RemainingBonusBalance(ctx, tx, tc.TenantID, g.ID)
				if err != nil {
					return err
				}
				items = append(items, toGrantResponse(g, remaining))
			}
			resp = newPagedResponse(items, p, total)
			return nil
		})
		if err != nil {
			deps.Logger.Error("list_grants_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list grants")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
