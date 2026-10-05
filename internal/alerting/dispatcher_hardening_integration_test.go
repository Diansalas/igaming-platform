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
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// manualClock is a test-only Clock whose Now() is set explicitly by the
// test (plan rule T-1: no sleep-and-hope, no wall-clock assertions).
// Sleep is a no-op - nothing in this package's own logic depends on
// Sleep actually blocking; RaiseDetached/Pending.Flush use it only
// between their own bounded attempts, and the dispatcher never calls it
// at all (due-work timing is entirely `next_attempt_at <= Now()`-driven).
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(start time.Time) *manualClock { return &manualClock{now: start} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Sleep(context.Context, time.Duration) {}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestDispatcher_BackoffHoldsRetryUntilElapsed is code review F-5 (mutant
// MA: "failed is always due"): with a fixed, controlled clock, a failed
// attempt is NOT redelivered before its backoff window elapses, and IS
// redelivered once the clock reaches it.
func TestDispatcher_BackoffHoldsRetryUntilElapsed(t *testing.T) {
	pool := scratchPool(t, "adispbackoff")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:backoff")
	seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	const backoff = time.Minute
	sink := &MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) { return OutcomeFailed, ErrorClassUnavailable }}
	disp := NewDispatcher(pool, DispatcherConfig{
		MaxAttempts: 10,
		Clock:       clock,
		Backoff:     func(int) time.Duration { return backoff },
	}, sink)

	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(sink.Attempts) != 1 {
		t.Fatalf("expected 1 attempt after pass 1, got %d", len(sink.Attempts))
	}

	// Not yet due: same clock, well before backoff elapses.
	clock.Advance(backoff / 2)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2 (not yet due): %v", err)
	}
	if len(sink.Attempts) != 1 {
		t.Fatalf("expected still exactly 1 attempt before backoff elapses, got %d", len(sink.Attempts))
	}

	// Now due: clock reaches next_attempt_at.
	clock.Advance(backoff/2 + time.Second)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 3 (due): %v", err)
	}
	if len(sink.Attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts once backoff elapsed, got %d", len(sink.Attempts))
	}
}

// TestDispatcher_EscalationOnlyAfterEscalateAfterAndNeverWhenAcked is code
// review F-3 (mutant MB: escalation predicate reduced to "next_escalation_at
// != nil") plus the ADR §7 requirement directly: a not-acked alert
// escalates only once escalate_after elapses; an acked alert never
// escalates.
func TestDispatcher_EscalationOnlyAfterEscalateAfterAndNeverWhenAcked(t *testing.T) {
	pool := scratchPool(t, "adispesc")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))

	// Two routes: step 0 (initial) and step 1 (escalation target), both
	// resolvable, so a successful escalation is observable as a second
	// 'sent' row at step 1.
	escalateAfter := 5 * time.Minute
	mustAddRouteWithEscalation(t, pool, admin, SeverityP1, 0, ChannelLog, "log:step0", &escalateAfter)
	mustAddRouteWithEscalation(t, pool, admin, SeverityP1, 1, ChannelLog, "log:step1", nil)

	alertID := seedOpenAlert(t, pool, tenantA, KindReconciliationRunFailed, "stream:"+uuid.NewString())
	disp := NewDispatcher(pool, DispatcherConfig{Clock: clock}, LogSink{})

	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1 (initial send): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 0)

	// Not yet due to escalate.
	clock.Advance(escalateAfter / 2)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2 (not yet due to escalate): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 0)

	// Ack it - escalation must never fire even once escalate_after would
	// otherwise be due.
	ackAlert(t, pool, admin, alertID)
	clock.Advance(escalateAfter)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 3 (acked, past escalate_after): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 0)
}

// TestDispatcher_EscalationFiresToRoutedStep is code review C-1 (mutant
// MB2: "disabling escalation entirely passes the suite" - there was no
// POSITIVE test that escalation actually happens): an un-acked alert,
// once escalate_after elapses, actually escalates to step 1 and is
// delivered there.
func TestDispatcher_EscalationFiresToRoutedStep(t *testing.T) {
	pool := scratchPool(t, "adispescpos")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))

	escalateAfter := 5 * time.Minute
	mustAddRouteWithEscalation(t, pool, admin, SeverityP1, 0, ChannelLog, "log:step0", &escalateAfter)
	mustAddRouteWithEscalation(t, pool, admin, SeverityP1, 1, ChannelLog, "log:step1", nil)

	alertID := seedOpenAlert(t, pool, tenantA, KindReconciliationRunFailed, "stream:"+uuid.NewString())
	disp := NewDispatcher(pool, DispatcherConfig{Clock: clock}, LogSink{})

	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1 (initial send): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 0)

	// Never acked, and escalate_after has now elapsed: the alert MUST
	// escalate to step 1 and be delivered there.
	clock.Advance(escalateAfter + time.Second)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2 (escalation due): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 1)
}

// TestDispatcher_EscalationFiresToUnroutedStep is the same positive
// escalation proof, for the case where step 1 has no route at all: the
// escalation must still be ATTEMPTED (never silently skipped) and land
// as 'unrouted' at step 1.
func TestDispatcher_EscalationFiresToUnroutedStep(t *testing.T) {
	pool := scratchPool(t, "adispescunr")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))

	escalateAfter := 5 * time.Minute
	// Only step 0 has a route - step 1 deliberately does not.
	mustAddRouteWithEscalation(t, pool, admin, SeverityP1, 0, ChannelLog, "log:step0", &escalateAfter)

	alertID := seedOpenAlert(t, pool, tenantA, KindReconciliationRunFailed, "stream:"+uuid.NewString())
	disp := NewDispatcher(pool, DispatcherConfig{Clock: clock}, LogSink{})

	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1 (initial send): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 0)

	clock.Advance(escalateAfter + time.Second)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2 (escalation due, no step-1 route): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "unrouted", 1)
}

// TestDispatcher_AckedUnroutedEscalationStepNeverEscalates is code review
// F-8: once an alert is acked, an escalation step beyond the first
// (step > 0) must never itself become due through the 'unrouted' branch
// either - only the very first attempt (step 0, no delivery row yet) is
// exempt from the ack check.
func TestDispatcher_AckedUnroutedEscalationStepNeverEscalates(t *testing.T) {
	pool := scratchPool(t, "adispf8")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))

	escalateAfter := 5 * time.Minute
	// Step 0 has a route (so the alert gets an initial 'sent'); step 1
	// deliberately has NONE, so escalating there would normally land in
	// the 'unrouted' branch.
	mustAddRouteWithEscalation(t, pool, admin, SeverityP1, 0, ChannelLog, "log:step0", &escalateAfter)

	alertID := seedOpenAlert(t, pool, tenantA, KindReconciliationRunFailed, "stream:"+uuid.NewString())
	disp := NewDispatcher(pool, DispatcherConfig{Clock: clock}, LogSink{})

	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1 (initial send): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 0)

	ackAlert(t, pool, admin, alertID)
	clock.Advance(escalateAfter + time.Second)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2 (acked, past escalate_after, step 1 unrouted): %v", err)
	}
	// Still 'sent' at step 0 - the acked alert never even attempted step
	// 1 (no 'unrouted' row was created there).
	assertLatestEventAndStep(t, pool, admin, alertID, "sent", 0)

	var step1UnroutedCount int
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_deliveries WHERE alert_id = $1 AND escalation_step = 1`, alertID).Scan(&step1UnroutedCount)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if step1UnroutedCount != 0 {
		t.Fatalf("expected no step-1 delivery row for an acked alert, got %d", step1UnroutedCount)
	}
}

// TestDispatcher_StaleClaimReclaimedThenDead is security IC-2 / code
// review F-1: a 'claimed' row with no outcome recorded (simulating a
// crash between claim and Deliver/record) is NOT permanently stranded -
// once its lease expires it becomes due again under a NEW attempt
// number, and it still reaches 'dead' plus alerting.delivery_dead once
// MaxAttempts is exhausted.
func TestDispatcher_StaleClaimReclaimedThenDead(t *testing.T) {
	pool := scratchPool(t, "adispstale")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:stale")
	alertID := seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	const lease = time.Minute
	sink := &MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) { return OutcomeFailed, ErrorClassUnavailable }}
	disp := NewDispatcher(pool, DispatcherConfig{
		MaxAttempts: 2,
		Clock:       clock,
		Backoff:     func(int) time.Duration { return 0 },
		ClaimLease:  lease,
	}, sink)

	// Simulate a crash: insert the 'claimed' row directly (attempt 0),
	// bypassing Deliver/record entirely - exactly what a process that
	// died between claim() and the outcome INSERT would leave behind.
	insertClaimedRow(t, pool, alertID, 0, 0)

	// Pass 1: lease not yet expired - must NOT reclaim.
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1 (lease not expired): %v", err)
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "claimed", 0)
	if len(sink.Attempts) != 0 {
		t.Fatalf("expected no delivery attempt before the lease expires, got %d", len(sink.Attempts))
	}

	// Pass 2: lease expired - reclaims as attempt 1 (MaxAttempts=2, so
	// this is the LAST allowed attempt) and, since the sink always
	// fails, records 'dead'.
	clock.Advance(lease + time.Second)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2 (lease expired): %v", err)
	}
	if len(sink.Attempts) != 1 {
		t.Fatalf("expected exactly 1 delivery attempt after reclaiming the stale claim, got %d", len(sink.Attempts))
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "dead", 0)

	var deadMetaCount int
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingDeliveryDead)).Scan(&deadMetaCount)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if deadMetaCount != 1 {
		t.Fatalf("expected exactly 1 alerting.delivery_dead alert after the reclaimed attempt was exhausted, got %d", deadMetaCount)
	}
}

// TestDispatcher_NoTransactionOpenDuringDeliver is AL-5 / code review
// F-4: the ctx Deliver receives must never be txscope-marked.
func TestDispatcher_NoTransactionOpenDuringDeliver(t *testing.T) {
	pool := scratchPool(t, "adispnotx")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:notx")
	seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	var sawHeld bool
	sink := &MockSink{DeliverFunc: func(ctx context.Context, _ Delivery) (Outcome, ErrorClass) {
		sawHeld = txscope.Held(ctx)
		return OutcomeSent, ErrorClassNone
	}}
	disp := NewDispatcher(pool, DispatcherConfig{Clock: newManualClock(time.Now().Add(3 * time.Second))}, sink)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(sink.Attempts) != 1 {
		t.Fatalf("expected exactly 1 delivery attempt, got %d", len(sink.Attempts))
	}
	if sawHeld {
		t.Fatal("expected Deliver's ctx to NOT be txscope-marked (AL-5)")
	}
}

// TestDispatcher_ClaimRaceIsDeterministic is code review F-6: two
// goroutines attempt to claim the EXACT SAME (alert, step, attempt_no)
// simultaneously (a start barrier removes scheduling slack), and exactly
// one must succeed - proven every run, not "usually". Run with
// -count=10 (and -race) per the verification instructions.
func TestDispatcher_ClaimRaceIsDeterministic(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	alertID := seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	disp := NewDispatcher(pool, DispatcherConfig{Clock: newManualClock(time.Now())}, LogSink{})

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = disp.claim(context.Background(), alertID, 0, 0)
		}(i)
	}
	close(start)
	wg.Wait()

	successCount := 0
	for _, ok := range results {
		if ok {
			successCount++
		}
	}
	if successCount != 1 {
		t.Fatalf("expected exactly 1 of 2 concurrent claims to succeed, got %d", successCount)
	}
}

func mustAddRouteWithEscalation(t *testing.T, pool *db.Pool, adminID uuid.UUID, severity Severity, step int, channel ChannelKind, recipientRef string, escalateAfter *time.Duration) {
	t.Helper()
	err := pool.WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, escalate_after, enabled, reason_code) VALUES ('platform', $1, $2, $3, $4, $5, true, 'initial_setup')`,
			string(severity), step, string(channel), recipientRef, escalateAfter)
		return err
	})
	if err != nil {
		t.Fatalf("add route with escalation: %v", err)
	}
}

func assertLatestEventAndStep(t *testing.T, pool *db.Pool, admin, alertID uuid.UUID, wantEvent string, wantStep int) {
	t.Helper()
	var gotEvent string
	var gotStep int
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT event, escalation_step FROM alert_deliveries WHERE alert_id = $1 ORDER BY recorded_at DESC, id DESC LIMIT 1`, alertID).Scan(&gotEvent, &gotStep)
	})
	if err != nil {
		t.Fatalf("read latest event/step: %v", err)
	}
	if gotEvent != wantEvent || gotStep != wantStep {
		t.Fatalf("expected latest delivery (event=%q, step=%d), got (event=%q, step=%d)", wantEvent, wantStep, gotEvent, gotStep)
	}
}

func ackAlert(t *testing.T, pool *db.Pool, adminID, alertID uuid.UUID) {
	t.Helper()
	err := pool.WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alerts SET state = 'acked' WHERE id = $1`, alertID)
		return err
	})
	if err != nil {
		t.Fatalf("ack alert: %v", err)
	}
}

// TestDispatcher_StaleClaimReclaimBeyondMaxAttemptsSkipsDelivery is code
// review C-4: when a reclaimed stale claim's new attempt number has
// ALREADY reached MaxAttempts (the entire retry budget was consumed by
// the one attempt that then got stranded), the dispatcher must record
// 'dead' directly, with NO further Sink.Deliver call - unlike
// TestDispatcher_StaleClaimReclaimedThenDead, where MaxAttempts=2 leaves
// the reclaimed attempt (1) still within budget and it must still be
// delivered once before becoming dead.
func TestDispatcher_StaleClaimReclaimBeyondMaxAttemptsSkipsDelivery(t *testing.T) {
	pool := scratchPool(t, "adispstalex")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	clock := newManualClock(time.Now().Add(3 * time.Second))
	addTestRoute(t, pool, admin, SeverityP2, 0, ChannelMock, "mock:stalex")
	alertID := seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	const lease = time.Minute
	sink := &MockSink{DeliverFunc: func(context.Context, Delivery) (Outcome, ErrorClass) { return OutcomeSent, ErrorClassNone }}
	disp := NewDispatcher(pool, DispatcherConfig{
		MaxAttempts: 1, // the entire budget is a single attempt
		Clock:       clock,
		Backoff:     func(int) time.Duration { return 0 },
		ClaimLease:  lease,
	}, sink)

	// Simulate a crash on the one and only allowed attempt (attempt 0).
	insertClaimedRow(t, pool, alertID, 0, 0)

	clock.Advance(lease + time.Second)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass (lease expired, budget already spent): %v", err)
	}

	if len(sink.Attempts) != 0 {
		t.Fatalf("expected NO delivery attempt for a reclaim that already exceeds MaxAttempts, got %d", len(sink.Attempts))
	}
	assertLatestEventAndStep(t, pool, admin, alertID, "dead", 0)

	var deadMetaCount int
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, string(KindAlertingDeliveryDead)).Scan(&deadMetaCount)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if deadMetaCount != 1 {
		t.Fatalf("expected exactly 1 alerting.delivery_dead alert, got %d", deadMetaCount)
	}
}

func insertClaimedRow(t *testing.T, pool *db.Pool, alertID uuid.UUID, step, attemptNo int) {
	t.Helper()
	err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event) VALUES ($1, $2, $3, 'claimed')`, alertID, step, attemptNo)
		return err
	})
	if err != nil {
		t.Fatalf("insert claimed row: %v", err)
	}
}
