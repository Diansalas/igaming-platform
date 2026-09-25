package httpserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// Stage 9 §8 route-table regression test.
//
// auth.RequirePlayerPrincipal has its own unit tests in internal/auth;
// what this file proves is different and is the part that actually
// regresses: that the gate is WIRED ONTO EVERY player self-service route
// in the real route table built by New. A middleware that exists but is
// missing from one registration line is worth nothing on that line, and
// the failure mode is silent.
//
// Deliberately a plain unit test with a nil DB: RequirePlayerPrincipal
// runs before any handler body, so no handler here ever reaches the
// database. If one of these ever returns 200/404/500 instead of 403, that
// IS the finding - it means the request got past the gate and into a
// handler.
func newPrincipalGateTestServer(t *testing.T, issuer *auth.Issuer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		DB:          nil, // never dereferenced: every request below is denied by middleware
		AuthIssuer:  issuer,
		ServiceName: "platform-api-test",
		// Registers the three Stage 7 play-simulation routes so they are
		// covered by the table below too.
		CasinoPlaySimulationEnabled: true,
		SportsbookEnabled:           true,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func principalGateTestIssuer(t *testing.T) *auth.Issuer {
	t.Helper()
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": "test-secret-at-least-32-characters-long"})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	return auth.NewIssuer(keys, "platform-api-test", "platform-api-test")
}

// playerOnlyRoutes is every route whose handler derives the acting
// player_account_id from the token subject and therefore must refuse a
// non-player principal.
//
// DELIBERATELY EXCLUDED, and why:
//   - GET /v1/me/sessions, DELETE /v1/me/sessions/{id} - "which devices am
//     I logged in on" is a self-service capability every principal type
//     owns over its OWN sessions (see routes.go's own comment). Covered by
//     TestSessionRoutes_RemainOpenToStaffPrincipals below instead, so the
//     exclusion is asserted rather than assumed.
var playerOnlyRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/v1/me"},
	{http.MethodGet, "/v1/me/residence"},
	{http.MethodPut, "/v1/me/residence"},
	{http.MethodGet, "/v1/me/wallets"},
	{http.MethodGet, "/v1/me/wallets/EUR"},
	{http.MethodPost, "/v1/me/deposits"},
	{http.MethodGet, "/v1/me/deposits"},
	{http.MethodGet, "/v1/me/deposits/" + uuid.NewString()},
	{http.MethodPost, "/v1/me/withdrawals"},
	{http.MethodGet, "/v1/me/withdrawals"},
	{http.MethodGet, "/v1/me/withdrawals/" + uuid.NewString()},
	{http.MethodPost, "/v1/me/withdrawals/" + uuid.NewString() + "/cancel"},
	{http.MethodGet, "/v1/me/casino/games"},
	{http.MethodPost, "/v1/me/casino/games/" + uuid.NewString() + "/launch"},
	{http.MethodGet, "/v1/me/casino/rounds"},
	{http.MethodPost, "/v1/me/casino/sessions/" + uuid.NewString() + "/wager"},
	{http.MethodPost, "/v1/me/casino/sessions/" + uuid.NewString() + "/win"},
	{http.MethodPost, "/v1/me/casino/sessions/" + uuid.NewString() + "/rollback"},
	{http.MethodPost, "/v1/me/sportsbook/bets"},
	{http.MethodGet, "/v1/me/sportsbook/bets"},
	{http.MethodPost, "/v1/me/rg/self-exclusion"},
	{http.MethodGet, "/v1/me/rg/status"},
	{http.MethodPost, "/v1/me/kyc/verifications"},
	{http.MethodGet, "/v1/me/kyc/verifications"},
	{http.MethodPost, "/v1/me/kyc/documents"},
	{http.MethodGet, "/v1/me/kyc/documents"},
	{http.MethodGet, "/v1/me/kyc/documents/" + uuid.NewString() + "/content"},
	{http.MethodPost, "/v1/me/email-verification/request"},
	{http.MethodGet, "/v1/bonus/grants"},
	{http.MethodGet, "/v1/bonus/grants/" + uuid.NewString() + "/progress"},
	{http.MethodPost, "/v1/bonus/coupons/redeem"},
}

func TestPlayerSelfServiceRoutes_RejectStaffPrincipal(t *testing.T) {
	issuer := principalGateTestIssuer(t)
	srv := newPrincipalGateTestServer(t, issuer)

	// A tenant_admin is the strongest realistic tenant-scoped staff
	// principal - if the gate holds for it, it holds for the narrower
	// roles (internal/auth's own table test covers every role).
	staffToken, err := issuer.Issue(uuid.NewString(), uuid.New(), auth.RoleTenantAdmin, auth.PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatalf("failed to issue staff token: %v", err)
	}

	for _, rt := range playerOnlyRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req, err := http.NewRequest(rt.method, srv.URL+rt.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("failed to build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+staffToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("a staff token reached a player-only route: expected 403, got %d", resp.StatusCode)
			}
		})
	}
}

// The same table with NO token at all must be 401 - proving each route is
// behind auth.Middleware as well, not only behind the principal gate.
func TestPlayerSelfServiceRoutes_RejectAnonymous(t *testing.T) {
	issuer := principalGateTestIssuer(t)
	srv := newPrincipalGateTestServer(t, issuer)

	for _, rt := range playerOnlyRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req, err := http.NewRequest(rt.method, srv.URL+rt.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("failed to build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("expected 401 with no bearer token, got %d", resp.StatusCode)
			}
		})
	}
}

// The deliberate exception, asserted rather than left implicit: the two
// session routes stay open to a staff principal, because they act only on
// the CALLER'S OWN sessions (auth.ListActiveSessions/auth.RevokeSession
// both filter by principal_type + principal_id, and sessions' RLS
// additionally requires app.principal_id to match). A staff token must
// therefore get PAST the middleware chain here - it reaches the handler,
// which is where the nil DB in this fixture surfaces as a 500. The
// assertion is precisely "not 403": if someone later adds
// RequirePlayerPrincipal to these two lines, staff lose the ability to
// see and revoke their own sessions, which is a real regression.
func TestSessionRoutes_RemainOpenToStaffPrincipals(t *testing.T) {
	issuer := principalGateTestIssuer(t)
	srv := newPrincipalGateTestServer(t, issuer)

	staffToken, err := issuer.Issue(uuid.NewString(), uuid.New(), auth.RoleSupport, auth.PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatalf("failed to issue staff token: %v", err)
	}

	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/v1/me/sessions"},
		{http.MethodDelete, "/v1/me/sessions/" + uuid.NewString()},
	} {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req, err := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			if err != nil {
				t.Fatalf("failed to build request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+staffToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusForbidden {
				t.Errorf("a staff principal must retain access to its OWN sessions; got 403")
			}
		})
	}
}
