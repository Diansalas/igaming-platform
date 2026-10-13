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
		auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newListMyGrantsHandler(deps))))
	mux.Handle("GET /v1/bonus/grants/{grantID}/progress",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newGetMyGrantProgressHandler(deps))))
	mux.Handle("POST /v1/bonus/coupons/redeem",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newRedeemCouponHandler(deps))))

	// Stage 5 (Operator Back Office MVP): the bonus admin READ surface
	// (bonus_admin_read_handlers.go) - campaign list, the change-request
	// approval queue, and grant list (tenant-wide or per-player). Purely
	// additive; PermBonusRead is this codebase's existing general
	// read-only bonus visibility permission (see that file's own top-of-
	// file doc comment for why it, not a new permission, is reused here).
	mux.Handle("GET /v1/admin/bonus/campaigns",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusRead)(newListCampaignsHandler(deps)))))
	mux.Handle("GET /v1/admin/bonus/change-requests",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusRead)(newListChangeRequestsHandler(deps)))))
	mux.Handle("GET /v1/admin/bonus/grants",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusRead)(newListGrantsHandler(deps)))))

	mux.Handle("POST /v1/admin/bonus/campaigns",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusCampaignCreate)(newCreateCampaignHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/grants",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusGrantIssue)(newIssueManualGrantHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/suggestions/{suggestionID}/claim",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusSuggestionReview)(newClaimSuggestionHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/suggestions/{suggestionID}/decide",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusSuggestionReview)(newDecideSuggestionHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/held-dispositions/{dispositionID}/resolve",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusHeldDispositionResolve)(newResolveHeldDispositionHandler(deps)))))

	// Stage 4H-B1 Wave 3 Phase 3 (item F): four-eyes filing/approval and
	// EOI minting - permission is checked DYNAMICALLY inside each handler
	// (changeOperationPermission/the operation_type switch in
	// bonus_governance_handlers.go), since the required permission
	// depends on a request-body field, not the route itself. Only
	// RequireTenantScope is applied at the route level.
	mux.Handle("POST /v1/admin/bonus/change-requests",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(newFileChangeRequestHandler(deps))))
	mux.Handle("POST /v1/admin/bonus/change-requests/{requestID}/decide",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(newDecideChangeRequestHandler(deps))))
	mux.Handle("POST /v1/admin/bonus/economic-operations",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(newMintEconomicOperationHandler(deps))))

	// Stage 4H-B1 Wave 3 Phase 3 (item F): campaign activate / offer
	// create+version+publish / manual-grant issue+activate / bulk-job
	// create+execute - the per-domain callers item E's four-eyes wrapper
	// functions need. RequirePermission here is the ordinary RBAC gate
	// (mirroring PermBonusCampaignActivate/PermBonusOfferManage/
	// PermBonusGrantIssue/PermBonusBulkExecute's existing role wiring,
	// security-architecture.md §B1.1) - the four-eyes CONSUME itself is
	// enforced independently, inside the domain function, never inferred
	// from this permission check alone.
	mux.Handle("POST /v1/admin/bonus/campaigns/{campaignID}/activate",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusCampaignActivate)(newActivateCampaignHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/offers",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusOfferManage)(newCreateOfferHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/offers/{offerID}/versions",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusOfferManage)(newCreateOfferVersionHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/offers/{offerID}/publish",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusOfferManage)(newPublishOfferHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/manual-grants",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusGrantIssue)(newIssueManualGrantRequestHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/manual-grants/{grantID}/activate",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusGrantIssue)(newActivateManualGrantHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/bulk-jobs",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusBulkExecute)(newCreateBulkGrantJobHandler(deps)))))
	mux.Handle("POST /v1/admin/bonus/bulk-jobs/{jobID}/execute",
		staffRoute(deps, stepUpFinancial, auth.RequireTenantScope(auth.RequirePermission(auth.PermBonusBulkExecute)(newExecuteBulkGrantJobHandler(deps)))))
}
