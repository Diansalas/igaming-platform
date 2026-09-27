package admission

import (
	"testing"
	"time"
)

func TestSuppressorWindow(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	s := NewSuppressor(10*time.Second, clock)

	emit, n := s.ShouldLog("k")
	if !emit || n != 0 {
		t.Fatalf("first call must emit with suppressed=0, got emit=%v n=%d", emit, n)
	}
	for i := 0; i < 5; i++ {
		emit, _ = s.ShouldLog("k")
		if emit {
			t.Fatal("calls within the window must be suppressed")
		}
	}
	clock.Advance(10 * time.Second)
	emit, n = s.ShouldLog("k")
	if !emit {
		t.Fatal("call after the window elapses must emit")
	}
	if n != 5 {
		t.Fatalf("suppressed count = %d, want 5", n)
	}
}

func TestSuppressorIndependentKeys(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	s := NewSuppressor(10*time.Second, clock)
	s.ShouldLog("a")
	emit, _ := s.ShouldLog("b")
	if !emit {
		t.Fatal("a different key must have its own independent window")
	}
}
