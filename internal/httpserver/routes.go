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
	// establish one). Register/login/refresh are additionally wrapped in
	// the Stage 9 §21 per-IP limiter: they are unauthenticated, they are
	// the platform's brute-force/credential-stuffing surface, and
	// register/login each pay a full ~64 MiB Argon2id cost per request
	// before any credential has been proven. See ratelimit.go for the
	// full rationale and for what this limiter deliberately is NOT.
	// Logout is not limited - it proves possession of an unguessable
	// refresh token, costs one indexed hash lookup, and refusing it would
	// only keep a session alive that its owner asked to end.
	mux.Handle("POST /v1/auth/register",
		rateLimitFunc(deps.authLimiter, rateBucketRegister, rateLimitRegisterPerMin, newRegisterHandler(deps)))
	mux.Handle("POST /v1/auth/login",
		rateLimitFunc(deps.authLimiter, rateBucketLogin, rateLimitLoginPerMinute, newLoginHandler(deps)))
	mux.Handle("POST /v1/auth/refresh",
		rateLimitFunc(deps.authLimiter, rateBucketRefresh, rateLimitRefreshPerMin, newRefreshHandler(deps)))
	mux.HandleFunc("POST /v1/auth/logout", newLogoutHandler(deps))

	// Player self-service - requires a valid player access token.
	mux.Handle("GET /v1/me", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newMeHandler(deps))))
	// The two session routes are deliberately NOT auth.RequirePlayerPrincipal-
	// gated, unlike every other /v1/me route: "which devices am I logged in
	// on, and log that one out" is a self-service capability every principal
	// type owns over its OWN sessions (newListSessionsHandler/
	// auth.RevokeSession both scope strictly to the caller's own
	// principal_type + principal_id, and sessions' RLS additionally requires
	// app.principal_id to match). A staff user managing their own sessions is
	// the intended behaviour here, not an authorization gap. ADR 0119 S4a: a
	// STAFF caller must still hold a live aal2 session (liveIfStaff); player
	// callers are unchanged.
	mux.Handle("GET /v1/me/sessions", liveIfStaff(deps, newListSessionsHandler(deps)))
	mux.Handle("DELETE /v1/me/sessions/{id}", liveIfStaff(deps, newRevokeSessionHandler(deps)))

	// Stage 4I Phase B: player self-service DECLARED residence read/write
	// (self-declared, unverified - HDR-J-3b) - see
	// internal/httpserver/player_residence_handlers.go's own doc comment.
	// No RequirePermission: a player always reads/writes their OWN
	// declared residence; jurisdiction_evidence_collection_active (checked
	// inside the handler, same transaction as the write) is what gates
	// whether the PUT is accepted at all.
	mux.Handle("GET /v1/me/residence", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newGetMyResidenceHandler(deps))))
	mux.Handle("PUT /v1/me/residence", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newSetMyResidenceHandler(deps))))

	// Staff auth - separate endpoints from player login (different
	// credential store, different tenant-resolution rule: platform_admin
	// omits tenant_slug entirely). ADR 0119 S3: two-step login with a
	// mandatory second factor - see staff_auth_routes.go.
	registerStaffAuthRoutes(mux, deps)

	// Platform-admin-only: tenant provisioning is inherently a
	// platform-level action (see docs/decisions/0011), never tenant-
	// scoped, so RequireTenantScope is deliberately NOT applied here -
	// it would lock out the only role permitted to call this.
	mux.Handle("POST /v1/admin/tenants",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermTenantWrite)(newCreateTenantHandler(deps))))

	// Stage 5 (Operator Back Office MVP): tenant list/detail. List is
	// platform-admin-only (checked inside the handler, same reasoning as
	// tenant creation - RequireTenantScope would reject the platform_admin's
	// nil-tenant token before the handler's own check could run); detail
	// uses canActOnTenant (platform_admin may read any tenant, a
	// tenant-scoped caller only its own).
	mux.Handle("GET /v1/admin/tenants",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermTenantRead)(newListTenantsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermTenantRead)(newGetTenantHandler(deps))))

	// Brand creation: platform_admin may target any tenant (via the path
	// parameter); tenant_admin may only target their own - enforced
	// explicitly inside the handler per ADR 0011, not by
	// RequireTenantScope (which would reject the platform_admin's
	// nil-tenant token before the handler's own, more precise check
	// could run).
	mux.Handle("POST /v1/admin/tenants/{tenantID}/brands",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermBrandWrite)(newCreateBrandHandler(deps))))

	// Stage 5 (Operator Back Office MVP): brand list/detail for a tenant -
	// same canActOnTenant authorization as brand creation.
	mux.Handle("GET /v1/admin/tenants/{tenantID}/brands",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermBrandRead)(newListBrandsHandler(deps))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/brands/{brandID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermBrandRead)(newGetBrandHandler(deps))))

	// ADR 0119 S5 + S6: staff lifecycle (invitations replace the removed admin-chosen-password creation route
	// POST /v1/admin/tenants/{tenantID}/staff; directory, suspend / reactivate / offboard / role / MFA reset, the
	// platform-staff four-eyes and the invitation acceptance) - staff_lifecycle_routes.go.
	registerStaffLifecycleRoutes(mux, deps)

	// Stage 3D staff-person linkage remediation - lets an admin bring a
	// legacy unlinked staff account into compliance with the mandatory-
	// Person-linkage withdrawal-governance policy (docs/decisions/0024).
	// Same permission/tenant-targeting rule as staff creation.
	// Deliberately NOT usable to CHANGE an existing link -
	// staff_users.person_id is append-only at the database layer
	// (migration 0034) - only to set one where none exists yet.
	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff/{staffID}/person-link",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLinkStaffPersonHandler(deps))))

	// Stage 4H-B0-R6 fix: the platform-scoped counterpart to the route
	// above, for a platform_admin (tenant_id IS NULL) staff account,
	// which the tenant-scoped route can never reach - see
	// newLinkPlatformStaffPersonHandler's own doc comment. Same
	// PermStaffManage gate; the handler itself further restricts the
	// CALLER to a platform-scoped principal (tenant_admin also holds
	// PermStaffManage but has no legitimate reason to touch a
	// platform-wide account).
	mux.Handle("POST /v1/admin/platform-staff/{staffID}/person-link",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newLinkPlatformStaffPersonHandler(deps))))

	// Player administration: tenant-scoped only in Stage 2 (a
	// platform_admin browsing an arbitrary tenant's players is deferred -
	// see docs/decisions/0011's "Consequences").
	mux.Handle("GET /v1/admin/players",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerRead)(newListPlayersHandler(deps)))))
	mux.Handle("GET /v1/admin/players/{id}",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerRead)(newGetPlayerHandler(deps)))))
	mux.Handle("POST /v1/admin/players/{id}/suspend",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerSuspend)(newSuspendPlayerHandler(deps)))))
	// Stage 5 (Operator Back Office MVP): the inverse of suspend - same
	// authorization tier (PermPlayerSuspend), same tenant scoping.
	mux.Handle("POST /v1/admin/players/{id}/reinstate",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermPlayerSuspend)(newReinstatePlayerHandler(deps)))))
	// Stage 4E: clear a player_account out of identity_review_required -
	// PermIdentityReviewManage (RoleCompliance only), never PermPlayerSuspend.
	mux.Handle("POST /v1/admin/players/{id}/identity-review/clear",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermIdentityReviewManage)(newClearIdentityReviewHandler(deps)))))

	// Audit trail: read-only, tenant-scoped.
	mux.Handle("GET /v1/admin/audit-log",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermAuditRead)(newListAuditLogHandler(deps)))))

	// Stage 5 (Operator Back Office MVP): the platform-scoped counterpart
	// to the tenant-scoped route above, for audit_log rows with
	// tenant_id IS NULL (AssignTenantLicence, tenant.created, and
	// operating-market audit rows per migration 0077) - see
	// newListPlatformAuditLogHandler's own doc comment. Deliberately NOT
	// wrapped in RequireTenantScope (which would reject the platform_admin's
	// nil-tenant token before the handler's own, more precise check could
	// run) - mirrors newCreateTenantHandler's own platform-scope pattern.
	mux.Handle("GET /v1/admin/platform/audit-log",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAuditRead)(newListPlatformAuditLogHandler(deps))))

	// PRH-2 G1 (ADR 0104 §5.1, KS-AUDIT-TENANT-1): a tenant's read-only
	// projection of platform-scope audit rows that concern it (today:
	// kill-switch actions on that tenant - migration 0109's
	// subject_tenant_id/subject_tenant_read RLS policy). RequireTenantScope
	// (unlike the platform route above) since this is, by definition, only
	// ever meaningful for a tenant-scoped caller. Additive: the two routes
	// above are unchanged (AT-7).
	mux.Handle("GET /v1/admin/audit-log/platform-actions",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermAuditRead)(newListPlatformActionsAuditLogHandler(deps)))))

	// PRH-2 G1 (ADR 0104 §3/§5.4; security confirmation N-2): the
	// display_name write path. "Another user" is PermStaffManage +
	// canActOnTenant (tenant callers limited to their own tenant's staff);
	// self-rename uses only the verified token subject and needs no
	// staff-management permission (any authenticated staff principal may
	// rename themselves).
	mux.Handle("PATCH /v1/admin/tenants/{tenantID}/staff/{staffID}/display-name",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermStaffManage)(newUpdateStaffDisplayNameHandler(deps))))
	mux.Handle("PATCH /v1/admin/staff/me/display-name",
		staffRoute(deps, stepUpNone, newSelfUpdateStaffDisplayNameHandler(deps)))

	// PRH-2 K1 (ADR 0099): the scoped financial capability grant admin
	// API - see capability_routes.go's own doc comment.
	registerCapabilityRoutes(mux, deps)
	registerManualAdjustmentRoutes(mux, deps)
	registerPaymentForceResolutionRoutes(mux, deps)
	// ADR 0114 S2/S4: read-only payment attempt and reconciliation views (R1, R2).
	registerPaymentAttemptReadRoutes(mux, deps)
	registerStaffReconciliationRoutes(mux, deps)
	registerWithdrawalHoldResolutionRoutes(mux, deps) // HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 s6, migration 0124)
	registerWithdrawalHoldCandidateRoutes(mux, deps)  // A10c: R3 hold-candidate list (read only)
	// B13 (ADR 0111 2.9): payout instruments.
	registerPayoutInstrumentRoutes(mux, deps)
	// ADR 0112 slice 4: launch authorisation (tenant and brand status governance).
	registerLaunchAuthorisationRoutes(mux, deps)
	// Migration 0177 (MKT-DUAL-1): licensing / jurisdiction change requests (two-person approval primitive).
	registerLicensingChangeRoutes(mux, deps)
	registerAMLRoutes(mux, deps)
}
