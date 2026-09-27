package admission

import (
	"sync"
	"testing"
	"time"
)

func TestBulkheadTryAcquireCaps(t *testing.T) {
	b := NewBulkhead(3)
	r1, ok := b.TryAcquire("a", 2)
	if !ok {
		t.Fatal("first acquire for a must succeed")
	}
	r2, ok := b.TryAcquire("a", 2)
	if !ok {
		t.Fatal("second acquire for a (within perKeyCap=2) must succeed")
	}
	if _, ok := b.TryAcquire("a", 2); ok {
		t.Fatal("third acquire for a must fail (perKeyCap=2)")
	}
	if _, ok := b.TryAcquire("_unknown", 1); !ok {
		t.Fatal("first acquire for _unknown (within unknownCap=1) must succeed")
	}
	if _, ok := b.TryAcquire("_unknown", 1); ok {
		t.Fatal("second acquire for _unknown must fail (unknownCap=1)")
	}
	// global cap 3 now fully consumed (2 for a, 1 for _unknown); a
	// DIFFERENT key must also fail even though its own per-key cap isn't
	// hit, because the global cap governs first.
	if _, ok := b.TryAcquire("b", 2); ok {
		t.Fatal("global cap must be enforced across keys")
	}
	r1()
	if _, ok := b.TryAcquire("b", 2); !ok {
		t.Fatal("releasing a slot must free the global cap for another key")
	}
	r2()
}

func TestBulkheadReleaseIdempotent(t *testing.T) {
	b := NewBulkhead(1)
	release, ok := b.TryAcquire("a", 1)
	if !ok {
		t.Fatal("acquire must succeed")
	}
	release()
	release() // must be a no-op, not a negative count
	release()
	global, forKey := b.InUse("a")
	if global != 0 || forKey != 0 {
		t.Fatalf("double release corrupted counts: global=%d forKey=%d", global, forKey)
	}
	if _, ok := b.TryAcquire("a", 1); !ok {
		t.Fatal("slot must be available exactly once after idempotent release")
	}
}

// TestBulkheadAcquireWaitsAndTimesOut drives Acquire's bounded wait via
// FakeClock only - no real sleeping (ADR 0097 §11 "no wall-clock
// assertions").
func TestBulkheadAcquireWaitsAndTimesOut(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	b := NewBulkhead(1)
	release, ok := b.TryAcquire("a", 1)
	if !ok {
		t.Fatal("first acquire must succeed")
	}

	done := make(chan bool, 1)
	go func() {
		_, ok := b.Acquire("a", 1, clock, 2*time.Second)
		done <- ok
	}()

	// Give the goroutine a chance to block on the timer, then let the
	// wait elapse without ever releasing the slot: it must time out.
	waitForTimerRegistered(t, clock)
	clock.Advance(2 * time.Second)
	if ok := <-done; ok {
		t.Fatal("Acquire must time out (false) when the slot never frees")
	}
	release()
}

// TestBulkheadAcquireWakesOnRelease: a waiter is admitted as soon as a
// slot frees, without waiting for its full timeout.
func TestBulkheadAcquireWakesOnRelease(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	b := NewBulkhead(1)
	release, ok := b.TryAcquire("a", 1)
	if !ok {
		t.Fatal("first acquire must succeed")
	}

	done := make(chan bool, 1)
	go func() {
		_, ok := b.Acquire("a", 1, clock, time.Hour)
		done <- ok
	}()
	waitForTimerRegistered(t, clock)
	release()
	if ok := <-done; !ok {
		t.Fatal("Acquire must be admitted promptly once a slot frees")
	}
}

// waitForTimerRegistered polls (real time, bounded, generous) until the
// FakeClock has at least one pending timer - i.e. the Acquire goroutine
// reached its select. This is test synchronization only, not a timing
// assertion on the admission code under test: it never determines
// pass/fail, only when it is safe to call Advance.
func waitForTimerRegistered(t *testing.T, clock *FakeClock) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		clock.mu.Lock()
		n := len(clock.timers)
		clock.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timer never registered")
}

// TestBulkheadRaces exercises concurrent acquire/release under -race,
// asserting the atomic high-water mark never exceeds the global cap and
// that InUse never goes negative (T8).
func TestBulkheadRaces(t *testing.T) {
	const globalCap = 8
	b := NewBulkhead(globalCap)
	var wg sync.WaitGroup
	keys := []string{"a", "b", "c", "_unknown"}
	for i := 0; i < 200; i++ {
		wg.Add(1)
		key := keys[i%len(keys)]
		go func(key string) {
			defer wg.Done()
			if release, ok := b.TryAcquire(key, 4); ok {
				release()
				release() // double release under race
			}
		}(key)
	}
	wg.Wait()
	if hw := b.HighWater(); hw > globalCap {
		t.Fatalf("high water %d exceeded global cap %d", hw, globalCap)
	}
	global, _ := b.InUse("a")
	if global < 0 {
		t.Fatalf("global in-use went negative: %d", global)
	}
}
