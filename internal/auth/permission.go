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

	// PermWithdrawalReview/Approve/Reject/Submit gate the four distinct
	// withdrawal-governance capabilities the Stage 3D directive requires
	// be separable (business decision #4/#5: "staff-management authority
	// and withdrawal-approval authority must be permission-separated,"
	// and no broad admin role may hold withdrawal authority merely by
	// being an admin - only by explicit grant). Stage 3B originally
	// collapsed all four into one PermWithdrawalApprove; splitting them
	// lets a future role hold, say, review-only visibility without
	// approval power, and makes "which roles can move money" a single,
	// auditable set of map entries rather than one broad permission.
	//
	// Holding one of these still only answers "may this role attempt
	// this action at all" - the approving/rejecting/submitting PRINCIPAL
	// additionally has to satisfy internal/withdrawal's own distinct-
	// approver/non-beneficiary/Person-linkage/active-status checks
	// (withdrawal-state-machine.md §5, ADR 0024) before any specific
	// call actually succeeds.
	PermWithdrawalReview  Permission = "withdrawal:review"
	PermWithdrawalApprove Permission = "withdrawal:approve"
	PermWithdrawalReject  Permission = "withdrawal:reject"
	PermWithdrawalSubmit  Permission = "withdrawal:submit"
	// PermProviderConfigWrite gates writing a tenant's ProviderCapability
	// configuration rows (docs/decisions/0022 §2.1 - a platform-level
	// administrative action, always audited).
	PermProviderConfigWrite Permission = "provider_config:write"

	// PermWithdrawalPolicyWrite gates the Stage 3D withdrawal_policies
	// admin API (docs/decisions/0024 §5). Deliberately its own
	// permission, never bundled with PermWithdrawalApprove: the role
	// that approves withdrawals must not also be the role that can
	// loosen the policy gating its own approvals (e.g. lowering a
	// threshold or turning off step-up immediately before approving,
	// then reverting it - withdrawal-state-machine.md §5 bypass #3).
	PermWithdrawalPolicyWrite Permission = "withdrawal_policy:write"

	// PermCasinoConfigWrite gates a tenant's own casino integration
	// configuration (Stage 4A, ADR 0025 §4/§11): writing a
	// CasinoProviderCapability row (which providers/assets/game types this
	// tenant routes to) and toggling a platform-catalogue title's
	// tenant/brand availability. Tenant-level routing configuration, not a
	// money-moving action, so it is bundled with the tenant's other
	// provider/config permissions rather than split out per sub-action -
	// mirrors PermProviderConfigWrite's identical scope for payments.
	PermCasinoConfigWrite Permission = "casino_config:write"

	// PermCasinoCatalogueManage gates registering/updating a title in the
	// PLATFORM-WIDE game catalogue (casino_games - ADR 0025 §2). Deliberately
	// its own, platform-only permission, never granted to RoleTenantAdmin:
	// a tenant may opt into a title the platform has already vetted
	// (PermCasinoConfigWrite) but must never be able to add an unvetted
	// title to the shared catalogue every other tenant can then also see.
	PermCasinoCatalogueManage Permission = "casino_catalogue:manage"

	// PermRGRestrictionWrite gates creating a staff-initiated Responsible
	// Gaming restriction (Stage 4D-RG, ADR 0026 §12) - today, only
	// self-exclusion, always scoped to the caller's own tenant/brand (never
	// platform-wide - see internal/rg.CreateStaffRestriction's own doc
	// comment for why). Deliberately its own permission, NEVER bundled into
	// PermPlayerSuspend/PermStaffManage/PermTenantWrite: the directive's own
	// explicit instruction is that this must not be automatically granted to
	// every broad administrator (RoleTenantAdmin does NOT get it, mirroring
	// the withdrawal-approval precedent - ADR 0024's identical separation-
	// of-duties rationale). Granted only to RoleCompliance - migration
	// 0037's staff_insert RLS policy independently enforces the
	// own-tenant-only scope at the database, not just here.
	PermRGRestrictionWrite Permission = "rg_restriction:write"
	// PermRGRestrictionRead gates reading another player's Responsible
	// Gaming restriction history (a player always sees their OWN via the
	// self-service endpoint regardless of this permission - RLS's
	// player_self_read policy, not RBAC, is what authorizes that).
	PermRGRestrictionRead Permission = "rg_restriction:read"

	// PermIdentityReviewManage gates clearing a player_account out of the
	// 'identity_review_required' status Stage 4E introduced (ADR 0027
	// §6/§8) - a security-sensitive decision identical in shape to
	// PermRGRestrictionWrite (Stage 4D-RG, ADR 0026 §12): clearing a
	// review incorrectly is exactly as capable of enabling self-exclusion
	// evasion as writing a restriction incorrectly, so it gets the SAME
	// separation-of-duties treatment - its own dedicated permission,
	// deliberately never bundled into PermPlayerSuspend/PermStaffManage/
	// PermTenantWrite, and NOT granted to RoleTenantAdmin or
	// RolePlatformAdmin. Granted only to RoleCompliance.
	PermIdentityReviewManage Permission = "identity_review:manage"

	// PermVerificationRead gates STAFF read access to another player's
	// KYC verification/document metadata (Stage 4F) - a player always
	// sees their OWN via the self-service endpoints regardless of this
	// permission, exactly like PermRGRestrictionRead's own precedent.
	// Granted to RoleCompliance and RoleTenantAdmin (an operational admin
	// reasonably needs to see a player's verification state), never
	// RolePlatformAdmin (no path to resolve a tenant-scoped
	// player_account at all - identical reasoning to every other
	// platform-admin exclusion in this file) and never RoleFinance
	// (directive §19's explicit "do not grant sensitive verification
	// access to Finance... unless explicitly justified" - no
	// justification exists).
	PermVerificationRead Permission = "verification:read"
	// PermVerificationReview gates APPROVING/REJECTING a verification or
	// document (Stage 4F) - deliberately its own permission, never
	// bundled into PermVerificationRead/PermPlayerSuspend/PermStaffManage,
	// mirroring PermRGRestrictionWrite/PermIdentityReviewManage's
	// identical separation-of-duties precedent exactly. Granted ONLY to
	// RoleCompliance - not RoleTenantAdmin (read-only visibility is
	// reasonable for an operational admin; approving/rejecting identity
	// evidence is a compliance-specific judgment call), not RoleFinance,
	// not RolePlatformAdmin.
	PermVerificationReview Permission = "verification:review"
)

// rolePermissions is a static, in-code role -> permission-set mapping.
// Stage 2 does not make this database-driven/partner-configurable - that
// would be a Stage 6 partner-console feature (custom roles), premature
// before there's a real multi-tenant admin surface to configure it from.
var rolePermissions = map[Role]map[Permission]bool{
	RolePlatformAdmin: permSet(
		PermTenantRead, PermTenantWrite, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
		PermCasinoCatalogueManage,
		// Deliberately NOT PermRGRestrictionWrite/Read (Stage 4D-RG, ADR
		// 0026 §12): platform_admin cannot resolve a specific tenant's
		// player_account at all today (PermPlayerRead is itself
		// RequireTenantScope-gated, and player_accounts' own RLS has no
		// platform-wide read policy), so it has no way to legitimately
		// exercise either permission - granting it would be exactly the
		// "capability nothing can actually use" CLAUDE.md's "no fake
		// completion" rule warns against. See CreateStaffRestriction's own
		// doc comment for the full reasoning and the recorded OPEN DECISION.
	),
	// Stage 3D business decision #4/#5: tenant_admin (a broad
	// administrative role that also holds PermStaffManage) deliberately
	// does NOT get any withdrawal-governance permission. Holding both
	// staff-management and withdrawal-approval authority in one role was
	// exactly the privilege-escalation path Stage 3C's specialist review
	// found (a tenant_admin could mint unlinked "finance" staff accounts
	// via PermStaffManage, then approve through them).
	//
	// Removing the grant here alone does NOT fully close that path -
	// Stage 3D's own specialist review (security/ledger-finance/architect,
	// independently) found that a tenant_admin retaining PermStaffManage
	// can still mint a BRAND NEW finance-role staff account (choosing its
	// password and person_id) and log in as it, since finance's
	// permissions come from ITS OWN role, not tenant_admin's. What
	// actually closes it is internal/httpserver/admin_routes.go's
	// newCreateStaffHandler restricting role="finance" creation to a
	// platform-scoped caller (platform_admin) - see that function's own
	// doc comment and ADR 0024's residual-findings section. This role-
	// permission removal remains necessary (it is what stops a
	// tenant_admin from acting AS tenant_admin on a withdrawal) but is not
	// sufficient on its own; the two together are what achieve the
	// decision. If a tenant genuinely needs one human to both administer
	// staff and approve withdrawals, that requires two separate role
	// grants on two separate accounts, never one role bundling both.
	RoleTenantAdmin: permSet(
		PermTenantRead, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
		PermProviderConfigWrite, PermWithdrawalPolicyWrite, PermCasinoConfigWrite,
		// PermRGRestrictionRead only, deliberately NOT PermRGRestrictionWrite -
		// Stage 4D-RG's own explicit instruction (mirroring Stage 3D business
		// decision #4/#5's withdrawal-approval precedent exactly): a broad
		// tenant administrator must never get RG-restriction WRITE authority
		// automatically just for holding PermStaffManage/PermPlayerSuspend.
		// Read-only visibility into restrictions affecting their own tenant's
		// players remains reasonable for an operational admin role.
		PermRGRestrictionRead,
		// Stage 4F: read-only verification/document visibility, same
		// reasoning as PermRGRestrictionRead immediately above - never
		// PermVerificationReview.
		PermVerificationRead,
	),
	RoleSupport: permSet(
		PermPlayerRead,
	),
	RoleCompliance: permSet(
		PermPlayerRead, PermPlayerSuspend, PermAuditRead,
		// The sole tenant-scoped grantee of PermRGRestrictionWrite (Stage
		// 4D-RG, ADR 0026 §12) - a tenant/brand-scoped restriction only
		// (migration 0037's staff_insert RLS policy rejects a platform-wide
		// row from any tenant-scoped connection); a genuinely platform-wide
		// restriction additionally requires RolePlatformAdmin.
		PermRGRestrictionWrite, PermRGRestrictionRead,
		// Stage 4E: the sole grantee of PermIdentityReviewManage - see that
		// permission's own doc comment for why it mirrors
		// PermRGRestrictionWrite's separation-of-duties treatment exactly.
		PermIdentityReviewManage,
		// Stage 4F: the sole grantee of PermVerificationReview, plus
		// read visibility - see both permissions' own doc comments.
		PermVerificationRead, PermVerificationReview,
	),
	// finance is Stage 3B's own role, dedicated solely to withdrawal
	// governance - it holds all four withdrawal permissions and nothing
	// else, and deliberately does NOT hold PermStaffManage (the other
	// half of Stage 3D's required separation).
	RoleFinance: permSet(
		PermWithdrawalReview, PermWithdrawalApprove, PermWithdrawalReject, PermWithdrawalSubmit,
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
