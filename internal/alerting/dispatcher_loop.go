package alerting

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Ticker is the injectable tick source for the dispatcher loop (plan T-1:
// never time.Sleep and hope). Production uses a time.Ticker; tests drive
// ticks by hand.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type timeTicker struct{ t *time.Ticker }

func (k timeTicker) C() <-chan time.Time { return k.t.C }
func (k timeTicker) Stop()               { k.t.Stop() }

// LoopConfig configures RunDispatcherLoopWithConfig.
type LoopConfig struct {
	// Interval between passes (default 15s, a technical default for devops
	// review - not a policy value).
	Interval time.Duration
	// NewTicker overrides the tick source (tests). Defaults to a time.Ticker.
	NewTicker func(d time.Duration) Ticker
	// DrainTimeout bounds how long an in-flight pass may keep running after
	// the parent context is cancelled (shutdown drain). Default 10s.
	DrainTimeout time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// ReadinessInterval is how often routing readiness is re-evaluated by its
	// OWN ticker, independent of how long a delivery pass takes (default: Interval).
	ReadinessInterval time.Duration
	// OnPass, if set, is called after every pass with its outcome
	// ("ok", "error" or "panic"). Test and metrics hook.
	OnPass func(result string)
}

// DefaultLoopInterval is the technical default pass interval (devops-reviewable,
// not a policy value). cmd/platform-api passes it to RunDispatcherLoop.
const DefaultLoopInterval = 15 * time.Second

// DefaultLoopDrainTimeout is how long RunDispatcherLoop waits for the pass in
// flight on shutdown; cmd/platform-api's bounded wait must exceed it.
const DefaultLoopDrainTimeout = 10 * time.Second

const defaultLoopDrainTimeout = DefaultLoopDrainTimeout

// PassRunner is what the loop drives. *Dispatcher satisfies it; tests
// substitute a fake to prove panic recovery and drain without a database.
type PassRunner interface {
	RunOnce(ctx context.Context) error
}

// RunDispatcherLoop runs the dispatcher with the default LoopConfig and
// the given interval. Blocks until ctx is done and the in-flight pass has
// drained. ADR 0102 6.1: wired in cmd/platform-api/main.go only after H
// merges (plan Rule 5).
func RunDispatcherLoop(ctx context.Context, d *Dispatcher, interval time.Duration) {
	RunDispatcherLoopWithConfig(ctx, d, LoopConfig{Interval: interval})
}

// RunDispatcherLoopWithConfig mirrors reconciliation.RunSchedulerLoop:
//   - one pass immediately on start (a restart must not wait a full
//     interval; a pass is always safe because claims are DB-enforced);
//   - then one pass per tick;
//   - every pass has its own panic recovery, so a panic in one pass never
//     kills the process or the loop (the platform must not die because
//     alerting did);
//   - shutdown drain: once ctx is cancelled no new pass starts, but a pass
//     already running is allowed to finish (bounded by DrainTimeout) so a
//     delivery is not abandoned between its claim and its recorded outcome.
//
// A pass failure is logged and counted; the loop continues. This function
// never returns an error and never touches a business transaction.
func RunDispatcherLoopWithConfig(ctx context.Context, d PassRunner, cfg LoopConfig) {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultLoopInterval
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = defaultLoopDrainTimeout
	}
	if cfg.NewTicker == nil {
		cfg.NewTicker = func(dur time.Duration) Ticker { return timeTicker{t: time.NewTicker(dur)} }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Routing readiness is configuration-only and must not depend on how long a
	// delivery pass takes: a slow or down vendor (each Deliver may run up to
	// ClaimLease/2 = 60s) would otherwise delay the evaluation beyond the
	// staleness window and flip alert_routing_ready (code review C1). It runs
	// on its own ticker whenever the runner can evaluate readiness. The default
	// staleness window is three intervals, so two consecutive missed or failed
	// evaluations are tolerated before the cache reads "stale = not ready".
	var readyWG sync.WaitGroup
	defer readyWG.Wait()
	if ev, ok := d.(readinessEvaluator); ok {
		ri := cfg.ReadinessInterval
		if ri <= 0 {
			ri = cfg.Interval
		}
		readyWG.Add(1)
		go func() {
			defer readyWG.Done()
			runReadinessLoop(ctx, ev, ri, logger)
		}()
	}

	pass := func() {
		if ctx.Err() != nil {
			return // shutting down: no new pass
		}
		result := runPassRecovered(ctx, d, cfg.DrainTimeout, logger)
		recordDispatcherPass(context.WithoutCancel(ctx), result)
		if cfg.OnPass != nil {
			cfg.OnPass(result)
		}
	}

	pass()

	ticker := cfg.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			pass()
		}
	}
}

// readinessEvaluator is implemented by *Dispatcher.
type readinessEvaluator interface {
	EvaluateReadiness(ctx context.Context) error
}

// readinessEvalTimeout bounds one readiness evaluation (a single short read).
const readinessEvalTimeout = 10 * time.Second

func runReadinessLoop(ctx context.Context, ev readinessEvaluator, interval time.Duration, logger *slog.Logger) {
	eval := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("alert_routing_readiness_panic", "panic_type", fmt.Sprintf("%T", r))
			}
		}()
		ectx, cancel := context.WithTimeout(ctx, readinessEvalTimeout)
		defer cancel()
		if err := ev.EvaluateReadiness(ectx); err != nil && ctx.Err() == nil {
			logger.Error("alert_routing_readiness_evaluation_failed", "error_type", fmt.Sprintf("%T", err))
		}
	}
	eval()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			eval()
		}
	}
}

// runPassRecovered runs exactly one d.RunOnce with panic recovery and the
// shutdown-drain context. It returns "ok", "error" or "panic".
func runPassRecovered(parent context.Context, d PassRunner, drain time.Duration, logger *slog.Logger) (result string) {
	// The pass context is detached from the parent's cancellation so an
	// in-flight pass can drain; it is cancelled DrainTimeout after the
	// parent is, or when the pass returns.
	passCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	stop := context.AfterFunc(parent, func() {
		time.AfterFunc(drain, cancel)
	})
	defer stop()

	defer func() {
		if r := recover(); r != nil {
			// Type only, never the value (security M-5).
			logger.Error("alert_dispatcher_pass_panic", "panic_type", fmt.Sprintf("%T", r))
			result = "panic"
		}
	}()
	if err := d.RunOnce(passCtx); err != nil {
		logger.Error("alert_dispatcher_pass_failed", "error", err)
		return "error"
	}
	return "ok"
}
