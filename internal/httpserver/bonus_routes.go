package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerBonusRoutes wires Stage 4H-B1 Wave 2's Bonus Engine HTTP
// surface - player-facing (own grants/progress, coupon redemption, no
// permission check needed beyond authentication - RLS/self-scoping is
// the isolation mechanism, mirroring every other player-facing route in
// this codebase) and staff-facing (RequireTenantScope + RequirePermission,
// never inferred from the UI, mirroring registerRiskRoutes).
func registerBonusRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/bonus/grants",
		auth.Middleware(deps.AuthIssuer)(newListMyGrantsHandler(deps)))
	mux.Handle("GET /v1/bonus/grants/{grantID}/progress",
		auth.Middleware(deps.AuthIssuer)(newGetMyGrantProgressHandler(deps)))
	mux.Handle("POST /v1/bonus/coupons/redeem",
		auth.Middleware(deps.AuthIssuer)(newRedeemCouponHandler(deps)))

	mux.Handle("POST /v1/admin/bonus/campaigns",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusCampaignCreate)(newCreateCampaignHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/grants",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusGrantIssue)(newIssueManualGrantHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/suggestions/{suggestionID}/claim",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusSuggestionReview)(newClaimSuggestionHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/suggestions/{suggestionID}/decide",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusSuggestionReview)(newDecideSuggestionHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/held-dispositions/{dispositionID}/resolve",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusHeldDispositionResolve)(newResolveHeldDispositionHandler(deps)))))
}
