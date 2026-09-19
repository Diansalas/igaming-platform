package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerJurisdictionRoutes wires Stage 4I's jurisdiction admin surface
// (docs/governance/stage-4i-canonical-model.md §11.1 items B-1/B-6).
//
// The route table is split exactly along the canonical model's own
// platform-vs-tenant line, mirroring registerAssetRegistryRoutes'
// identical split:
//
//   - /v1/admin/jurisdictions, /v1/admin/licences   PLATFORM-ONLY.
//     PermJurisdictionRegistryManage. Deliberately NOT wrapped in
//     RequireTenantScope: a platform_admin's token carries a nil
//     tenant_id, which RequireTenantScope would reject before the
//     handler ran.
//   - /v1/admin/jurisdiction-resolution-active/**   TENANT-SCOPED.
//     PermJurisdictionResolutionActiveWrite, RequireTenantScope, tenant
//     resolved from the token and never from the payload.
//
// Deliberately NOT exposed: any endpoint that RESOLVES a jurisdiction.
// internal/jurisdiction.Resolve takes tenant/brand/player as
// server-resolved values and is consumed in-process only - an HTTP
// endpoint accepting a jurisdiction identifier from a caller would be
// exactly the spoofable-input shape Stage 4H-B0-R5 finding S-6a (and
// this stage's own JV-1/JV-2 ruling) forbids.
func registerJurisdictionRoutes(mux *http.ServeMux, deps Deps) {
	// --- Platform-wide registries ---

	mux.Handle("GET /v1/admin/jurisdictions",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newListJurisdictionsHandler(deps))))
	mux.Handle("POST /v1/admin/jurisdictions",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newCreateJurisdictionHandler(deps))))

	mux.Handle("GET /v1/admin/licences",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newListLicencesHandler(deps))))
	mux.Handle("POST /v1/admin/licences",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newCreateLicenceHandler(deps))))

	// --- Tenant-scoped resolution-active fact (item B-6) ---

	mux.Handle("GET /v1/admin/jurisdiction-resolution-active",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermJurisdictionResolutionActiveWrite)(newListResolutionActiveHandler(deps)))))
	mux.Handle("PUT /v1/admin/jurisdiction-resolution-active/{operationClass}",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermJurisdictionResolutionActiveWrite)(newSetResolutionActiveHandler(deps)))))

	// --- Tenant-licence binding (Stage 4I Phase A) ---
	//
	// PLATFORM-ONLY, PermTenantLicenceAssign. Deliberately NOT wrapped in
	// RequireTenantScope, same reasoning as the jurisdictions/licences
	// registry block above: a platform_admin token carries a nil
	// tenant_id, which RequireTenantScope would reject before the handler
	// ran, and a platform_admin is explicitly the only role permitted to
	// target ANY tenant here (tenants has no RLS; the tenant id comes
	// from the path, never the body).
	mux.Handle("PUT /v1/admin/tenants/{tenantID}/licence",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermTenantLicenceAssign)(newAssignTenantLicenceHandler(deps))))
}
