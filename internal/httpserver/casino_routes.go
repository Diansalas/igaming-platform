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
	mux.Handle("GET /v1/me/casino/games", auth.Middleware(deps.AuthIssuer)(newListCasinoGamesHandler(deps)))
	mux.Handle("POST /v1/me/casino/games/{gameID}/launch", auth.Middleware(deps.AuthIssuer)(newLaunchCasinoGameHandler(deps)))

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
