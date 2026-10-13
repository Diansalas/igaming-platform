package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerRGRoutes wires every Stage 4D-RG Responsible Gaming endpoint.
// Mirrors registerCasinoRoutes' pattern. Explicitly NOT exposed this
// stage: any "end restriction early" mutation, a cross-tenant
// staff-initiated restriction, or any control besides self-exclusion
// (ADR 0026 §15's documented extension points). Since ADR 0115 slice S2
// the limit, time-out and RG-configuration WRITE APIs are mounted by
// registerRGLimitRoutes (rg_limits_routes.go), called at the end of this
// function; reality checks remain unmounted.
func registerRGRoutes(mux *http.ServeMux, deps Deps) {
	// Player self-service: create my own platform-wide self-exclusion, and
	// read my own restriction status. No permission gate beyond ordinary
	// authentication - a player has an inherent right to self-exclude and
	// to see their own status; RLS (migration 0037's player_self_*
	// policies), not RBAC, is what authorizes both.
	mux.Handle("POST /v1/me/rg/self-exclusion", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newCreateSelfExclusionHandler(deps))))
	mux.Handle("GET /v1/me/rg/status", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newGetMyRGStatusHandler(deps))))

	// Staff/admin: create and read a tenant/brand-scoped restriction for a
	// named player_account_id. Both RequireTenantScope-gated - every
	// current grantee (RoleCompliance for write, RoleCompliance/
	// RoleTenantAdmin for read) is a tenant-scoped role; see ADR 0026 §12
	// for why no platform-scoped staff-initiated path exists yet.
	mux.Handle("POST /v1/admin/rg/restrictions",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermRGRestrictionWrite)(newCreateStaffRestrictionHandler(deps)))))
	mux.Handle("GET /v1/admin/rg/restrictions",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermRGRestrictionRead)(newListRestrictionsForAccountHandler(deps)))))

	// ADR 0115 slice S2: RG limit, time-out and configuration write APIs.
	registerRGLimitRoutes(mux, deps)
}
