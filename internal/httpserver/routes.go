package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerIdentityRoutes wires every Stage 2 identity/auth/admin endpoint
// into mux. Grouped here (rather than scattered across server.go) so the
// full route table - and which middleware chain guards each route - is
// visible in one place.
func registerIdentityRoutes(mux *http.ServeMux, deps Deps) {
	// Player auth - no bearer token required (that's the point: these
	// establish one).
	mux.HandleFunc("POST /v1/auth/register", newRegisterHandler(deps))
	mux.HandleFunc("POST /v1/auth/login", newLoginHandler(deps))
	mux.HandleFunc("POST /v1/auth/refresh", newRefreshHandler(deps))
	mux.HandleFunc("POST /v1/auth/logout", newLogoutHandler(deps))

	// Player self-service - requires a valid player access token.
	mux.Handle("GET /v1/me", auth.Middleware(deps.AuthIssuer)(newMeHandler(deps)))
	mux.Handle("GET /v1/me/sessions", auth.Middleware(deps.AuthIssuer)(newListSessionsHandler(deps)))
	mux.Handle("DELETE /v1/me/sessions/{id}", auth.Middleware(deps.AuthIssuer)(newRevokeSessionHandler(deps)))

	// Stage 4I Phase B: player self-service DECLARED residence read/write
	// (self-declared, unverified - HDR-J-3b) - see
	// internal/httpserver/player_residence_handlers.go's own doc comment.
	// No RequirePermission: a player always reads/writes their OWN
	// declared residence; jurisdiction_evidence_collection_active (checked
	// inside the handler, same transaction as the write) is what gates
	// whether the PUT is accepted at all.
	mux.Handle("GET /v1/me/residence", auth.Middleware(deps.AuthIssuer)(newGetMyResidenceHandler(deps)))
	mux.Handle("PUT /v1/me/residence", auth.Middleware(deps.AuthIssuer)(newSetMyResidenceHandler(deps)))

	// Staff auth - separate endpoint from player login (different
	// credential store, different tenant-resolution rule: platform_admin
	// omits tenant_slug entirely).
	mux.HandleFunc("POST /v1/staff/auth/login", newStaffLoginHandler(deps))

	// Platform-admin-only: tenant provisioning is inherently a
	// platform-level action (see docs/decisions/0011), never tenant-
	// scoped, so RequireTenantScope is deliberately NOT applied here -
	// it would lock out the only role permitted to call this.
	mux.Handle("POST /v1/admin/tenants",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermTenantWrite)(newCreateTenantHandler(deps))))

	// Stage 5 (Operator Back Office MVP): tenant list/detail. List is
	// platform-admin-only (checked inside the handler, same reasoning as
	// tenant creation - RequireTenantScope would reject the platform_admin's
	// nil-tenant token before the handler's own check could run); detail
	// uses canActOnTenant (platform_admin may read any tenant, a
	// tenant-scoped caller only its own).
	mux.Handle("GET /v1/admin/tenants",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermTenantRead)(newListTenantsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermTenantRead)(newGetTenantHandler(deps))))

	// Brand creation: platform_admin may target any tenant (via the path
	// parameter); tenant_admin may only target their own - enforced
	// explicitly inside the handler per ADR 0011, not by
	// RequireTenantScope (which would reject the platform_admin's
	// nil-tenant token before the handler's own, more precise check
	// could run).
	mux.Handle("POST /v1/admin/tenants/{tenantID}/brands",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermBrandWrite)(newCreateBrandHandler(deps))))

	// Stage 5 (Operator Back Office MVP): brand list/detail for a tenant -
	// same canActOnTenant authorization as brand creation.
	mux.Handle("GET /v1/admin/tenants/{tenantID}/brands",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermBrandRead)(newListBrandsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/brands/{brandID}",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermBrandRead)(newGetBrandHandler(deps))))

	// Staff creation: same platform_admin-may-target-any-tenant,
	// tenant_admin-only-their-own rule as brand creation.
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))

	// Stage 3D staff-person linkage remediation - lets an admin bring a
	// legacy unlinked staff account into compliance with the mandatory-
	// Person-linkage withdrawal-governance policy (docs/decisions/0024).
	// Same permission/tenant-targeting rule as staff creation.
	// Deliberately NOT usable to CHANGE an existing link -
	// staff_users.person_id is append-only at the database layer
	// (migration 0034) - only to set one where none exists yet.
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/person-link",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newLinkStaffPersonHandler(deps))))

	// Stage 4H-B0-R6 fix: the platform-scoped counterpart to the route
	// above, for a platform_admin (tenant_id IS NULL) staff account,
	// which the tenant-scoped route can never reach - see
	// newLinkPlatformStaffPersonHandler's own doc comment. Same
	// PermStaffManage gate; the handler itself further restricts the
	// CALLER to a platform-scoped principal (tenant_admin also holds
	// PermStaffManage but has no legitimate reason to touch a
	// platform-wide account).
	mux.Handle("POST /v1/admin/platform-staff/{staffID}/person-link",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newLinkPlatformStaffPersonHandler(deps))))

	// Player administration: tenant-scoped only in Stage 2 (a
	// platform_admin browsing an arbitrary tenant's players is deferred -
	// see docs/decisions/0011's "Consequences").
	mux.Handle("GET /v1/admin/players",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerRead)(newListPlayersHandler(deps)))))
	mux.Handle("GET /v1/admin/players/{id}",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerRead)(newGetPlayerHandler(deps)))))
	mux.Handle("POST /v1/admin/players/{id}/suspend",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerSuspend)(newSuspendPlayerHandler(deps)))))
	// Stage 5 (Operator Back Office MVP): the inverse of suspend - same
	// authorization tier (PermPlayerSuspend), same tenant scoping.
	mux.Handle("POST /v1/admin/players/{id}/reinstate",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerSuspend)(newReinstatePlayerHandler(deps)))))
	// Stage 4E: clear a player_account out of identity_review_required -
	// PermIdentityReviewManage (RoleCompliance only), never PermPlayerSuspend.
	mux.Handle("POST /v1/admin/players/{id}/identity-review/clear",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermIdentityReviewManage)(newClearIdentityReviewHandler(deps)))))

	// Audit trail: read-only, tenant-scoped.
	mux.Handle("GET /v1/admin/audit-log",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermAuditRead)(newListAuditLogHandler(deps)))))

	// Stage 5 (Operator Back Office MVP): the platform-scoped counterpart
	// to the tenant-scoped route above, for audit_log rows with
	// tenant_id IS NULL (AssignTenantLicence, tenant.created, and
	// operating-market audit rows per migration 0077) - see
	// newListPlatformAuditLogHandler's own doc comment. Deliberately NOT
	// wrapped in RequireTenantScope (which would reject the platform_admin's
	// nil-tenant token before the handler's own, more precise check could
	// run) - mirrors newCreateTenantHandler's own platform-scope pattern.
	mux.Handle("GET /v1/admin/platform/audit-log",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermAuditRead)(newListPlatformAuditLogHandler(deps))))
}
