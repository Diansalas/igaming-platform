package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerAssetRegistryRoutes wires Stage 4H-B0-R6's Asset Registry and
// Asset Authorization admin API (ADR 0037 §C.5.1's nine canonical
// operations, plus the two four-eyes support operations §C.5.3 requires).
//
// The route table is split exactly along ADR 0037 §C.1's two-tier line,
// and the split is visible here rather than buried in handlers:
//
//   - /v1/admin/assets/**            layers 1-3. PermAssetRegistryManage,
//     platform-only. Deliberately NOT wrapped in RequireTenantScope: a
//     platform_admin's token carries a nil tenant_id, which
//     RequireTenantScope would reject before the handler ran - the same
//     reasoning registerCasinoRoutes gives for the platform-wide game
//     catalogue and routes.go gives for tenant provisioning.
//   - /v1/admin/asset-authorizations/**  layers 4-7.
//     PermAssetAuthorizationWrite, RequireTenantScope, tenant resolved
//     from the token and never from the payload.
//
// Deliberately NOT exposed: any HTTP endpoint that EVALUATES eligibility.
// AssetAuthorization.CheckEligibility takes tenant/brand/jurisdiction as
// server-resolved values, and no per-player jurisdiction resolver exists
// anywhere in this codebase yet (Stage 4G-FINAL Part C). An endpoint
// accepting a jurisdiction identifier from a caller would be exactly the
// spoofable input Stage 4H-B0-R5 finding S-6a warns about, so the check
// is available to in-process domains only, via the Go service.
func registerAssetRegistryRoutes(mux *http.ServeMux, deps Deps) {
	// --- Layers 1-3: platform-admin only ---

	mux.Handle("GET /v1/admin/assets",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newListAssetsHandler(deps))))

	// Four-eyes: file a change request, then have a DIFFERENT platform
	// principal decide it. Both are prerequisites for create/activate/
	// platform-authorize, never optional paperwork around them.
	mux.Handle("POST /v1/admin/assets/change-requests",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newFileAssetChangeRequestHandler(deps))))
	mux.Handle("POST /v1/admin/assets/change-requests/{id}/decision",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newDecideAssetChangeRequestHandler(deps))))

	mux.Handle("POST /v1/admin/assets",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newCreateAssetHandler(deps))))
	mux.Handle("PATCH /v1/admin/assets/{code}",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newUpdateAssetMetadataHandler(deps))))
	mux.Handle("PUT /v1/admin/assets/{code}/activation",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newSetAssetActivationHandler(deps))))
	mux.Handle("PUT /v1/admin/assets/{code}/platform-authorization",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newSetAssetPlatformAuthorizationHandler(deps))))
	// The platform-wide layer-7 default row (tenant_id NULL, ADR 0037
	// §A.6) - platform-only, unlike the tenant override below.
	mux.Handle("PUT /v1/admin/assets/{code}/operation-eligibility",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAssetRegistryManage)(newSetPlatformOperationEligibilityHandler(deps))))

	// --- Layers 4-7: tenant-scoped ---

	mux.Handle("GET /v1/admin/asset-authorizations",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermAssetAuthorizationWrite)(newListAssetAuthorizationsHandler(deps)))))
	mux.Handle("PUT /v1/admin/asset-authorizations/{scopeKind}",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermAssetAuthorizationWrite)(newAuthorizeAssetScopeHandler(deps)))))
	mux.Handle("PUT /v1/admin/asset-authorizations/operation-eligibility/{code}",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermAssetAuthorizationWrite)(newSetTenantOperationEligibilityHandler(deps)))))
}
