package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// ErrListActiveTenants and ErrListNonActiveTenants are wrapped by the errors RunSweep and RunSweepTenants
// return when a tenant LISTING (not a per-tenant run) fails, so the scheduler can name the phase of the
// durable alert without parsing text.
var (
	ErrListActiveTenants    = errors.New("reconciliation: list tenants for sweep")
	ErrListNonActiveTenants = errors.New("reconciliation: list non-active tenants for observation")
)

// sourceKey distinguishes two payment statement sources of one tenant in the run tracker.
func sourceKey(src statement.PaymentStatementSource) string {
	if src == nil {
		return ""
	}
	return src.ProviderID()
}

// raiseSweepFailed is a test seam (production value only: alerting.RaiseSweepFailed).
var raiseSweepFailed = alerting.RaiseSweepFailed

// handleSweepError makes every sweep failure that is not attributable to one tenant a durable alert
// (R3-LISTING-FAILURE-LOG-ONLY-1) instead of only a log line: a tenant listing failure (active or non-active
// tenants) means nothing, or only part of the platform, was reconciled in this sweep. Per-tenant run failures
// keep their own reconciliation.run_failed alerts (raised in the stream functions). A failure caused by the
// loop's own context being cancelled (graceful shutdown) is not a failure and raises nothing. Writing the
// alert is best effort and never stops the loop: a failed write is logged and counted
// (alert_raise_failures_total, inside the alerting package).
func handleSweepError(ctx context.Context, pool *db.Pool, logger *slog.Logger, err error) {
	if err == nil {
		return
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return // graceful shutdown mid-sweep
	}
	phase := alerting.SweepPhaseUnknown
	switch {
	case errors.Is(err, ErrListActiveTenants):
		phase = alerting.SweepPhaseListActive
	case errors.Is(err, ErrListNonActiveTenants):
		phase = alerting.SweepPhaseListNonActive
	}
	if logger != nil {
		logger.Error("reconciliation sweep: tenant listing failed (durable alert reconciliation.sweep_failed raised; per-tenant failures have their own run_failed alerts)",
			"phase", phase, "error", err)
	}
	alertSweepFailure(ctx, pool, logger, phase, sqlstateClassOf(err))
}

// recoverSweepPanic is deferred by the scheduler tick. Specialist review (backend, P0): the loop runs in a bare
// `go` statement with no equivalent of the HTTP path's recoverMiddleware; without a recover, a panic anywhere
// in RunSweep would crash the whole process, defeating "reconciliation failing must never take down the
// platform it exists to protect". The panic becomes a durable alert too. Only the panic's Go TYPE is logged
// (a value can wrap a tenant id, a query or a credential). recover() is called directly by this deferred
// function, as the language requires.
func recoverSweepPanic(ctx context.Context, pool *db.Pool, logger *slog.Logger) {
	if r := recover(); r != nil {
		if logger != nil {
			logger.Error("reconciliation sweep: recovered from panic", "panic_type", fmt.Sprintf("%T", r))
		}
		alertSweepFailure(ctx, pool, logger, alerting.SweepPhasePanic, "58")
	}
}

func alertSweepFailure(ctx context.Context, pool *db.Pool, logger *slog.Logger, phase, class string) {
	if err := raiseSweepFailed(ctx, pool, phase, class); err != nil && logger != nil {
		logger.Error("sweep_failed_alert_not_written", "phase", phase, "sqlstate_class", class)
	}
}
