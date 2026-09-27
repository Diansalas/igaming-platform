package admission

import (
	"testing"
	"time"
)

// TestBurstAccommodation is T5: exactly burst admitted; burst+1 rejected
// with Retry-After = ceil(1/rate); advancing by one emission interval
// admits exactly one more; sustained rate is never rejected. Entirely
// driven by FakeClock - no wall-clock assertion.
func TestBurstAccommodation(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	lim := NewGCRALimiter(10, 5, 100, time.Minute, "_overflow", clock)

	for i := 0; i < 5; i++ {
		ok, _ := lim.Allow("k")
		if !ok {
			t.Fatalf("request %d within burst must be admitted", i)
		}
	}
	ok, ra := lim.Allow("k")
	if ok {
		t.Fatal("request beyond burst must be rejected")
	}
	if ra != 1*time.Second { // 1/rate = 1/10s = 100ms -> ceil to 1s
		t.Fatalf("retry-after = %v, want 1s (ceil(1/rate))", ra)
	}

	clock.Advance(100 * time.Millisecond) // one emission interval (1/rate)
	ok, _ = lim.Allow("k")
	if !ok {
		t.Fatal("exactly one more admission after one emission interval")
	}
	ok, _ = lim.Allow("k")
	if ok {
		t.Fatal("must not admit a second one at the same instant")
	}

	// Sustained at exactly `rate` (one every emission interval) is never
	// rejected.
	for i := 0; i < 50; i++ {
		clock.Advance(100 * time.Millisecond)
		ok, _ := lim.Allow("k")
		if !ok {
			t.Fatalf("sustained rate must never be rejected (iteration %d)", i)
		}
	}
}

// TestRetryAfterClamp checks the [1s, 60s] clamp (ADR 0097 §5.1).
func TestRetryAfterClamp(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	lim := NewGCRALimiter(0.01, 1, 10, time.Minute, "_overflow", clock) // T = 100s
	ok, _ := lim.Allow("k")
	if !ok {
		t.Fatal("first request must be admitted (burst=1)")
	}
	_, ra := lim.Allow("k")
	if ra != 60*time.Second {
		t.Fatalf("retry-after must clamp to 60s, got %v", ra)
	}
}

// TestCardinalityOverflow is part of T3: once maxKeys distinct keys are
// tracked, a new key shares the overflow bucket instead of growing the
// map further.
func TestCardinalityOverflow(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	lim := NewGCRALimiter(1000, 1, 2, time.Minute, "_overflow", clock)
	lim.Allow("a")
	lim.Allow("b")
	if lim.Keys() != 2 {
		t.Fatalf("want 2 keys, got %d", lim.Keys())
	}
	lim.Allow("c") // must fold into _overflow, not grow to 3
	if got := lim.Keys(); got > 3 {
		t.Fatalf("key table must stay bounded, got %d keys", got)
	}
	if !lim.OverflowOccurred() {
		t.Fatal("overflow must be recorded")
	}
}

// TestIdleEviction: a key idle past idleEvict, at full tokens, is
// evicted - freeing memory - without resetting any OTHER key's state.
func TestIdleEviction(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	lim := NewGCRALimiter(1, 1, 100, 10*time.Second, "_overflow", clock)
	lim.Allow("idle")
	if lim.Keys() != 1 {
		t.Fatal("expected 1 key")
	}
	clock.Advance(20 * time.Second)
	lim.Allow("other") // triggers eviction scan
	if lim.Keys() != 1 {
		t.Fatalf("idle key must be evicted, other key created; got %d keys", lim.Keys())
	}
}

// TestGCRANeverResetsOnOverflow: unlike ratelimit.go's fail-open auth
// limiter, overflow must never wipe existing keys' budgets (ADR 0097 §7).
func TestGCRANeverResetsOnOverflow(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	lim := NewGCRALimiter(1, 1, 1, time.Minute, "_overflow", clock)
	lim.Allow("a") // consumes a's only burst token
	ok, _ := lim.Allow("a")
	if ok {
		t.Fatal("a's single burst token must already be consumed")
	}
	lim.Allow("b") // forces overflow (maxKeys=1)
	ok, _ = lim.Allow("a")
	if ok {
		t.Fatal("overflow of a DIFFERENT key must not reset a's own bucket")
	}
}
