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

	// Reads (lic4 slice 4a): PermJurisdictionRegistryRead (platform_admin), step-up none.
	mux.Handle("GET /v1/admin/jurisdictions",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newListJurisdictionsHandler(deps))))
	mux.Handle("GET /v1/admin/jurisdictions/{jurisdictionID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newGetJurisdictionHandler(deps))))
	mux.Handle("POST /v1/admin/jurisdictions",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newCreateJurisdictionHandler(deps))))

	mux.Handle("GET /v1/admin/licences",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newListLicencesHandler(deps))))
	mux.Handle("GET /v1/admin/licences/{licenceID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newGetLicenceHandler(deps))))
	mux.Handle("GET /v1/admin/licence-country-ceilings",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newListLicenceCountryCeilingsHandler(deps))))
	mux.Handle("GET /v1/admin/operating-country-policies",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newListOperatingCountryPoliciesHandler(deps))))
	// lic9 (migration 0180): the explicit operating bases (platform_admin read; writes only through licensing changes).
	mux.Handle("GET /v1/admin/operating-bases",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newListOperatingBasesHandler(deps))))
	mux.Handle("GET /v1/admin/operating-bases/{basisID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newGetOperatingBasisHandler(deps))))
	// c2: TENANT-scoped read of the caller's OWN operating bases (PermOperatingBasisTenantRead, RequireTenantScope; the tenant is the
	// authenticated context's, there is no tenant parameter). Redacted projection; see tenant_operating_basis_read_handlers.go.
	mux.Handle("GET /v1/admin/tenant/operating-bases",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermOperatingBasisTenantRead)(newListOwnOperatingBasesHandler(deps)))))
	mux.Handle("GET /v1/admin/tenant/operating-bases/{basisID}",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermOperatingBasisTenantRead)(newGetOwnOperatingBasisHandler(deps)))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/jurisdiction-configs",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newListTenantJurisdictionConfigsHandler(deps))))
	mux.Handle("POST /v1/admin/licences",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newCreateLicenceHandler(deps))))

	// --- Licence lifecycle (licensing slice 1, migration 0176) ---
	//
	// PLATFORM-ONLY, PermJurisdictionRegistryManage, step-up critical on every write. Single actor in this slice
	// (maker-checker for widening changes is slice 2); the licence id comes from the path.
	mux.Handle("PUT /v1/admin/licences/{licenceID}/status",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newSetLicenceStatusHandler(deps))))
	mux.Handle("PUT /v1/admin/licences/{licenceID}/validity-dates",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newSetLicenceValidityDatesHandler(deps))))
	mux.Handle("PUT /v1/admin/licences/{licenceID}/review-due",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newSetLicenceReviewDueHandler(deps))))
	mux.Handle("PUT /v1/admin/licences/{licenceID}/evidence-reference",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermJurisdictionRegistryManage)(newSetLicenceEvidenceReferenceHandler(deps))))
	mux.Handle("GET /v1/admin/licences/{licenceID}/versions",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermJurisdictionRegistryRead)(newListLicenceVersionsHandler(deps))))

	// --- Tenant-scoped resolution-active fact (item B-6) ---

	mux.Handle("GET /v1/admin/jurisdiction-resolution-active",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermJurisdictionResolutionActiveWrite)(newListResolutionActiveHandler(deps)))))
	mux.Handle("PUT /v1/admin/jurisdiction-resolution-active/{operationClass}",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermJurisdictionResolutionActiveWrite)(newSetResolutionActiveHandler(deps)))))

	// --- Tenant-scoped evidence-collection-active fact (Stage 4I Phase B) ---
	//
	// Same shape as the resolution-active block immediately above -
	// TENANT-SCOPED, RequireTenantScope, tenant resolved from the token and
	// never from the payload - but gated by
	// PermJurisdictionEvidenceCollectionActivate rather than
	// PermJurisdictionResolutionActiveWrite (RoleCompliance-only, never
	// RoleTenantAdmin: see that permission's own doc comment in
	// internal/auth/permission.go for why). The GET reuses the PUT's own
	// write permission rather than a separate read permission, mirroring
	// the resolution-active block's identical choice above.
	mux.Handle("GET /v1/admin/jurisdiction-evidence-collection",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermJurisdictionEvidenceCollectionActivate)(newListEvidenceCollectionActiveHandler(deps)))))
	mux.Handle("PUT /v1/admin/jurisdiction-evidence-collection/{evidenceType}",
		staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermJurisdictionEvidenceCollectionActivate)(newSetEvidenceCollectionActiveHandler(deps)))))

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
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermTenantLicenceAssign)(newAssignTenantLicenceHandler(deps))))
}
