package httpserver

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ADR 0112 slice 4, DB-free: the complete mapping of launchgov / database refusals to the closed
// LAUNCH_* API codes. Every sentinel, every guard SQLSTATE, wrapped and bare.
func TestLaunchErrorCode_MapsEveryRefusal(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("outer: %w", e) }
	guard := func(code string) error { return wrap(&launchgov.GuardError{Code: code, Message: "m"}) }
	pre := func(base error, codes ...string) error {
		return wrap(&launchgov.PreconditionError{Base: base, Codes: codes})
	}
	cases := []struct {
		name   string
		err    error
		code   apierror.Code
		status int
		checks []string
	}{
		{"not found", wrap(launchgov.ErrNotFound), apierror.CodeNotFound, 404, nil},
		{"no person", wrap(launchgov.ErrNoPerson), apierror.CodeLaunchNoPerson, 403, nil},
		{"forbidden", wrap(launchgov.ErrForbidden), apierror.CodeForbidden, 403, nil},
		{"acting session refused", launchgov.ErrActingSession, apierror.CodeForbidden, 403, nil},
		{"invalid input", wrap(launchgov.ErrInvalidInput), apierror.CodeValidation, 400, nil},
		{"unacknowledged", pre(launchgov.ErrUnacknowledged, "R-ALERTS", "R-LIC-VALID"), apierror.CodeLaunchUnacknowledgedChecks, 409, []string{"R-ALERTS", "R-LIC-VALID"}},
		{"precondition", pre(launchgov.ErrPreconditionFailed, "H-2:licence_id_required"), apierror.CodeLaunchPreconditionFailed, 409, []string{"H-2:licence_id_required"}},
		{"H-6 withdrawals holding (Go)", pre(launchgov.ErrWithdrawalsHolding, "H-6:withdrawals_holding_funds"), apierror.CodeLaunchWithdrawalsHolding, 409, []string{"H-6:withdrawals_holding_funds"}},
		{"request pending", wrap(launchgov.ErrRequestPending), apierror.CodeLaunchRequestPending, 409, nil},
		{"self approval", wrap(launchgov.ErrSelfApproval), apierror.CodeLaunchSelfApproval, 409, nil},
		{"already decided", wrap(launchgov.ErrAlreadyDecided), apierror.CodeLaunchAlreadyDecided, 409, nil},
		{"illegal transition", wrap(launchgov.ErrIllegalTransition), apierror.CodeLaunchIllegalTransition, 409, nil},
		{"subject closed", wrap(launchgov.ErrSubjectClosed), apierror.CodeLaunchSubjectClosed, 409, nil},
		{"not pending (decided / expired / superseded)", wrap(launchgov.ErrNotPending), apierror.CodeLaunchRequestNotPending, 409, nil},
		{"stale readiness", wrap(launchgov.ErrStaleReadiness), apierror.CodeLaunchStaleReadiness, 409, nil},
		{"prior reject (LA014)", &launchgov.PriorRejectError{RequestID: uuid.New()}, apierror.CodeLaunchPriorReject, 409, nil},
		{"prior rejecter", wrap(launchgov.ErrPriorRejecter), apierror.CodeLaunchPriorRejecter, 409, nil},
		{"not legacy baseline", wrap(launchgov.ErrNotLegacyBaseline), apierror.CodeLaunchNotLegacyBaseline, 409, nil},
		{"lock timeout / deadlock", wrap(launchgov.ErrRetryableLockTimeout), apierror.CodeLaunchRetryableLockTimeout, 409, nil},
		{"refused at execution", &launchgov.RefusedAtExecutionError{RequestID: uuid.New(), Code: "approvals_stale"}, apierror.CodeLaunchRefusedAtExecution, 409, []string{"approvals_stale"}},
		{"GP020 open bets", wrap(&tenant.CloseBlockedError{SportsbookOpenBets: 3}), apierror.CodeTenantCloseBlockedOpenRounds, 409, nil},
		{"LA023 database backstop", guard("LA023"), apierror.CodeLaunchWithdrawalsHolding, 409, []string{"LA023"}},
		{"LA025 database backstop (ADR 0113 s4.3: non-zero withdrawal hold balance)", guard("LA025"), apierror.CodeLaunchWithdrawalsHolding, 409, []string{"LA025"}},
		{"H-6 hold balance precondition", &launchgov.PreconditionError{Base: launchgov.ErrWithdrawalsHolding, Codes: []string{launchgov.H6HoldBalanceNonZero}}, apierror.CodeLaunchWithdrawalsHolding, 409, []string{launchgov.H6HoldBalanceNonZero}},
		{"LA024 database backstop", guard("LA024"), apierror.CodeLaunchOpenBetsBlockClosure, 409, []string{"LA024"}},
		{"LA001 session", guard("LA001"), apierror.CodeForbidden, 403, nil},
		{"LA011", guard("LA011"), apierror.CodeLaunchGuardRefused, 409, []string{"LA011"}},
		{"LA012", guard("LA012"), apierror.CodeLaunchGuardRefused, 409, []string{"LA012"}},
		{"LA013", guard("LA013"), apierror.CodeLaunchGuardRefused, 409, []string{"LA013"}},
		{"LA020", guard("LA020"), apierror.CodeLaunchGuardRefused, 409, []string{"LA020"}},
		{"LA030", guard("LA030"), apierror.CodeLaunchGuardRefused, 409, []string{"LA030"}},
		{"AP001", guard("AP001"), apierror.CodeLaunchProofRefused, 403, []string{"AP001"}},
		{"AP004", guard("AP004"), apierror.CodeLaunchProofRefused, 403, []string{"AP004"}},
		{"AP005", guard("AP005"), apierror.CodeLaunchProofRefused, 403, []string{"AP005"}},
		{"no proof issuer", wrap(launchgov.ErrProofUnavailable), apierror.CodeInternal, 500, nil},
		{"isolation level", wrap(launchgov.ErrIsolationLevel), apierror.CodeInternal, 500, nil},
		{"unknown error", errors.New("pq: boom with secrets"), apierror.CodeInternal, 500, nil},
	}
	for _, tc := range cases {
		code, msg, checks := launchErrorCode(tc.err)
		if code != tc.code || apierror.Status(code) != tc.status {
			t.Errorf("%s: got %s (%d), want %s (%d)", tc.name, code, apierror.Status(code), tc.code, tc.status)
		}
		if fmt.Sprint(checks) != fmt.Sprint(tc.checks) {
			t.Errorf("%s: check codes %v, want %v", tc.name, checks, tc.checks)
		}
		// The message is a fixed sentence: never the underlying error text, never a legal conclusion.
		if strings.Contains(msg, "secrets") || strings.Contains(msg, "outer") || strings.Contains(msg, "pq:") {
			t.Errorf("%s: the message leaks the underlying error: %q", tc.name, msg)
		}
		for _, banned := range []string{"legally", "illegal", "licensed", "authorised to operate"} {
			if strings.Contains(strings.ToLower(msg), banned) {
				t.Errorf("%s: the message states a legal conclusion (%q): %q", tc.name, banned, msg)
			}
		}
	}
}

// writeLaunchError produces the envelope with check_codes only when there are some.
func TestWriteLaunchError_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	c := launchCall{requestID: "req-1", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	writeLaunchError(rec, c, "request", &launchgov.PreconditionError{Base: launchgov.ErrPreconditionFailed, Codes: []string{"H-3:config_missing_or_not_in_effect"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"LAUNCH_PRECONDITION_FAILED"`) ||
		!strings.Contains(rec.Body.String(), `"check_codes":["H-3:config_missing_or_not_in_effect"]`) || !strings.Contains(rec.Body.String(), `"request_id":"req-1"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// An unclassified (internal) error: a fixed 500 body, the underlying text only in the log.
	rec = httptest.NewRecorder()
	writeLaunchError(rec, c, "request", fmt.Errorf("pq: password authentication failed for user igaming (secret-detail): %w", launchgov.ErrProofUnavailable))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret-detail") || strings.Contains(rec.Body.String(), "pq:") ||
		!strings.Contains(rec.Body.String(), `"message":"internal error"`) {
		t.Fatalf("an internal error must be a fixed body: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	writeLaunchError(rec, c, "get", launchgov.ErrNotFound)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "check_codes") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// Route table pin (mutation-kill for the route authorization): each launch route is mounted with
// exactly the static permission ADR 0112 6.1 names, behind RequireStaffPrincipal, and nothing else
// is mounted under the launch names. DB-free.
func TestLaunchRoutes_PermissionPerRoute_Pinned(t *testing.T) {
	src, err := os.ReadFile("launch_authorisation_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.Handle\("(GET|POST) (/v1/[^"]+)",\s*\n\s*staff\((auth\.Perm[A-Za-z]+),`)
	got := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		got[m[1]+" "+m[2]] = m[3]
	}
	want := map[string]string{
		"GET /v1/admin/tenants/{tenantID}/launch-readiness":                           "auth.PermLaunchAuthorisationRead",
		"POST /v1/admin/tenants/{tenantID}/launch-authorisations":                     "auth.PermLaunchAuthorisationRequest",
		"GET /v1/admin/tenants/{tenantID}/launch-authorisations":                      "auth.PermLaunchAuthorisationRead",
		"GET /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}":          "auth.PermLaunchAuthorisationRead",
		"POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/approve": "auth.PermLaunchAuthorisationApprove",
		"POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/reject":  "auth.PermLaunchAuthorisationApprove",
		"POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/cancel":  "auth.PermLaunchAuthorisationRequest",
		"POST /v1/admin/tenants/{tenantID}/brands/{brandID}/suspend":                  "auth.PermLaunchAuthorisationSuspend",
		"POST /v1/admin/tenants/{tenantID}/suspend":                                   "auth.PermLaunchAuthorisationSuspend",
		"GET /v1/admin/tenants/{tenantID}/launch-status-history":                      "auth.PermLaunchAuthorisationRead",
		"GET /v1/admin/launch-authorisations":                                         "auth.PermLaunchAuthorisationRead",
	}
	if len(got) != len(want) {
		t.Fatalf("mounted %d launch routes, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: permission %q, want %q", k, got[k], v)
		}
	}
	// The wrapper must keep the staff-principal gate and the permission gate, in that order.
	if !strings.Contains(string(src), "auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(auth.RequirePermission(perm)(h)))") {
		t.Error("the launch routes must be wrapped Middleware -> RequireStaffPrincipal -> RequirePermission")
	}
	// The tenant is never read from a body or a query: beginLaunchCall is the only source.
	if strings.Contains(string(src), "body.TenantID") || strings.Contains(string(src), `Get("tenant_id")`) {
		t.Error("tenant_id must never be read from a body or query")
	}
	// canActOnTenant answers 404 (no existence oracle), never 403, for a foreign tenant.
	// (The service re-checks the tenant and RLS binds the session, so a mutant that drops the route
	// check is behaviourally equivalent over HTTP; this source pin is what keeps the route check.)
	if !regexp.MustCompile(`if !canActOnTenant\(tc, target\) \{[^}]*CodeNotFound`).MatchString(string(src)) {
		t.Error("a foreign tenant must answer 404 not_found")
	}
}

// With no launch service wired a request is answered 503 before any database access (DB-free).
func TestLaunchRoutes_NilService_503_DBFree(t *testing.T) {
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "t", "t")
	h := New(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AuthIssuer: issuer, ServiceName: "t", AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour})
	tok, err := issuer.Issue(uuid.NewString(), uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/admin/launch-authorisations", "/v1/admin/tenants/" + uuid.NewString() + "/launch-status-history"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "LAUNCH_AUTHORISATION_UNAVAILABLE") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
	// A player token is refused before the service is consulted (RequireStaffPrincipal).
	pt, _ := issuer.Issue(uuid.NewString(), uuid.New(), auth.RolePlayer, auth.PrincipalPlayer, time.Hour)
	req := httptest.NewRequest("GET", "/v1/admin/launch-authorisations", nil)
	req.Header.Set("Authorization", "Bearer "+pt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("player token: %d %s", rec.Code, rec.Body)
	}
	// Anonymous: 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/admin/launch-authorisations", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", rec.Code)
	}
}
