package marketgate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/geolocation"
)

// stubbornLocator ignores its context entirely and answers only when released.
type stubbornLocator struct {
	release chan struct{}
	calls   int
	mu      sync.Mutex
}

func (l *stubbornLocator) GetSignal(_ context.Context, _ geolocation.SignalRequest) (geolocation.SignalResult, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	<-l.release
	return geolocation.SignalResult{Outcome: geolocation.SignalResolved, CountryCode: "MT"}, nil
}

// SR8-4: the deadline holds even when the Locator never looks at its context,
// and the late (resolved!) answer is discarded.
func TestLocate_HardDeadlineWithALocatorThatIgnoresCancellation(t *testing.T) {
	l := &stubbornLocator{release: make(chan struct{})}
	g := New(l)
	g.timeout = 30 * time.Millisecond
	start := time.Now()
	ev := g.Locate(context.Background(), LocateRequest{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Locate blocked %v on a Locator that ignores its context", elapsed)
	}
	if ev.Known() {
		t.Fatal("a lookup that missed the deadline must be unknown")
	}
	close(l.release) // the late answer arrives after the deadline: discarded, goroutine exits
	deadline := time.Now().Add(2 * time.Second)
	for len(g.slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(g.slots) != 0 {
		t.Fatalf("the abandoned lookup goroutine did not release its slot (%d held)", len(g.slots))
	}
}

// SR8-4: abandoned lookups are bounded; beyond the bound a request is unknown at once.
func TestLocate_AbandonedLookupsAreBounded(t *testing.T) {
	l := &stubbornLocator{release: make(chan struct{})}
	defer close(l.release)
	g := New(l)
	g.timeout = 5 * time.Millisecond
	for i := 0; i < maxInflightLookups; i++ {
		if g.Locate(context.Background(), LocateRequest{}).Known() {
			t.Fatal("unexpected evidence")
		}
	}
	l.mu.Lock()
	callsBefore := l.calls
	l.mu.Unlock()
	start := time.Now()
	if g.Locate(context.Background(), LocateRequest{}).Known() {
		t.Fatal("over-bound lookup must be unknown")
	}
	if time.Since(start) > 4*time.Millisecond {
		t.Fatalf("over-bound lookup should refuse immediately, took %v", time.Since(start))
	}
	time.Sleep(20 * time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls != callsBefore {
		t.Fatal("an over-bound request must not start another lookup goroutine")
	}
}

func TestOpCodes_SportsbookAndCasinoBet(t *testing.T) {
	op, prod, ok := opCodes(OpSportsbookWager)
	if !ok || op != "wagering" || prod == nil || *prod != "sportsbook" {
		t.Fatalf("sportsbook: %q %v %v", op, prod, ok)
	}
	op, prod, ok = opCodes(OpCasinoBet)
	if !ok || op != "wagering" || prod == nil || *prod != "casino" {
		t.Fatalf("casino bet: %q %v %v", op, prod, ok)
	}
}

// SR8-5: the refusal audit is sampled per key; the refusal itself never is.
func TestRefusalSampler_BurstThenRefillAndSuppressedCount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newRefusalSampler()
	s.now = func() time.Time { return now }
	k := refusalKey{tenant: uuid.New(), brand: uuid.New(), op: OpRegistration, reason: ReasonNotPermitted}
	for i := 0; i < refusalAuditBurst; i++ {
		if w, sup := s.take(k); !w || sup != 0 {
			t.Fatalf("burst row %d: write=%v suppressed=%d", i, w, sup)
		}
	}
	for i := 0; i < 20; i++ {
		if w, _ := s.take(k); w {
			t.Fatal("over-burst refusal must not write an audit row")
		}
	}
	now = now.Add(refusalAuditRefill)
	w, sup := s.take(k)
	if !w || sup != 20 {
		t.Fatalf("after one refill interval: want a row recording 20 suppressed, got write=%v suppressed=%d", w, sup)
	}
	if w, _ := s.take(k); w {
		t.Fatal("only one token per refill interval")
	}
}

func TestRefusalSampler_KeysAreIndependent(t *testing.T) {
	s := newRefusalSampler()
	a := refusalKey{tenant: uuid.New(), brand: uuid.New(), op: OpDeposit, reason: ReasonNotPermitted, player: uuid.New()}
	b := a
	b.player = uuid.New()
	for i := 0; i < refusalAuditBurst+3; i++ {
		s.take(a)
	}
	if w, _ := s.take(b); !w {
		t.Fatal("another player's refusals must not be suppressed by this player's flood")
	}
	c := a
	c.reason = ReasonEvidenceUnavailable
	if w, _ := s.take(c); !w {
		t.Fatal("a different reason has its own bucket")
	}
}

func TestRefusalSampler_MapIsBounded(t *testing.T) {
	s := newRefusalSampler()
	for i := 0; i < refusalAuditMaxKeys+10; i++ {
		s.take(refusalKey{tenant: uuid.New(), op: OpLaunch})
	}
	if len(s.buckets) > refusalAuditMaxKeys {
		t.Fatalf("bucket map grew past its bound: %d", len(s.buckets))
	}
}

func TestUngatedForTests_AllowsAndIsNamed(t *testing.T) {
	d, err := UngatedForTests(context.Background(), nil)
	if err != nil || !d.Allowed {
		t.Fatalf("UngatedForTests must allow, got %+v %v", d, err)
	}
}
