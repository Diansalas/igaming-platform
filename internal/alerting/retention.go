package alerting

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Alert history retention (ALERT-RETENTION-1, migration 0157). The deletion itself is the database function
// alerting_purge_resolved, called by (*Dispatcher).PurgeResolved under the alert_dispatcher identity
// (dispatcher.go); this file is the bounded loop that drives it and its metrics. Only RESOLVED alerts older than
// the configured window are removed; open and acknowledged alerts never are. The window is configuration
// (ALERT_RETENTION_DAYS, floor 7 days, checked again by the database); with it unset the loop is not started.

// PurgeResult is one alerting_purge_resolved call.
type PurgeResult struct {
	Alerts, Occurrences, Deliveries int64
	// Capped is true when the batch limit was reached, i.e. more eligible alerts may remain.
	Capped bool
}

// Purger is what the retention loop drives (*Dispatcher satisfies it; tests substitute a fake).
type Purger interface {
	PurgeResolved(ctx context.Context, olderThan time.Duration, batch int) (PurgeResult, error)
}

// RetentionConfig configures RunRetentionLoop.
type RetentionConfig struct {
	// OlderThan is the retention window (required, >= 7 days).
	OlderThan time.Duration
	// Interval between purge ticks (default 24h).
	Interval time.Duration
	// Batch is the per-call alert limit (default 5000, max 50000).
	Batch int
	// MaxBatchesPerTick bounds how many capped batches one tick may chain (default 20), so a large backlog is
	// drained over several ticks instead of holding the database for one long run.
	MaxBatchesPerTick int
	// CallTimeout bounds one purge call (default 2 minutes).
	CallTimeout time.Duration
	// NewTicker overrides the tick source (tests).
	NewTicker func(d time.Duration) Ticker
	Logger    *slog.Logger
	// OnTick is called after every tick with its result ("ok", "error", "panic"); test hook.
	OnTick func(result string)
}

const (
	defaultRetentionInterval = 24 * time.Hour
	defaultRetentionBatch    = 5000
	defaultRetentionBatches  = 20
	defaultRetentionTimeout  = 2 * time.Minute
	// MinRetention mirrors the database floor in alerting_purge_resolved.
	MinRetention = 7 * 24 * time.Hour
)

var (
	retentionLastRunUnix atomic.Int64
	retentionMeter       = meter
)

var retentionPurgedTotal, _ = retentionMeter.Int64Counter("alert_retention_purged_total",
	metric.WithDescription("Alert history rows removed by the retention purge, by table (alerts, occurrences, deliveries)."))

var retentionRunsTotal, _ = retentionMeter.Int64Counter("alert_retention_runs_total",
	metric.WithDescription("Retention purge ticks by result: ok, error, panic."))

var _, _ = retentionMeter.Int64ObservableGauge("alert_retention_last_run_unix_seconds",
	metric.WithDescription("Unix time of the last SUCCESSFUL retention purge tick; absent until one has run."),
	metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
		if v := retentionLastRunUnix.Load(); v != 0 {
			o.Observe(v)
		}
		return nil
	}))

func recordPurged(ctx context.Context, r PurgeResult) {
	if retentionPurgedTotal == nil {
		return
	}
	for table, n := range map[string]int64{"alerts": r.Alerts, "occurrences": r.Occurrences, "deliveries": r.Deliveries} {
		if n > 0 {
			retentionPurgedTotal.Add(ctx, n, metric.WithAttributes(attribute.String("table", table)))
		}
	}
}

func recordRetentionRun(ctx context.Context, result string) {
	if retentionRunsTotal != nil {
		retentionRunsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	}
}

// ValidateRetentionConfig reports an unusable configuration (refused before the loop starts).
func ValidateRetentionConfig(cfg RetentionConfig) error {
	if cfg.OlderThan < MinRetention {
		return fmt.Errorf("alerting: retention window %v is below the %v floor", cfg.OlderThan, MinRetention)
	}
	if cfg.Batch < 0 || cfg.Batch > 50000 {
		return fmt.Errorf("alerting: retention batch %d is out of range", cfg.Batch)
	}
	return nil
}

// RunRetentionLoop purges immediately on start and then once per interval until ctx is done. A tick that
// fails or panics is logged and counted and the loop continues; it never returns an error and never touches a
// business transaction. Only counts are logged.
func RunRetentionLoop(ctx context.Context, p Purger, cfg RetentionConfig) {
	if ValidateRetentionConfig(cfg) != nil {
		// A caller that skipped validation must not purge with a bad window: refuse to run at all.
		slog.Default().Error("alert_retention_loop_refused", "reason", "invalid_config")
		return
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultRetentionInterval
	}
	if cfg.Batch == 0 {
		cfg.Batch = defaultRetentionBatch
	}
	if cfg.MaxBatchesPerTick <= 0 {
		cfg.MaxBatchesPerTick = defaultRetentionBatches
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultRetentionTimeout
	}
	if cfg.NewTicker == nil {
		cfg.NewTicker = func(d time.Duration) Ticker { return timeTicker{t: time.NewTicker(d)} }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tick := func() {
		if ctx.Err() != nil {
			return
		}
		result := retentionTick(ctx, p, cfg, logger)
		recordRetentionRun(context.WithoutCancel(ctx), result)
		if cfg.OnTick != nil {
			cfg.OnTick(result)
		}
	}
	tick()
	ticker := cfg.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			tick()
		}
	}
}

func retentionTick(ctx context.Context, p Purger, cfg RetentionConfig, logger *slog.Logger) (result string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("alert_retention_panic", "panic_type", fmt.Sprintf("%T", r))
			result = "panic"
		}
	}()
	var total PurgeResult
	for i := 0; i < cfg.MaxBatchesPerTick; i++ {
		callCtx, cancel := context.WithTimeout(ctx, cfg.CallTimeout)
		r, err := p.PurgeResolved(callCtx, cfg.OlderThan, cfg.Batch)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("alert_retention_failed", "error_type", fmt.Sprintf("%T", err))
			}
			return "error"
		}
		recordPurged(context.WithoutCancel(ctx), r)
		total.Alerts += r.Alerts
		total.Occurrences += r.Occurrences
		total.Deliveries += r.Deliveries
		if !r.Capped {
			break
		}
	}
	retentionLastRunUnix.Store(time.Now().Unix())
	logger.Info("alert_retention_purged", "alerts", total.Alerts, "occurrences", total.Occurrences, "deliveries", total.Deliveries)
	return "ok"
}
