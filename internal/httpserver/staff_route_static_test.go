package httpserver

// ADR 0119 slice S4a: the static completeness test of the staff route chain (section 7.3) and its negative controls,
// the golden classification, the chain composition pin, and a DB-free HTTP sweep of every staff route.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// routeClassIdents is the closed set of class identifiers a staffRoute call may name. It must list every routeClass
// package variable (TestStaffRoutes_ClassIdentsAreTheClosedSet).
var routeClassIdents = map[string]routeClass{
	"stepUpNone": stepUpNone, "stepUpFinancial": stepUpFinancial, "stepUpCritical": stepUpCritical,
	"stepUpByLaunchAction": stepUpByLaunchAction, "stepUpByFinancialPolicyRun": stepUpByFinancialPolicyRun,
	"stepUpByAlertKind": stepUpByAlertKind, "stepUpByRGLimitDirection": stepUpByRGLimitDirection,
	"stepUpByAlertRouteManager": stepUpByAlertRouteManager,
	"stepUpByAMLHoldPlace":      stepUpByAMLHoldPlace, "stepUpByAMLTransition": stepUpByAMLTransition,
	"stepUpByKYCResidence": stepUpByKYCResidence,
}

// mixedPrincipalStaffPaths are the self-service routes any principal may call, where a STAFF caller must still pass
// liveness (liveIfStaff).
var mixedPrincipalStaffPaths = map[string]bool{"/v1/me/sessions": true, "/v1/me/sessions/{id}": true}

// isStaffRoutePath is ADR 0119 section 7.3's coverage: every /v1/admin/ route, the staff self-service surface and the
// step-up endpoint.
func isStaffRoutePath(path string) bool {
	return strings.HasPrefix(path, "/v1/admin/") || path == "/v1/admin" || strings.HasPrefix(path, "/v1/staff/me/") ||
		strings.HasPrefix(path, "/v1/staff/mfa/") || path == "/v1/staff/auth/step-up"
}

type staffRegistration struct {
	file, pattern, class string
}

// scanStaffRoutes parses the given sources (file name -> Go source) and returns every staff registration and every
// problem: a staff path registered other than as mux.Handle("<literal>", staffRoute(deps, <class ident>, ...)), a
// class that is not one of routeClassIdents, a pattern that is not a literal or has no method, a staffRoute or
// liveIfStaff call that is not the direct handler argument of a mux.Handle call, and a mixed-principal session route
// not wrapped in liveIfStaff.
func scanStaffRoutes(srcs map[string]string) ([]staffRegistration, []string) {
	var regs []staffRegistration
	var problems []string
	fset := token.NewFileSet()
	names := make([]string, 0, len(srcs))
	for n := range srcs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, srcs[name], 0)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: parse: %v", name, err))
			continue
		}
		sanctioned := map[*ast.CallExpr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") || len(call.Args) != 2 {
				return true
			}
			pos := fset.Position(call.Pos())
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				problems = append(problems, fmt.Sprintf("%s: a %s pattern is not a string literal (cannot be classified)", pos, sel.Sel.Name))
				return true
			}
			pattern := strings.Trim(lit.Value, "\"`")
			// Code review CR 2: EVERY pattern of the package has the strict shape "<METHOD> /<path>" (exactly one
			// space, no host, no other whitespace) and literal first two path segments, so no registration can match
			// a staff path while escaping the classification below (a tab or double space would hide the method, a
			// host or a leading wildcard would match /v1/admin/... under a pattern that does not start with it).
			if !strictPatternRe.MatchString(pattern) {
				problems = append(problems, fmt.Sprintf("%s: pattern %q is not \"<METHOD> /<path>\" (one space, no host, METHOD in GET|POST|PUT|PATCH|DELETE)", pos, pattern))
				return true
			}
			method, path, hasMethod := strings.Cut(pattern, " ")
			if segs := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3); strings.Contains(segs[0], "{") || len(segs) > 1 && strings.Contains(segs[1], "{") {
				problems = append(problems, fmt.Sprintf("%s: pattern %q has a wildcard in its first two path segments (it could match a staff path)", pos, pattern))
				return true
			}
			h, _ := call.Args[1].(*ast.CallExpr)
			switch {
			case isStaffRoutePath(path):
				if !hasMethod || method == "" {
					problems = append(problems, fmt.Sprintf("%s: staff route %q has no method", pos, pattern))
					return true
				}
				if sel.Sel.Name != "Handle" || h == nil || !isIdentCall(h, "staffRoute") || len(h.Args) != 3 || !isIdent(h.Args[0], "deps") {
					problems = append(problems, fmt.Sprintf("%s: staff route %q is not registered as staffRoute(deps, <class>, <handler>)", pos, pattern))
					return true
				}
				classIdent, ok := h.Args[1].(*ast.Ident)
				class, known := routeClassIdents[identName(classIdent, ok)]
				if !ok || !known {
					problems = append(problems, fmt.Sprintf("%s: staff route %q: the class must be one of the routeClass identifiers", pos, pattern))
					return true
				}
				sanctioned[h] = true
				regs = append(regs, staffRegistration{file: name, pattern: pattern, class: class.String()})
			case mixedPrincipalStaffPaths[path]:
				if h == nil || !isIdentCall(h, "liveIfStaff") || len(h.Args) != 2 || !isIdent(h.Args[0], "deps") {
					problems = append(problems, fmt.Sprintf("%s: %q must be registered as liveIfStaff(deps, <handler>)", pos, pattern))
					return true
				}
				sanctioned[h] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if ok && (isIdentCall(call, "staffRoute") || isIdentCall(call, "liveIfStaff")) && !sanctioned[call] {
				problems = append(problems, fmt.Sprintf("%s: %s may only be called as the handler argument of mux.Handle with a literal staff pattern",
					fset.Position(call.Pos()), call.Fun.(*ast.Ident).Name))
			}
			return true
		})
	}
	return regs, problems
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isIdentCall(c *ast.CallExpr, name string) bool { return isIdent(c.Fun, name) }

func identName(id *ast.Ident, ok bool) string {
	if !ok || id == nil {
		return ""
	}
	return id.Name
}

// goldenDiff compares registrations with the golden table, both ways, and reports duplicates.
func goldenDiff(regs []staffRegistration, golden map[string]string) []string {
	var diffs []string
	seen := map[string]bool{}
	for _, r := range regs {
		if seen[r.pattern] {
			diffs = append(diffs, fmt.Sprintf("%s registered twice", r.pattern))
		}
		seen[r.pattern] = true
		want, ok := golden[r.pattern]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s (%s, class %s) is not in the golden table: add it to staffRouteGolden with its section 8.1 class", r.pattern, r.file, r.class))
		case want != r.class:
			diffs = append(diffs, fmt.Sprintf("%s is registered as %s, the golden table says %s", r.pattern, r.class, want))
		}
	}
	for p := range golden {
		if !seen[p] {
			diffs = append(diffs, fmt.Sprintf("%s is in the golden table but not registered", p))
		}
	}
	sort.Strings(diffs)
	return diffs
}

// packageSources returns every non-test .go file of this package.
func packageSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	if len(out) < 20 {
		t.Fatalf("found only %d source files: the scan's discovery is broken", len(out))
	}
	return out
}

// Coverage (section 7.3): every staff route of the package is a staffRoute with a closed-set class.
func TestStaffRoutes_EveryStaffRouteRegisteredThroughHelper(t *testing.T) {
	regs, problems := scanStaffRoutes(packageSources(t))
	for _, p := range problems {
		t.Error(p)
	}
	if len(regs) != len(staffRouteGolden) {
		t.Errorf("found %d staffRoute registrations, the golden table has %d", len(regs), len(staffRouteGolden))
	}
}

// Classification: the registered classes equal the golden table exactly (section 8.2).
func TestStaffRoutes_GoldenClassification(t *testing.T) {
	regs, problems := scanStaffRoutes(packageSources(t))
	if len(problems) > 0 {
		t.Fatalf("scan problems: %v", problems)
	}
	for _, d := range goldenDiff(regs, staffRouteGolden) {
		t.Error(d)
	}
}

// Every mutating route carries an explicit class from the closed set, and every read is "none".
func TestStaffRoutes_EveryMutatingRouteClassified(t *testing.T) {
	valid := map[string]bool{}
	for _, c := range routeClassIdents {
		valid[c.String()] = true
	}
	mutating := 0
	for p, c := range staffRouteGolden {
		method, _, _ := strings.Cut(p, " ")
		if !valid[c] {
			t.Errorf("%s: class %q is not in the closed set", p, c)
		}
		switch method {
		case http.MethodGet:
			if c != "none" {
				t.Errorf("%s: a read is classified %s (reads never step up)", p, c)
			}
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			mutating++
		default:
			t.Errorf("%s: unexpected method", p)
		}
	}
	// 178 staff routes at S4a: 108 mutating, 70 reads. S4b adds POST /v1/staff/auth/step-up and
	// POST /v1/staff/mfa/recovery-codes/regenerate: 181 routes (S1b adds GET /v1/admin/alerts/{alertID}), 110 mutating. A new route changes this pin and the
	// golden table together.
	// ADR 0119 S5 + S6: +24 lifecycle routes (8 reads, 16 mutating), -1 the removed legacy staff creation route:
	// 203 routes + S1b GET /v1/admin/alerts/{alertID} = 204 routes, 125 mutating.
	// ADR 0116 A7: +11 AML officer routes (3 reads, 8 mutating): 215 routes, 133 mutating.
	// ADR 0115 S4-min (A6): +GET and +POST /v1/admin/rg/enforcement = 217 routes, 134 mutating.
	// A5 (WITHDRAWAL-REVERSAL-1): +POST /v1/admin/withdrawals/{id}/payout-reversal = 218 routes, 135 mutating.
	// A10c: +GET payment-attempts/{attemptID}/events, +GET reconciliation/runs/{runID}, +GET reconciliation/mismatches/{mismatchID},
	// +GET withdrawal-hold-candidates (4 reads) = 222 routes, 135 mutating.
	// F6 / SA-3: +GET /v1/admin/platform/status (1 read) = 223 routes, 135 mutating.
	// Licensing slice 1 (migration 0176): +4 licence lifecycle PUTs, +1 GET licence versions = 228 routes, 139 mutating.
	// Licensing slice 2 (migration 0177, MKT-DUAL-1): +6 licensing change routes (2 reads, 4 mutating) = 234 routes, 143 mutating.
	// lic4 slice 4a: +5 platform reads (jurisdiction by id, licence by id, licence country ceilings, operating country policies,
	// tenant jurisdiction configs) = 239 routes, 143 mutating (the lifecycle PUTs keep their routes).
	// lic9 (migration 0180): +2 platform reads (operating bases list and get); filing uses the existing POST /v1/admin/licensing-changes.
	// lic10 (L-20): +GET and +4 POST KYC enforcement policy routes (1 read, 4 mutating) = 244 routes, 147 mutating.
	// lic10 (migration 0181, G8/Q21): +2 reads, +3 mutating residence maker-checker routes = 249 routes, 150 mutating; merged with lic9: 251 routes.
	if mutating != 156 || len(staffRouteGolden) != 263 {
		t.Errorf("golden table: %d routes, %d mutating; pinned 263 and 156", len(staffRouteGolden), mutating)
		// lic11 (migration 0182): +8 withdrawal restriction policy routes (4 reads, 4 mutating): 259 routes, 154 mutating (summed with lic9 and lic10).
		// lic18: +2 reads, +2 mutating AML screening trigger-policy and officer-resolution routes: 263 routes, 156 mutating.
	}
}

// The class identifiers are exactly the routeClass package variables, each valid and distinct, and the classifier
// names are the pinned closed set of section 8.2 (plus ADR 0115 S2's RG limit classifier).
func TestStaffRoutes_ClassIdentsAreTheClosedSet(t *testing.T) {
	src, err := os.ReadFile("staff_route.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`(?m)^\t(stepUp\w+)\s+= routeClass\{`).FindAllStringSubmatch(string(src), -1)
	if len(declared) != len(routeClassIdents) {
		t.Fatalf("staff_route.go declares %d routeClass values, the scan knows %d", len(declared), len(routeClassIdents))
	}
	strs := map[string]bool{}
	for _, d := range declared {
		c, ok := routeClassIdents[d[1]]
		if !ok || !c.valid() {
			t.Errorf("%s is not a known valid class", d[1])
		}
		strs[c.String()] = true
	}
	if len(strs) != len(routeClassIdents) {
		t.Error("two class identifiers have the same value")
	}
	want := []string{"alertResolveClass", "alertRouteManageClass", "amlHoldPlaceClass", "amlTransitionClass", "financialPolicyChangeClass", "kycResidenceReviewClass", "launchActionClass", "rgLimitChangeClass"}
	var got []string
	for n := range routeClassifiers {
		got = append(got, n)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("classifier set %v, want %v", got, want)
	}
	for _, bad := range []routeClass{{}, {static: "loose"}, {classifier: "unknown"}, {static: auth.StepUpCritical, classifier: "launchActionClass"}} {
		if bad.valid() {
			t.Errorf("%+v must be invalid", bad)
		}
	}
	for _, s := range []auth.StepUpClass{auth.StepUpNone, auth.StepUpFinancial, auth.StepUpCritical} {
		if !s.Valid() {
			t.Errorf("%s must be valid", s)
		}
	}
	if auth.StepUpClass("").Valid() || auth.StepUpClass("NONE").Valid() {
		t.Error("only the three lower-case classes are valid")
	}
}

// Negative controls: each planted offender fails the scan (or the golden comparison), so the scan is not vacuous.
func TestStaffRoutes_StaticScan_PlantedOffendersFail(t *testing.T) {
	wrap := func(body string) map[string]string {
		return map[string]string{"planted.go": "package httpserver\nfunc planted(mux *http.ServeMux, deps Deps, h http.Handler) {\n" + body + "\n}\n"}
	}
	for name, body := range map[string]string{
		"hand-registered admin route (the pre-S4a shape)": `mux.Handle("POST /v1/admin/evil", auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermTenantWrite)(h)))`,
		"staff self-service route without the helper":     `mux.HandleFunc("GET /v1/staff/me/thing", h.ServeHTTP)`,
		"step-up endpoint without the helper":             `mux.Handle("POST /v1/staff/auth/step-up", h)`,
		"class from a variable":                           "c := stepUpCritical\nmux.Handle(\"POST /v1/admin/x\", staffRoute(deps, c, h))",
		"class as a composite literal":                    `mux.Handle("POST /v1/admin/x", staffRoute(deps, routeClass{static: "critical"}, h))`,
		"helper result stored in a variable":              "x := staffRoute(deps, stepUpNone, h)\nmux.Handle(\"GET /v1/admin/y\", x)",
		"admin pattern without a method":                  `mux.Handle("/v1/admin/z", staffRoute(deps, stepUpNone, h))`,
		"non-literal pattern":                             "p := \"POST /v1/admin/q\"\nmux.Handle(p, staffRoute(deps, stepUpNone, h))",
		"session route without liveIfStaff":               `mux.Handle("DELETE /v1/me/sessions/{id}", auth.Middleware(deps.AuthIssuer)(h))`,
		"helper wrapped by another middleware":            `mux.Handle("GET /v1/admin/w", withSomething(staffRoute(deps, stepUpNone, h)))`,
		// Code review CR 2 (X3/X4): shapes that would match a staff path without being recognised as one.
		"tab instead of a space":        "mux.Handle(\"POST\t/v1/admin/tab\", auth.Middleware(deps.AuthIssuer)(h))",
		"double space":                  `mux.Handle("POST  /v1/admin/two", auth.Middleware(deps.AuthIssuer)(h))`,
		"host-qualified pattern":        `mux.Handle("POST api.example.com/v1/admin/host", auth.Middleware(deps.AuthIssuer)(h))`,
		"wildcard first segment":        `mux.Handle("POST /{version}/admin/w", auth.Middleware(deps.AuthIssuer)(h))`,
		"wildcard second segment":       `mux.Handle("POST /v1/{area}/w", auth.Middleware(deps.AuthIssuer)(h))`,
		"catch-all wildcard":            `mux.Handle("GET /{rest...}", h)`,
		"method outside the closed set": `mux.Handle("HEAD /v1/admin/head", staffRoute(deps, stepUpNone, h))`,
		"pattern with trailing space":   `mux.Handle("GET /v1/admin/sp ", staffRoute(deps, stepUpNone, h))`,
	} {
		_, problems := scanStaffRoutes(wrap(body))
		if len(problems) == 0 {
			t.Errorf("%s: the scan did not flag %s", name, body)
		}
	}
	// A correctly shaped but UNCLASSIFIED route (the mutant "a new route registered without a golden entry") passes
	// the shape scan and fails the golden comparison.
	regs, problems := scanStaffRoutes(wrap(`mux.Handle("POST /v1/admin/brand-new", staffRoute(deps, stepUpNone, h))`))
	if len(problems) != 0 || len(regs) != 1 {
		t.Fatalf("a well-formed registration must scan cleanly: %v %v", regs, problems)
	}
	if d := goldenDiff(regs, map[string]string{}); len(d) != 1 || !strings.Contains(d[0], "not in the golden table") {
		t.Errorf("an unclassified route must fail the golden comparison: %v", d)
	}
	// A class swapped on a real route (critical -> financial) fails the golden comparison.
	real, _ := scanStaffRoutes(packageSources(t))
	for i := range real {
		if real[i].pattern == "POST /v1/admin/tenants/{tenantID}/capability-grants/requests/{requestID}/approve" {
			real[i].class = "financial"
		}
	}
	if d := goldenDiff(real, staffRouteGolden); len(d) != 1 {
		t.Errorf("a swapped class must give exactly one diff, got %v", d)
	}
	// The real package plus one hand-registered route (the negative control on the real sources).
	srcs := packageSources(t)
	srcs["zz_planted.go"] = "package httpserver\nfunc zzPlanted(mux *http.ServeMux, deps Deps, h http.Handler) {\n" +
		`mux.Handle("POST /v1/admin/tenants/{tenantID}/planted", auth.Middleware(deps.AuthIssuer)(h))` + "\n}\n"
	if _, problems := scanStaffRoutes(srcs); len(problems) != 1 {
		t.Errorf("the planted hand-registered route must be the one problem, got %v", problems)
	}
}

// The chain composition is pinned (section 7.3 order): Middleware -> RequireStaffPrincipal -> liveness -> aal2 ->
// the route's own gates. Removing or reordering a stage fails here.
func TestStaffRoute_ChainComposition(t *testing.T) {
	src, err := os.ReadFile("staff_route.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		// ADR 0119 S4b: requireStepUp(class) is the last link before the route's own gates.
		"return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(requireLiveStaffSession(deps)(auth.RequireAAL2(requireStepUp(deps, class)(inner)))))",
		"staff := requireLiveStaffSession(deps)(auth.RequireAAL2(inner))",
		"return auth.Middleware(deps.AuthIssuer)(http.HandlerFunc(",
	} {
		if strings.Count(s, want) != 1 {
			t.Errorf("staff_route.go must contain exactly once: %s", want)
		}
	}
	for name, f := range map[string]func(){
		"invalid class": func() { staffRoute(Deps{}, routeClass{}, http.NotFoundHandler()) },
		"nil handler":   func() { staffRoute(Deps{}, stepUpNone, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("staffRoute must panic at registration for: %s", name)
				}
			}()
			f()
		}()
	}
	if stepUpByAlertKind.String() != "classifier:alertResolveClass" || stepUpCritical.String() != "critical" {
		t.Error("routeClass.String spelling changed")
	}
}

// ---- DB-free HTTP sweep over every golden route --------------------------------------------------------------------

const sweepSecret = "s4a-sweep-signing-secret-at-least-32-chars"

func sweepServer(t *testing.T, logs io.Writer) (http.Handler, *auth.Issuer) {
	t.Helper()
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": sweepSecret})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "sweep", "sweep")
	// No database: every staff request that reaches the liveness check fails CLOSED. The conditional route groups are
	// mounted with zero-value subsystems: no handler is ever reached in this test.
	return New(Deps{
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), StaffMFA: testStaffMFA(),
		AuthIssuer: issuer, ServiceName: "sweep", AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour,
		PaymentOrchestrator: &payments.Orchestrator{}, ProviderCredentials: &providercred.Subsystem{},
		SportsbookEnabled: true, SportsbookSettlementSimulationEnabled: true,
	}), issuer
}

// sweepSign signs claims with the sweep key (a token that never went through Issue / IssueStaff).
func sweepSign(t *testing.T, c auth.Claims) string {
	t.Helper()
	now := time.Now()
	c.RegisteredClaims = jwt.RegisteredClaims{Subject: uuid.NewString(), Issuer: "sweep", Audience: jwt.ClaimStrings{"sweep"},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), ID: uuid.NewString()}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString([]byte(sweepSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var pathParamRe = regexp.MustCompile(`\{[^}]+\}`)

// strictPatternRe is the only route-pattern shape the package may register (code review CR 2).
var strictPatternRe = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) /\S*$`)

// concretePath fills every {param} of a golden pattern.
func concretePath(pattern string) (method, path string) {
	method, p, _ := strings.Cut(pattern, " ")
	return method, pathParamRe.ReplaceAllStringFunc(p, func(string) string { return uuid.NewString() })
}

// "aal2 required everywhere" and "liveness on every route", without a database: for EVERY golden route, an anonymous
// request is 401, a player or service token is 403 before any database work, a staff token without the assurance
// claims, at aal 1, or with a role outside the closed staff set is 401, and a valid aal2 staff token reaches the
// liveness check, which fails closed (503) - the handler is never reached.
func TestStaffRoutes_DBFree_ChainOnEveryRoute(t *testing.T) {
	var logs bytes.Buffer
	h, issuer := sweepServer(t, &logs)
	now := time.Now()
	sv := int64(0)
	staffClaims := func(role auth.Role, aal int) auth.Claims {
		return auth.Claims{TenantID: uuid.Nil, Role: role, PrincipalType: auth.PrincipalStaff, SessionID: uuid.NewString(),
			AAL: aal, AMR: []string{"pwd", "otp"}, AuthTime: jwt.NewNumericDate(now), MFAAt: jwt.NewNumericDate(now), StaffVersion: &sv}
	}
	player, err := issuer.Issue(uuid.NewString(), uuid.New(), auth.RolePlayer, auth.PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service, err := issuer.Issue(uuid.NewString(), uuid.Nil, auth.RolePlayer, auth.PrincipalService, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	staff, err := issuer.IssueStaff(unitStaffParams(uuid.New(), uuid.Nil, auth.RolePlatformAdmin), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	noAssurance := sweepSign(t, auth.Claims{TenantID: uuid.Nil, Role: auth.RolePlatformAdmin, PrincipalType: auth.PrincipalStaff})
	aal1 := sweepSign(t, staffClaims(auth.RolePlatformAdmin, 1))
	unknownRole := sweepSign(t, staffClaims("made_up_role", auth.StaffAAL))
	if sweepSign(t, staffClaims(auth.RolePlatformAdmin, auth.StaffAAL)) == "" {
		t.Fatal("control token")
	}

	patterns := make([]string, 0, len(staffRouteGolden))
	for p := range staffRouteGolden {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		method, path := concretePath(p)
		for _, tc := range []struct {
			name, bearer string
			status       int
			code         string
		}{
			{"anonymous", "", http.StatusUnauthorized, "unauthorized"},
			{"player", player, http.StatusForbidden, "forbidden"},
			{"service", service, http.StatusForbidden, "forbidden"},
			{"staff without assurance claims", noAssurance, http.StatusUnauthorized, "unauthorized"},
			{"staff at aal 1", aal1, http.StatusUnauthorized, "unauthorized"},
			{"staff role outside the closed set", unknownRole, http.StatusUnauthorized, "unauthorized"},
			{"valid aal2 staff, no database", staff, http.StatusServiceUnavailable, "service_unavailable"},
		} {
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("%s (%s): %d %s, want %d %s", p, tc.name, rec.Code, strings.TrimSpace(rec.Body.String()), tc.status, tc.code)
			}
		}
	}
	// The 503s came from the liveness check itself, once per route (no handler ran).
	if n := strings.Count(logs.String(), `"msg":"staff_liveness_failed"`); n != len(patterns) {
		t.Errorf("staff_liveness_failed logged %d times, want one per route (%d)", n, len(patterns))
	}
}

// liveIfStaff: a player passes straight through to the handler (no database needed); a staff caller must pass the
// liveness check (503 without a database).
func TestLiveIfStaff_PlayerPassesStaffNeedsLiveness(t *testing.T) {
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": sweepSecret})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "sweep", "sweep")
	reached := 0
	h := liveIfStaff(Deps{AuthIssuer: issuer, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(http.StatusTeapot) }))
	player, _ := issuer.Issue(uuid.NewString(), uuid.New(), auth.RolePlayer, auth.PrincipalPlayer, time.Hour)
	staff, _ := issuer.IssueStaff(unitStaffParams(uuid.New(), uuid.New(), auth.RoleSupport), time.Hour)
	for _, tc := range []struct {
		bearer string
		status int
	}{{player, http.StatusTeapot}, {staff, http.StatusServiceUnavailable}, {"", http.StatusUnauthorized}} {
		req := httptest.NewRequest(http.MethodGet, "/v1/me/sessions", nil)
		if tc.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("got %d, want %d", rec.Code, tc.status)
		}
	}
	if reached != 1 {
		t.Errorf("the handler ran %d times, want once (the player)", reached)
	}
}

// The "classified on arrival" list names routes that do not exist yet; a concrete one that is now registered must
// move into the golden table (and off the list) in the slice that registers it.
func TestStaffRoutes_ClassifiedOnArrivalAreNotRegistered(t *testing.T) {
	if len(staffRoutesClassifiedOnArrival) == 0 {
		t.Fatal("the classified-on-arrival list is empty: remove this test with the list")
	}
	regs, _ := scanStaffRoutes(packageSources(t))
	registered := map[string]bool{}
	for _, r := range regs {
		registered[r.pattern] = true
	}
	offenders := func(list []string) []string {
		var out []string
		for _, entry := range list {
			route, _, _ := strings.Cut(entry, " (")
			method, _, _ := strings.Cut(route, " ")
			switch method {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				if registered[route] || staffRouteGolden[route] != "" {
					out = append(out, route)
				}
			}
		}
		return out
	}
	for _, route := range offenders(staffRoutesClassifiedOnArrival) {
		t.Errorf("%s is registered: move it from staffRoutesClassifiedOnArrival into staffRouteGolden", route)
	}
	// ADR 0119 S4b moved the last two concrete entries (the step-up endpoint and the regeneration) into the golden
	// table; the remaining entries are descriptive. Non-vacuity: a planted concrete entry naming a registered route
	// is reported.
	if got := offenders([]string{"POST /v1/staff/auth/step-up (planted)", "GET /v1/admin/not-a-route (planted)"}); len(got) != 1 {
		t.Errorf("planted classified-on-arrival entry: want exactly the registered one reported, got %v", got)
	}
}

// Code review CR 2: routes are registered only in this package. Every other non-test Go file under internal/ and cmd/
// is scanned for an http.NewServeMux, a ServeMux field or variable being handed a route (Handle / HandleFunc with a
// string-literal pattern), or a direct http.Handle / http.HandleFunc on the default mux. The allowlist is this package.
func TestStaffRoutes_NoRouteRegistrationOutsideHTTPServer(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{filepath.Join("internal", "httpserver"): true}
	fset := token.NewFileSet()
	scanned := 0
	var offenders []string
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if allow[filepath.Dir(rel)] {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			offenders = append(offenders, routeRegistrations(fset, f)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files: the walk is broken", scanned)
	}
	for _, o := range offenders {
		t.Errorf("route registration outside internal/httpserver (bypasses the staff chain checks): %s", o)
	}
	// Negative control: a planted registration in another package is found.
	pf, err := parser.ParseFile(fset, "planted.go", "package x\nimport \"net/http\"\nfunc f(h http.Handler) { m := http.NewServeMux(); m.Handle(\"GET /v1/admin/x\", h); http.HandleFunc(\"/y\", nil) }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := routeRegistrations(fset, pf); len(got) != 3 {
		t.Fatalf("the planted NewServeMux, Handle and http.HandleFunc must all be found, got %v", got)
	}
}

// routeRegistrations returns every route-registration call in f: http.NewServeMux(), any X.Handle / X.HandleFunc whose
// first argument is a string literal, and http.Handle / http.HandleFunc.
func routeRegistrations(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, _ := sel.X.(*ast.Ident)
		switch {
		case sel.Sel.Name == "NewServeMux" && pkg != nil && pkg.Name == "http":
			out = append(out, fmt.Sprintf("%s: http.NewServeMux", fset.Position(call.Pos())))
		case (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") && pkg != nil && pkg.Name == "http":
			out = append(out, fmt.Sprintf("%s: http.%s", fset.Position(call.Pos()), sel.Sel.Name))
		case (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") && len(call.Args) == 2:
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				out = append(out, fmt.Sprintf("%s: %s(%s)", fset.Position(call.Pos()), sel.Sel.Name, lit.Value))
			}
		}
		return true
	})
	return out
}

// livenessLogLine returns the last JSON log line whose msg is msg, decoded, and its raw text.
func livenessLogLine(t *testing.T, logs string, msg string) (map[string]any, string) {
	t.Helper()
	var last string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, `"msg":"`+msg+`"`) {
			last = l
		}
	}
	if last == "" {
		t.Fatalf("no %s log line in %s", msg, logs)
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(last), &m); err != nil {
		t.Fatal(err)
	}
	return m, last
}

// Code review CR 3 / CR 4 (DB-free): a liveness fault is logged at ERROR as staff_liveness_failed with the staff id,
// the token's tenant scope, the sid, the method, the route pattern and the client IP (request_id exactly once); the
// same fault on a request whose client has gone away is staff_liveness_cancelled at INFO, never a database error.
func TestRequireLiveStaffSession_FaultLogFieldsAndClientCancellation(t *testing.T) {
	var logs bytes.Buffer
	h, issuer := sweepServer(t, &logs)
	p := unitStaffParams(uuid.New(), uuid.New(), auth.RoleSupport)
	tok, err := issuer.IssueStaff(p, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/staff/me/mfa", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no database: %d", rec.Code)
	}
	m, raw := livenessLogLine(t, logs.String(), "staff_liveness_failed")
	for k, want := range map[string]string{
		"level": "ERROR", "reason": "db_error", "staff_id": p.StaffID.String(), "token_tenant": p.TenantID.String(),
		"sid": p.SessionID.String(), "method": "GET", "route": "GET /v1/staff/me/mfa", "client_ip": "192.0.2.1",
	} {
		if m[k] != want {
			t.Errorf("staff_liveness_failed %s = %v, want %s", k, m[k], want)
		}
	}
	if n := strings.Count(raw, `"request_id"`); n != 1 {
		t.Errorf("request_id appears %d times in %s", n, raw)
	}
	if strings.Contains(raw, tok) {
		t.Fatal("the token reached the log")
	}

	logs.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req = httptest.NewRequest(http.MethodGet, "/v1/staff/me/mfa", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(logs.String(), "staff_liveness_failed") || strings.Contains(logs.String(), "db_error") {
		t.Fatalf("a cancelled request was logged as a database fault: %s", logs.String())
	}
	m, _ = livenessLogLine(t, logs.String(), "staff_liveness_cancelled")
	if m["level"] != "INFO" || m["reason"] != "client_cancelled" {
		t.Fatalf("cancelled: %v", m)
	}
}
