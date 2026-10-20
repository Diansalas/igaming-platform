package httpserver

// ADR 0115 S4-min / S5-min: GET /v1/me/rg/options - the server-owned choices of the player's RG screen
// (periods, ceilings, cooling-off, time-out durations, self-exclusion durations). Read-only; it enforces
// nothing. See internal/rg/options.go for what is configuration and what is an engineering default.
//
// Player principals only (auth.RequirePlayerPrincipal); the tenant and the account come from the verified
// token, the asset from the query (validated, never trusted as an identity). It runs under WithTenant, not
// WithPlayerScope: the configuration is not player data and the configuration readers refuse a
// player-scoped session.

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

type rgLimitOptionResponse struct {
	Kind                    string   `json:"kind"`
	ConfigInForce           bool     `json:"config_in_force"`
	Mandatory               bool     `json:"mandatory"`
	AllowedPeriods          []string `json:"allowed_periods"`
	Ceiling                 *string  `json:"ceiling"`
	Scope                   string   `json:"scope,omitempty"`
	IncreaseCooldownSeconds *int64   `json:"increase_cooldown_seconds"`
	// UnavailableReason (lic27a): missing_floor | calendar_conflict. Absent when the kind's options are offered.
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type rgOptionsResponse struct {
	AssetCode          string                  `json:"asset_code"`
	EnforcementEnabled bool                    `json:"limit_enforcement_enabled"`
	Limits             []rgLimitOptionResponse `json:"limits"`
	TimeOutHours       []int                   `json:"time_out_hours"`
	SelfExclusionDays  []int                   `json:"self_exclusion_days"`
	SelfExclusionIndef bool                    `json:"self_exclusion_indefinite_allowed"`
}

func toRGOptionsResponse(o rg.Options) rgOptionsResponse {
	resp := rgOptionsResponse{AssetCode: o.AssetCode, EnforcementEnabled: o.EnforcementEnabled, TimeOutHours: o.TimeOutHours,
		SelfExclusionDays: o.SelfExclusionDays, SelfExclusionIndef: true, Limits: []rgLimitOptionResponse{}}
	for _, l := range o.Limits {
		periods := make([]string, 0, len(l.AllowedPeriods))
		for _, p := range l.AllowedPeriods {
			periods = append(periods, string(p))
		}
		resp.Limits = append(resp.Limits, rgLimitOptionResponse{Kind: string(l.Kind), ConfigInForce: l.ConfigInForce, Mandatory: l.Mandatory,
			AllowedPeriods: periods, Ceiling: l.Ceiling, Scope: string(l.Scope), IncreaseCooldownSeconds: l.IncreaseCooldownSeconds, UnavailableReason: l.UnavailableReason})
	}
	if resp.TimeOutHours == nil {
		resp.TimeOutHours = []int{}
	}
	return resp
}

func newGetMyRGOptionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		asset := r.URL.Query().Get("asset_code")
		if asset == "" {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "asset_code is required")
			return
		}
		var opts rg.Options
		err := deps.DB.WithTenant(r.Context(), c.tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			opts, e = rg.ReadOptions(ctx, tx, c.tc.TenantID, c.subject, asset)
			return e
		})
		if err != nil {
			if errors.Is(err, rg.ErrUsageScope) || errors.Is(err, rg.ErrUsageIsolation) {
				c.logger.Error("rg_options_scope", "error_class", "internal")
				apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
				return
			}
			_ = writeRGError(w, c, "options", err)
			return
		}
		writeJSON(w, http.StatusOK, toRGOptionsResponse(opts))
	}
}
