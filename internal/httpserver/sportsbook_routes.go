package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerSportsbookRoutes wires every Stage 6 sportsbook vertical-slice
// endpoint. Mirrors registerCasinoRoutes' pattern exactly.
//
// Explicitly NOT exposed this stage (this stage's own directive, CLAUDE.md's
// stage-gate rule): any settlement/void/partial-settlement/cashout
// endpoint, any live/in-play odds endpoint, any multi-leg/accumulator bet
// endpoint, and any endpoint that would let a client name its own
// tenant_id/player_account_id/wallet_id (every identifying field is
// resolved server-side from the authenticated session).
func registerSportsbookRoutes(mux *http.ServeMux, deps Deps) {
	// Catalogue browse - public, no authentication (platform-wide,
	// read-open catalogue data, no RLS - see this package's own doc
	// comment for why no tenant/brand availability layer gates it this
	// stage).
	mux.HandleFunc("GET /v1/sportsbook/sports", newListSportsHandler(deps))
	mux.HandleFunc("GET /v1/sportsbook/events/{id}", newGetEventHandler(deps))

	// Player self-service - bet placement and own bet history.
	mux.Handle("POST /v1/me/sportsbook/bets", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newPlaceBetHandler(deps))))
	mux.Handle("GET /v1/me/sportsbook/bets", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newListMyBetsHandler(deps))))

	// Back Office - tenant-wide, staff-scoped, read-only bet visibility.
	mux.Handle("GET /v1/admin/sportsbook/bets",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermSportsbookBetRead)(newListAdminBetsHandler(deps)))))

	// Stage 9.2 (ADR 0083 §5.2/§9.2): platform-admin jurisdiction/market
	// gating governance for sb_jurisdiction_restrictions - never
	// RequireTenantScope-wrapped, same reasoning as PUT
	// /v1/admin/casino/games (a platform-admin token's tenant_id is nil).
	mux.Handle("POST /v1/admin/sportsbook/jurisdiction-restrictions",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermSportsbookJurisdictionRestrictionManage)(newCreateSportsbookJurisdictionRestrictionHandler(deps))))
	mux.Handle("POST /v1/admin/sportsbook/jurisdiction-restrictions/{id}/withdraw",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermSportsbookJurisdictionRestrictionManage)(newWithdrawSportsbookJurisdictionRestrictionHandler(deps))))
	mux.Handle("GET /v1/admin/sportsbook/jurisdiction-restrictions",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermSportsbookJurisdictionRestrictionRead)(newListSportsbookJurisdictionRestrictionsHandler(deps))))

	// Stage 9.2 Part B2 / Wave 3 (ADR 0083 §6.2.3/§9.2): TENANT-scoped
	// cross-player book-exposure limit governance for sb_exposure_limits -
	// RequireTenantScope-wrapped (unlike the jurisdiction-restriction
	// routes immediately above), mirroring registerRiskRoutes exactly:
	// this table carries a tenant_id and is ordinary tenant-owned
	// commercial configuration, not a platform-wide control.
	mux.Handle("POST /v1/admin/sportsbook/exposure-limits",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermSportsbookExposureLimitManage)(newCreateSportsbookExposureLimitHandler(deps)))))
	mux.Handle("POST /v1/admin/sportsbook/exposure-limits/{id}/disable",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermSportsbookExposureLimitManage)(newDisableSportsbookExposureLimitHandler(deps)))))
	mux.Handle("GET /v1/admin/sportsbook/exposure-limits",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermSportsbookExposureLimitRead)(newListSportsbookExposureLimitsHandler(deps)))))
}
