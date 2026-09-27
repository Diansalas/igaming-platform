package admission

import (
	"sync"
	"time"
)

// Suppressor bounds admission log volume under a flood (ADR 0097 §8): at
// most one line per key per window; the next call after the window
// elapses reports how many were suppressed in between. Its key space is
// exactly the same bounded set as the limiter/bulkhead keys it is used
// alongside (the caller passes a (tier, key) composite), so log volume
// under a flood is <= keys / window, never unbounded.
type Suppressor struct {
	mu     sync.Mutex
	clock  Clock
	window time.Duration
	state  map[string]*suppressEntry
}

type suppressEntry struct {
	windowStart time.Time
	suppressed  int
}

// NewSuppressor constructs a Suppressor with the given window (ADR 0097
// §8 default: 10s).
func NewSuppressor(window time.Duration, clock Clock) *Suppressor {
	return &Suppressor{clock: clock, window: window, state: make(map[string]*suppressEntry)}
}

// ShouldLog reports whether the caller should emit a log line for key
// now. If false, the caller must not log; the eventual next true call
// reports how many calls were suppressed since the window opened.
func (s *Suppressor) ShouldLog(key string) (emit bool, suppressedCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	e, ok := s.state[key]
	if !ok || now.Sub(e.windowStart) >= s.window {
		prevSuppressed := 0
		if ok {
			prevSuppressed = e.suppressed
		}
		s.state[key] = &suppressEntry{windowStart: now}
		return true, prevSuppressed
	}
	e.suppressed++
	return false, e.suppressed
}
