package auth

// ADR 0119 slice S4a: the auth-package pieces of the staff route chain
// (section 7.3). The HTTP-layer pieces that need the database (the per-request
// liveness check and the staffRoute registration helper) live in
// internal/httpserver.
//
//   - StaffRoles / IsStaffRole: the CLOSED set of staff roles (S3 re-review
//     condition S3-R12). Issue, IssueStaff and Verify bind a staff principal
//     to exactly this set; any other role string on a staff principal is
//     refused, at mint time and at verification time.
//   - StepUpClass: the closed step-up vocabulary of section 8. In S4a a class
//     is only RECORDED per route; RequireStepUp (the enforcement) is S4b.
//   - StaffSession / StaffSessionFromContext: the verified session assurance
//     claims of a staff token, attached by Middleware.
//   - RequireAAL2: refuses a staff request whose verified claims are not aal2.

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// staffRoles is the closed set of staff roles. It mirrors the staff_users.role
// CHECK (migration 0064) and identity.StaffRole; a parity test pins all three.
// Adding a role needs a migration, an identity constant, a rolePermissions
// entry and this set, in one change.
var staffRoles = [...]Role{
	RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleCompliance, RoleFinance,
	RoleRiskManager, RolePromotionsManager, RoleBonusOperations,
}

// StaffRoles returns a copy of the closed staff role set.
func StaffRoles() []Role {
	out := make([]Role, len(staffRoles))
	copy(out, staffRoles[:])
	return out
}

// IsStaffRole reports whether role is one of the closed set of staff roles.
func IsStaffRole(role Role) bool {
	for _, r := range staffRoles {
		if r == role {
			return true
		}
	}
	return false
}

// StepUpClass is the step-up class of a staff route (ADR 0119 section 8): a
// closed vocabulary. In S4a it is only recorded; S4b's RequireStepUp enforces
// it (critical: 5 min, financial: 10 min, none: aal2 is enough).
type StepUpClass string

const (
	StepUpNone      StepUpClass = "none"
	StepUpFinancial StepUpClass = "financial"
	StepUpCritical  StepUpClass = "critical"
)

// Valid reports whether c is one of the three classes.
func (c StepUpClass) Valid() bool {
	switch c {
	case StepUpNone, StepUpFinancial, StepUpCritical:
		return true
	}
	return false
}

// StaffSession is the verified session assurance of a staff access token
// (ADR 0119 section 7.1). It is attached to the request context by Middleware
// for staff tokens only, after Verify has accepted every claim.
type StaffSession struct {
	SessionID    uuid.UUID
	AAL          int
	AMR          []string
	AuthTime     time.Time
	MFAAt        time.Time
	StaffVersion int64
}

type staffSessionKey struct{}

// withStaffSession attaches s to ctx. Only Middleware calls it, from verified
// claims.
func withStaffSession(ctx context.Context, s StaffSession) context.Context {
	return context.WithValue(ctx, staffSessionKey{}, s)
}

// StaffSessionFromContext returns the verified staff session assurance of the
// request, if the bearer token was a staff token.
func StaffSessionFromContext(ctx context.Context) (StaffSession, bool) {
	s, ok := ctx.Value(staffSessionKey{}).(StaffSession)
	return s, ok
}

// staffSessionFromClaims builds the context value from claims Verify accepted
// (checkAssurance guarantees every field is present on a staff token).
func staffSessionFromClaims(c *Claims) (StaffSession, bool) {
	if c.PrincipalType != PrincipalStaff || c.StaffVersion == nil || c.AuthTime == nil || c.MFAAt == nil {
		return StaffSession{}, false
	}
	sid, err := uuid.Parse(c.SessionID)
	if err != nil || sid == uuid.Nil {
		return StaffSession{}, false
	}
	return StaffSession{
		SessionID: sid, AAL: c.AAL, AMR: append([]string(nil), c.AMR...),
		AuthTime: c.AuthTime.Time, MFAAt: c.MFAAt.Time, StaffVersion: *c.StaffVersion,
	}, true
}

// RequireAAL2 refuses a request unless it carries a verified staff session at
// aal2 (ADR 0119 section 7.3). Verify already refuses a staff token with any
// other aal, so this is defence in depth: a staff route never runs without a
// session assurance in its context, whatever change is later made to Verify.
// It answers 401 (the credential is not sufficient), never 403.
func RequireAAL2(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		s, ok := StaffSessionFromContext(r.Context())
		if err != nil || tc.PrincipalType != string(PrincipalStaff) || !ok || s.AAL != StaffAAL {
			w.Header().Set("WWW-Authenticate", `Bearer realm="platform-api", error="insufficient_user_authentication"`)
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "a multi-factor staff session is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
