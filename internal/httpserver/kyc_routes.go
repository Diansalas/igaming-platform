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
	mux.Handle("POST /v1/me/kyc/verifications", auth.Middleware(deps.AuthIssuer)(newCreateMyVerificationHandler(deps)))
	mux.Handle("GET /v1/me/kyc/verifications", auth.Middleware(deps.AuthIssuer)(newListMyVerificationsHandler(deps)))
	mux.Handle("POST /v1/me/kyc/documents", auth.Middleware(deps.AuthIssuer)(newUploadMyDocumentHandler(deps)))
	mux.Handle("GET /v1/me/kyc/documents", auth.Middleware(deps.AuthIssuer)(newListMyDocumentsHandler(deps)))
	mux.Handle("GET /v1/me/kyc/documents/{id}/content", auth.Middleware(deps.AuthIssuer)(newGetMyDocumentContentHandler(deps)))

	mux.Handle("GET /v1/admin/kyc/verifications",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newListVerificationsForAccountHandler(deps)))))
	mux.Handle("POST /v1/admin/kyc/verifications/{id}/review",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationReview)(newReviewVerificationHandler(deps)))))
	mux.Handle("GET /v1/admin/kyc/documents",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newListDocumentsForAccountHandler(deps)))))
	mux.Handle("POST /v1/admin/kyc/documents/{id}/review",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationReview)(newReviewDocumentHandler(deps)))))
	mux.Handle("GET /v1/admin/kyc/documents/{id}/content",
		auth.Middleware(deps.AuthIssuer)(auth.RequireTenantScope(auth.RequirePermission(auth.PermVerificationRead)(newGetDocumentContentHandler(deps)))))

	mux.HandleFunc("POST /v1/webhooks/kyc/{tenantSlug}/{providerID}", newKYCWebhookHandler(deps))
}
