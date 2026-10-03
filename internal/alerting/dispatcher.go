// This file, together with fallback.go, is the closed set of files
// permitted to reference db.ServiceAlertDispatcher (AL-10; enforced by
// static_dispatcher_identity_test.go). Business code never uses this
// identity - ScopedRunner has no constructor for it (scope.go).
package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// ChannelKind is the provider-neutral delivery channel a route selects
// (ADR §6.2). Only "log" and "mock" exist in PRH-2 - a real channel is
// PROVIDER DEPENDENT and gated by HD-PRH2-4-OPS (§6.2 preconditions).
type ChannelKind string

const (
	ChannelLog  ChannelKind = "log"
	ChannelMock ChannelKind = "mock"
)

// Outcome is what Sink.Deliver reports.
type Outcome string

const (
	OutcomeSent   Outcome = "sent"
	OutcomeFailed Outcome = "failed"
)

// ErrorClass mirrors migration 0110's alert_deliveries.last_error_class
// enum exactly.
type ErrorClass string

const (
	ErrorClassNone          ErrorClass = ""
	ErrorClassTimeout       ErrorClass = "timeout"
	ErrorClassUnavailable   ErrorClass = "unavailable"
	ErrorClassRejected      ErrorClass = "rejected"
	ErrorClassMisconfigured ErrorClass = "misconfigured"
	ErrorClassUnknown       ErrorClass = "unknown"
)

// Delivery is what a Sink actually delivers (ADR §6.2). It carries no
// provider credential and no route configuration - only what the sink
// needs to render/deliver the message.
type Delivery struct {
	IdempotencyKey string // "<alert_id>:<step>:<attempt_no>" - per-attempt key, for logs and metrics only
	// DedupKey is "<alert_id>:<step>", stable across retry attempts (ADR 0102
	// 16.3 item 3). A real channel adapter MUST dedupe on this key, never on
	// IdempotencyKey, or a retry after a transient failure becomes a second send.
	DedupKey string
	// Discriminator is the alert's server-side stable id string (for example
	// "attempt:<id>:reason:<reason>"). It is not PII; it tells the responder
	// WHICH condition within a Kind fired.
	Discriminator   string
	AlertID         uuid.UUID
	Kind            Kind
	Severity        Severity
	SubjectTenantID uuid.UUID
	Attributes      map[string]AttrValue
	RecipientRef    string
}

// Sink is the provider-neutral delivery interface (ADR §6.2). No
// transaction is ever open while Deliver runs (AL-5) - RunOnce enforces
// this structurally by calling Deliver strictly between two short,
// already-closed transactions.
type Sink interface {
	ChannelKind() ChannelKind
	Deliver(ctx context.Context, d Delivery) (Outcome, ErrorClass)
}

// LogSink is IMPLEMENTED on merge (ADR §6.2 table): it "delivers" by
// writing a structured log line at a severity-appropriate level. It never
// fails.
type LogSink struct {
	Logger *slog.Logger
}

func (LogSink) ChannelKind() ChannelKind { return ChannelLog }

func (s LogSink) Deliver(_ context.Context, d Delivery) (Outcome, ErrorClass) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	level := slog.LevelWarn
	if d.Severity == SeverityP1 {
		level = slog.LevelError
	}
	logger.Log(context.Background(), level, "alert_delivery",
		"alert_id", d.AlertID, "kind", d.Kind, "severity", d.Severity,
		"recipient_ref", d.RecipientRef, "discriminator", d.Discriminator,
		"idempotency_key", d.IdempotencyKey, "dedup_key", d.DedupKey)
	return OutcomeSent, ErrorClassNone
}

// MockSink is a MOCK sink (ADR §6.2 table) with configurable failure and
// timeout, for tests and for exercising the dispatcher's retry/dead/
// escalation logic without a real vendor. Deliver is called for every
// delivery attempt made through this sink; DeliverFunc, if set,
// overrides the default (always succeed) behaviour.
type MockSink struct {
	// DeliverFunc, if non-nil, is called for every Deliver, with the
	// exact ctx Deliver received - tests use this to assert AL-5/F-4 (no
	// transaction open during Deliver) via txscope.Held(ctx). Defaults to
	// always returning (OutcomeSent, "").
	DeliverFunc func(ctx context.Context, d Delivery) (Outcome, ErrorClass)

	// Attempts records every Delivery passed to this sink, in order -
	// tests assert on it directly rather than parsing log output.
	Attempts []Delivery
}

// SyntheticComponent marks MockSink as a synthetic test/dev double
// (internal/providerkind.Synthetic) - MOCK-ADAPTER-PROD-1's startup guard
// refuses any unmarked mock-like type from ever running with
// GuardEnvironment() == "production".
func (*MockSink) SyntheticComponent() {}

func (*MockSink) ChannelKind() ChannelKind { return ChannelMock }

func (s *MockSink) Deliver(ctx context.Context, d Delivery) (Outcome, ErrorClass) {
	s.Attempts = append(s.Attempts, d)
	if s.DeliverFunc != nil {
		return s.DeliverFunc(ctx, d)
	}
	return OutcomeSent, ErrorClassNone
}

// DispatcherConfig is the dispatcher's technical defaults (ADR §14 item
// 8: "Retry and backoff parameters: technical defaults, for devops to
// review" - these are exactly that, not policy).
type DispatcherConfig struct {
	MaxAttempts int // per escalation step, before "dead"
	Backoff     func(attempt int) time.Duration
	Clock       Clock

	// ClaimLease bounds how long a 'claimed' delivery row is treated as
	// in-flight before the dispatcher treats it as stale and reclaims it
	// under a NEW attempt number (security IC-2 / code review F-1): a
	// crash, deploy, OOM, or a failed record-outcome INSERT between
	// claim and Deliver/record must never strand an alert forever. A
	// reclaimed stale attempt counts toward MaxAttempts exactly like an
	// ordinary failed attempt, so a permanently wedged channel still
	// reaches "dead" and alerting.delivery_dead eventually.
	ClaimLease time.Duration
}

// DefaultDispatcherConfig is the technical default: 5 attempts per step,
// exponential backoff starting at 1s capped at 1 minute, and a 2-minute
// claim lease (technical default, devops-reviewable - ADR §14 item 8).
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		MaxAttempts: 5,
		Backoff:     defaultBackoff,
		Clock:       SystemClock{},
		ClaimLease:  2 * time.Minute,
	}
}

// defaultBackoffMaxShift caps the shift amount `attempt` is clamped to
// before `1 << attempt` is computed - code review F-10: an unclamped
// shift overflows (undefined/huge) once attempt reaches the width of an
// int (>=63 on a 64-bit platform, but exponential backoff already
// exceeds any sane bound long before that; 20 is generously conservative
// and still comfortably saturates the 1-minute cap below well before it
// is reached).
const defaultBackoffMaxShift = 20

func defaultBackoff(attempt int) time.Duration {
	shift := attempt
	if shift < 0 {
		shift = 0
	}
	if shift > defaultBackoffMaxShift {
		shift = defaultBackoffMaxShift
	}
	d := time.Second * time.Duration(int64(1)<<uint(shift))
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

// Dispatcher runs the ADR §6.1 delivery loop under the
// db.ServiceAlertDispatcher identity.
type Dispatcher struct {
	pool   *db.Pool
	sinks  map[ChannelKind]Sink
	config DispatcherConfig
}

// NewDispatcher builds a Dispatcher over pool, delivering through sinks
// (keyed by their own ChannelKind()). At least a LogSink should normally
// be registered since alert_routes ships empty in PRH-2 and MOCK-channel
// routes are test-only.
func NewDispatcher(pool *db.Pool, config DispatcherConfig, sinks ...Sink) *Dispatcher {
	m := make(map[ChannelKind]Sink, len(sinks))
	for _, s := range sinks {
		m[s.ChannelKind()] = s
	}
	if config.Clock == nil {
		config.Clock = SystemClock{}
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = DefaultDispatcherConfig().MaxAttempts
	}
	if config.Backoff == nil {
		config.Backoff = DefaultDispatcherConfig().Backoff
	}
	if config.ClaimLease <= 0 {
		config.ClaimLease = DefaultDispatcherConfig().ClaimLease
	}
	return &Dispatcher{pool: pool, sinks: m, config: config}
}

type dueAlert struct {
	id              uuid.UUID
	kind            Kind
	severity        Severity
	simulation      bool
	subjectTenantID uuid.UUID
	discriminator   string
	attributes      map[string]AttrValue
	acked           bool

	step      int
	attemptNo int
	due       bool

	// wasUnrouted is true when this alert's latest delivery row was
	// already 'unrouted' at this exact step - code review F-7: if a
	// route still does not exist this pass, processOne skips the
	// redundant markUnrouted DB round-trip entirely rather than
	// repeating an INSERT ... ON CONFLICT DO NOTHING that can never
	// insert a row.
	wasUnrouted bool

	// staleExhausted is set when this row is a reclaimed stale claim
	// (readDueWork's "claimed" case) whose new attempt number has ALREADY
	// reached d.config.MaxAttempts (code review C-4): processOne must
	// record 'dead' directly, without ever calling Sink.Deliver again -
	// a channel that is wedged badly enough to strand its lease should
	// not get one more live delivery attempt after every retry budget is
	// already spent; it is already dead, the reclaim just discovered it.
	staleExhausted bool
}

// RunOnce performs exactly one dispatcher pass: read due work (a short
// read-only transaction), deliver with no transaction open (AL-5), then
// record the outcome (a fresh short transaction) - ADR §6.1 steps 1-3.
func (d *Dispatcher) RunOnce(ctx context.Context) error {
	now := d.config.Clock.Now()

	var due []dueAlert
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		list, err := d.readDueWork(ctx, tx, now)
		due = list
		return err
	})
	if err != nil {
		return err
	}

	// Code review F-7: resolve each distinct (severity, step) route at
	// most once per pass, not once per alert - routes ship empty in
	// PRH-2, so every open alert is permanently unrouted until
	// HD-PRH2-4-OPS, and re-querying alert_routes per-alert, per-pass
	// forever is pure waste at any real alert volume.
	cache := make(map[routeCacheKey]*resolvedRoute)
	for _, w := range due {
		d.processOne(ctx, w, now, cache)
	}
	return nil
}

// routeCacheKey scopes resolveRoute's per-pass cache.
type routeCacheKey struct {
	severity Severity
	step     int
}

func (d *Dispatcher) cachedResolveRoute(ctx context.Context, cache map[routeCacheKey]*resolvedRoute, severity Severity, step int, now time.Time) (*resolvedRoute, error) {
	key := routeCacheKey{severity: severity, step: step}
	if r, ok := cache[key]; ok {
		return r, nil
	}
	r, err := d.resolveRoute(ctx, severity, step, now)
	if err != nil {
		return nil, err
	}
	cache[key] = r // nil is a valid, cacheable "no route this pass" result
	return r, nil
}

func (d *Dispatcher) readDueWork(ctx context.Context, tx pgx.Tx, now time.Time) ([]dueAlert, error) {
	// ORDER BY d.recorded_at DESC, d.id DESC (code review F-9): recorded_at
	// is DB time (now()), never the injected Clock, so two rows recorded
	// within the same clock tick need a deterministic tiebreaker or a
	// test/fake clock set in the past could pick the wrong "latest" row.
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.kind, a.severity, a.simulation, a.subject_tenant_id, a.discriminator, a.attributes, a.state,
		       ld.escalation_step, ld.attempt_no, ld.event, ld.next_attempt_at, ld.next_escalation_at, ld.recorded_at
		FROM alerts a
		LEFT JOIN LATERAL (
			SELECT d.escalation_step, d.attempt_no, d.event, d.next_attempt_at, d.next_escalation_at, d.recorded_at
			FROM alert_deliveries d
			WHERE d.alert_id = a.id
			ORDER BY d.recorded_at DESC, d.id DESC
			LIMIT 1
		) ld ON true
		WHERE a.state IN ('open', 'acked')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []dueAlert
	for rows.Next() {
		var (
			w                               dueAlert
			subjectTenantID                 *uuid.UUID
			attrsJSON                       []byte
			state                           string
			step, attemptNo                 *int
			event                           *string
			nextAttemptAt, nextEscalationAt *time.Time
			recordedAt                      *time.Time
		)
		if err := rows.Scan(&w.id, &w.kind, &w.severity, &w.simulation, &subjectTenantID, &w.discriminator, &attrsJSON, &state,
			&step, &attemptNo, &event, &nextAttemptAt, &nextEscalationAt, &recordedAt); err != nil {
			return nil, err
		}
		if subjectTenantID != nil {
			w.subjectTenantID = *subjectTenantID
		}
		w.attributes = decodeAttributes(attrsJSON)
		w.acked = state == "acked"

		switch {
		case event == nil:
			w.step, w.attemptNo, w.due = 0, 0, true
		case *event == "unrouted":
			// Re-evaluated every pass (ADR §6.1) - a route may have been
			// added since the last pass. Code review F-8: an already-
			// unrouted escalation step (step > 0) must not itself
			// escalate further while acked - only step 0's "no
			// delivery row yet" case is exempt from the ack check (it
			// is the FIRST attempt, not an escalation).
			w.step, w.attemptNo = *step, 0
			w.due = *step == 0 || !w.acked
			w.wasUnrouted = true
		case *event == "failed":
			w.step, w.attemptNo = *step, *attemptNo+1
			w.due = nextAttemptAt != nil && !now.Before(*nextAttemptAt)
		case *event == "sent":
			w.step, w.attemptNo = *step+1, 0
			w.due = !w.acked && nextEscalationAt != nil && !now.Before(*nextEscalationAt)
		case *event == "claimed":
			// Security IC-2 / code review F-1: a stale claim (no outcome
			// recorded within the lease) is due again, under a NEW
			// attempt number, so it counts toward MaxAttempts/dead
			// exactly like an ordinary failed attempt.
			w.step, w.attemptNo = *step, *attemptNo+1
			stale := recordedAt != nil && now.Sub(*recordedAt) >= d.config.ClaimLease
			w.due = stale
			if stale {
				recordStaleClaim(ctx)
				slog.Default().Warn("alert_dispatcher_stale_claim_reclaimed", "alert_id", w.id, "escalation_step", *step, "attempt_no", *attemptNo)
				// Code review C-4: the reclaimed attempt number
				// (w.attemptNo) is still a LEGITIMATE attempt as long as
				// it is within the MaxAttempts budget - exactly like an
				// ordinary failed delivery's last attempt (attemptNo ==
				// MaxAttempts-1) still gets to run and only becomes
				// 'dead' AFTER it also fails (recordFailedOrDead). Only a
				// reclaim landing at or past the budget itself
				// (w.attemptNo >= MaxAttempts, i.e. the whole budget was
				// already spent by earlier attempts before this stale one
				// was even reclaimed) skips delivery and records 'dead'
				// directly.
				w.staleExhausted = w.attemptNo >= d.config.MaxAttempts
			}
		default: // "dead", "suppressed_simulation": never due again from this loop
			w.due = false
		}
		if w.due {
			out = append(out, w)
		}
	}
	return out, rows.Err()
}

func decodeAttributes(raw []byte) map[string]AttrValue {
	if len(raw) == 0 {
		return map[string]AttrValue{}
	}
	var m map[string]AttrValue
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]AttrValue{}
	}
	return m
}

func (d *Dispatcher) processOne(ctx context.Context, w dueAlert, now time.Time, cache map[routeCacheKey]*resolvedRoute) {
	if w.simulation {
		d.suppressSimulation(ctx, w)
		return
	}

	route, err := d.cachedResolveRoute(ctx, cache, w.severity, w.step, now)
	if err != nil {
		slog.Default().Error("alert_dispatcher_route_lookup_failed", "alert_id", w.id, "error", err)
		return
	}
	if route == nil {
		if w.wasUnrouted {
			// Nothing changed since last pass at this step - skip the
			// redundant INSERT ... ON CONFLICT DO NOTHING round-trip
			// (F-7). The row already exists; there is nothing to mark.
			return
		}
		d.markUnrouted(ctx, w)
		return
	}

	sink, ok := d.sinks[route.channelKind]
	if !ok {
		// A route names a channel this dispatcher instance has no Sink
		// for - treat exactly like "no route" for this pass (misconfigured
		// deployment, not the alert's fault); logged loudly so it is
		// diagnosable.
		slog.Default().Error("alert_dispatcher_no_sink_for_channel", "channel_kind", route.channelKind, "alert_id", w.id)
		return
	}

	claimed := d.claim(ctx, w.id, w.step, w.attemptNo)
	if !claimed {
		return // another dispatcher instance already claimed this attempt
	}

	// Code review C-4: a reclaimed stale claim whose new attempt number
	// has already reached MaxAttempts is already dead - record it
	// directly, with no further Deliver call, rather than spending one
	// more live attempt against a channel that just proved it can strand
	// a delivery past its own claim lease.
	if w.staleExhausted {
		d.recordDeadDirect(ctx, w, route, ErrorClassUnknown, "stale claim reclaimed past MaxAttempts; delivery not re-attempted")
		return
	}

	// AL-5 / code review F-4: no transaction may ever be open while
	// Deliver runs. txscope.Held is the same defence-in-depth runtime
	// guard internal/db's other "must never run with a pooled connection
	// held" call sites use (ADR 0094 INV-POOL) - every db.WithPlatformService/
	// WithTenant/etc. call above this point closes its own transaction
	// before returning, so ctx here must never be marked; if it somehow
	// were (a future refactor bug), fail loudly rather than silently
	// deliver with a transaction open.
	if txscope.Held(ctx) {
		slog.Default().Error("alert_dispatcher_deliver_with_tx_held", "alert_id", w.id)
		return
	}

	// Code review C-4: bound every Deliver call so a hung sink cannot
	// hold a claimed row past its own ClaimLease indefinitely - the
	// timeout is derived from, and kept strictly BELOW, ClaimLease so a
	// slow-but-alive channel times out here first and gets recorded as a
	// normal failed attempt, rather than the row sitting 'claimed' until
	// a LATER pass's stale-claim reclaim has to notice it instead. Half
	// the lease leaves headroom for the record-outcome round-trip that
	// follows Deliver to still land safely inside the lease window.
	deliverCtx, cancel := context.WithTimeout(ctx, d.config.ClaimLease/2)
	defer cancel()

	outcome, errClass := sink.Deliver(deliverCtx, Delivery{
		IdempotencyKey:  fmt.Sprintf("%s:%d:%d", w.id, w.step, w.attemptNo),
		DedupKey:        fmt.Sprintf("%s:%d", w.id, w.step),
		Discriminator:   w.discriminator,
		AlertID:         w.id,
		Kind:            w.kind,
		Severity:        w.severity,
		SubjectTenantID: w.subjectTenantID,
		Attributes:      w.attributes,
		RecipientRef:    route.recipientRef,
	})

	if outcome == OutcomeSent {
		d.recordSent(ctx, w, route, now)
		return
	}
	d.recordFailedOrDead(ctx, w, route, errClass, now)
}
