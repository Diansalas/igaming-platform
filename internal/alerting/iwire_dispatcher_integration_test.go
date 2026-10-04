//go:build integration

package alerting

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ADR 0102 16.3 item 3 (mandatory before any real channel): the sink gets a
// DedupKey "<alert_id>:<step>" that is STABLE across retry attempts, while the
// per-attempt IdempotencyKey keeps changing; escalation to the next step is a
// different DedupKey. The paged payload carries the discriminator (which
// condition within a Kind fired) and the subject tenant, never another
// tenant's data.
func TestIWire_Dispatcher_DedupKeyStableAcrossRetries_DiscriminatorInPayload(t *testing.T) {
	pool := scratchPool(t, "iwdedup")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:dedup")
	disc := "switch:" + uuid.NewString()
	alertID := seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, disc)

	calls := 0
	sink := &MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) {
		calls++
		if calls < 3 {
			return OutcomeFailed, ErrorClassTimeout
		}
		return OutcomeSent, ErrorClassNone
	}}
	disp := NewDispatcher(pool, DispatcherConfig{MaxAttempts: 5, Clock: clock, Backoff: func(int) time.Duration { return time.Minute }}, sink)
	for i := 0; i < 3; i++ {
		if err := disp.RunOnce(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		clock.Advance(2 * time.Minute)
	}
	if len(sink.Attempts) != 3 {
		t.Fatalf("want 3 attempts (2 failures then success), got %d", len(sink.Attempts))
	}
	wantDedup := fmt.Sprintf("%s:%d", alertID, 0)
	seen := map[string]bool{}
	for i, d := range sink.Attempts {
		if d.DedupKey != wantDedup {
			t.Fatalf("attempt %d DedupKey = %q, want the stable %q", i, d.DedupKey, wantDedup)
		}
		if want := fmt.Sprintf("%s:%d:%d", alertID, 0, i); d.IdempotencyKey != want {
			t.Fatalf("attempt %d IdempotencyKey = %q, want %q", i, d.IdempotencyKey, want)
		}
		seen[d.IdempotencyKey] = true
		if d.Discriminator != disc {
			t.Fatalf("attempt %d Discriminator = %q, want %q", i, d.Discriminator, disc)
		}
		if d.SubjectTenantID != tenantA || d.AlertID != alertID {
			t.Fatalf("attempt %d payload ids: %+v", i, d)
		}
		if d.Severity != SeverityP2 || d.Kind != KindPaymentKillSwitchEngaged {
			t.Fatalf("attempt %d severity/kind: %+v", i, d)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("the per-attempt IdempotencyKey must differ per attempt: %v", seen)
	}
	assertLatestEvent(t, pool, admin, alertID, "sent")
}

// Severity handling: a p1 alert resolves ONLY the p1 route; a p2-only route
// leaves it unrouted (never silently delivered to a lower-severity channel).
func TestIWire_Dispatcher_SeverityRoutesAreNotInterchangeable(t *testing.T) {
	pool := scratchPool(t, "iwsev")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:p2-only")
	p1Alert := seedOpenAlert(t, pool, tenantA, KindPaymentWebhookIntegrity, "provider:x:reason:"+uuid.NewString()[:8])

	sink := &MockSink{}
	disp := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, sink)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := attemptsOfKind(sink, KindPaymentWebhookIntegrity); len(got) != 0 {
		t.Fatalf("a p1 alert must not be delivered through a p2 route: %+v", got)
	}
	assertLatestEvent(t, pool, admin, p1Alert, "unrouted")

	addTestRoute(t, pool, admin, SeverityP1, 0, ChannelMock, "mock:p1")
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// (The shared alerting.unrouted meta-alert is p2 and legitimately uses the p2 route.)
	if got := attemptsOfKind(sink, KindPaymentWebhookIntegrity); len(got) != 1 || got[0].RecipientRef != "mock:p1" {
		t.Fatalf("once a p1 route exists the unrouted p1 alert is delivered to it: %+v", got)
	}
	assertLatestEvent(t, pool, admin, p1Alert, "sent")
}

// A LogSink delivery of a P1 puts the discriminator in the structured log line.
func TestIWire_LogSink_CarriesDiscriminatorAndDedupKey(t *testing.T) {
	var buf strings.Builder
	sink := LogSink{Logger: newTextLogger(&buf)}
	sink.Deliver(context.Background(), Delivery{
		AlertID: uuid.New(), Kind: KindPaymentWebhookIntegrity, Severity: SeverityP1,
		Discriminator: "attempt:abc:reason:sync_amount_mismatch", DedupKey: "id:0", IdempotencyKey: "id:0:1", RecipientRef: "mock:x",
	})
	out := buf.String()
	for _, want := range []string{"attempt:abc:reason:sync_amount_mismatch", "dedup_key=id:0", "level=ERROR"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log line lacks %q: %s", want, out)
		}
	}
}

// The loop wired to a real dispatcher: a sink that panics mid-delivery is
// recovered per pass (the process survives), the stranded 'claimed' row is
// reclaimed once its lease expires, and the alert is then delivered.
func TestIWire_DispatcherLoop_PanicRecoveredThenStaleClaimReclaimedAndDelivered(t *testing.T) {
	pool := scratchPool(t, "iwloop")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:loop")
	alertID := seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	const lease = time.Minute
	n := 0
	sink := &MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) {
		n++
		if n == 1 {
			panic("sink exploded")
		}
		return OutcomeSent, ErrorClassNone
	}}
	disp := NewDispatcher(pool, DispatcherConfig{MaxAttempts: 5, Clock: clock, ClaimLease: lease, Backoff: func(int) time.Duration { return 0 }}, sink)

	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	results := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunDispatcherLoopWithConfig(ctx, disp, LoopConfig{
			Interval: time.Hour, DrainTimeout: time.Hour, Logger: quietLogger(),
			NewTicker: func(time.Duration) Ticker { return &fakeTicker{ch: tick} },
			OnPass:    func(r string) { results <- r },
		})
	}()

	if got := <-results; got != "panic" {
		t.Fatalf("pass 1 result %q, want panic (recovered)", got)
	}
	assertLatestEvent(t, pool, admin, alertID, "claimed") // stranded between claim and outcome

	clock.Advance(lease + time.Second)
	tick <- time.Time{}
	if got := <-results; got != "ok" {
		t.Fatalf("pass 2 result %q, want ok", got)
	}
	assertLatestEvent(t, pool, admin, alertID, "sent")
	cancel()
	<-done
}

func newTextLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func attemptsOfKind(s *MockSink, k Kind) []Delivery {
	var out []Delivery
	for _, d := range s.Attempts {
		if d.Kind == k {
			out = append(out, d)
		}
	}
	return out
}
