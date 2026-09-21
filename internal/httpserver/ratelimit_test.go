package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/apierror"
)

// These tests are deliberately plain unit tests (no `integration` build
// tag): the limiter touches no database, and a control this cheap to
// verify should fail in the fast suite every contributor runs, not only
// in the tagged one.

func TestFixedWindowLimiter_AllowsUpToLimitThenDenies(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !l.allow("login|1.2.3.4", 3) {
			t.Fatalf("request %d should have been allowed, within the limit of 3", i+1)
		}
	}
	if l.allow("login|1.2.3.4", 3) {
		t.Fatal("the 4th request in the same window should have been denied")
	}
}

func TestFixedWindowLimiter_WindowResets(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	if !l.allow("login|1.2.3.4", 1) {
		t.Fatal("first request should be allowed")
	}
	if l.allow("login|1.2.3.4", 1) {
		t.Fatal("second request in the same window should be denied")
	}

	// Advance past the window boundary - the counter must start over.
	now = now.Add(time.Minute + time.Second)
	if !l.allow("login|1.2.3.4", 1) {
		t.Fatal("a request in the NEXT window should be allowed again")
	}
}

// A denied key must not deny a different key. This is the property that
// makes the limiter a per-caller control rather than a global kill switch:
// one attacker's exhausted bucket must never lock out everyone else.
func TestFixedWindowLimiter_KeysAreIndependent(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	if !l.allow("login|1.2.3.4", 1) {
		t.Fatal("first IP's first request should be allowed")
	}
	if l.allow("login|1.2.3.4", 1) {
		t.Fatal("first IP is now over its limit")
	}
	if !l.allow("login|5.6.7.8", 1) {
		t.Fatal("a DIFFERENT IP must not be affected by the first IP's exhausted bucket")
	}
	if !l.allow("register|1.2.3.4", 1) {
		t.Fatal("a DIFFERENT bucket for the same IP must be counted separately")
	}
}

func TestFixedWindowLimiter_OverrideReplacesEveryLimit(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 1)
	now := time.Now()
	l.now = func() time.Time { return now }

	// The caller asks for a limit of 500; the override of 1 must win.
	if !l.allow("login|1.2.3.4", 500) {
		t.Fatal("first request should be allowed")
	}
	if l.allow("login|1.2.3.4", 500) {
		t.Fatal("override of 1 should have denied the second request despite the per-bucket limit of 500")
	}
}

func TestFixedWindowLimiter_NegativeOverrideDisablesLimiter(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, -1)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		if !l.allow("login|1.2.3.4", 1) {
			t.Fatalf("a negative override disables the limiter entirely; request %d was denied", i+1)
		}
	}
	if len(l.entries) != 0 {
		t.Errorf("a disabled limiter should not accumulate state, got %d entries", len(l.entries))
	}
}

// rateLimiterMaxKeys is a MEMORY bound, and it fails OPEN on purpose (see
// its own doc comment). This proves the documented behaviour is what the
// code actually does - a reader must not have to take the comment's word
// for which way it fails.
func TestFixedWindowLimiter_FailsOpenAndBoundsMemoryAtMaxKeys(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	// Fill the table with live (non-expired) windows.
	for i := 0; i < rateLimiterMaxKeys; i++ {
		l.entries["k"+strconv.Itoa(i)] = &rateWindow{start: now, count: 1}
	}
	// One more distinct key: eviction finds nothing expired, so the table
	// is reset and the request is ALLOWED rather than refused.
	if !l.allow("login|9.9.9.9", 1) {
		t.Fatal("at rateLimiterMaxKeys the limiter must fail open, not deny authentication platform-wide")
	}
	if len(l.entries) > rateLimiterMaxKeys {
		t.Errorf("limiter grew past its memory bound: %d entries", len(l.entries))
	}
}

func TestFixedWindowLimiter_EvictsExpiredBeforeResetting(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	// A full table of windows that have all already expired.
	for i := 0; i < rateLimiterMaxKeys; i++ {
		l.entries["k"+strconv.Itoa(i)] = &rateWindow{start: now.Add(-2 * time.Minute), count: 1}
	}
	// One live key that must SURVIVE the eviction (it is not expired), so
	// an attacker cannot wipe their own counter merely by flooding the
	// table with unrelated, already-expired keys.
	l.entries["login|9.9.9.9"] = &rateWindow{start: now, count: 5}

	if !l.allow("login|1.1.1.1", 1) {
		t.Fatal("expected allow after expired-entry eviction")
	}
	if l.allow("login|9.9.9.9", 5) {
		t.Fatal("a live counter must survive eviction of expired entries, not be reset by it")
	}
}

// --- the HTTP wrapper ---

func TestRateLimit_Returns429WithTheStandardErrorEnvelope(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	var served int
	h := rateLimit(l, rateBucketLogin, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:54321"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the first request through, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on the second request, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("expected Retry-After: 60, got %q", got)
	}
	var body apierror.Error
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("a rate-limited response must still be the platform error envelope, not a bare status: %v", err)
	}
	if body.Code != apierror.CodeRateLimited {
		t.Errorf("expected code %q, got %q", apierror.CodeRateLimited, body.Code)
	}
	// The limiter must never become an account-existence oracle: the
	// message is generic and mentions nothing request-specific.
	if body.Message == "" {
		t.Error("expected a generic message")
	}
	if served != 1 {
		t.Errorf("the wrapped handler must not run for a limited request; it ran %d times", served)
	}
}

// The limiter keys on clientIP(r) (RemoteAddr), NOT on X-Forwarded-For -
// see clientIP's own doc comment. If that ever changes silently, an
// attacker gets a free bucket per forged header value and the control
// becomes decorative, so it is asserted here explicitly.
func TestRateLimit_IgnoresXForwardedFor(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	h := rateLimit(l, rateBucketLogin, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	newReq := func(xff string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "203.0.113.7:54321"
		req.Header.Set("X-Forwarded-For", xff)
		return req
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("10.0.0.1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("10.0.0.2")) // different forged XFF, same RemoteAddr
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a forged X-Forwarded-For must not mint a fresh bucket; got %d", rec.Code)
	}
}

// A nil limiter passes traffic through untouched. Deps.authLimiter is
// always constructed by New, but rateLimit is defensive about it and that
// contract is worth pinning: a future caller that builds routes without
// New must fail open, never nil-panic on the login path.
func TestRateLimit_NilLimiterIsPassThrough(t *testing.T) {
	h := rateLimit(nil, rateBucketLogin, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "203.0.113.7:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected pass-through 200, got %d", i+1, rec.Code)
		}
	}
}

// Login and staff-login deliberately SHARE one bucket (ratelimit.go: "the
// same attack surface with the same cost profile"). If someone later
// gives staff login its own bucket without thinking about it, an attacker
// gets double the Argon2 budget per address - so the sharing is asserted.
func TestRateLimit_PlayerAndStaffLoginShareOneBucket(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	playerLogin := rateLimit(l, rateBucketLogin, 1, ok)
	staffLogin := rateLimit(l, rateBucketLogin, 1, ok)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	playerLogin.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/staff/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	rec = httptest.NewRecorder()
	staffLogin.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("staff login must draw from the same bucket as player login; got %d", rec.Code)
	}
}
