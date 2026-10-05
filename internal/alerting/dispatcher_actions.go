package alerting

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type resolvedRoute struct {
	id            uuid.UUID
	channelKind   ChannelKind
	recipientRef  string
	escalateAfter *time.Duration
	enabled       bool
}

// UnroutedReason is the closed alert_deliveries.unrouted_reason vocabulary.
type UnroutedReason string

const (
	// UnroutedNoRoute: no current route exists for (severity, step).
	UnroutedNoRoute UnroutedReason = "no_route"
	// UnroutedChannelDisabled: a current route exists but is not enabled.
	UnroutedChannelDisabled UnroutedReason = "channel_disabled"
	// UnroutedNoSink: an enabled route names a channel this binary has no sink for.
	UnroutedNoSink UnroutedReason = "no_sink"
)

// resolveRoute looks up the effective (severity, step) route (ADR §6.1)
// as of now (T-1: the clock is a query parameter, never the database's
// own now()). alert_routes ships empty in PRH-2 (HD-PRH2-4), so this
// returns (nil, nil) until a platform admin configures one.
//
// Code review F-9: `effective_from` is written by migration 0110's
// `DEFAULT now()` - real, DB wall-clock time, NOT the injected Clock. A
// test that sets a fake Clock's Now() to a time BEFORE a route row's
// real insertion time will therefore never see that route (the
// `effective_from <= $3` predicate below compares the injected `now`
// against a DB-clock timestamp) - tests must set the fake clock to a
// time at or after the real wall clock when a route needs to resolve.
// This is a property of comparing two different clocks, not a bug; it is
// documented here because it is easy to trip over.
func (d *Dispatcher) resolveRoute(ctx context.Context, severity Severity, step int, now time.Time) (*resolvedRoute, error) {
	var r resolvedRoute
	var escalateAfter *time.Duration
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, channel_kind, COALESCE(recipient_ref, ''), escalate_after, enabled
			FROM alert_routes
			WHERE scope = 'platform' AND severity = $1 AND escalation_step = $2
			  AND superseded_at IS NULL AND effective_from <= $3
			ORDER BY effective_from DESC, id DESC
			LIMIT 1
		`, string(severity), step, now).Scan(&r.id, &r.channelKind, &r.recipientRef, &escalateAfter, &r.enabled)
	})
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.escalateAfter = escalateAfter
	return &r, nil
}

// claim performs the ADR §6.1 "the claimed row is the claim" mechanic:
// exactly one dispatcher instance's INSERT of (alert_id, step, attempt_no,
// 'claimed') wins, via alert_deliveries' own UNIQUE (alert_id,
// escalation_step, attempt_no, event) constraint. A second instance's
// identical attempt returns false and does nothing further this pass -
// this is the "two dispatchers, one claim" invariant.
func (d *Dispatcher) claim(ctx context.Context, alertID uuid.UUID, step, attemptNo int) bool {
	var id uuid.UUID
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event)
			VALUES ($1, $2, $3, 'claimed')
			ON CONFLICT DO NOTHING
			RETURNING id
		`, alertID, step, attemptNo).Scan(&id)
	})
	if err == pgx.ErrNoRows {
		return false
	}
	if err != nil {
		slog.Default().Error("alert_dispatcher_claim_failed", "alert_id", alertID, "error", err)
		return false
	}
	return true
}

func (d *Dispatcher) recordSent(ctx context.Context, w dueAlert, route *resolvedRoute, now time.Time) {
	var nextEscalationAt *time.Time
	if route.escalateAfter != nil {
		t := now.Add(*route.escalateAfter)
		nextEscalationAt = &t
	}
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, route_id, channel_kind, next_escalation_at)
			VALUES ($1, $2, $3, 'sent', $4, $5, $6)
		`, w.id, w.step, w.attemptNo, route.id, string(route.channelKind), nextEscalationAt)
		return err
	})
	if err != nil {
		slog.Default().Error("alert_dispatcher_record_sent_failed", "alert_id", w.id, "error", err)
		return
	}
	recordDeliveryAttempt(ctx, route.channelKind, "sent", ErrorClassNone, d.channelIsHuman(route.channelKind))
}

func (d *Dispatcher) recordFailedOrDead(ctx context.Context, w dueAlert, route *resolvedRoute, errClass ErrorClass, now time.Time) {
	errClass = errClass.normalized()
	// A permanent class (rejected, misconfigured) is terminal at once: retrying
	// cannot help and must not burn the budget. A retryable class is dead only
	// when the severity's attempt budget is spent.
	if !errClass.Retryable() || w.attemptNo+1 >= d.config.attemptBudget(w.severity) {
		d.recordDeadDirect(ctx, w, route, errClass, "")
		return
	}

	next := now.Add(d.config.backoffFor(w.severity, w.attemptNo))
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, route_id, channel_kind, last_error_class, next_attempt_at)
			VALUES ($1, $2, $3, 'failed', $4, $5, $6, $7)
		`, w.id, w.step, w.attemptNo, route.id, string(route.channelKind), string(errClass), next)
		return err
	})
	if err != nil {
		slog.Default().Error("alert_dispatcher_record_failed_failed", "alert_id", w.id, "error", err)
		return
	}
	recordDeliveryAttempt(ctx, route.channelKind, "failed", errClass, d.channelIsHuman(route.channelKind))
}

// channelIsHuman reports the wired sink's HumanNotification (false if unwired).
func (d *Dispatcher) channelIsHuman(k ChannelKind) bool {
	s, ok := d.sinks[k]
	return ok && s.HumanNotification()
}

// recordDeadDirect records a 'dead' delivery row for w at its CURRENT
// (w.step, w.attemptNo) - shared by recordFailedOrDead's ordinary
// "exhausted MaxAttempts after a failed Deliver" path and processOne's
// code review C-4 "reclaimed stale claim already past MaxAttempts, never
// even attempted" path. logDetail, if non-empty, is logged alongside the
// failure for diagnosability - it carries no meaning for the DB row
// itself (last_error_class is the only persisted classification).
func (d *Dispatcher) recordDeadDirect(ctx context.Context, w dueAlert, route *resolvedRoute, errClass ErrorClass, logDetail string) {
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, route_id, channel_kind, last_error_class)
			VALUES ($1, $2, $3, 'dead', $4, $5, $6)
		`, w.id, w.step, w.attemptNo, route.id, string(route.channelKind), string(errClass))
		return err
	})
	if err != nil {
		slog.Default().Error("alert_dispatcher_record_dead_failed", "alert_id", w.id, "error", err, "detail", logDetail)
		return
	}
	if logDetail != "" {
		slog.Default().Warn("alert_dispatcher_dead_without_delivery", "alert_id", w.id, "escalation_step", w.step, "attempt_no", w.attemptNo, "detail", logDetail)
	}
	recordDead(ctx, string(route.channelKind))
	recordDeliveryAttempt(ctx, route.channelKind, "dead", errClass, d.channelIsHuman(route.channelKind))
	d.raiseMeta(ctx, KindAlertingDeliveryDead, w.kind)
}

func (d *Dispatcher) markUnrouted(ctx context.Context, w dueAlert, reason UnroutedReason) {
	var id uuid.UUID
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, unrouted_reason)
			VALUES ($1, $2, 0, 'unrouted', $3)
			ON CONFLICT DO NOTHING
			RETURNING id
		`, w.id, w.step, string(reason)).Scan(&id)
	})
	if err == pgx.ErrNoRows {
		// Already marked unrouted for this (alert, step) - LF F9: no
		// incrementing attempt_no, no repeated meta occurrence.
		return
	}
	if err != nil {
		slog.Default().Error("alert_dispatcher_mark_unrouted_failed", "alert_id", w.id, "error", err)
		return
	}
	// RETURNING yielded a row: this is genuinely new. Count and raise the
	// meta occurrence exactly once (ADR §6.1 point 2).
	recordUnrouted(ctx, w.severity, reason)
	d.raiseMeta(ctx, KindAlertingUnrouted, w.kind)
}

func (d *Dispatcher) suppressSimulation(ctx context.Context, w dueAlert) {
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event)
			VALUES ($1, 0, 0, 'suppressed_simulation')
			ON CONFLICT DO NOTHING
		`, w.id)
		return err
	})
	if err != nil {
		slog.Default().Error("alert_dispatcher_suppress_simulation_failed", "alert_id", w.id, "error", err)
	}
}

// raiseMeta raises one occurrence of a meta-Kind (alerting.unrouted or
// alerting.delivery_dead), UNLESS originalKind is itself a meta-Kind
// (SR-6/AL-13: meta-Kinds never recurse - an alerting.* alert that is
// itself unrouted or that dies raises no further meta occurrence).
func (d *Dispatcher) raiseMeta(ctx context.Context, metaKind Kind, originalKind Kind) {
	if IsMetaKind(originalKind) {
		return
	}
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return Raise(ctx, tx, Alert{
			Kind:          metaKind,
			Discriminator: "severity:" + string(mustSeverityOf(originalKind)),
			Attributes:    map[string]AttrValue{},
		})
	})
	if err != nil {
		slog.Default().Error("alert_dispatcher_raise_meta_failed", "meta_kind", metaKind, "error", err)
	}
}

func mustSeverityOf(k Kind) Severity {
	if def, ok := Def(k); ok {
		return def.Severity
	}
	return SeverityP2
}

// loadRouteConfigs reads the configuration-only projection of every current
// platform route (readiness evaluation). It lives here because this is one of
// the three files allowed to use the dispatcher identity (AL-10).
func (d *Dispatcher) loadRouteConfigs(ctx context.Context, now time.Time) ([]RouteConfig, error) {
	var routes []RouteConfig
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, severity, escalation_step, channel_kind, enabled, recipient_ref IS NOT NULL,
			       alerting_channel_kind_is_human_notification(channel_kind), effective_from
			FROM alert_routes
			WHERE scope = 'platform' AND superseded_at IS NULL AND effective_from <= $1
		`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RouteConfig
			if err := rows.Scan(&r.ID, &r.Severity, &r.Step, &r.ChannelKind, &r.Enabled, &r.HasRecipient, &r.DBHuman, &r.EffectiveFrom); err != nil {
				return err
			}
			routes = append(routes, r)
		}
		return rows.Err()
	})
	return routes, err
}
