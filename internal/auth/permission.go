package auth

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Permission is a fine-grained, named capability. Routes are gated by
// permission, not by hardcoding a list of roles per route - this is the
// "permission-oriented design" the Stage 2 instructions ask for, kept to
// a small, curated set actually used by something in this codebase
// (per CLAUDE.md's scope-expansion test) rather than a speculative
// enterprise-wide permission catalog.
type Permission string

const (
	PermTenantRead    Permission = "tenant:read"
	PermTenantWrite   Permission = "tenant:write"
	PermBrandRead     Permission = "brand:read"
	PermBrandWrite    Permission = "brand:write"
	PermPlayerRead    Permission = "player:read"
	PermPlayerSuspend Permission = "player:suspend"
	PermAuditRead     Permission = "audit:read"
	PermStaffManage   Permission = "staff:manage"

	// PermWithdrawalApprove gates the withdrawal four-eyes approval/
	// rejection endpoints and the staff withdrawal queue (Stage 3B). The
	// approving PRINCIPAL still has to satisfy internal/withdrawal's own
	// distinct-approver/non-beneficiary checks - this permission only
	// answers "may this role approve withdrawals at all", not "may this
	// specific call succeed" (withdrawal-state-machine.md §5).
	PermWithdrawalApprove Permission = "withdrawal:approve"
	// PermProviderConfigWrite gates writing a tenant's ProviderCapability
	// configuration rows (docs/decisions/0022 §2.1 - a platform-level
	// administrative action, always audited).
	PermProviderConfigWrite Permission = "provider_config:write"
)

// rolePermissions is a static, in-code role -> permission-set mapping.
// Stage 2 does not make this database-driven/partner-configurable - that
// would be a Stage 6 partner-console feature (custom roles), premature
// before there's a real multi-tenant admin surface to configure it from.
var rolePermissions = map[Role]map[Permission]bool{
	RolePlatformAdmin: permSet(
		PermTenantRead, PermTenantWrite, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
	),
	RoleTenantAdmin: permSet(
		PermTenantRead, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
		PermWithdrawalApprove, PermProviderConfigWrite,
	),
	RoleSupport: permSet(
		PermPlayerRead,
	),
	RoleCompliance: permSet(
		PermPlayerRead, PermPlayerSuspend, PermAuditRead,
	),
	// finance is Stage 3B's own role: withdrawal four-eyes approval is
	// exactly the capability the Stage 2 instructions predicted this role
	// would need.
	RoleFinance: permSet(
		PermWithdrawalApprove,
	),
	RolePlayer: permSet(),
}

func permSet(perms ...Permission) map[Permission]bool {
	m := make(map[Permission]bool, len(perms))
	for _, p := range perms {
		m[p] = true
	}
	return m
}

// RoleHasPermission reports whether role includes perm. An unknown role
// has no permissions (fails closed).
func RoleHasPermission(role Role, perm Permission) bool {
	return rolePermissions[role][perm]
}

// RequirePermission denies a request unless the authenticated caller's
// role includes perm. This replaces Stage 1's role-list-based
// RequireRole - permission checks are enforced server-side and are never
// satisfied by anything the requesting UI does or doesn't show.
func RequirePermission(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := observability.RequestIDFromContext(r.Context())
			tc, err := tenant.FromContext(r.Context())
			if err != nil {
				apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
				return
			}
			if !RoleHasPermission(Role(tc.Role), perm) {
				apierror.Write(w, requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireTenantScope denies a request unless the authenticated caller
// has a specific (non-nil) tenant_id. Platform-scoped principals (see
// docs/decisions/0011-platform-scoped-identity-tokens.md) hold a
// nil-tenant token and must be denied here, before a handler that
// assumes a real tenant id (e.g. to call db.WithTenant) ever runs.
func RequireTenantScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
			return
		}
		if tc.TenantID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this operation requires a tenant-scoped identity")
			return
		}
		next.ServeHTTP(w, r)
	})
}
