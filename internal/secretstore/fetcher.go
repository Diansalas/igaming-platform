package secretstore

import (
	"container/list"
	"context"
	"crypto/hmac"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// Platform constants (security review 07-w2a-design-review-security.md §5;
// ADR 0093 amendment A4). Changing any of them needs a `security` review.
const (
	// StoreCallTimeout bounds one store call, including its retry.
	StoreCallTimeout = 2 * time.Second
	// StoreMaxRetries is the number of retries after a counting failure,
	// inside StoreCallTimeout.
	StoreMaxRetries = 1
	// StoreRetryBackoff is the pause before the retry.
	StoreRetryBackoff = 50 * time.Millisecond
	// MaxConcurrentStoreCalls (S) bounds concurrent store calls per
	// process. It protects the store and the process; it no longer has
	// anything to do with pooled DB connections, because no connection is
	// ever held across a store call (ADR 0094 INV-POOL).
	MaxConcurrentStoreCalls = 4
	// MaxConcurrentStoreCallsPerTenant (P) bounds one tenant's concurrent
	// store calls on one backend (ADR 0094 §4.2), so one tenant's outage
	// holds at most P of the S slots. 2, not 1: security condition C8's
	// cold-start test (4 distinct cold refs of one tenant at 150 ms store
	// latency, 0 rejections) fails at 1 and passes at 2
	// (TestFetcher_ColdStartFourRefs_NoRejection).
	MaxConcurrentStoreCallsPerTenant = 2
	// MaxDegradedStoreCalls (D) bounds the store calls held by ALL
	// degraded tenants together (a tenant is degraded while its
	// (scheme, tenant) breaker is not closed or has >= 1 consecutive
	// counting failure), so S - D slots always stay available to healthy
	// tenants once failures have been observed (ADR 0094 §6).
	MaxDegradedStoreCalls = 2
	// SlotWait is how long a HEALTHY caller waits for a store-call
	// admission (and how long a healthy caller joining another caller's
	// in-flight fetch waits for its result) before failing fast. A
	// degraded tenant's callers never wait.
	SlotWait = 250 * time.Millisecond
	// MultiTenantDegradedWarnThreshold distinct degraded tenants on one
	// backend at once raise one rate-limited warn line
	// (secret_store_multi_tenant_degraded), so a real backend-wide outage
	// is visible as one event (security condition C9).
	MultiTenantDegradedWarnThreshold = 3
	// MultiTenantDegradedWarnInterval rate-limits that line per backend.
	MultiTenantDegradedWarnInterval = time.Minute
	// BreakerTripThreshold consecutive counting failures open a breaker.
	BreakerTripThreshold = 3
	// BreakerInitialCooldown is the first open period; it doubles on each
	// failed half-open probe up to BreakerMaxCooldown and resets after a
	// successful probe.
	BreakerInitialCooldown = 15 * time.Second
	BreakerMaxCooldown     = 60 * time.Second
	// NegativeTTLCounting is the per-ref negative cache after a counting
	// failure; NegativeTTLPerRef after a non-counting per-ref error
	// (including store_config and a fingerprint mismatch).
	NegativeTTLCounting = 5 * time.Second
	NegativeTTLPerRef   = 30 * time.Second
	// IntegrityAlertInterval rate-limits the P1 integrity alert per ref.
	IntegrityAlertInterval = 5 * time.Minute
	// CacheMaxEntries bounds the positive (and negative) cache LRU.
	CacheMaxEntries = 1024
	// CacheTTL is when a positive entry is refreshed (not evicted).
	CacheTTL = 10 * time.Minute
	// CacheMaxStale is how long a positive entry may be served while the
	// store is failing.
	CacheMaxStale = 60 * time.Minute
)

// Fingerprinter computes the keyed fp1: fingerprint of secret bytes.
type Fingerprinter func(secret []byte) string

// Fetcher is the process-wide guarded path from a handle row to its secret
// bytes (security review §5 as amended by ADR 0094 §4.2): a positive cache
// keyed on (tenant, ref, fingerprint) with the fingerprint compared on
// EVERY hit, a per-key negative cache, a circuit breaker per (backend,
// tenant), a process-wide bound on concurrent store calls with a
// per-tenant cap and a budget for degraded tenants, and single-flight per
// key.
//
// The Fetcher never decides whether a credential may be used. Its caller
// has already read the handle row in the same request (so a revoked
// handle never reaches Fetch, in any breaker state). Fetch refuses to run
// while its caller holds a pooled DB transaction (txscope.Held).
type Fetcher struct {
	router      *Router
	fingerprint Fingerprinter
	logger      *slog.Logger
	now         func() time.Time
	sleep       func(context.Context, time.Duration)
	slotWait    time.Duration

	mu        sync.Mutex
	positive  *lru
	negative  *lru
	flights   map[cacheKey]*flight
	breakers  map[breakerKey]*breaker
	lastAlert map[string]time.Time
	lastWarn  map[string]time.Time

	// Admission accounting (guarded by mu). changed is closed and replaced
	// on every release so waiting healthy owners re-check.
	inFlight         int
	inFlightTenant   map[breakerKey]int
	degradedInFlight int
	changed          chan struct{}
}

// breakerKey scopes a breaker and the per-tenant admission count to one
// tenant on one backend.
type breakerKey struct {
	scheme string
	tenant uuid.UUID
}

// FetcherOption configures a Fetcher (tests inject a clock and a logger).
type FetcherOption func(*Fetcher)

// WithClock injects the clock used for cache ages, negative TTLs and the
// breaker.
func WithClock(now func() time.Time) FetcherOption { return func(f *Fetcher) { f.now = now } }

// WithLogger sets the logger that receives the P1 integrity alert.
func WithLogger(l *slog.Logger) FetcherOption { return func(f *Fetcher) { f.logger = l } }

// WithSleep injects the retry backoff sleep (tests).
func WithSleep(s func(context.Context, time.Duration)) FetcherOption {
	return func(f *Fetcher) { f.sleep = s }
}

// NewFetcher builds the Fetcher over router. fp must be the platform keyed
// fingerprint function; a nil fp or router makes every Fetch fail closed.
func NewFetcher(router *Router, fp Fingerprinter, opts ...FetcherOption) *Fetcher {
	f := &Fetcher{
		router:      router,
		fingerprint: fp,
		logger:      slog.Default(),
		now:         time.Now,
		sleep:       sleepCtx,
		slotWait:    SlotWait,
		positive:    newLRU(CacheMaxEntries),
		negative:    newLRU(CacheMaxEntries),
		flights:     map[cacheKey]*flight{},
		breakers:    map[breakerKey]*breaker{},
		lastAlert:   map[string]time.Time{},
		lastWarn:    map[string]time.Time{},

		inFlightTenant: map[breakerKey]int{},
		changed:        make(chan struct{}),
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// The admission constants must keep 0 < P <= D < S (ADR 0094 §4.2), so a
// degraded tenant can always probe and healthy tenants always keep S - D
// slots.
func init() {
	if MaxConcurrentStoreCallsPerTenant <= 0 || MaxConcurrentStoreCallsPerTenant > MaxDegradedStoreCalls ||
		MaxDegradedStoreCalls >= MaxConcurrentStoreCalls {
		panic(fmt.Sprintf("secretstore: admission constants violate 0 < P <= D < S (P=%d D=%d S=%d)",
			MaxConcurrentStoreCallsPerTenant, MaxDegradedStoreCalls, MaxConcurrentStoreCalls))
	}
}

// Router returns the Fetcher's router.
func (f *Fetcher) Router() *Router { return f.router }

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

type cacheKey struct {
	tenant      uuid.UUID
	ref         string
	fingerprint string
}

type positiveEntry struct {
	secret    Secret
	fetchedAt time.Time
}

type negativeEntry struct {
	class ErrorClass
	until time.Time
}

type flight struct {
	done   chan struct{}
	secret Secret
	err    error
}

// Fetch returns ref's secret for tenantID if - and only if - its keyed
// fingerprint equals fingerprint. Errors are *Error: ClassNoBackend,
// ClassIntegrity (P1, never served), or a store class.
//
// Fetch must never be called while the caller holds a pooled DB
// transaction (ADR 0094 INV-POOL): a ctx marked by txscope fails closed
// with ClassStoreConfig, before any cache, breaker or store access, and
// logs secret_fetch_with_tx_held.
func (f *Fetcher) Fetch(ctx context.Context, tenantID uuid.UUID, ref Ref, fingerprint string) (Secret, error) {
	if txscope.Held(ctx) {
		logger := slog.Default()
		if f != nil && f.logger != nil {
			logger = f.logger
		}
		logTxHeld(logger, "secretstore.Fetcher.Fetch", tenantID)
		return Secret{}, classError(ClassStoreConfig)
	}
	if f == nil || f.fingerprint == nil || f.router == nil {
		return Secret{}, classError(ClassNoBackend)
	}
	store, ok := f.router.Backend(ref.Scheme())
	if !ok {
		return Secret{}, classError(ClassNoBackend)
	}
	key := cacheKey{tenant: tenantID, ref: ref.String(), fingerprint: fingerprint}
	now := f.now()

	// 1. Positive cache. The fingerprint is compared on EVERY hit (T12).
	var stale *positiveEntry
	f.mu.Lock()
	if v, hit := f.positive.get(key); hit {
		e := v.(positiveEntry)
		if !f.matches(e.secret, fingerprint) {
			f.positive.remove(key)
			f.mu.Unlock()
			f.integrityFailure(tenantID, ref)
			return Secret{}, classError(ClassIntegrity)
		}
		if now.Sub(e.fetchedAt) < CacheTTL {
			f.mu.Unlock()
			return e.secret, nil
		}
		if now.Sub(e.fetchedAt) < CacheMaxStale {
			stale = &e
		}
	}

	// 2. Negative cache.
	if v, hit := f.negative.get(key); hit {
		n := v.(negativeEntry)
		if now.Before(n.until) {
			f.mu.Unlock()
			if stale != nil && n.class.CountsTowardBreaker() {
				return stale.secret, nil
			}
			return Secret{}, classError(n.class)
		}
		f.negative.remove(key)
	}

	// 3. Join an in-flight fetch of the same key, or start one. A
	// degraded tenant's followers never wait (ADR 0094 §4.2 point 4).
	bk := breakerKey{scheme: ref.Scheme(), tenant: tenantID}
	if fl, inFlight := f.flights[key]; inFlight {
		wait := f.slotWait
		if f.degradedLocked(bk) {
			wait = -1
		}
		f.mu.Unlock()
		return f.await(ctx, fl, stale, wait)
	}
	br := f.breakerLocked(bk)
	permitted, probe := br.allow(now)
	if !permitted {
		f.mu.Unlock()
		return serveStaleOr(stale, ClassUnavailable)
	}
	fl := &flight{done: make(chan struct{})}
	f.flights[key] = fl
	f.mu.Unlock()

	f.own(ctx, fl, key, bk, store, ref, br, probe)
	return f.await(ctx, fl, stale, 0)
}

// logTxHeld is the INV-POOL guard's single log line: the entry point and
// the tenant id only.
func logTxHeld(logger *slog.Logger, entryPoint string, tenantID uuid.UUID) {
	logger.Error("secret_fetch_with_tx_held",
		"entry_point", entryPoint,
		"tenant_id", tenantID.String())
}

// breakerLocked returns bk's breaker, creating a closed one. Guarded by
// f.mu.
func (f *Fetcher) breakerLocked(bk breakerKey) *breaker {
	b, ok := f.breakers[bk]
	if !ok {
		b = newBreaker()
		f.breakers[bk] = b
	}
	return b
}

// degradedLocked reports whether bk is degraded: its breaker is not
// closed, or it has at least one consecutive counting failure. Guarded by
// f.mu.
func (f *Fetcher) degradedLocked(bk breakerKey) bool {
	b, ok := f.breakers[bk]
	return ok && (b.state != breakerClosed || b.consecutive > 0)
}

// own performs the single store call for key: take a slot (waiting at most
// SlotWait), call the store under a context detached from the caller's
// cancellation but bounded by StoreCallTimeout, record the outcome, and
// publish the result to every waiter.
func (f *Fetcher) own(ctx context.Context, fl *flight, key cacheKey, bk breakerKey, store Store, ref Ref, br *breaker, probe bool) {
	defer func() {
		f.mu.Lock()
		delete(f.flights, key)
		f.mu.Unlock()
		close(fl.done)
	}()

	release, ok := f.admit(ctx, bk)
	if !ok {
		// Admission lost: never negative-cached (ADR 0094 §4.2 point 3 -
		// that would turn contention into denial), and an aborted probe is
		// not counted.
		if probe {
			f.mu.Lock()
			br.abortProbe()
			f.mu.Unlock()
		}
		fl.err = classError(ClassUnavailable)
		return
	}
	defer release()

	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StoreCallTimeout)
	defer cancel()
	secret, err := f.callWithRetry(callCtx, store, ref)

	now := f.now()
	integrity := false
	f.mu.Lock()
	// Re-resolve the breaker: another flight may have pruned the object
	// this call started with (a probe's breaker is never pruned while the
	// probe is in flight, so a probe records on the same object).
	br = f.breakerLocked(bk)
	switch {
	case err != nil:
		class := ClassOf(err)
		br.record(now, probe, class.CountsTowardBreaker())
		if class.CountsTowardBreaker() {
			f.warnMultiTenantDegradedLocked(bk.scheme, now)
		}
		ttl := NegativeTTLPerRef
		if class.CountsTowardBreaker() {
			ttl = NegativeTTLCounting
		}
		f.negative.put(key, negativeEntry{class: class, until: now.Add(ttl)})
		fl.err = classError(class)
	case !f.matches(secret, key.fingerprint):
		// The store answered (so the backend is reachable), but the bytes
		// are not the ones the handle row pins: never served.
		br.record(now, probe, false)
		f.positive.remove(key)
		f.negative.put(key, negativeEntry{class: ClassIntegrity, until: now.Add(NegativeTTLPerRef)})
		fl.err = classError(ClassIntegrity)
		integrity = true
	default:
		br.record(now, probe, false)
		f.positive.put(key, positiveEntry{secret: secret, fetchedAt: now})
		fl.secret = secret
	}
	f.pruneBreakerLocked(bk)
	f.mu.Unlock()
	if integrity {
		f.integrityFailure(key.tenant, ref)
	}
}

func (f *Fetcher) callWithRetry(ctx context.Context, store Store, ref Ref) (Secret, error) {
	var lastErr error
	for attempt := 0; attempt <= StoreMaxRetries; attempt++ {
		if attempt > 0 {
			deadline, _ := ctx.Deadline()
			if time.Until(deadline) <= 2*StoreRetryBackoff {
				break
			}
			f.sleep(ctx, StoreRetryBackoff)
		}
		s, err := callStore(ctx, store, ref)
		if err == nil {
			return s, nil
		}
		lastErr = err
		if !ClassOf(err).CountsTowardBreaker() {
			return Secret{}, classError(ClassOf(err))
		}
	}
	return Secret{}, classError(ClassOf(lastErr))
}

// callStore invokes the backend, folding a panic or a context expiry into
// ClassUnavailable.
func callStore(ctx context.Context, store Store, ref Ref) (s Secret, err error) {
	defer func() {
		if recover() != nil {
			s, err = Secret{}, classError(ClassUnavailable)
		}
	}()
	s, err = store.Get(ctx, ref)
	if err == nil && ctx.Err() != nil {
		return Secret{}, classError(ClassUnavailable)
	}
	if err != nil {
		return Secret{}, classError(ClassOf(err))
	}
	return s, nil
}

// admit takes one store-call admission for bk (ADR 0094 §4.2): a global
// slot (at most S), a per-tenant token (at most P for bk), and - when bk is
// degraded - a degraded token (at most D across all degraded tenants). A
// healthy owner waits up to slotWait; a degraded owner never waits. It
// returns the release func on success.
func (f *Fetcher) admit(ctx context.Context, bk breakerKey) (func(), bool) {
	var timeout <-chan time.Time
	for {
		f.mu.Lock()
		degraded := f.degradedLocked(bk)
		if f.inFlight < MaxConcurrentStoreCalls && f.inFlightTenant[bk] < MaxConcurrentStoreCallsPerTenant &&
			(!degraded || f.degradedInFlight < MaxDegradedStoreCalls) {
			f.inFlight++
			f.inFlightTenant[bk]++
			if degraded {
				f.degradedInFlight++
			}
			f.mu.Unlock()
			return func() { f.release(bk, degraded) }, true
		}
		if degraded {
			f.mu.Unlock()
			return nil, false
		}
		changed := f.changed
		f.mu.Unlock()
		if timeout == nil {
			t := time.NewTimer(f.slotWait)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case <-changed:
		case <-timeout:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

// release returns an admission taken by admit and wakes waiting owners.
func (f *Fetcher) release(bk breakerKey, degraded bool) {
	f.mu.Lock()
	f.inFlight--
	if n := f.inFlightTenant[bk] - 1; n > 0 {
		f.inFlightTenant[bk] = n
	} else {
		delete(f.inFlightTenant, bk)
	}
	if degraded {
		f.degradedInFlight--
	}
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// pruneBreakerLocked deletes bk's breaker once it is closed with no
// counting failure (equivalent to absent), so the map is bounded by the
// tenants with recent failures. Guarded by f.mu.
func (f *Fetcher) pruneBreakerLocked(bk breakerKey) {
	if b, ok := f.breakers[bk]; ok && b.state == breakerClosed && b.consecutive == 0 && !b.probeInFlight {
		delete(f.breakers, bk)
	}
}

// warnMultiTenantDegradedLocked logs secret_store_multi_tenant_degraded,
// at most once per MultiTenantDegradedWarnInterval per backend, when at
// least MultiTenantDegradedWarnThreshold distinct tenants on scheme are
// degraded at once (security condition C9). Guarded by f.mu.
func (f *Fetcher) warnMultiTenantDegradedLocked(scheme string, now time.Time) {
	n := 0
	for bk := range f.breakers {
		if bk.scheme == scheme && f.degradedLocked(bk) {
			n++
		}
	}
	if n < MultiTenantDegradedWarnThreshold {
		return
	}
	if last, seen := f.lastWarn[scheme]; seen && now.Sub(last) < MultiTenantDegradedWarnInterval {
		return
	}
	f.lastWarn[scheme] = now
	f.logger.Warn("secret_store_multi_tenant_degraded", "scheme", scheme, "degraded_tenants", n)
}

// await waits for fl. wait 0 means "until done" (the owner, whose own call
// is already bounded); a healthy follower waits at most SlotWait and then
// fails fast (serving a stale entry if one is within max-stale); wait < 0
// (a degraded tenant's follower) does not wait at all.
func (f *Fetcher) await(ctx context.Context, fl *flight, stale *positiveEntry, wait time.Duration) (Secret, error) {
	if wait < 0 {
		select {
		case <-fl.done:
		default:
			return serveStaleOr(stale, ClassUnavailable)
		}
	}
	var timeout <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-fl.done:
	case <-timeout:
		return serveStaleOr(stale, ClassUnavailable)
	case <-ctx.Done():
		return serveStaleOr(stale, ClassUnavailable)
	}
	if fl.err != nil {
		class := ClassOf(fl.err)
		if class.CountsTowardBreaker() {
			return serveStaleOr(stale, class)
		}
		return Secret{}, classError(class)
	}
	return fl.secret, nil
}

func serveStaleOr(stale *positiveEntry, class ErrorClass) (Secret, error) {
	if stale != nil {
		return stale.secret, nil
	}
	return Secret{}, classError(class)
}

// matches compares the keyed fingerprint of s with want in constant time.
func (f *Fetcher) matches(s Secret, want string) bool {
	got := f.fingerprint(s.b)
	return hmac.Equal([]byte(got), []byte(want))
}

// integrityFailure emits the P1 alert, at most once per ref per
// IntegrityAlertInterval. It never logs secret bytes; the ref and
// fingerprint are staff-only identifiers, the tenant id is not secret.
func (f *Fetcher) integrityFailure(tenantID uuid.UUID, ref Ref) {
	now := f.now()
	f.mu.Lock()
	last, seen := f.lastAlert[ref.String()]
	if seen && now.Sub(last) < IntegrityAlertInterval {
		f.mu.Unlock()
		return
	}
	f.lastAlert[ref.String()] = now
	if len(f.lastAlert) > CacheMaxEntries {
		for k, t := range f.lastAlert {
			if now.Sub(t) >= IntegrityAlertInterval {
				delete(f.lastAlert, k)
			}
		}
	}
	f.mu.Unlock()
	f.logger.Error("provider_credential_integrity_failure",
		"alert", "P1",
		"reason", "credential_integrity",
		"tenant_id", tenantID.String(),
		"secret_ref", ref.String())
}

// BreakerState reports the breaker state for tenant on scheme (tests and
// metrics): "none" if the scheme has no backend, "closed" for a tenant
// with no breaker entry.
func (f *Fetcher) BreakerState(scheme string, tenant uuid.UUID) string {
	if _, routed := f.router.Backend(scheme); !routed {
		return "none"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.breakers[breakerKey{scheme: scheme, tenant: tenant}]
	if !ok {
		return "closed"
	}
	return b.stateAt(f.now())
}

// AdmissionSnapshot reports the admission counters (tests and metrics):
// total store calls in flight, and those held by degraded tenants.
func (f *Fetcher) AdmissionSnapshot() (inFlight, degradedInFlight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight, f.degradedInFlight
}

// --- circuit breaker --------------------------------------------------------

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// breaker is guarded by Fetcher.mu.
type breaker struct {
	state         breakerState
	consecutive   int
	openUntil     time.Time
	cooldown      time.Duration
	probeInFlight bool
}

func newBreaker() *breaker { return &breaker{cooldown: BreakerInitialCooldown} }

// allow reports whether a store call may start now, and whether it is the
// single half-open probe.
func (b *breaker) allow(now time.Time) (permitted, probe bool) {
	switch b.state {
	case breakerClosed:
		return true, false
	case breakerOpen:
		if now.Before(b.openUntil) {
			return false, false
		}
		b.state = breakerHalfOpen
	}
	if b.probeInFlight {
		return false, false
	}
	b.probeInFlight = true
	return true, true
}

func (b *breaker) abortProbe() { b.probeInFlight = false }

func (b *breaker) record(now time.Time, probe, counted bool) {
	if probe {
		b.probeInFlight = false
		if counted {
			b.cooldown *= 2
			if b.cooldown > BreakerMaxCooldown {
				b.cooldown = BreakerMaxCooldown
			}
			b.state = breakerOpen
			b.openUntil = now.Add(b.cooldown)
			return
		}
		b.state = breakerClosed
		b.consecutive = 0
		b.cooldown = BreakerInitialCooldown
		return
	}
	if !counted {
		b.consecutive = 0
		return
	}
	b.consecutive++
	if b.state == breakerClosed && b.consecutive >= BreakerTripThreshold {
		b.state = breakerOpen
		b.openUntil = now.Add(b.cooldown)
	}
}

func (b *breaker) stateAt(now time.Time) string {
	switch b.state {
	case breakerOpen:
		if now.Before(b.openUntil) {
			return "open"
		}
		return "half_open"
	case breakerHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// --- bounded LRU (guarded by Fetcher.mu) -----------------------------------

type lru struct {
	max   int
	ll    *list.List
	items map[cacheKey]*list.Element
}

type lruItem struct {
	key cacheKey
	val any
}

func newLRU(max int) *lru {
	return &lru{max: max, ll: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *lru) get(k cacheKey) (any, bool) {
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruItem).val, true
	}
	return nil, false
}

func (c *lru) put(k cacheKey, v any) {
	if e, ok := c.items[k]; ok {
		e.Value.(*lruItem).val = v
		c.ll.MoveToFront(e)
		return
	}
	c.items[k] = c.ll.PushFront(&lruItem{key: k, val: v})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*lruItem).key)
	}
}

func (c *lru) remove(k cacheKey) {
	if e, ok := c.items[k]; ok {
		c.ll.Remove(e)
		delete(c.items, k)
	}
}

func (c *lru) len() int { return c.ll.Len() }

// CacheLen reports the positive cache size (tests).
func (f *Fetcher) CacheLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.positive.len()
}
