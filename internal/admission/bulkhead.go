package admission

import (
	"sync"
	"sync/atomic"
	"time"
)

// Bulkhead is the one counting-semaphore primitive ADR 0097 §5.2 uses for
// A4a (webhook in-flight), A4b (the pre-verification DB gate) and B2 (the
// per-tenant domain-transaction cap): a global cap G and a per-key cap
// chosen by the caller at acquisition time (<=G is the caller's
// responsibility - ADR 0097 §9.3 validates this at config load). The
// per-call cap parameter is what lets one Bulkhead instance serve several
// domains that each need their own "known key" vs "_unknown" cap (ADR
// 0097 §4.2/§4.4): the caller passes InFlightPerKey for a known preKey and
// InFlightUnknown for a domain's collapsed "_unknown" key - the Bulkhead
// itself only ever enforces "this key's own count vs the cap given for
// THIS call" plus the shared global count.
//
// Per-key counters exist only while > 0 (bounded cardinality <= number of
// concurrently-held keys, itself <= G). Release is idempotent (sync.Once
// per acquisition) - a double Release is a no-op, never a negative count.
type Bulkhead struct {
	mu        sync.Mutex
	globalCap int

	global int
	perKey map[string]int

	// notify is closed and replaced on every Release, so a blocked
	// Acquire wakes up and retries instead of polling.
	notify chan struct{}

	highWater int64 // atomic high-water mark of concurrently-held slots
}

// NewBulkhead constructs a Bulkhead with the given global cap.
func NewBulkhead(globalCap int) *Bulkhead {
	return &Bulkhead{globalCap: globalCap, perKey: make(map[string]int), notify: make(chan struct{})}
}

func (b *Bulkhead) tryAcquireLocked(key string, perKeyCap int) bool {
	if b.global >= b.globalCap {
		return false
	}
	if b.perKey[key] >= perKeyCap {
		return false
	}
	b.global++
	b.perKey[key]++
	if int64(b.global) > atomic.LoadInt64(&b.highWater) {
		atomic.StoreInt64(&b.highWater, int64(b.global))
	}
	return true
}

func (b *Bulkhead) releaseLocked(key string) {
	b.global--
	b.perKey[key]--
	if b.perKey[key] <= 0 {
		delete(b.perKey, key)
	}
	ch := b.notify
	b.notify = make(chan struct{})
	close(ch)
}

// releaseFunc wraps releaseLocked in a sync.Once so a caller that calls
// the returned function twice never double-decrements (ADR 0097 §5.2).
func (b *Bulkhead) releaseFunc(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.releaseLocked(key)
			b.mu.Unlock()
		})
	}
}

// TryAcquire is A4a's non-blocking acquire: it returns ok=false
// immediately if the global cap or this key's own perKeyCap is
// exhausted, never waiting.
func (b *Bulkhead) TryAcquire(key string, perKeyCap int) (release func(), ok bool) {
	b.mu.Lock()
	ok = b.tryAcquireLocked(key, perKeyCap)
	b.mu.Unlock()
	if !ok {
		return nil, false
	}
	return b.releaseFunc(key), true
}

// Acquire waits up to wait (driven by clock, never real time directly) for
// a slot. It returns ok=false if wait elapses first. Waiting never holds
// mu, and never holds any resource the caller isn't itself responsible
// for (ADR 0097 §5.2 "waiting is goroutine time only").
func (b *Bulkhead) Acquire(key string, perKeyCap int, clock Clock, wait time.Duration) (release func(), ok bool) {
	deadline := clock.Now().Add(wait)
	for {
		b.mu.Lock()
		if b.tryAcquireLocked(key, perKeyCap) {
			b.mu.Unlock()
			return b.releaseFunc(key), true
		}
		notifyCh := b.notify
		b.mu.Unlock()

		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			return nil, false
		}
		timer := clock.NewTimer(remaining)
		select {
		case <-notifyCh:
			timer.Stop()
			continue
		case <-timer.C():
			return nil, false
		}
	}
}

// InUse returns the current global in-use count and the count for one key
// (0 if untracked), for tests and gauges.
func (b *Bulkhead) InUse(key string) (global, forKey int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.global, b.perKey[key]
}

// HighWater returns the atomic high-water mark of concurrently held slots
// across the life of this Bulkhead (T8).
func (b *Bulkhead) HighWater() int {
	return int(atomic.LoadInt64(&b.highWater))
}
