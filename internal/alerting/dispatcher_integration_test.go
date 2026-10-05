//go:build integration

package alerting

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func seedOpenAlert(t *testing.T, pool *db.Pool, tenantID uuid.UUID, kind Kind, discriminator string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenantID, string(kind), discriminator).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed open alert: %v", err)
	}
	return id
}

// addTestRoute inserts an alert_routes row as the platform admin (the
// route write API is platform-admin-only and audited per the ADR; I-wire
// builds the real admin endpoint, this is the raw insert an admin session
// is entitled to make).
func addTestRoute(t *testing.T, pool *db.Pool, adminID uuid.UUID, severity Severity, step int, channel ChannelKind, recipientRef string) {
	t.Helper()
	err := pool.WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code) VALUES ('platform', $1, $2, $3, $4, true, 'initial_setup')`,
			string(severity), step, string(channel), recipientRef)
		return err
	})
	if err != nil {
		t.Fatalf("add route: %v", err)
	}
}

func assertLatestEvent(t *testing.T, pool *db.Pool, admin, alertID uuid.UUID, want string) {
	t.Helper()
	var got string
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT event FROM alert_deliveries WHERE alert_id = $1 ORDER BY recorded_at DESC LIMIT 1`, alertID).Scan(&got)
	})
	if err != nil {
		t.Fatalf("read latest event: %v", err)
	}
	if got != want {
		t.Fatalf("expected latest delivery event %q, got %q", want, got)
	}
}

// TestDispatcher_ZeroRoutesProducesUnroutedAndMetaOccurrence is LF test 8:
// with zero routes, M business alerts produce exactly M unrouted rows
// across their own (alert, step=0), and the shared alerting.unrouted
// alert accumulates exactly M occurrences (HD-PRH2-4: alert_routes ships
// empty). Also exercises unrouted idempotency across K dispatcher passes
// (LF F9). The shared meta-alert itself ALSO becomes unrouted (it gets
// its own unrouted delivery row, expected M+1 total) but raises no
// FURTHER alerting.unrouted occurrence about itself (SR-6/AL-13).
func TestDispatcher_ZeroRoutesProducesUnroutedAndMetaOccurrence(t *testing.T) {
	pool := scratchPool(t, "adisp0")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	const m = 3
	for i := 0; i < m; i++ {
		seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())
	}

	disp := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, LogSink{})
	// K = 3 passes: a repeat pass over an already-unrouted (alert, step)
	// must not add more unrouted rows or more meta occurrences.
	for pass := 0; pass < 3; pass++ {
		if err := disp.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce pass %d: %v", pass, err)
		}
	}

	var unroutedRows, metaAlerts, metaOccurrences int
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alert_deliveries WHERE event = 'unrouted'`).Scan(&unroutedRows); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingUnrouted)).Scan(&metaAlerts); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_occurrences o JOIN alerts a ON a.id = o.alert_id WHERE a.kind = $1`, string(KindAlertingUnrouted)).Scan(&metaOccurrences)
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if unroutedRows != m+1 {
		t.Fatalf("expected exactly %d unrouted delivery rows (%d business alerts + the meta-alert's own), got %d", m+1, m, unroutedRows)
	}
	if metaAlerts != 1 {
		t.Fatalf("expected exactly 1 shared alerting.unrouted alert (deduped by severity), got %d", metaAlerts)
	}
	if metaOccurrences != m {
		t.Fatalf("expected exactly %d alerting.unrouted occurrences, got %d", m, metaOccurrences)
	}
}

// TestDispatcher_MockSinkRetryThenDead exercises bounded retry -> dead ->
// alerting.delivery_dead.
func TestDispatcher_MockSinkRetryThenDead(t *testing.T) {
	pool := scratchPool(t, "adisp1")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:always-fails")

	alertID := seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	sink := &MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) { return OutcomeFailed, ErrorClassUnavailable }}
	disp := NewDispatcher(pool, DispatcherConfig{MaxAttempts: 2, Clock: fakeInstantClock{}, Backoff: func(int) time.Duration { return 0 }}, sink)

	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}

	assertLatestEvent(t, pool, admin, alertID, "dead")

	var deadMetaCount int
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingDeliveryDead)).Scan(&deadMetaCount)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if deadMetaCount != 1 {
		t.Fatalf("expected exactly 1 alerting.delivery_dead alert, got %d", deadMetaCount)
	}
	if len(sink.Attempts) != 2 {
		t.Fatalf("expected exactly 2 delivery attempts, got %d", len(sink.Attempts))
	}
}

// TestDispatcher_RouteAddedLaterGetsDelivered.
func TestDispatcher_RouteAddedLaterGetsDelivered(t *testing.T) {
	pool := scratchPool(t, "adisp2")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	alertID := seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	disp := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, LogSink{})
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass without route: %v", err)
	}
	assertLatestEvent(t, pool, admin, alertID, "unrouted")

	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelLog, "log:ops")
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass with route: %v", err)
	}
	assertLatestEvent(t, pool, admin, alertID, "sent")
}

// TestDispatcher_SimulationAlertNeverDelivered is AL-9.
func TestDispatcher_SimulationAlertNeverDelivered(t *testing.T) {
	pool := scratchPool(t, "adisp3")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	addTestRoute(t, pool, admin, SeverityP3, 0, ChannelLog, "log:ops")
	alertID := seedOpenAlert(t, pool, tenantA, KindSimulationCasinoPlay, "session:s1:reason:x")

	disp := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, LogSink{})
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	assertLatestEvent(t, pool, admin, alertID, "suppressed_simulation")
}

// TestDispatcher_MetaKindsDoNotRecurse is SR-6/AL-13: an alerting.* alert
// that is itself unrouted raises no further alerting.unrouted occurrence.
func TestDispatcher_MetaKindsDoNotRecurse(t *testing.T) {
	pool := scratchPool(t, "adisp4")
	admin := seedPlatformAdmin(t, pool)

	// Seed an alerting.unrouted alert directly, as the dispatcher identity
	// would via the meta-only insert policy.
	err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return Raise(ctx, tx, Alert{Kind: KindAlertingUnrouted, Discriminator: "severity:p2", Attributes: map[string]AttrValue{}})
	})
	if err != nil {
		t.Fatalf("seed meta alert: %v", err)
	}

	disp := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, LogSink{})
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var count int
	err = pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingUnrouted)).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	// The seeded alert itself is unrouted (dedup collapses any further
	// occurrence into the SAME row via the shared severity:p2 key), and
	// processing it must never spawn a SECOND alerting.unrouted alert.
	if count != 1 {
		t.Fatalf("expected exactly 1 alerting.unrouted alert (no recursion), got %d", count)
	}
}

// TestDispatcher_TwoDispatchersOneClaim is the multi-instance invariant:
// two dispatcher instances racing the same due work claim exactly once
// each (at-least-once, never double-delivered for the same attempt).
func TestDispatcher_TwoDispatchersOneClaim(t *testing.T) {
	pool := scratchPool(t, "adisp5")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:ops")
	seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	sinkA := &MockSink{}
	sinkB := &MockSink{}
	dispA := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, sinkA)
	dispB := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, sinkB)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- dispA.RunOnce(context.Background()) }()
	go func() { defer wg.Done(); errs <- dispB.RunOnce(context.Background()) }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("dispatcher RunOnce: %v", err)
		}
	}

	total := len(sinkA.Attempts) + len(sinkB.Attempts)
	if total != 1 {
		t.Fatalf("expected exactly 1 delivery attempt across both dispatcher instances, got %d", total)
	}
}
