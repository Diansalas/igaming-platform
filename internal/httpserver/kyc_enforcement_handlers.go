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
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// enforcementDecisionResponse is the exact, closed field set ADR 0096 §6
// authorizes - never kyc_verifications.reason, person/document data, or
// an amount.
type enforcementDecisionResponse struct {
	ID             string `json:"id"`
	Operation      string `json:"operation"`
	Outcome        string `json:"outcome"`
	MatchedTrigger string `json:"matched_trigger,omitempty"`
	PolicyVersion  string `json:"policy_version"`
	DecidedAt      string `json:"decided_at"`
}

type listEnforcementDecisionsResponse struct {
	Decisions  []enforcementDecisionResponse `json:"decisions"`
	NextBefore string                        `json:"next_before,omitempty"`
}

// newListKYCEnforcementDecisionsHandler implements
// GET /v1/admin/kyc/enforcement-decisions?player_account_id=...
// (ADR 0096 §6, security condition 7): tenant comes ONLY from the
// authenticated staff context (never a query parameter); a
// player_account_id belonging to a different tenant, or that does not
// exist, returns the SAME 404; roles are gated by
// PermKYCEnforcementDecisionRead (compliance/platform_admin only, never
// tenant_admin); pagination is keyset on (decided_at, id) with a
// server-enforced max page size (kyc.MaxEnforcementDecisionsPageSize).
func newListKYCEnforcementDecisionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		playerAccountID, err := uuid.Parse(r.URL.Query().Get("player_account_id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "player_account_id is required and must be a UUID")
			return
		}

		limit := 0
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if v, convErr := strconv.Atoi(raw); convErr == nil {
				limit = v
			}
		}

		var beforeDecidedAt *time.Time
		var beforeID *uuid.UUID
		if raw := r.URL.Query().Get("before_decided_at"); raw != "" {
			if t, convErr := time.Parse(time.RFC3339Nano, raw); convErr == nil {
				beforeDecidedAt = &t
			}
		}
		if raw := r.URL.Query().Get("before_id"); raw != "" {
			if id, convErr := uuid.Parse(raw); convErr == nil {
				beforeID = &id
			}
		}

		var decisions []kyc.EnforcementDecisionRecord
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			// The SAME 404 as a nonexistent id for a player_account_id
			// belonging to a different tenant - RLS on player_accounts
			// already scopes this lookup to tc.TenantID.
			if _, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID); err != nil {
				return err
			}
			decisions, err = kyc.ListEnforcementDecisionsForPlayer(ctx, tx, kyc.ListEnforcementDecisionsParams{
				TenantID: tc.TenantID, PlayerAccountID: playerAccountID, Limit: limit,
				BeforeDecidedAt: beforeDecidedAt, BeforeID: beforeID,
			})
			return err
		})
		if err != nil {
			if errors.Is(err, identity.ErrNotFound) {
				apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
				return
			}
			logger.Error("list_kyc_enforcement_decisions_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list kyc enforcement decisions")
			return
		}

		resp := listEnforcementDecisionsResponse{Decisions: make([]enforcementDecisionResponse, 0, len(decisions))}
		for _, d := range decisions {
			resp.Decisions = append(resp.Decisions, enforcementDecisionResponse{
				ID: d.ID.String(), Operation: d.Operation, Outcome: d.Outcome,
				MatchedTrigger: d.MatchedTrigger, PolicyVersion: d.PolicyVersion,
				DecidedAt: d.DecidedAt.UTC().Format(rfc3339),
			})
		}
		if len(decisions) > 0 {
			last := decisions[len(decisions)-1]
			resp.NextBefore = last.DecidedAt.UTC().Format(time.RFC3339Nano) + "," + last.ID.String()
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
