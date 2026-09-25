package httpserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// Stage 9 §21 route-table regression test, the counterpart to
// player_surface_principal_test.go: ratelimit_test.go proves the limiter
// behaves correctly in isolation; this proves it is actually WIRED to the
// unauthenticated credential endpoints in the route table New builds, and
// only to those.
//
// Uses Deps.AuthRateLimitPerMinute = 1 so the assertion needs exactly two
// requests and no timing. Nil DB is fine: the first request through each
// route is allowed and fails inside its handler (503/500) - only the
// SECOND request's status is under test, and the limiter runs before any
// handler.
func newRateLimitedTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": "test-secret-at-least-32-characters-long"})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		DB:                     nil,
		AuthIssuer:             auth.NewIssuer(keys, "platform-api-test", "platform-api-test"),
		ServiceName:            "platform-api-test",
		AuthRateLimitPerMinute: 1,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postRaw(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// rateLimitedRoutes is every route that must be behind the limiter.
// Grouped by bucket so the shared-bucket relationships are visible: all
// four credential routes share one bucket, and both logins share another,
// which is why each sub-test below needs its own server.
func TestUnauthenticatedCredentialRoutes_AreRateLimited(t *testing.T) {
	for _, path := range []string{
		"/v1/auth/register",
		"/v1/auth/login",
		"/v1/staff/auth/login",
		"/v1/auth/refresh",
		"/v1/auth/password-reset/request",
		"/v1/auth/password-reset/confirm",
		"/v1/auth/email-verification/confirm",
	} {
		t.Run(path, func(t *testing.T) {
			// Fresh server per route so one bucket's exhaustion cannot be
			// mistaken for another's (login and staff login deliberately
			// share a bucket; the three credential routes share one too).
			srv := newRateLimitedTestServer(t)

			first := postRaw(t, srv, path)
			_ = first.Body.Close()
			if first.StatusCode == http.StatusTooManyRequests {
				t.Fatalf("the FIRST request must be allowed through, got 429")
			}

			second := postRaw(t, srv, path)
			defer func() { _ = second.Body.Close() }()
			if second.StatusCode != http.StatusTooManyRequests {
				t.Errorf("%s is not rate limited: expected 429 on the second request, got %d", path, second.StatusCode)
			}
			if got := second.Header.Get("Retry-After"); got == "" {
				t.Error("expected a Retry-After header on a 429")
			}
		})
	}
}

// Logout is deliberately NOT limited (routes.go's own comment): it proves
// possession of an unguessable refresh token, costs one indexed hash
// lookup, and refusing it would only keep alive a session its owner asked
// to end. Asserted so the exclusion stays a decision, not an oversight.
func TestLogoutRoute_IsNotRateLimited(t *testing.T) {
	srv := newRateLimitedTestServer(t)
	for i := 0; i < 5; i++ {
		resp := postRaw(t, srv, "/v1/auth/logout")
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			t.Fatalf("logout must not be rate limited; request %d got 429", i+1)
		}
	}
}

// The authenticated email-verification REQUEST endpoint is deliberately
// excluded from the per-IP bucket (it is already per-account limited via
// auth.CountRecentCredentialTokens). It must still refuse an anonymous
// caller - i.e. the exclusion must not have been implemented by dropping
// its auth middleware.
func TestAuthenticatedEmailVerificationRequest_StillRequiresAuth(t *testing.T) {
	srv := newRateLimitedTestServer(t)
	for i := 0; i < 3; i++ {
		resp := postRaw(t, srv, "/v1/me/email-verification/request")
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status != http.StatusUnauthorized {
			t.Fatalf("request %d: expected 401, got %d", i+1, status)
		}
	}
}

// A negative override disables the limiter entirely (Deps.AuthRateLimit
// PerMinute's documented escape hatch for a deployment behind a load
// balancer, where RemoteAddr collapses to one address). Proven end to
// end, because "we can turn it off" is exactly the kind of claim that
// silently stops being true.
func TestAuthRateLimitPerMinute_NegativeDisablesTheLimiter(t *testing.T) {
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": "test-secret-at-least-32-characters-long"})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		AuthIssuer:             auth.NewIssuer(keys, "platform-api-test", "platform-api-test"),
		ServiceName:            "platform-api-test",
		AuthRateLimitPerMinute: -1,
	}))
	defer srv.Close()

	for i := 0; i < 10; i++ {
		resp := postRaw(t, srv, "/v1/auth/login")
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			t.Fatalf("a negative override must disable the limiter; request %d got 429", i+1)
		}
	}
}

// Authenticated, non-credential routes are NOT behind the per-IP limiter:
// it exists to bound unauthenticated Argon2id cost and credential
// guessing, and applying it to ordinary gameplay traffic would make every
// player behind one carrier-grade NAT share a 60/minute budget. Asserted
// so a future "just add it everywhere" change has to argue with a test.
func TestAuthenticatedPlayerRoutes_AreNotPerIPRateLimited(t *testing.T) {
	srv := newRateLimitedTestServer(t)
	for i := 0; i < 10; i++ {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/me/wallets", nil)
		if err != nil {
			t.Fatalf("failed to build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+uuid.NewString()) // invalid: expect 401, never 429
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			t.Fatalf("request %d: authenticated routes must not share the credential limiter", i+1)
		}
	}
}
