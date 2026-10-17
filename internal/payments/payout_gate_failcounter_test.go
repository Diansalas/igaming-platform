package payments

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Closure security review F-4: with MORE than gateFailCountsMax attempts failing closed at once (a missing key everywhere),
// every attempt still reaches the alert threshold. The previous wipe-at-bound counter reset all counts every time the map
// filled, so in a round-robin outage of > 4096 attempts no attempt ever reached 3 and the T2 / T12 alert never fired.
func TestGateFailCounter_MassOutageAboveBoundStillAlerts(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	c := newGateFailCounter(gateFailCountsMax, gateFailStaleAfter, func() time.Time { return clock })
	const stuck = gateFailCountsMax + 1000
	ids := make([]uuid.UUID, stuck)
	for i := range ids {
		ids[i] = uuid.New()
	}
	alerted := make([]bool, stuck)
	for round := 1; round <= gateUnavailableAlertAfter; round++ {
		for i, id := range ids { // one sweep ticks every stuck attempt once, in the same order (the worst case for a wipe)
			clock = clock.Add(time.Millisecond)
			if n := c.fail(id); n >= gateUnavailableAlertAfter {
				alerted[i] = true
			}
		}
	}
	for i, a := range alerted {
		if !a {
			t.Fatalf("attempt %d of %d never reached the alert threshold after %d consecutive failing sweeps", i, stuck, gateUnavailableAlertAfter)
		}
	}
	if c.len() > gateFailCountsMax {
		t.Fatalf("memory must stay bounded: %d > %d", c.len(), gateFailCountsMax)
	}
	// The tracked attempts kept their real counts (nothing still failing was dropped).
	if n := c.fail(ids[0]); n != gateUnavailableAlertAfter+1 {
		t.Fatalf("a tracked attempt's count must survive the overflow: %d", n)
	}
}

// Below the bound the behaviour is the SF4-5 one: the threshold is reached only on the 3rd consecutive failure, and a tick
// that decides resets the count.
func TestGateFailCounter_ThresholdAndReset(t *testing.T) {
	c := newGateFailCounter(8, time.Hour, time.Now)
	id := uuid.New()
	for i := 1; i < gateUnavailableAlertAfter; i++ {
		if n := c.fail(id); n >= gateUnavailableAlertAfter {
			t.Fatalf("tick %d alerted early (%d)", i, n)
		}
	}
	c.reset(id)
	if n := c.fail(id); n != 1 {
		t.Fatalf("a deciding tick resets the count, got %d", n)
	}
}

// Stale entries (attempts no longer ticked) are evicted to make room, so a later new outage is counted normally rather
// than alerting on the first failure.
func TestGateFailCounter_StaleEntriesAreEvicted(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	c := newGateFailCounter(4, time.Hour, func() time.Time { return clock })
	for i := 0; i < 4; i++ {
		c.fail(uuid.New())
	}
	clock = clock.Add(2 * time.Hour)
	id := uuid.New()
	if n := c.fail(id); n != 1 {
		t.Fatalf("after evicting stale entries the new attempt is counted from 1, got %d", n)
	}
	if c.len() != 1 {
		t.Fatalf("stale entries must be gone: %d", c.len())
	}
	// Full of FRESH entries: an untracked failing attempt is reported at the threshold (alert), never silently dropped.
	for i := 0; i < 3; i++ {
		c.fail(uuid.New())
	}
	if n := c.fail(uuid.New()); n != gateUnavailableAlertAfter {
		t.Fatalf("overflow with fresh entries must alert now, got %d", n)
	}
}
