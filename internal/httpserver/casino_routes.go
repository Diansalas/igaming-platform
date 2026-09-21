package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerCasinoRoutes wires every Stage 4A casino-integration endpoint.
// Mirrors registerFinancialRoutes' pattern (financial_routes.go) - grouped
// here so the full route table, and which middleware chain guards each
// route, is visible in one place.
//
// Explicitly NOT exposed this stage (CLAUDE.md's Stage 4A scope gate):
// any endpoint naming a real casino provider, any endpoint that would let
// a client name its own tenant_id/player_account_id/wallet_id (every
// identifying field is resolved server-side from the authenticated
// session), and a back-office catalogue-management UI (API only).
func registerCasinoRoutes(mux *http.ServeMux, deps Deps) {
	// Player self-service catalogue read + game launch.
	mux.Handle("GET /v1/me/casino/games", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newListCasinoGamesHandler(deps))))
	mux.Handle("POST /v1/me/casino/games/{gameID}/launch", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newLaunchCasinoGameHandler(deps))))

	// Player self-service round history (Stage 7 §16).
	mux.Handle("GET /v1/me/casino/rounds", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newListMyCasinoRoundsHandler(deps))))

	// Stage 7 §6/§7: mock-provider play simulation for the player's own
	// launch session - see casino_play_handlers.go's own doc comment for
	// why this exists only because no real provider integration exists yet
	// this stage. Gated behind CasinoPlaySimulationEnabled (never routed
	// at all when false, not merely 404'd inside the handler) - specialist
	// review finding (architect/security/ledger-finance, independently):
	// a mock provider is by definition one that says yes to everything, so
	// registering these routes unconditionally would ship a player-
	// authenticated money-minting/reversal surface in every deployment of
	// this binary, relying only on "no tenant happens to be configured
	// against a non-mock provider" - an assumption that holds today only
	// because no other adapter exists yet. cmd/platform-api/main.go sets
	// this outside production.
	if deps.CasinoPlaySimulationEnabled {
		mux.Handle("POST /v1/me/casino/sessions/{sessionID}/wager", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newWagerCasinoRoundHandler(deps))))
		mux.Handle("POST /v1/me/casino/sessions/{sessionID}/win", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newWinCasinoRoundHandler(deps))))
		mux.Handle("POST /v1/me/casino/sessions/{sessionID}/rollback", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newRollbackCasinoRoundHandler(deps))))
	}

	// Back Office - tenant-wide, staff-scoped, read-only round visibility
	// (Stage 7 §15).
	mux.Handle("GET /v1/admin/casino/rounds",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermCasinoTransactionRead)(newListAdminCasinoRoundsHandler(deps)))))

	// Provider callback (bet/win/rollback) - no bearer-token middleware (a
	// provider webhook is not an authenticated platform principal); the
	// handler resolves tenant scope from the URL's tenant slug and
	// verifies the payload's signature via the named adapter, mirroring
	// newPaymentWebhookHandler's identical rationale.
	mux.HandleFunc("POST /v1/webhooks/casino/{tenantSlug}/{providerID}", newCasinoWebhookHandler(deps))

	// Platform-wide game catalogue (platform_admin only - see
	// PermCasinoCatalogueManage's own doc comment). Deliberately NOT
	// wrapped in RequireTenantScope: a platform_admin's token carries a
	// nil tenant_id, which RequireTenantScope would reject before this
	// handler ever ran (same reasoning as tenant provisioning in
	// routes.go).
	mux.Handle("PUT /v1/admin/casino/games",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermCasinoCatalogueManage)(newUpsertCasinoGameHandler(deps))))

	// Tenant-scoped casino provider capability configuration.
	mux.Handle("PUT /v1/admin/casino/providers/{providerID}/capability",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermCasinoConfigWrite)(newWriteCasinoCapabilityHandler(deps)))))

	// Tenant-scoped game availability (opt-in) configuration.
	mux.Handle("PUT /v1/admin/casino/games/{gameID}/availability",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermCasinoConfigWrite)(newSetCasinoGameAvailabilityHandler(deps)))))
}
