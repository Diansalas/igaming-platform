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
	mux.Handle("POST /v1/me/sportsbook/bets", auth.Middleware(deps.AuthIssuer)(newPlaceBetHandler(deps)))
	mux.Handle("GET /v1/me/sportsbook/bets", auth.Middleware(deps.AuthIssuer)(newListMyBetsHandler(deps)))

	// Back Office - tenant-wide, staff-scoped, read-only bet visibility.
	mux.Handle("GET /v1/admin/sportsbook/bets",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermSportsbookBetRead)(newListAdminBetsHandler(deps)))))
}
