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

// ErrorClass mirrors migration 0108's alert_deliveries.last_error_class
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
	IdempotencyKey  string // "<alert_id>:<step>:<attempt_no>" - at-least-once dedup key for a real channel
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
		"recipient_ref", d.RecipientRef, "idempotency_key", d.IdempotencyKey)
	return OutcomeSent, ErrorClassNone
}

// MockSink is a MOCK sink (ADR §6.2 table) with configurable failure and
// timeout, for tests and for exercising the dispatcher's retry/dead/
// escalation logic without a real vendor. Deliver is called for every
// delivery attempt made through this sink; DeliverFunc, if set,
// overrides the default (always succeed) behaviour.
type MockSink struct {
	// DeliverFunc, if non-nil, is called for every Deliver. Defaults to
	// always returning (OutcomeSent, "").
	DeliverFunc func(Delivery) (Outcome, ErrorClass)

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

func (s *MockSink) Deliver(_ context.Context, d Delivery) (Outcome, ErrorClass) {
	s.Attempts = append(s.Attempts, d)
	if s.DeliverFunc != nil {
		return s.DeliverFunc(d)
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
}

// DefaultDispatcherConfig is the technical default: 5 attempts per step,
// exponential backoff starting at 1s, capped at 1 minute.
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		MaxAttempts: 5,
		Backoff: func(attempt int) time.Duration {
			d := time.Second * time.Duration(1<<uint(attempt))
			if d > time.Minute {
				d = time.Minute
			}
			return d
		},
		Clock: SystemClock{},
	}
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
	return &Dispatcher{pool: pool, sinks: m, config: config}
}

// RunDispatcherLoop runs d.RunOnce every interval until ctx is done. It is
// exported, UNWIRED (ADR §6.1: "I-core ships the dispatcher as an unwired
// function. I-wire adds one line to main.go after H merges") - I-wire or
// H is responsible for calling this from cmd/platform-api/main.go; this
// package deliberately never imports cmd/platform-api.
func RunDispatcherLoop(ctx context.Context, d *Dispatcher, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.RunOnce(ctx); err != nil {
				slog.Default().Error("alert_dispatcher_pass_failed", "error", err)
			}
		}
	}
}

type dueAlert struct {
	id              uuid.UUID
	kind            Kind
	severity        Severity
	simulation      bool
	subjectTenantID uuid.UUID
	attributes      map[string]AttrValue
	acked           bool

	step      int
	attemptNo int
	due       bool
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

	for _, w := range due {
		d.processOne(ctx, w, now)
	}
	return nil
}

func (d *Dispatcher) readDueWork(ctx context.Context, tx pgx.Tx, now time.Time) ([]dueAlert, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.kind, a.severity, a.simulation, a.subject_tenant_id, a.attributes, a.state,
		       ld.escalation_step, ld.attempt_no, ld.event, ld.next_attempt_at, ld.next_escalation_at
		FROM alerts a
		LEFT JOIN LATERAL (
			SELECT d.escalation_step, d.attempt_no, d.event, d.next_attempt_at, d.next_escalation_at
			FROM alert_deliveries d
			WHERE d.alert_id = a.id
			ORDER BY d.recorded_at DESC
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
		)
		if err := rows.Scan(&w.id, &w.kind, &w.severity, &w.simulation, &subjectTenantID, &attrsJSON, &state,
			&step, &attemptNo, &event, &nextAttemptAt, &nextEscalationAt); err != nil {
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
			// added since the last pass.
			w.step, w.attemptNo, w.due = *step, 0, true
		case *event == "failed":
			w.step, w.attemptNo = *step, *attemptNo+1
			w.due = nextAttemptAt != nil && !now.Before(*nextAttemptAt)
		case *event == "sent":
			w.step, w.attemptNo = *step+1, 0
			w.due = !w.acked && nextEscalationAt != nil && !now.Before(*nextEscalationAt)
		default: // "dead", "suppressed_simulation", "claimed": never due again from this loop
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

func (d *Dispatcher) processOne(ctx context.Context, w dueAlert, now time.Time) {
	if w.simulation {
		d.suppressSimulation(ctx, w)
		return
	}

	route, err := d.resolveRoute(ctx, w.severity, w.step, now)
	if err != nil {
		slog.Default().Error("alert_dispatcher_route_lookup_failed", "alert_id", w.id, "error", err)
		return
	}
	if route == nil {
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

	outcome, errClass := sink.Deliver(ctx, Delivery{
		IdempotencyKey:  fmt.Sprintf("%s:%d:%d", w.id, w.step, w.attemptNo),
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
