package alertingtest

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// Subject is one channel under conformance test plus the hooks the suite
// needs. Optional hooks are nil when the channel cannot do that thing (the
// matching case is then skipped, never silently passed for a channel that
// could).
type Subject struct {
	Sink alerting.Sink
	// Notifications returns the number of distinct human-visible
	// notifications produced so far (nil: the channel has no countable
	// notification, as for the log channel).
	Notifications func() int
	// Hang makes the next Deliver block until its context is done (nil: the
	// channel cannot hang).
	Hang func()
	// Fail makes subsequent deliveries fail with the class (nil: the channel
	// never fails).
	Fail func(class alerting.ErrorClass)
	// Logs returns everything the channel logged.
	Logs func() string
}

// Factory builds a fresh Subject whose logging goes to logger.
type Factory func(t *testing.T, logger *slog.Logger) Subject

// canarySecret is placed in every message and in the recipient_ref-adjacent
// fields; no channel may ever log it.
const canarySecret = "CANARY-SECRET-3f9a1c7e5b2d8046"

func delivery(dedupKey, idemKey, recipient string) alerting.Delivery {
	return alerting.Delivery{
		IdempotencyKey: idemKey, DedupKey: dedupKey,
		Discriminator: "attempt:conformance", AlertID: uuid.New(),
		Kind: alerting.KindPaymentKillSwitchEngaged, Severity: alerting.SeverityP2,
		Attributes:   map[string]alerting.AttrValue{"token": canarySecret, "note": "<script>alert(1)</script>\x1b[31m"},
		RecipientRef: recipient,
	}
}

// RunChannelConformance is the slim conformance suite every alert channel
// must pass before it can be merged (ADR 0102 section 17.7 / 18): dedupe on
// DedupKey, context cancellation, error classification, no secrets in logs,
// and recipient_ref treated strictly as opaque data (never dialled).
func RunChannelConformance(t *testing.T, factory Factory) {
	t.Helper()
	newSubject := func(t *testing.T) (Subject, *strings.Builder) {
		var sb strings.Builder
		s := factory(t, slog.New(slog.NewTextHandler(&lockedWriter{w: &sb}, nil)))
		return s, &sb
	}

	t.Run("not_a_human_notification_unless_declared", func(t *testing.T) {
		s, _ := newSubject(t)
		if s.Sink.HumanNotification() {
			t.Fatalf("channel %q claims HumanNotification; no human channel is implemented", s.Sink.ChannelKind())
		}
	})

	t.Run("dedupes_on_dedup_key_across_retries", func(t *testing.T) {
		s, _ := newSubject(t)
		dedup := uuid.NewString() + ":0"
		for attempt := 0; attempt < 3; attempt++ {
			out, class := s.Sink.Deliver(context.Background(), delivery(dedup, dedup+":"+string(rune('0'+attempt)), "ops-target"))
			if out != alerting.OutcomeSent || class != alerting.ErrorClassNone {
				t.Fatalf("attempt %d: got %q/%q, want sent", attempt, out, class)
			}
		}
		if s.Notifications != nil {
			if n := s.Notifications(); n != 1 {
				t.Fatalf("three attempts with one DedupKey produced %d notifications, want exactly 1 (adapter must dedupe on DedupKey, never the per-attempt key)", n)
			}
		}
	})

	t.Run("honours_context_cancellation", func(t *testing.T) {
		s, _ := newSubject(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Sink.Deliver(ctx, delivery("k:0", "k:0:0", "ops-target"))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Deliver ignored an already-cancelled context")
		}
		if s.Hang == nil {
			return
		}
		s.Hang()
		hctx, hcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer hcancel()
		type res struct {
			o alerting.Outcome
			c alerting.ErrorClass
		}
		ch := make(chan res, 1)
		go func() {
			o, c := s.Sink.Deliver(hctx, delivery("k:1", "k:1:0", "ops-target"))
			ch <- res{o, c}
		}()
		select {
		case r := <-ch:
			if r.o != alerting.OutcomeFailed || r.c != alerting.ErrorClassTimeout {
				t.Fatalf("hung channel returned %q/%q, want failed/timeout", r.o, r.c)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("hung Deliver did not return after its context expired")
		}
	})

	t.Run("classifies_every_failure", func(t *testing.T) {
		s, _ := newSubject(t)
		if s.Fail == nil {
			t.Skip("channel never fails")
		}
		for _, c := range []alerting.ErrorClass{alerting.ErrorClassTimeout, alerting.ErrorClassUnavailable,
			alerting.ErrorClassRejected, alerting.ErrorClassMisconfigured, alerting.ErrorClassUnknown} {
			s.Fail(c)
			out, got := s.Sink.Deliver(context.Background(), delivery(uuid.NewString()+":0", "x", "ops-target"))
			if out != alerting.OutcomeFailed || got != c {
				t.Fatalf("scripted %q: got %q/%q", c, out, got)
			}
		}
	})

	t.Run("never_logs_secrets_or_attribute_values", func(t *testing.T) {
		s, logs := newSubject(t)
		s.Sink.Deliver(context.Background(), delivery(uuid.NewString()+":0", "x", "ops-target"))
		out := logs.String()
		if s.Logs != nil {
			out += s.Logs()
		}
		if strings.Contains(out, canarySecret) || strings.Contains(out, "<script>") {
			t.Fatalf("channel logged attribute content: %s", out)
		}
	})

	t.Run("recipient_ref_is_data_never_a_network_address", func(t *testing.T) {
		// A local listener stands in for an SSRF target: a channel that dials,
		// resolves or fetches recipient_ref would connect to it.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("no loopback listener: %v", err)
		}
		defer func() { _ = ln.Close() }()
		var conns atomic.Int32
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				conns.Add(1)
				_ = c.Close()
			}
		}()
		s, _ := newSubject(t)
		for _, ref := range []string{ln.Addr().String(), "http://" + ln.Addr().String() + "/hook", "169.254.169.254:80"} {
			s.Sink.Deliver(context.Background(), delivery(uuid.NewString()+":0", "x", ref))
		}
		time.Sleep(50 * time.Millisecond)
		if n := conns.Load(); n != 0 {
			t.Fatalf("channel connected to a recipient_ref-derived address %d time(s)", n)
		}
	})
}

// lockedWriter serialises writes so concurrent Deliver calls can share a
// logger under -race.
type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
