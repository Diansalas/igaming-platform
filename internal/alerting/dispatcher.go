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

// Retryable reports whether a failure of this class may succeed on a later
// attempt. rejected (the channel refused the message) and misconfigured (the
// channel or its target is wrongly set up) are PERMANENT: retrying cannot
// help, so they go straight to terminal failure without burning the retry
// budget. timeout, unavailable and unknown are retryable; an unrecognised or
// empty class is treated as unknown. Pinned by a table test.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ErrorClassRejected, ErrorClassMisconfigured:
		return false
	default:
		return true
	}
}

// normalized maps any value outside the five persisted classes (including
// the empty class a buggy sink might return with OutcomeFailed) to unknown,
// so the last_error_class CHECK can never refuse the outcome row.
func (c ErrorClass) normalized() ErrorClass {
	switch c {
	case ErrorClassTimeout, ErrorClassUnavailable, ErrorClassRejected, ErrorClassMisconfigured, ErrorClassUnknown:
		return c
	default:
		return ErrorClassUnknown
	}
}

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
	// HumanNotification reports whether a successful Deliver through this
	// channel reaches a PERSON. It is false for log and mock. A "sent" through
	// a non-human channel is NEVER presented as a human notification: metrics
	// and the status API label it non_human, and routing readiness counts only
	// routes whose channel answers true here (security M-3). No channel
	// answers true in this repository: a real channel is NOT IMPLEMENTED.
	HumanNotification() bool
	// Deliver must dedupe on d.DedupKey (never d.IdempotencyKey), honour ctx,
	// never log message bodies or credentials, and classify every failure.
	// recipient_ref (d.RecipientRef) is an opaque vendor target id: it is
	// never a credential and never a network address, and an adapter must
	// never use it as a host, URL, path, header or template (security M-7).
	Deliver(ctx context.Context, d Delivery) (Outcome, ErrorClass)
}

// LogSink is IMPLEMENTED on merge (ADR §6.2 table): it "delivers" by
// writing a structured log line at a severity-appropriate level. It never
// fails.
type LogSink struct {
	Logger *slog.Logger
}

func (LogSink) ChannelKind() ChannelKind { return ChannelLog }

// HumanNotification is false: a log line is not a notification to any person.
func (LogSink) HumanNotification() bool { return false }

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

// HumanNotification is false: a mock never reaches a person.
func (*MockSink) HumanNotification() bool { return false }

func (s *MockSink) Deliver(ctx context.Context, d Delivery) (Outcome, ErrorClass) {
	s.Attempts = append(s.Attempts, d)
	if s.DeliverFunc != nil {
		return s.DeliverFunc(ctx, d)
	}
	return OutcomeSent, ErrorClassNone
}

// SeverityBudget is one severity's retry budget: how many attempts a step gets
// before it is terminally dead, and the ceiling of the exponential backoff.
type SeverityBudget struct {
	MaxAttempts int
	BackoffCap  time.Duration
}

// Retry budgets per severity. These are technical defaults (ADR 0102 section
// 14 item 8 and section 18), reviewable by devops, NOT policy values. They are
// code constants deliberately: no env var exists for them (tuning is a
// deferred item, ADR 0102 section 18 "Deferred").
const (
	P1MaxAttempts = 8
	P1BackoffCap  = 5 * time.Minute
	P2MaxAttempts = 5
	P2BackoffCap  = time.Minute
	P3MaxAttempts = 3
	P3BackoffCap  = time.Minute
)

// RetryBudgets groups the per-severity budgets.
type RetryBudgets struct {
	P1, P2, P3 SeverityBudget
}

// DefaultRetryBudgets returns the constants above.
func DefaultRetryBudgets() RetryBudgets {
	return RetryBudgets{
		P1: SeverityBudget{MaxAttempts: P1MaxAttempts, BackoffCap: P1BackoffCap},
		P2: SeverityBudget{MaxAttempts: P2MaxAttempts, BackoffCap: P2BackoffCap},
		P3: SeverityBudget{MaxAttempts: P3MaxAttempts, BackoffCap: P3BackoffCap},
	}
}

// For returns the budget for a severity; an unrecognised severity gets the p3
// budget (the smallest).
func (b RetryBudgets) For(s Severity) SeverityBudget {
	switch s {
	case SeverityP1:
		return b.P1
	case SeverityP2:
		return b.P2
	default:
		return b.P3
	}
}

// DispatcherConfig is the dispatcher's technical defaults (ADR §14 item
// 8: "Retry and backoff parameters: technical defaults, for devops to
// review" - these are exactly that, not policy).
type DispatcherConfig struct {
	// Budgets are the per-severity retry budgets (zero value: the defaults).
	Budgets RetryBudgets
	// MaxAttempts, when > 0, overrides every severity's attempt budget (test
	// and diagnostic override; production leaves it 0 and uses Budgets).
	MaxAttempts int
	// Backoff, when non-nil, overrides the capped exponential backoff for
	// every severity (tests).
	Backoff func(attempt int) time.Duration
	Clock   Clock

	// ClaimLease bounds how long a 'claimed' delivery row is treated as
	// in-flight before the dispatcher treats it as stale and reclaims it
	// under a NEW attempt number (security IC-2 / code review F-1): a
	// crash, deploy, OOM, or a failed record-outcome INSERT between
	// claim and Deliver/record must never strand an alert forever. A
	// reclaimed stale attempt counts toward the attempt budget exactly like an
	// ordinary failed attempt, so a permanently wedged channel still
	// reaches "dead" and alerting.delivery_dead eventually.
	ClaimLease time.Duration

	// Logger receives the dispatcher's own log lines that this round added
	// (panic recovery, readiness transitions). nil means slog.Default().
	Logger *slog.Logger

	// ReadinessStaleAfter is how old the cached readiness evaluation may be
	// before it counts as NOT READY (stale = not ready). Default: three loop
	// intervals.
	ReadinessStaleAfter time.Duration
}

// DefaultDispatcherConfig is the technical default: per-severity budgets (p1
// 8 attempts / 5 min cap, p2 5 / 1 min, p3 3 / 1 min), exponential backoff
// starting at 1s, and a 2-minute claim lease (technical default,
// devops-reviewable - ADR §14 item 8).
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		Budgets:             DefaultRetryBudgets(),
		Clock:               SystemClock{},
		ClaimLease:          2 * time.Minute,
		ReadinessStaleAfter: 3 * DefaultLoopInterval,
	}
}

// defaultBackoffMaxShift caps the shift amount `attempt` is clamped to
// before `1 << attempt` is computed - code review F-10: an unclamped
// shift overflows (undefined/huge) once attempt reaches the width of an
// int (>=63 on a 64-bit platform, but exponential backoff already
// exceeds any sane bound long before that; 20 is generously conservative
// and still comfortably saturates every cap below well before it is
// reached).
const defaultBackoffMaxShift = 20

func cappedBackoff(attempt int, limit time.Duration) time.Duration {
	shift := attempt
	if shift < 0 {
		shift = 0
	}
	if shift > defaultBackoffMaxShift {
		shift = defaultBackoffMaxShift
	}
	d := time.Second * time.Duration(int64(1)<<uint(shift))
	if d > limit {
		d = limit
	}
	return d
}

// attemptBudget returns the attempt budget for a severity.
func (c DispatcherConfig) attemptBudget(s Severity) int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return c.Budgets.For(s).MaxAttempts
}

// backoffFor returns the delay before retrying after the given failed attempt.
func (c DispatcherConfig) backoffFor(s Severity, attempt int) time.Duration {
	if c.Backoff != nil {
		return c.Backoff(attempt)
	}
	return cappedBackoff(attempt, c.Budgets.For(s).BackoffCap)
}

// Dispatcher runs the ADR §6.1 delivery loop under the
// db.ServiceAlertDispatcher identity.
type Dispatcher struct {
	pool   *db.Pool
	sinks  map[ChannelKind]Sink
	config DispatcherConfig

	readiness readinessTracker
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
	def := DefaultDispatcherConfig()
	if config.Clock == nil {
		config.Clock = SystemClock{}
	}
	if config.Budgets == (RetryBudgets{}) {
		config.Budgets = def.Budgets
	}
	if config.ClaimLease <= 0 {
		config.ClaimLease = def.ClaimLease
	}
	if config.ReadinessStaleAfter <= 0 {
		config.ReadinessStaleAfter = def.ReadinessStaleAfter
	}
	d := &Dispatcher{pool: pool, sinks: m, config: config}
	d.readiness.clock = config.Clock
	d.readiness.staleAfter = config.ReadinessStaleAfter
	registerReadinessSource(&d.readiness)
	return d
}

func (d *Dispatcher) logger() *slog.Logger {
	if d.config.Logger != nil {
		return d.config.Logger
	}
	return slog.Default()
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

	// escalateOnDead is set for a p1 alert whose step n delivery is terminally
	// dead and unacknowledged: step n+1 is due immediately IF an enabled route
	// exists for it. With no such route the alert stays terminal (no extra
	// unrouted row, to avoid noise).
	escalateOnDead bool
}

// RunOnce performs exactly one dispatcher pass: read due work (a short
// read-only transaction), deliver with no transaction open (AL-5), then
// record the outcome (a fresh short transaction) - ADR §6.1 steps 1-3.
func (d *Dispatcher) RunOnce(ctx context.Context) error {
	now := d.config.Clock.Now()

	var due []dueAlert
	// Routing readiness is configuration-only and cheap; a failure to evaluate
	// is logged and leaves the cache to go stale (stale = not ready).
	if err := d.EvaluateReadiness(ctx); err != nil {
		d.logger().Error("alert_routing_readiness_evaluation_failed", "error_type", fmt.Sprintf("%T", err))
	}

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
		d.processOneRecovered(ctx, w, now, cache)
	}
	return nil
}

// processOneRecovered isolates one alert: a panic while processing it is
// recovered and does not end the pass, so every later alert is still handled.
// Only the panic's Go TYPE is logged, never its value (security M-5): a value
// can wrap a request, URL, header or body carrying a credential.
func (d *Dispatcher) processOneRecovered(ctx context.Context, w dueAlert, now time.Time, cache map[routeCacheKey]*resolvedRoute) {
	defer func() {
		if r := recover(); r != nil {
			d.logger().Error("alert_dispatcher_alert_panic", "alert_id", w.id,
				"escalation_step", w.step, "attempt_no", w.attemptNo, "panic_type", fmt.Sprintf("%T", r))
		}
	}()
	d.processOne(ctx, w, now, cache)
}

// deliverRecovered calls the sink and converts a panic into a failed/unknown
// outcome, so the claimed row is recorded as a normal failed attempt instead
// of being stranded until its lease expires. Type-only logging (security M-5).
func (d *Dispatcher) deliverRecovered(ctx context.Context, sink Sink, w dueAlert, msg Delivery) (outcome Outcome, class ErrorClass) {
	defer func() {
		if r := recover(); r != nil {
			d.logger().Error("alert_dispatcher_deliver_panic", "alert_id", w.id, "escalation_step", w.step,
				"attempt_no", w.attemptNo, "channel_kind", string(sink.ChannelKind()), "panic_type", fmt.Sprintf("%T", r))
			outcome, class = OutcomeFailed, ErrorClassUnknown
		}
	}()
	return sink.Deliver(ctx, msg)
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
				w.staleExhausted = w.attemptNo >= d.config.attemptBudget(w.severity)
			}
		case *event == "dead" && w.severity == SeverityP1 && !w.acked:
			// p1 escalate-on-dead: a terminally failed step n is followed by
			// step n+1 when an enabled route for it exists (processOne
			// decides; none means it stays terminal).
			w.step, w.attemptNo = *step+1, 0
			w.due, w.escalateOnDead = true, true
		default: // other "dead", "suppressed_simulation": never due again from this loop
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
	if route == nil || !route.enabled {
		if w.escalateOnDead {
			return // no enabled step n+1 route: terminal, no extra row
		}
		if w.wasUnrouted {
			// Nothing changed since last pass at this step - skip the
			// redundant INSERT ... ON CONFLICT DO NOTHING round-trip
			// (F-7). The row already exists; there is nothing to mark.
			return
		}
		reason := UnroutedNoRoute
		if route != nil {
			reason = UnroutedChannelDisabled
		}
		d.markUnrouted(ctx, w, reason)
		return
	}

	sink, ok := d.sinks[route.channelKind]
	if !ok {
		// N-4: an enabled route names a channel this binary has no Sink for.
		// This is a visible, counted unrouted row (reason no_sink), not a log
		// line: a black-holed route must never look like a delivered alert.
		if !w.wasUnrouted {
			d.markUnrouted(ctx, w, UnroutedNoSink)
		}
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

	outcome, errClass := d.deliverRecovered(deliverCtx, sink, w, Delivery{
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
