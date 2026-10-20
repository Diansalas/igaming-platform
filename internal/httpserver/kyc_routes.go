package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerKYCRoutes wires Stage 4F's player verification/document
// endpoints and the staff/compliance review surface. Mirrors
// registerRGRoutes' pattern exactly: player self-service needs no
// permission beyond ordinary authentication (an inherent right to manage
// one's own verification/documents); staff routes are permission-gated
// and RequireTenantScope'd, identical to every other tenant-scoped admin
// surface in this codebase. The provider webhook has no bearer-token
// middleware at all - identical rationale to newCasinoWebhookHandler/
// newPaymentWebhookHandler (a provider is not an authenticated platform
// principal; tenant resolution and payload authentication are the
// handler's own responsibility).
func registerKYCRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("POST /v1/me/kyc/verifications", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newCreateMyVerificationHandler(deps))))
	mux.Handle("GET /v1/me/kyc/verifications", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newListMyVerificationsHandler(deps))))
	mux.Handle("POST /v1/me/kyc/documents", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newUploadMyDocumentHandler(deps))))
	mux.Handle("GET /v1/me/kyc/documents", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newListMyDocumentsHandler(deps))))
	mux.Handle("GET /v1/me/kyc/documents/{id}/content", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newGetMyDocumentContentHandler(deps))))

	mux.Handle("GET /v1/admin/kyc/verifications",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newListVerificationsForAccountHandler(deps)))))
	// Stage 5 Back Office: the tenant-wide case queue (no player_account_id
	// required) - see newListKYCCasesHandler's own doc comment.
	mux.Handle("GET /v1/admin/kyc/cases",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newListKYCCasesHandler(deps)))))
	mux.Handle("POST /v1/admin/kyc/verifications/{id}/review",
		staffRoute(deps, stepUpByKYCResidence, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationReview)(newReviewVerificationHandler(deps)))))
	mux.Handle("GET /v1/admin/kyc/documents",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newListDocumentsForAccountHandler(deps)))))
	mux.Handle("POST /v1/admin/kyc/documents/{id}/review",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationReview)(newReviewDocumentHandler(deps)))))
	mux.Handle("GET /v1/admin/kyc/documents/{id}/content",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newGetDocumentContentHandler(deps)))))

	// PRH-I3 (ADR 0096 §6, security condition 7): compliance/platform_admin
	// only - never RoleTenantAdmin, even though it holds the broader
	// PermVerificationRead.
	mux.Handle("GET /v1/admin/kyc/enforcement-decisions",
		staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermKYCEnforcementDecisionRead)(newListKYCEnforcementDecisionsHandler(deps)))))

	// KYC-WH-1 (Stage 10.2, ADR 0091, ruling J7): registered only when
	// BOTH conditions hold - see Deps.KYCWebhookEnabled's own doc comment.
	// Absent means the mux returns a genuine 404, never a 503 from inside
	// a registered handler (K11).
	if deps.KYCWebhookEnabled && deps.KYCOrchestrator != nil {
		mux.HandleFunc("POST /v1/webhooks/kyc/{tenantSlug}/{providerID}", newKYCWebhookHandler(deps))
	}
}

// registerKYCEnforcementPolicyRoutes wires the lic10 (L-20) KYC enforcement policy authoring surface. PLATFORM-ONLY
// (platform_admin), deliberately NOT wrapped in RequireTenantScope (a platform token carries no tenant), step-up critical on
// every write. See kyc_enforcement_policy_handlers.go for what the routes refuse by design.
func registerKYCEnforcementPolicyRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/admin/kyc/enforcement-policies",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermKYCEnforcementPolicyRead)(newListKYCEnforcementPoliciesHandler(deps))))
	mux.Handle("POST /v1/admin/kyc/enforcement-policies",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermKYCEnforcementPolicyManage)(newCreateKYCEnforcementPolicyHandler(deps))))
	mux.Handle("POST /v1/admin/kyc/enforcement-policies/{policyID}/activate",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermKYCEnforcementPolicyManage)(newActivateKYCEnforcementPolicyHandler(deps))))
	mux.Handle("POST /v1/admin/kyc/enforcement-policies/{policyID}/withdraw",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermKYCEnforcementPolicyManage)(newWithdrawKYCEnforcementPolicyHandler(deps))))
	mux.Handle("POST /v1/admin/kyc/enforcement-policies/{policyID}/supersede",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermKYCEnforcementPolicyManage)(newSupersedeKYCEnforcementPolicyHandler(deps))))
}
