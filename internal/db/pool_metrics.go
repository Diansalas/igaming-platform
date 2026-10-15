package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// poolStatter is the part of *pgxpool.Pool the metrics read.
type poolStatter interface{ Stat() *pgxpool.Stat }

// RegisterPoolMetrics exports the application pool's saturation signals (pending-signals.yml DbPoolSaturation)
// through the global OpenTelemetry meter provider:
//
//	db_pool_max_connections             the configured ceiling
//	db_pool_acquired_connections        connections checked out right now
//	db_pool_idle_connections            idle connections
//	db_pool_acquire_wait_seconds_total  cumulative time callers spent waiting for a connection
//	db_pool_empty_acquires_total        acquires that had to wait because the pool was empty
//
// There is one pool per process, so the instruments carry NO attributes (nothing to label, no tenant, no query
// text). It is called once at startup, after the pool is built and before traffic. The instrument names are
// written out literally at each call (not through a helper) because deploy/observability's parity test finds
// emitted metrics by scanning for them.
func RegisterPoolMetrics(p *Pool) error {
	if p == nil || p.pool == nil {
		return errors.New("db: RegisterPoolMetrics: nil pool")
	}
	return registerPoolMetrics(otel.Meter("github.com/Diansalas/igaming-platform/internal/db"), p.pool)
}

func registerPoolMetrics(meter metric.Meter, s poolStatter) error {
	if _, err := meter.Int64ObservableGauge("db_pool_max_connections",
		metric.WithDescription("Configured maximum size of the application database pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(s.Stat().MaxConns()))
			return nil
		})); err != nil {
		return fmt.Errorf("db: register pool metrics: %w", err)
	}
	if _, err := meter.Int64ObservableGauge("db_pool_acquired_connections",
		metric.WithDescription("Connections currently checked out of the application database pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(s.Stat().AcquiredConns()))
			return nil
		})); err != nil {
		return fmt.Errorf("db: register pool metrics: %w", err)
	}
	if _, err := meter.Int64ObservableGauge("db_pool_idle_connections",
		metric.WithDescription("Idle connections in the application database pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(s.Stat().IdleConns()))
			return nil
		})); err != nil {
		return fmt.Errorf("db: register pool metrics: %w", err)
	}
	if _, err := meter.Float64ObservableCounter("db_pool_acquire_wait_seconds_total",
		metric.WithDescription("Cumulative seconds callers spent waiting to acquire a database connection; its rate rising with acquired close to max is pool saturation."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(s.Stat().AcquireDuration().Seconds())
			return nil
		})); err != nil {
		return fmt.Errorf("db: register pool metrics: %w", err)
	}
	if _, err := meter.Int64ObservableCounter("db_pool_empty_acquires_total",
		metric.WithDescription("Acquires that found the pool empty and had to wait for a connection."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(s.Stat().EmptyAcquireCount())
			return nil
		})); err != nil {
		return fmt.Errorf("db: register pool metrics: %w", err)
	}
	return nil
}
