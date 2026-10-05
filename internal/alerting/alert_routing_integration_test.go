//go:build integration

package alerting_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/alerting/alertingtest"
)

// ALERT-DELIVERY-1 routing readiness (ADR 0102 section 18): dispatcher
// behaviour against a real database through the REAL RUNTIME ROLE (World
// asserts the role is neither superuser nor BYPASSRLS).

func rec(c *alertingtest.RecordingChannel, id uuid.UUID) []alerting.Delivery {
	var out []alerting.Delivery
	for _, d := range c.Calls() {
		if d.AlertID == id {
			out = append(out, d)
		}
	}
	return out
}

func pass(t *testing.T, d *alerting.Dispatcher, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := d.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // real clock: let next_attempt_at (now+0) fall due
	}
}

func zeroBackoff() func(int) time.Duration { return func(int) time.Duration { return 0 } }

// N-4: an unrouted alert is a visible row with a closed reason, never a log line.
func TestRouting_UnroutedReasons_NoRoute_Disabled_NoSink(t *testing.T) {
	w := alertingtest.NewWorld(t, "arr_unr")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)

	p1 := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())  // p1
	p2 := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString()) // p2
	w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "mock-target", false)   // present but disabled

	d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{}, alerting.LogSink{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	pass(t, d, 2)
	if ev, reason := w.LatestEvent(admin, p1); ev != "unrouted" || reason != "no_route" {
		t.Fatalf("p1: %q/%q, want unrouted/no_route", ev, reason)
	}
	if ev, reason := w.LatestEvent(admin, p2); ev != "unrouted" || reason != "channel_disabled" {
		t.Fatalf("p2: %q/%q, want unrouted/channel_disabled", ev, reason)
	}

	// An ENABLED route naming a channel with no sink wired in this binary
	// (mock is not wired; only LogSink is) is a no_sink row: N-4.
	w2 := alertingtest.NewWorld(t, "arr_nosink")
	admin2 := w2.SeedAdmin()
	tenant2 := w2.SeedTenant(admin2)
	w2.AddRoute(admin2, alerting.SeverityP1, 0, alerting.ChannelMock, "mock-target", true)
	a := w2.SeedAlert(tenant2, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
	d2 := alerting.NewDispatcher(w2.Pool, alerting.DispatcherConfig{}, alerting.LogSink{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	pass(t, d2, 3)
	if ev, reason := w2.LatestEvent(admin2, a); ev != "unrouted" || reason != "no_sink" {
		t.Fatalf("%q/%q, want unrouted/no_sink", ev, reason)
	}
	var unrouted, metaOcc int
	if err := w2.Pool.WithPlatformAdmin(context.Background(), admin2, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alert_deliveries WHERE alert_id = $1 AND event = 'unrouted'`, a).Scan(&unrouted); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_occurrences o JOIN alerts x ON x.id = o.alert_id WHERE x.kind = 'alerting.unrouted'`).Scan(&metaOcc)
	}); err != nil {
		t.Fatal(err)
	}
	if unrouted != 1 {
		t.Fatalf("exactly one unrouted row per (alert, step), got %d across 3 passes", unrouted)
	}
	if metaOcc != 1 {
		t.Fatalf("the unrouted meta-alert must be raised once for the no_sink row, got %d occurrences", metaOcc)
	}
}

// Once an enabled route with a wired sink exists, a previously unrouted alert is delivered.
func TestRouting_EnabledRouteDeliversViaTestChannel_NonHuman(t *testing.T) {
	w := alertingtest.NewWorld(t, "arr_ok")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "mock-target", true)
	id := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	ch := alertingtest.NewRecordingChannel()
	d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{}, ch)
	pass(t, d, 2)
	if ev, _ := w.LatestEvent(admin, id); ev != "sent" {
		t.Fatalf("latest event %q, want sent", ev)
	}
	if got := rec(ch, id); len(got) != 1 || got[0].RecipientRef != "mock-target" || got[0].DedupKey != id.String()+":0" {
		t.Fatalf("calls: %+v", got)
	}
	// Delivered to a non-human channel is never a person being notified.
	if alerting.DeliveryState("sent", alerting.ChannelMock) != alerting.DeliveryStateRecordedNonHuman {
		t.Fatal("sent via mock must derive recorded_non_human")
	}
}

func TestRouting_PermanentErrorsAreTerminalImmediately_RetryableBurnBudget(t *testing.T) {
	for _, tc := range []struct {
		class        alerting.ErrorClass
		wantAttempts int
	}{
		{alerting.ErrorClassRejected, 1},
		{alerting.ErrorClassMisconfigured, 1},
		{alerting.ErrorClassUnavailable, 5}, // p2 budget
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			w := alertingtest.NewWorld(t, "arr_perm")
			admin := w.SeedAdmin()
			tenant := w.SeedTenant(admin)
			w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "mock-target", true)
			id := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
			ch := alertingtest.NewRecordingChannel()
			ch.FailWith(tc.class)
			d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff()}, ch)
			pass(t, d, 8)
			if ev, _ := w.LatestEvent(admin, id); ev != "dead" {
				t.Fatalf("latest %q, want dead (events %v)", ev, w.Events(admin, id))
			}
			if n := len(rec(ch, id)); n != tc.wantAttempts {
				t.Fatalf("%d delivery attempts, want %d (events %v)", n, tc.wantAttempts, w.Events(admin, id))
			}
			// dead raised exactly one delivery_dead meta occurrence for this class of failure
			var meta int
			if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'alerting.delivery_dead'`).Scan(&meta)
			}); err != nil {
				t.Fatal(err)
			}
			if meta != 1 {
				t.Fatalf("delivery_dead meta alerts = %d, want 1", meta)
			}
		})
	}
}

func TestRouting_P1BudgetIsEightAttempts(t *testing.T) {
	w := alertingtest.NewWorld(t, "arr_p1")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	w.AddRoute(admin, alerting.SeverityP1, 0, alerting.ChannelMock, "mock-target", true)
	id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
	ch := alertingtest.NewRecordingChannel()
	ch.FailWith(alerting.ErrorClassTimeout)
	d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff()}, ch)
	pass(t, d, 12)
	if n := len(rec(ch, id)); n != 8 {
		t.Fatalf("p1 made %d attempts, want 8 (events %v)", n, w.Events(admin, id))
	}
	if ev, _ := w.LatestEvent(admin, id); ev != "dead" {
		t.Fatalf("latest %q, want dead", ev)
	}
}

// A panic in one delivery is recovered per alert: the failure is recorded, the
// pass continues to the next alert, and only the panic TYPE reaches the logs.
func TestRouting_PanicInOneDeliveryDoesNotEndThePass_AndNeverLogsTheValue(t *testing.T) {
	const canary = "CANARY-PANIC-SECRET-77c1d9"
	var buf bytes.Buffer
	var mu sync.Mutex
	lw := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) })
	logger := slog.New(slog.NewTextHandler(lw, nil))
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	w := alertingtest.NewWorld(t, "arr_pan")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "mock-target", true)
	first := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	second := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	ch := alertingtest.NewRecordingChannel()
	ch.PanicOnce(canary) // an error-ish value carrying a secret
	d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff(), Logger: logger}, ch)
	if err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	evFirst, _ := w.LatestEvent(admin, first)
	evSecond, _ := w.LatestEvent(admin, second)
	got := []string{evFirst, evSecond}
	if (got[0] != "failed" || got[1] != "sent") && (got[0] != "sent" || got[1] != "failed") {
		t.Fatalf("one alert must be failed (the panicked one) and the other sent in the SAME pass: %v", got)
	}
	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	if strings.Contains(logs, canary) {
		t.Fatalf("the panic VALUE leaked into logs: %s", logs)
	}
	if !strings.Contains(logs, "panic_type=string") {
		t.Fatalf("the panic type must be logged: %s", logs)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A hung channel times out as a normal failed attempt; nothing is left stranded in 'claimed'.
func TestRouting_HungChannelBecomesTimeoutFailure(t *testing.T) {
	w := alertingtest.NewWorld(t, "arr_hang")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "mock-target", true)
	id := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	ch := alertingtest.NewRecordingChannel()
	ch.BlockUntilCtxDone()
	d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff(), ClaimLease: 200 * time.Millisecond}, ch)
	if err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev, _ := w.LatestEvent(admin, id); ev != "failed" {
		t.Fatalf("latest %q, want failed (events %v)", ev, w.Events(admin, id))
	}
	var class string
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT last_error_class FROM alert_deliveries WHERE alert_id = $1 AND event = 'failed'`, id).Scan(&class)
	}); err != nil {
		t.Fatal(err)
	}
	if class != "timeout" {
		t.Fatalf("class %q, want timeout", class)
	}
}

// p1 escalate-on-dead.
func TestRouting_P1EscalatesOnDead_P2DoesNot_AckedDoesNot_NoNextRouteTerminal(t *testing.T) {
	t.Run("p1 with a step-1 route escalates to it", func(t *testing.T) {
		w := alertingtest.NewWorld(t, "arr_esc")
		admin := w.SeedAdmin()
		tenant := w.SeedTenant(admin)
		w.AddRoute(admin, alerting.SeverityP1, 0, alerting.ChannelMock, "step-zero", true)
		w.AddRoute(admin, alerting.SeverityP1, 1, alerting.ChannelMock, "step-one", true)
		id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
		ch := alertingtest.NewRecordingChannel()
		ch.Script(alertingtest.Failed(alerting.ErrorClassRejected), alertingtest.Sent())
		d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff()}, ch)
		pass(t, d, 4)
		want := []string{"0:0:claimed", "0:0:dead", "1:0:claimed", "1:0:sent"}
		if got := w.Events(admin, id); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("events %v, want %v", got, want)
		}
		calls := rec(ch, id)
		if len(calls) != 2 || calls[1].RecipientRef != "step-one" || calls[1].DedupKey != id.String()+":1" {
			t.Fatalf("calls %+v", calls)
		}
	})
	t.Run("p1 with no step-1 route stays terminal with no extra row", func(t *testing.T) {
		w := alertingtest.NewWorld(t, "arr_esc2")
		admin := w.SeedAdmin()
		tenant := w.SeedTenant(admin)
		w.AddRoute(admin, alerting.SeverityP1, 0, alerting.ChannelMock, "step-zero", true)
		id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
		ch := alertingtest.NewRecordingChannel()
		ch.FailWith(alerting.ErrorClassRejected)
		d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff()}, ch)
		pass(t, d, 4)
		if got := w.Events(admin, id); strings.Join(got, ",") != "0:0:claimed,0:0:dead" {
			t.Fatalf("events %v", got)
		}
	})
	t.Run("a disabled step-1 route does not escalate", func(t *testing.T) {
		w := alertingtest.NewWorld(t, "arr_esc3")
		admin := w.SeedAdmin()
		tenant := w.SeedTenant(admin)
		w.AddRoute(admin, alerting.SeverityP1, 0, alerting.ChannelMock, "step-zero", true)
		w.AddRoute(admin, alerting.SeverityP1, 1, alerting.ChannelMock, "step-one", false)
		id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
		ch := alertingtest.NewRecordingChannel()
		ch.FailWith(alerting.ErrorClassRejected)
		d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff()}, ch)
		pass(t, d, 4)
		if got := w.Events(admin, id); strings.Join(got, ",") != "0:0:claimed,0:0:dead" {
			t.Fatalf("events %v", got)
		}
	})
	t.Run("p2 dead is terminal even with a step-1 route", func(t *testing.T) {
		w := alertingtest.NewWorld(t, "arr_esc4")
		admin := w.SeedAdmin()
		tenant := w.SeedTenant(admin)
		w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "step-zero", true)
		w.AddRoute(admin, alerting.SeverityP2, 1, alerting.ChannelMock, "step-one", true)
		id := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
		ch := alertingtest.NewRecordingChannel()
		ch.FailWith(alerting.ErrorClassRejected)
		d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff()}, ch)
		pass(t, d, 4)
		if got := w.Events(admin, id); strings.Join(got, ",") != "0:0:claimed,0:0:dead" {
			t.Fatalf("events %v", got)
		}
	})
	t.Run("an acknowledged p1 does not escalate", func(t *testing.T) {
		w := alertingtest.NewWorld(t, "arr_esc5")
		admin := w.SeedAdmin()
		tenant := w.SeedTenant(admin)
		w.AddRoute(admin, alerting.SeverityP1, 0, alerting.ChannelMock, "step-zero", true)
		w.AddRoute(admin, alerting.SeverityP1, 1, alerting.ChannelMock, "step-one", true)
		id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
		ch := alertingtest.NewRecordingChannel()
		ch.FailWith(alerting.ErrorClassRejected)
		d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: zeroBackoff()}, ch)
		pass(t, d, 1) // dead at step 0, step 1 is due on the next pass
		if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE alerts SET state = 'acked' WHERE id = $1`, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		pass(t, d, 3)
		if got := w.Events(admin, id); strings.Join(got, ",") != "0:0:claimed,0:0:dead" {
			t.Fatalf("events %v", got)
		}
	})
}

// Readiness is configuration-only: a log/mock route never counts, and today nothing can be ready.
func TestRouting_ReadinessNeverReadyWithoutAHumanChannel_ConfigOnly(t *testing.T) {
	w := alertingtest.NewWorld(t, "arr_rdy")
	admin := w.SeedAdmin()
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }), nil))
	clk := &stepClock{now: time.Now()}
	d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Clock: clk, Logger: logger, ReadinessStaleAfter: time.Minute},
		alerting.LogSink{Logger: logger}, alertingtest.NewRecordingChannel())

	eff := d.ReadinessEffective()
	for _, r := range eff {
		if r.Ready || r.Reason != alerting.ReadinessNotEvaluated {
			t.Fatalf("before any evaluation everything is not ready: %+v", r)
		}
	}
	if err := d.EvaluateReadiness(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range d.ReadinessEffective() {
		if r.Ready {
			t.Fatalf("ready with no routes: %+v", r)
		}
	}
	mu.Lock()
	first := buf.String()
	mu.Unlock()
	if !strings.Contains(first, "alert_routing_not_ready") || !strings.Contains(first, "missing_operator_input") {
		t.Fatalf("not-ready must be logged at Error with the missing input: %s", first)
	}

	// Enabled log AND mock routes for p1 and p2 at step 0: still not ready.
	for _, sev := range []alerting.Severity{alerting.SeverityP1, alerting.SeverityP2} {
		w.AddRoute(admin, sev, 0, alerting.ChannelLog, "log-target", true)
	}
	clk.now = time.Now()
	if err := d.EvaluateReadiness(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range d.ReadinessEffective() {
		if r.Required && (r.Ready || r.Reason != alerting.ReadinessNotHuman) {
			t.Fatalf("a log route must report channel_not_human_notification: %+v", r)
		}
		if r.Required && (r.RoutesEnabled != 1 || r.HumanRoutesEnabled != 0) {
			t.Fatalf("counts: %+v", r)
		}
	}

	// Stale = not ready, with no new evaluation.
	clk.now = clk.now.Add(2 * time.Minute)
	for _, r := range d.ReadinessEffective() {
		if r.Reason != alerting.ReadinessStale {
			t.Fatalf("after the stale window every severity reports evaluation_stale: %+v", r)
		}
	}
}

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time                       { return c.now }
func (c *stepClock) Sleep(context.Context, time.Duration) {}

// lateHumanSink panics from HumanNotification AFTER its first Deliver (a panic
// outside Deliver, in the recording path): the outer per-alert recovery must
// keep the pass alive.
type lateHumanSink struct {
	*alertingtest.RecordingChannel
	armed bool
}

func (s *lateHumanSink) Deliver(ctx context.Context, d alerting.Delivery) (alerting.Outcome, alerting.ErrorClass) {
	s.armed = true
	return s.RecordingChannel.Deliver(ctx, d)
}

func (s *lateHumanSink) HumanNotification() bool {
	if s.armed {
		panic("late panic outside Deliver")
	}
	return false
}

func TestRouting_PanicOutsideDeliverDoesNotEndThePass(t *testing.T) {
	w := alertingtest.NewWorld(t, "arr_pan2")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "mock-target", true)
	first := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	second := w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))},
		&lateHumanSink{RecordingChannel: alertingtest.NewRecordingChannel()})
	if err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{first, second} {
		if ev, _ := w.LatestEvent(admin, id); ev != "sent" {
			t.Fatalf("alert %s: %q, want sent (a panic outside Deliver must not strand later alerts)", id, ev)
		}
	}
}
