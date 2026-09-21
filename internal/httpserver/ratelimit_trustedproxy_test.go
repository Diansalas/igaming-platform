package httpserver

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// These tests close S9.1-LAUNCH-1: the rate limiter's client-identity
// extraction must be safe by default (TrustedProxyCount=0 never reads
// X-Forwarded-For) and, once a specific number of trusted proxy hops is
// configured, must extract the real client from the correct position in
// the header regardless of what an attacker injects further left in it.

// A direct client with no proxy in front of it is keyed on RemoteAddr,
// with TrustedProxyCount left at its safe default of 0.
func TestTrustedProxyClientIP_DirectClient_UsesRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:54321"

	got := trustedProxyClientIP(req, 0)
	if got != "203.0.113.7" {
		t.Fatalf("expected 203.0.113.7, got %q", got)
	}
}

// With TrustedProxyCount=0, an X-Forwarded-For header supplied directly by
// an untrusted caller must be ignored entirely, even though it is present
// and well-formed - this is the property that keeps the header safe by
// default (the header can be set by ANY client, not just a proxy).
func TestTrustedProxyClientIP_ZeroHops_IgnoresXForwardedForEntirely(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")

	got := trustedProxyClientIP(req, 0)
	if got != "203.0.113.7" {
		t.Fatalf("TrustedProxyCount=0 must ignore X-Forwarded-For; expected 203.0.113.7, got %q", got)
	}
}

// A single configured trusted hop: the trusted proxy appends exactly one
// entry to the right, and that entry is the client's real address,
// regardless of anything the client itself put in X-Forwarded-For before
// the proxy ever saw the request.
func TestTrustedProxyClientIP_OneHop_ExtractsRealClient(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	// RemoteAddr is the trusted proxy's own address (it made the direct
	// TCP connection) - irrelevant to the extraction once trustedHops>0.
	req.RemoteAddr = "10.0.0.5:443"
	// The attacker-supplied left entry ("9.9.9.9", a value the CLIENT set
	// before ever reaching the proxy) must be ignored; the proxy appended
	// the real client "198.51.100.9" to the right.
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 198.51.100.9")

	got := trustedProxyClientIP(req, 1)
	if got != "198.51.100.9" {
		t.Fatalf("expected the trusted proxy's own appended entry 198.51.100.9, got %q", got)
	}
}

// A two-hop chain (edge proxy -> internal load balancer -> app): the real
// client is the second-from-right entry, and an attacker prepending
// arbitrary extra junk on the left must not shift which entry is trusted -
// counting from the right is exactly what makes left-side injection
// irrelevant.
func TestTrustedProxyClientIP_TwoHops_IgnoresLeftInjection(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "10.0.0.9:443"
	// "evil,evil2" simulates a client trying to inject two fake hops on
	// the left; "203.0.113.55" is the real client the first trusted proxy
	// saw; "10.0.0.1" is the first proxy's own address, appended by the
	// second (innermost) trusted proxy.
	req.Header.Set("X-Forwarded-For", "evil,evil2, 203.0.113.55, 10.0.0.1")

	got := trustedProxyClientIP(req, 2)
	if got != "203.0.113.55" {
		t.Fatalf("expected 203.0.113.55, got %q", got)
	}
}

// Malformed/insufficient input must fall back to RemoteAddr, never to "no
// limit" and never to a client-controlled value used verbatim.
func TestTrustedProxyClientIP_FallsBackToRemoteAddrOnMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		xff  string
	}{
		{"missing header entirely", ""},
		{"fewer entries than configured hops", "198.51.100.9"},
		{"non-IP entry at the trusted position", "not-an-ip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
			req.RemoteAddr = "203.0.113.7:54321"
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := trustedProxyClientIP(req, 2)
			if got != "203.0.113.7" {
				t.Fatalf("expected fallback to RemoteAddr 203.0.113.7, got %q", got)
			}
		})
	}
}

// End-to-end through the HTTP wrapper: with a configured trusted-proxy
// chain, two distinct real clients behind the SAME proxy (same RemoteAddr)
// must be rate-limited INDEPENDENTLY - proving the limiter is actually
// keying on the extracted client identity, not silently collapsing to the
// proxy's own address.
func TestRateLimit_TrustedProxy_DistinctClientsAreLimitedIndependently(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0, 1)
	now := time.Now()
	l.now = func() time.Time { return now }

	h := rateLimit(l, rateBucketLogin, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	newReq := func(realClient string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "10.0.0.5:443" // the shared proxy's own address
		req.Header.Set("X-Forwarded-For", realClient)
		return req
	}

	// Client A's first request: allowed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("198.51.100.1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("client A's first request should be allowed, got %d", rec.Code)
	}
	// Client A's second request: denied (limit of 1).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("198.51.100.1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client A's second request should be denied, got %d", rec.Code)
	}
	// Client B, behind the SAME proxy, must be unaffected.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("198.51.100.2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("client B must not be affected by client A's exhausted bucket, got %d", rec.Code)
	}
}

// Rate-limit exhaustion under the trusted-proxy path must still return the
// standard 429 + Retry-After response, exactly like the direct-client path.
func TestRateLimit_TrustedProxy_ExhaustionReturns429WithRetryAfter(t *testing.T) {
	l := newFixedWindowLimiter(time.Minute, 0, 1)
	now := time.Now()
	l.now = func() time.Time { return now }

	h := rateLimit(l, rateBucketLogin, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "10.0.0.5:443"
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		return req
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("expected first request through, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("expected Retry-After: 60, got %q", got)
	}
}

// Concurrent requests from the SAME key must be counted correctly with no
// race: exactly `limit` of them succeed, regardless of how many goroutines
// race to call allow() simultaneously. Run this file's tests with -race to
// prove the shared counter has no data race, and this assertion to prove
// the mutex actually makes the check-and-increment atomic (a naive
// check-then-increment without the lock covering both steps would let more
// than `limit` requests through under contention).
func TestFixedWindowLimiter_ConcurrentAllow_IsRaceSafeAndExact(t *testing.T) {
	const limit = 50
	const attempts = 500

	l := newFixedWindowLimiter(time.Minute, 0, 0)
	now := time.Now()
	l.now = func() time.Time { return now }

	var wg sync.WaitGroup
	var allowedCount int32
	var mu sync.Mutex

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("login|203.0.113.7", limit) {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != limit {
		t.Fatalf("expected exactly %d allowed under concurrent contention, got %d", limit, allowedCount)
	}
}

// The same concurrency property, exercised through the full HTTP wrapper
// with a configured trusted-proxy hop, so the client-key extraction path
// is included in the race check too.
func TestRateLimit_TrustedProxy_ConcurrentRequestsAreRaceSafeAndExact(t *testing.T) {
	const limit = 20
	const attempts = 200

	l := newFixedWindowLimiter(time.Minute, 0, 1)
	now := time.Now()
	l.now = func() time.Time { return now }

	var served int32
	var mu sync.Mutex
	h := rateLimit(l, rateBucketLogin, limit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))

	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
			req.RemoteAddr = "10.0.0.5:443"
			req.Header.Set("X-Forwarded-For", "198.51.100.1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
		}()
	}
	wg.Wait()

	if served != limit {
		t.Fatalf("expected exactly %d served under concurrent contention, got %d", limit, served)
	}
}
