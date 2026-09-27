package admission

import (
	"sync"
	"sync/atomic"
	"time"
)

// Bulkhead is the one counting-semaphore primitive ADR 0097 §5.2 uses for
// A4a (webhook in-flight), A4b (the pre-verification DB gate) and B2 (the
// per-tenant domain-transaction cap): a global cap G, a per-key cap K<=G,
// and a separate cap U for one designated "unknown" key. Per-key counters
// exist only while > 0 (bounded cardinality <= G). Release is idempotent
// (sync.Once per acquisition) - a double Release is a no-op, never a
// negative count.
type Bulkhead struct {
	mu         sync.Mutex
	globalCap  int
	perKeyCap  int
	unknownCap int
	unknownKey string

	global int
	perKey map[string]int

	// notify is closed and replaced on every Release, so a blocked
	// Acquire wakes up and retries instead of polling.
	notify chan struct{}

	highWater int64 // atomic high-water mark of concurrently-held slots
}

// NewBulkhead constructs a Bulkhead. globalCap bounds total concurrently
// held slots; perKeyCap bounds any one key (including derived pre-auth
// keys); unknownCap separately bounds unknownKey (ADR 0097 §4.2's single
// shared "_unknown" bucket per domain). unknownKey == "" disables the
// separate unknown cap (every key uses perKeyCap).
func NewBulkhead(globalCap, perKeyCap, unknownCap int, unknownKey string) *Bulkhead {
	return &Bulkhead{
		globalCap: globalCap, perKeyCap: perKeyCap, unknownCap: unknownCap, unknownKey: unknownKey,
		perKey: make(map[string]int), notify: make(chan struct{}),
	}
}

func (b *Bulkhead) capFor(key string) int {
	if b.unknownKey != "" && key == b.unknownKey {
		return b.unknownCap
	}
	return b.perKeyCap
}

func (b *Bulkhead) tryAcquireLocked(key string) bool {
	if b.global >= b.globalCap {
		return false
	}
	if b.perKey[key] >= b.capFor(key) {
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
// immediately if the global or per-key cap is exhausted, never waiting.
func (b *Bulkhead) TryAcquire(key string) (release func(), ok bool) {
	b.mu.Lock()
	ok = b.tryAcquireLocked(key)
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
func (b *Bulkhead) Acquire(key string, clock Clock, wait time.Duration) (release func(), ok bool) {
	deadline := clock.Now().Add(wait)
	for {
		b.mu.Lock()
		if b.tryAcquireLocked(key) {
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
