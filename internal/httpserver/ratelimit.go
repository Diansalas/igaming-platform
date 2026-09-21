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
//   - It keys on the caller's identity as determined by clientKey (below),
//     which by default is clientIP(r) (RemoteAddr) - see clientIP's own
//     doc comment. X-Forwarded-For is used ONLY when a deployment
//     explicitly configures Deps.TrustedProxyCount (env TRUSTED_PROXY_COUNT)
//     to the exact number of its own trusted reverse-proxy hops; the
//     default of 0 means X-Forwarded-For is never read for rate-limiting
//     purposes, which is the safe behavior for an unconfigured deployment
//     (Deps.TrustedProxyCount's own doc comment has the full trust model;
//     this closes launch-gate item S9.1-LAUNCH-1). CONSEQUENCE OF LEAVING
//     IT AT 0 BEHIND A REAL LOAD BALANCER: every request arrives from the
//     balancer's own address, and this limiter degrades from "per client"
//     to "per service, globally" - whoever introduces that proxy MUST set
//     TRUSTED_PROXY_COUNT to the number of hops it controls, or raise/
//     disable these limits via AUTH_RATE_LIMIT_PER_MINUTE, or move the
//     control to the edge.
//   - AuthRateLimitPerMinute/TrustedProxyCount are both plumbed end to end
//     through internal/config -> cmd/platform-api/main.go -> this package
//     (this closes launch-gate item S9.1-LAUNCH-2) - the
//     AUTH_RATE_LIMIT_PER_MINUTE and TRUSTED_PROXY_COUNT environment
//     variables reach here without a code change.
package httpserver

import (
	"net"
	"net/http"
	"strconv"
	"strings"
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
	// trustedProxyHops is Deps.TrustedProxyCount, carried onto the limiter
	// itself so clientKey has it without threading it through every call
	// site. See trustedProxyClientIP's own doc comment for the trust model.
	trustedProxyHops int
}

func newFixedWindowLimiter(window time.Duration, override int, trustedProxyHops int) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		window:           window,
		entries:          make(map[string]*rateWindow),
		now:              time.Now,
		override:         override,
		trustedProxyHops: trustedProxyHops,
	}
}

// clientKey is the per-request identity the limiter counts against -
// trustedProxyClientIP applied to this limiter's own configured hop count.
func (l *fixedWindowLimiter) clientKey(r *http.Request) string {
	return trustedProxyClientIP(r, l.trustedProxyHops)
}

// trustedProxyClientIP determines the caller's IP for rate-limiting when
// trustedHops trusted reverse-proxy hops are configured in front of this
// service (Deps.TrustedProxyCount / TRUSTED_PROXY_COUNT):
//
//   - trustedHops <= 0: X-Forwarded-For is never read. RemoteAddr
//     (clientIP(r)) is the answer, unconditionally - the safe default for
//     an unconfigured deployment. A header the caller can set to anything
//     is worthless without a known number of trusted hops to skip past it,
//     and reading it anyway would let any client mint itself a fresh rate
//     limit bucket for free.
//   - trustedHops == N > 0: this deployment's own reverse-proxy chain is N
//     hops deep, and each of those N hops appends exactly one entry to the
//     RIGHT of X-Forwarded-For - the standard behavior of nginx/HAProxy/
//     ALB/etc ("append the address of whoever just connected to me,
//     comma-separated, after whatever was already there"). The real
//     client's address is therefore always the Nth entry counting from
//     the right, REGARDLESS of how much garbage a malicious client
//     prepends on the LEFT of its own X-Forwarded-For before the request
//     ever reaches the first trusted hop: the trusted hops only ever add
//     to the right, so counting from the right is exactly what makes
//     left-side injection irrelevant. For example, with trustedHops=1 and
//     a client-supplied header of "9.9.9.9" arriving at the single trusted
//     proxy, the proxy forwards "9.9.9.9, <real client>" - the rightmost
//     entry is always the one the trusted proxy itself appended.
//
// Any failure mode (header absent, fewer than trustedHops comma-separated
// entries, an entry that doesn't parse as an IP) falls back to RemoteAddr -
// never to "no limit" and never to trusting attacker-controlled input as
// the fallback.
func trustedProxyClientIP(r *http.Request, trustedHops int) string {
	if trustedHops <= 0 {
		return clientIP(r)
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return clientIP(r)
	}
	parts := strings.Split(xff, ",")
	if len(parts) < trustedHops {
		return clientIP(r)
	}
	candidate := strings.TrimSpace(parts[len(parts)-trustedHops])
	if candidate == "" || net.ParseIP(candidate) == nil {
		return clientIP(r)
	}
	return candidate
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
		if !limiter.allow(bucket+"|"+limiter.clientKey(r), limit) {
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
