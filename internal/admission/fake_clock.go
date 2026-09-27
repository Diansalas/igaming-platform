package admission

import (
	"sort"
	"sync"
	"time"
)

// FakeClock is a virtual clock/timer seam for deterministic tests (ADR
// 0097 §5.1, §11 QA review item 7: "T11 must use a virtual clock seam, not
// a wall-clock bound"). Advance moves time forward and fires every timer
// whose deadline is now due, in deadline order. Safe for concurrent use.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	seq    int
}

// NewFakeClock returns a FakeClock starting at start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

// SyntheticComponent is a structural, no-import marker satisfying
// providerkind's repo-wide mock-naming hygiene scan (internal/providerkind/
// completeness_scan.go) - FakeClock is a test double for time, never a
// wired provider.Registration, but the scan is name-based and package-
// agnostic. This package stays stdlib-only (no import of internal/
// providerkind is needed or added) - only the method shape matters.
func (f *FakeClock) SyntheticComponent() {}

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d and fires (synchronously, in
// deadline order) every timer whose deadline is now <= the new time.
// Firing sends on the timer's channel without blocking if nobody is
// receiving yet (buffered 1, matching time.Timer's own semantics).
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	due := f.timers[:0:0]
	var remaining []*fakeTimer
	for _, t := range f.timers {
		if !t.stopped && !t.deadline.After(now) {
			due = append(due, t)
		} else {
			remaining = append(remaining, t)
		}
	}
	f.timers = remaining
	sort.Slice(due, func(i, j int) bool {
		if due[i].deadline.Equal(due[j].deadline) {
			return due[i].seq < due[j].seq
		}
		return due[i].deadline.Before(due[j].deadline)
	})
	f.mu.Unlock()
	for _, t := range due {
		select {
		case t.ch <- now:
		default:
		}
	}
}

// NewTimer returns a virtual timer that fires the next time Advance moves
// the clock at or past now+d. d<=0 fires on the very next Advance call
// (even Advance(0)) so callers using a zero/negative wait get an
// immediate, deterministic timeout rather than blocking forever.
func (f *FakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	t := &fakeTimer{clock: f, deadline: f.now.Add(d), ch: make(chan time.Time, 1), seq: f.seq}
	f.timers = append(f.timers, t)
	return t
}

type fakeTimer struct {
	clock    *FakeClock
	deadline time.Time
	ch       chan time.Time
	seq      int
	stopped  bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	already := t.stopped
	t.stopped = true
	return !already
}
