// Stage 9 §21 (API security/resilience): a minimal, in-process, per-IP
// fixed-window rate limiter for the small set of UNAUTHENTICATED
// credential-handling endpoints - the classic brute-force/abuse targets.
//
// WHY THIS EXISTS (the concrete gap it closes, not a generic "APIs should
// have rate limits" gesture):
//
//  1. Credential stuffing. internal/identity/login_attempt.go's lockout is
//     per-IDENTIFIER (brand+email / tenant+email): five failures against
//     ONE account in fifteen minutes. It does exactly nothing against an
//     attacker trying ONE password against ten thousand different accounts,
//     which is what a real credential-stuffing run looks like. Nothing else
//     in the platform bounded that before this file.
//
//  2. Argon2id cost amplification. internal/auth.HashPassword and
//     VerifyPassword deliberately cost ~64 MiB of memory and ~100 ms of CPU
//     each (password.go's own doc comment: that cost IS the defense against
//     offline cracking of a stolen hash). Every unauthenticated request to
//     /v1/auth/register, /v1/auth/login and /v1/staff/auth/login pays that
//     cost before any authentication exists - login pays it even for an
//     email that has no account at all (auth.DummyPasswordHash's
//     constant-time branch). Unbounded, that turns the platform's own
//     password-hardening parameters into a cheap remote memory-exhaustion
//     lever: N concurrent requests = N x 64 MiB, from an attacker who needs
//     no credentials and no account.
//
//  3. Reset/verification token grinding and email bombing.
//     /v1/auth/password-reset/request is per-account rate limited
//     (auth.CountRecentCredentialTokens) but not per-caller, so one caller
//     can still walk a list of addresses; the two /confirm endpoints had no
//     bound of any kind.
//
// WHAT THIS IS NOT. This is a single-process, in-memory limiter. It is NOT
// a distributed rate limiter and NOT an edge/WAF control, and it must not
// be described as either:
//
//   - With more than one platform-api replica, each replica enforces its
//     own independent window, so the effective platform-wide limit is
//     (limit x replicas). That is still a bound; it is not the configured
//     number.
//   - It keys on clientIP(r), which is RemoteAddr - see clientIP's own doc
//     comment: X-Forwarded-For is deliberately NOT trusted, because no
//     specific reverse-proxy chain is configured yet. CONSEQUENCE, AND THE
//     REASON THE DEFAULTS BELOW ARE DELIBERATELY GENEROUS: once a load
//     balancer sits in front of this service, every request arrives from
//     the balancer's own address, and this limiter degrades from "per
//     client" to "per service, globally." Whoever introduces that proxy
//     MUST either teach clientIP to trust its X-Forwarded-For, or raise/
//     disable these limits via Deps.AuthRateLimitPerMinute, or move the
//     control to the edge - otherwise the platform silently caps its own
//     total login throughput. Recorded as launch-gate item S9.1-LAUNCH-1
//     in docs/security/security-architecture.md's Stage 9 section.
//   - Deps.AuthRateLimitPerMinute is the intended escape hatch for the
//     bullet above, but it is NOT yet plumbed from internal/config or
//     cmd/platform-api - see S9.1-LAUNCH-2 in the same section. Until
//     that exists, these defaults can only be changed by editing this
//     file, which is precisely what the escape hatch was meant to avoid.
package httpserver

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
)

// Per-bucket defaults, in requests per IP per minute. Chosen to be
// comfortably above any plausible legitimate burst from a single shared
// egress IP (carrier-grade NAT, a corporate office, an app store review
// device farm) while still turning "unbounded" into a fixed, small
// multiple of the Argon2 cost per attacker address.
const (
	// rateLimitWindow is the fixed window every bucket below counts within.
	rateLimitWindow = time.Minute

	// rateBucketLogin covers POST /v1/auth/login and
	// POST /v1/staff/auth/login together (one shared bucket: they are the
	// same attack surface with the same cost profile).
	rateBucketLogin           = "login"
	rateLimitLoginPerMinute   = 60
	rateBucketRegister        = "register"
	rateLimitRegisterPerMin   = 20
	rateBucketRefresh         = "refresh"
	rateLimitRefreshPerMin    = 120
	rateBucketCredential      = "credential"
	rateLimitCredentialPerMin = 20
)

// rateLimiterMaxKeys caps how many distinct (bucket, IP) pairs are tracked
// at once. Past it the limiter resets its table rather than growing without
// bound - deliberately failing OPEN, not closed: an attacker who can reach
// this service from ~100k distinct real source addresses (RemoteAddr cannot
// be spoofed across a completed TCP handshake) already has a botnet this
// control was never going to stop, and refusing every login in that
// situation would convert their resource exhaustion into a complete
// authentication outage for legitimate players. Bounded memory is the
// property being protected here, not the rate limit itself.
const rateLimiterMaxKeys = 100_000

type rateWindow struct {
	start time.Time
	count int
}

// fixedWindowLimiter is a per-key fixed-window counter. Fixed-window (not
// token bucket / sliding log) on purpose: it is the simplest thing that
// bounds the attack, it needs one int and one timestamp per key, and its
// worst case - up to 2x the limit across a window boundary - is irrelevant
// at the order of magnitude that matters here.
type fixedWindowLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string]*rateWindow
	// now is injectable so the tests can advance time deterministically
	// instead of sleeping through a real window.
	now func() time.Time
	// override, when > 0, replaces every per-bucket limit (see
	// Deps.AuthRateLimitPerMinute). Negative disables the limiter entirely.
	override int
}

func newFixedWindowLimiter(window time.Duration, override int) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		window:   window,
		entries:  make(map[string]*rateWindow),
		now:      time.Now,
		override: override,
	}
}

// allow reports whether one more request against key is permitted, and
// consumes a slot if so.
func (l *fixedWindowLimiter) allow(key string, limit int) bool {
	if l.override < 0 {
		return true
	}
	if l.override > 0 {
		limit = l.override
	}
	if limit <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if len(l.entries) >= rateLimiterMaxKeys {
		l.evictExpiredLocked(now)
		if len(l.entries) >= rateLimiterMaxKeys {
			// Still full of live windows - fail open (see
			// rateLimiterMaxKeys' own doc comment) rather than grow.
			l.entries = make(map[string]*rateWindow)
		}
	}

	e, ok := l.entries[key]
	if !ok || now.Sub(e.start) >= l.window {
		l.entries[key] = &rateWindow{start: now, count: 1}
		return true
	}
	if e.count >= limit {
		return false
	}
	e.count++
	return true
}

func (l *fixedWindowLimiter) evictExpiredLocked(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.start) >= l.window {
			delete(l.entries, k)
		}
	}
}

// rateLimit wraps h with a per-IP limit for one named bucket. A rejected
// request gets 429 with the platform's ordinary error envelope (never a
// bare status), plus Retry-After - and, deliberately, the SAME generic
// message regardless of which endpoint or whether the caller's target
// account exists, so the limiter itself never becomes an account-existence
// oracle.
func rateLimit(limiter *fixedWindowLimiter, bucket string, limit int, h http.Handler) http.Handler {
	if limiter == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(bucket+"|"+clientIP(r), limit) {
			requestID := observability.RequestIDFromContext(r.Context())
			w.Header().Set("Retry-After", strconv.Itoa(int(limiter.window.Seconds())))
			apierror.Write(w, requestID, apierror.CodeRateLimited, "too many requests; try again shortly")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// rateLimitFunc is rateLimit for a bare http.HandlerFunc route.
func rateLimitFunc(limiter *fixedWindowLimiter, bucket string, limit int, h http.HandlerFunc) http.Handler {
	return rateLimit(limiter, bucket, limit, h)
}
