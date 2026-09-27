package admission

import (
	"sync"
	"time"
)

// GCRALimiter is a keyed GCRA (generic cell rate algorithm) token bucket
// with burst (ADR 0097 §5.1). Each key stores one theoretical arrival
// time (TAT). A request is admitted iff now >= TAT - tau (tau = the burst
// tolerance); admitting then sets TAT = max(TAT, now) + T (T = the
// emission interval, 1s/rate). This is exactly a token bucket, computed
// with integer/duration arithmetic only (no float drift) and driven
// entirely by an injected Clock, so it is deterministic under
// FakeClock.Advance (ADR 0097 §5.1, T5).
//
// The key space is bounded: at most maxKeys distinct keys are tracked.
// Once at capacity, any key not already present is folded into one
// shared overflowKey bucket instead of growing unbounded (ADR 0097 §4.2/
// §4.3/§7 "degrade to bounded, never unbounded, never reset" - unlike
// ratelimit.go's auth limiter, GCRALimiter never resets on overflow).
// Entries idle at full tokens for longer than idleEvict are evicted
// lazily on the next call, so steady-state memory tracks live keys, not
// historical ones.
type GCRALimiter struct {
	mu         sync.Mutex
	clock      Clock
	rate       float64 // events per second
	burst      int
	maxKeys    int
	idleEvict  time.Duration
	overflowOn string // sentinel key name used for overflow

	tat      map[string]time.Time
	lastSeen map[string]time.Time

	overflowLogged bool // latched true once, for the caller's one error log
}

// NewGCRALimiter constructs a GCRALimiter. rate must be > 0 and burst >=
// 1 (ADR 0097 §9.3); the caller is expected to have already validated
// this at startup (config.go) - NewGCRALimiter itself does not validate,
// to keep this leaf package free of a validation-error type duplicated
// elsewhere.
func NewGCRALimiter(rate float64, burst, maxKeys int, idleEvict time.Duration, overflowKey string, clock Clock) *GCRALimiter {
	return &GCRALimiter{
		clock: clock, rate: rate, burst: burst, maxKeys: maxKeys, idleEvict: idleEvict,
		overflowOn: overflowKey,
		tat:        make(map[string]time.Time),
		lastSeen:   make(map[string]time.Time),
	}
}

// Allow reports whether a request for key is admitted now, using this
// limiter's configured rate/burst. On rejection it returns the caller's
// suggested Retry-After, clamped to [1s, 60s] (ADR 0097 §5.1).
func (g *GCRALimiter) Allow(key string) (admitted bool, retryAfter time.Duration) {
	return g.AllowWithParams(key, g.rate, g.burst)
}

// AllowWithParams is Allow with an explicit (rate, burst) instead of this
// limiter's configured default - it lets one GCRALimiter instance serve
// several operator overrides (ADR 0097 §9.2: an override is scoped to one
// (domain, provider_id[, tenant]) key, not the whole domain), while still
// sharing the same bounded key table, eviction and overflow behaviour.
// The (rate, burst) used for a given key is whatever the caller passes at
// the moment the key is FIRST created; a later call with different
// params does not retroactively rescale an existing key's TAT - overrides
// are static configuration set once at startup, so this is not reached in
// practice.
func (g *GCRALimiter) AllowWithParams(key string, rate float64, burst int) (admitted bool, retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.evictLocked()

	if _, present := g.tat[key]; !present && len(g.tat) >= g.maxKeys && g.maxKeys > 0 {
		key = g.overflowOn
		g.overflowLogged = true
	}

	now := g.clock.Now()
	emissionInterval := time.Duration(float64(time.Second) / rate)
	tau := emissionInterval * time.Duration(burst-1)

	prevTAT, present := g.tat[key]
	if !present || prevTAT.Before(now) {
		prevTAT = now
	}
	allowAt := prevTAT.Add(-tau)
	if now.Before(allowAt) {
		wait := allowAt.Sub(now)
		ra := ceilToSeconds(wait)
		if ra < time.Second {
			ra = time.Second
		}
		if ra > 60*time.Second {
			ra = 60 * time.Second
		}
		g.lastSeen[key] = now
		return false, ra
	}
	g.tat[key] = prevTAT.Add(emissionInterval)
	g.lastSeen[key] = now
	return true, 0
}

// OverflowOccurred reports (and latches) whether the key table has ever
// overflowed into the shared overflow bucket, for the caller's one-time
// error log (ADR 0097 §7).
func (g *GCRALimiter) OverflowOccurred() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.overflowLogged
}

// Keys reports the current number of tracked keys (for the
// webhook_limiter_keys{tier} gauge, ADR 0097 §8).
func (g *GCRALimiter) Keys() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.tat)
}

// evictLocked drops keys idle (no Allow call) for longer than idleEvict,
// whose bucket is at full tokens (TAT <= now, i.e. no debt owed - ADR
// 0097 §4.2 "evicted after IdleEvict at full tokens"). Called with mu
// held.
func (g *GCRALimiter) evictLocked() {
	if g.idleEvict <= 0 {
		return
	}
	now := g.clock.Now()
	for k, last := range g.lastSeen {
		if now.Sub(last) < g.idleEvict {
			continue
		}
		if tat, ok := g.tat[k]; ok && tat.After(now) {
			continue // still owes tokens; not idle at full tokens yet
		}
		delete(g.tat, k)
		delete(g.lastSeen, k)
	}
}

func ceilToSeconds(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	secs := d / time.Second
	if d%time.Second != 0 {
		secs++
	}
	return secs * time.Second
}
