package httpserver

// ADR 0119 slice S4a (section 7.3): the ONE staff route chain.
//
// ROUTE-REGISTRATION FREEZE (docs/plans/adr0119-s4a-route-registration.md):
// every staff-authenticated route is registered as
//
//	mux.Handle("<METHOD> <literal path>", staffRoute(deps, <class>, <inner>))
//
// where <class> is one of the routeClass values below and <inner> is the
// route's own permission / tenant-scope gates and handler. The pattern stays a
// string literal in the mux.Handle call (the webhook route guard and the
// OpenAPI drift tests read it). TestStaffRoutes_* (staff_route_static_test.go)
// fail the build for a /v1/admin/ (or /v1/staff/me/, /v1/staff/mfa/,
// /v1/staff/auth/step-up) route registered any other way, for a class that is
// not one of these identifiers, and for a route missing from (or classified
// differently than) the golden table.

import (
	"fmt"
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// routeClass is the step-up classification of a staff route (ADR 0119 section
// 8.2): either a static auth.StepUpClass or a named classifier. A classifier
// route's class depends on the validated request (section 8.2); its ceiling is
// critical. S4b enforces it: requireStepUp (staff_stepup.go) is the last link
// of the chain, and stepUpClassifiers holds one Go function per classifier.
type routeClass struct {
	static     auth.StepUpClass
	classifier string
}

var (
	stepUpNone      = routeClass{static: auth.StepUpNone}
	stepUpFinancial = routeClass{static: auth.StepUpFinancial}
	stepUpCritical  = routeClass{static: auth.StepUpCritical}

	// Classifier routes (section 8.2; ADR 0115 S2 for the RG limit route).
	stepUpByLaunchAction       = routeClass{classifier: "launchActionClass"}          // none for suspend; critical otherwise
	stepUpByFinancialPolicyRun = routeClass{classifier: "financialPolicyChangeClass"} // none for tighten-only callers; critical for authors
	stepUpByAlertKind          = routeClass{classifier: "alertResolveClass"}          // critical for P1 / security kinds; none otherwise
	stepUpByRGLimitDirection   = routeClass{classifier: "rgLimitChangeClass"}         // critical for an increase or removal; none for a decrease
	stepUpByAlertRouteManager  = routeClass{classifier: "alertRouteManageClass"}      // critical for a caller the handler admits; none otherwise
)

// routeClassifiers is the closed set of classifier names (pinned by the static
// test; stepUpClassifiers holds one Go function per name).
var routeClassifiers = map[string]bool{
	"launchActionClass": true, "financialPolicyChangeClass": true, "alertResolveClass": true, "rgLimitChangeClass": true,
	"alertRouteManageClass": true,
}

// valid reports whether c is exactly one of: a valid static class, or a known
// classifier.
func (c routeClass) valid() bool {
	if c.classifier != "" {
		return c.static == "" && routeClassifiers[c.classifier]
	}
	return c.static.Valid()
}

// String is the golden-table spelling: "none", "financial", "critical" or
// "classifier:<name>".
func (c routeClass) String() string {
	if c.classifier != "" {
		return "classifier:" + c.classifier
	}
	return string(c.static)
}

// staffRoute wraps inner in the ADR 0119 section 7.3 staff chain:
//
//	auth.Middleware -> auth.RequireStaffPrincipal -> requireLiveStaffSession -> auth.RequireAAL2 -> requireStepUp(class) -> inner
//
// inner carries the route's own gates (auth.RequirePermission, and
// auth.RequireTenantScope where the route needs it). requireStepUp (S4b)
// enforces the class before inner (a deviation from section 7.3's order,
// recorded in ADR 0119 section 26). A non-staff token
// is refused before any database work; the liveness transaction commits and
// releases its connection before inner runs. An invalid class or a nil
// handler is a programming error and panics at registration (process start),
// never at request time.
func staffRoute(deps Deps, class routeClass, inner http.Handler) http.Handler {
	if !class.valid() {
		panic(fmt.Sprintf("httpserver: staffRoute: invalid step-up class %+v", class))
	}
	if inner == nil {
		panic("httpserver: staffRoute: nil handler")
	}
	return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(requireLiveStaffSession(deps)(auth.RequireAAL2(requireStepUp(deps, class)(inner)))))
}

// liveIfStaff is the chain of the two mixed-principal self-service routes
// (GET /v1/me/sessions, DELETE /v1/me/sessions/{id}): any authenticated
// principal may manage its OWN sessions, but a STAFF caller must still hold a
// live aal2 session (a revoked staff access token must not list or touch
// sessions). Player and service callers pass through unchanged.
func liveIfStaff(deps Deps, inner http.Handler) http.Handler {
	staff := requireLiveStaffSession(deps)(auth.RequireAAL2(inner))
	return auth.Middleware(deps.AuthIssuer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStaffPrincipal(r) {
			staff.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	}))
}
