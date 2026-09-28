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
}

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
			SELECT id, channel_kind, recipient_ref, escalate_after
			FROM alert_routes
			WHERE scope = 'platform' AND severity = $1 AND escalation_step = $2
			  AND superseded_at IS NULL AND effective_from <= $3
			ORDER BY effective_from DESC
			LIMIT 1
		`, string(severity), step, now).Scan(&r.id, &r.channelKind, &r.recipientRef, &escalateAfter)
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
	}
}

func (d *Dispatcher) recordFailedOrDead(ctx context.Context, w dueAlert, route *resolvedRoute, errClass ErrorClass, now time.Time) {
	if w.attemptNo+1 >= d.config.MaxAttempts {
		err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, route_id, channel_kind, last_error_class)
				VALUES ($1, $2, $3, 'dead', $4, $5, $6)
			`, w.id, w.step, w.attemptNo, route.id, string(route.channelKind), string(errClass))
			return err
		})
		if err != nil {
			slog.Default().Error("alert_dispatcher_record_dead_failed", "alert_id", w.id, "error", err)
			return
		}
		recordDead(ctx, string(route.channelKind))
		d.raiseMeta(ctx, KindAlertingDeliveryDead, w.kind)
		return
	}

	next := now.Add(d.config.Backoff(w.attemptNo))
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, route_id, channel_kind, last_error_class, next_attempt_at)
			VALUES ($1, $2, $3, 'failed', $4, $5, $6, $7)
		`, w.id, w.step, w.attemptNo, route.id, string(route.channelKind), string(errClass), next)
		return err
	})
	if err != nil {
		slog.Default().Error("alert_dispatcher_record_failed_failed", "alert_id", w.id, "error", err)
	}
}

func (d *Dispatcher) markUnrouted(ctx context.Context, w dueAlert) {
	var id uuid.UUID
	err := d.pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event)
			VALUES ($1, $2, 0, 'unrouted')
			ON CONFLICT DO NOTHING
			RETURNING id
		`, w.id, w.step).Scan(&id)
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
	recordUnrouted(ctx, w.severity)
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
