package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerStaffAuthRoutes mounts the ADR 0119 S3 staff authentication
// endpoints (section 14.1). Every one except GET /v1/staff/me/mfa is
// unauthenticated (it establishes authentication) and rate limited per IP:
// the password step shares the `login` bucket with player login; the
// second-factor and enrolment steps use the `mfa` bucket (30 per minute,
// section 6.5); the enrolment-token redemption uses the `credential` bucket.
// The request logger never logs a body (loggingMiddleware), and every
// response of these routes is no-store.
func registerStaffAuthRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("POST /v1/staff/auth/login",
		rateLimitFunc(deps.authLimiter, rateBucketLogin, rateLimitLoginPerMinute, newStaffLoginHandler(deps)))
	mux.Handle("POST /v1/staff/auth/mfa/verify",
		rateLimitFunc(deps.authLimiter, rateBucketMFA, rateLimitMFAPerMin, newStaffMFAVerifyHandler(deps)))
	mux.Handle("POST /v1/staff/auth/mfa/recover",
		rateLimitFunc(deps.authLimiter, rateBucketMFA, rateLimitMFAPerMin, newStaffMFARecoverHandler(deps)))
	mux.Handle("POST /v1/staff/auth/enrol/start",
		rateLimitFunc(deps.authLimiter, rateBucketMFA, rateLimitMFAPerMin, newStaffEnrolStartHandler(deps)))
	mux.Handle("POST /v1/staff/auth/enrol/confirm",
		rateLimitFunc(deps.authLimiter, rateBucketMFA, rateLimitMFAPerMin, newStaffEnrolConfirmHandler(deps)))
	mux.Handle("POST /v1/staff/auth/enrol/redeem",
		rateLimitFunc(deps.authLimiter, rateBucketCredential, rateLimitCredentialPerMin, newStaffEnrolRedeemHandler(deps)))
	// Own account only: the subject comes from the token (no permission for MFA self-service, section 14.3).
	mux.Handle("GET /v1/staff/me/mfa",
		auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(newStaffMFAStatusHandler(deps))))
}
