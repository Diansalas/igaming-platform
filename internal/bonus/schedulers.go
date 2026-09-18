// Scheduler wiring for the three sweep/scheduler jobs this Wave adds
// (deposit_sweep.go, cashback_scheduler.go, expiry_sweep.go), reusing
// internal/reconciliation/scheduler.go's exact pattern per every one of
// this Wave's own contract sections (§7.18.2 item 1, §7.18.4 item 1-2,
// the expiry sweep's own dependency-map entry): per-tenant
// pg_try_advisory_xact_lock (never a plain pg_advisory_lock - the "xact"
// family releases automatically at COMMIT/ROLLBACK, so a crashed
// process can never leave a tenant permanently un-swept),
// hashtextextended (not hashtext) for the full 64-bit lock-key space
// (mirroring TryRunLedgerVsProjectionForTenant's own documented
// collision-avoidance rationale), one pool.WithTenant transaction per
// tenant per job, and a RunSchedulerLoop-style ticker.
package bonus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// systemSchedulerActorID is the fixed, well-known system principal every
// job in this file records as its own activating actor - mirroring
// TriggerActorForAutomated's own ActorSystem convention (ActivateGrant
// never persists a non-nil actor id for ActorSystem, see
// nonNilActorID), so this value is never actually written to a Grant's
// own audit/Progress trail; it exists only because several call sites'
// Go signatures require SOME uuid.UUID value to pass through.
var systemSchedulerActorID = uuid.Nil

// allActiveTenantIDs reads every active tenant id via a platform-scoped
// connection - the one legitimate cross-tenant read a sweep needs,
// mirroring internal/reconciliation.allTenantIDs exactly (this package
// cannot import that unexported helper, so it is duplicated here per
// this repo's own per-package test/scheduler-helper convention, not
// re-derived differently).
func allActiveTenantIDs(ctx context.Context, pool *db.Pool) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM tenants WHERE status = 'active'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// tryAdvisoryLockedTenantJob runs fn for tenantID inside its own
// tenant-scoped transaction, guarded by a pg_try_advisory_xact_lock keyed
// on lockNamespace+tenantID - two overlapping ticks for the SAME tenant
// serialize (never race to double-issue a Grant for the same window/
// event), and a tenant whose lock is already held by another in-flight
// tick is skipped for this tick, not blocked or failed.
func tryAdvisoryLockedTenantJob(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, lockNamespace string, fn func(ctx context.Context, tx pgx.Tx) error) (acquired bool, err error) {
	err = pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if lockErr := tx.QueryRow(ctx,
			`SELECT pg_try_advisory_xact_lock(hashtextextended($1 || $2::text, 0))`,
			lockNamespace+":", tenantID,
		).Scan(&acquired); lockErr != nil {
			return fmt.Errorf("bonus: acquire %s advisory lock: %w", lockNamespace, lockErr)
		}
		if !acquired {
			return nil
		}
		return fn(ctx, tx)
	})
	return acquired, err
}

// --- Deposit sweep scheduler wiring ---

// RunDepositSweep runs RunDepositSweepForTenant for every active tenant,
// isolating each tenant's own outcome/failure from every other tenant's
// - mirrors internal/reconciliation.RunSweep's own per-tenant isolation
// exactly (one tenant's error never aborts the sweep for the rest).
func RunDepositSweep(ctx context.Context, pool *db.Pool, logger *slog.Logger) error {
	tenantIDs, err := allActiveTenantIDs(ctx, pool)
	if err != nil {
		return fmt.Errorf("bonus: deposit sweep: list tenants: %w", err)
	}
	for _, tenantID := range tenantIDs {
		var outcome DepositSweepOutcome
		acquired, err := tryAdvisoryLockedTenantJob(ctx, pool, tenantID, "bonus_deposit_sweep", func(ctx context.Context, tx pgx.Tx) error {
			var runErr error
			outcome, runErr = RunDepositSweepForTenant(ctx, tx, tenantID, systemSchedulerActorID)
			if runErr != nil {
				return runErr
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_deposit_sweep.tick",
				TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeSuccess,
				Metadata: map[string]any{"events_processed": outcome.EventsProcessed, "grants_issued": outcome.GrantsIssued, "grants_denied": outcome.GrantsDenied},
			})
		})
		if err != nil {
			if logger != nil {
				logger.Error("bonus deposit sweep: tenant tick failed", "tenant_id", tenantID, "error", err)
			}
			if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_deposit_sweep.tick_failed",
					TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
					Metadata: map[string]any{"error": err.Error()},
				})
			}); auditErr != nil && logger != nil {
				logger.Error("bonus deposit sweep: failed to audit tenant failure", "tenant_id", tenantID, "error", auditErr)
			}
			continue
		}
		if logger != nil && acquired && outcome.EventsProcessed > 0 {
			logger.Info("bonus deposit sweep: tenant tick complete", "tenant_id", tenantID,
				"events_processed", outcome.EventsProcessed, "grants_issued", outcome.GrantsIssued, "grants_denied", outcome.GrantsDenied)
		}
	}
	return nil
}

// RunDepositSweepSchedulerLoop mirrors reconciliation.RunSchedulerLoop
// exactly: one immediate run, then every interval thereafter, with its
// own panic recovery (this loop runs in a bare `go` statement with no
// HTTP-path-style recoverMiddleware backstop).
func RunDepositSweepSchedulerLoop(ctx context.Context, pool *db.Pool, logger *slog.Logger, interval time.Duration) {
	runSchedulerLoop(ctx, logger, interval, "bonus deposit sweep", func() error { return RunDepositSweep(ctx, pool, logger) })
}

// --- Cashback scheduler wiring ---

// RunCashbackSweep runs RunCashbackSchedulerForTenant for every active
// tenant, identical isolation posture to RunDepositSweep.
func RunCashbackSweep(ctx context.Context, pool *db.Pool, logger *slog.Logger) error {
	tenantIDs, err := allActiveTenantIDs(ctx, pool)
	if err != nil {
		return fmt.Errorf("bonus: cashback scheduler: list tenants: %w", err)
	}
	asOf := time.Now().UTC()
	for _, tenantID := range tenantIDs {
		var outcome CashbackSchedulerOutcome
		acquired, err := tryAdvisoryLockedTenantJob(ctx, pool, tenantID, "bonus_cashback_scheduler", func(ctx context.Context, tx pgx.Tx) error {
			var runErr error
			outcome, runErr = RunCashbackSchedulerForTenant(ctx, tx, tenantID, systemSchedulerActorID, asOf)
			if runErr != nil {
				return runErr
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_cashback_scheduler.tick",
				TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeSuccess,
				Metadata: map[string]any{"windows_evaluated": outcome.WindowsEvaluated, "grants_issued": outcome.GrantsIssued, "grants_denied": outcome.GrantsDenied},
			})
		})
		if err != nil {
			if logger != nil {
				logger.Error("bonus cashback scheduler: tenant tick failed", "tenant_id", tenantID, "error", err)
			}
			if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_cashback_scheduler.tick_failed",
					TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
					Metadata: map[string]any{"error": err.Error()},
				})
			}); auditErr != nil && logger != nil {
				logger.Error("bonus cashback scheduler: failed to audit tenant failure", "tenant_id", tenantID, "error", auditErr)
			}
			continue
		}
		if logger != nil && acquired && outcome.WindowsEvaluated > 0 {
			logger.Info("bonus cashback scheduler: tenant tick complete", "tenant_id", tenantID,
				"windows_evaluated", outcome.WindowsEvaluated, "grants_issued", outcome.GrantsIssued, "grants_denied", outcome.GrantsDenied)
		}
	}
	return nil
}

// RunCashbackSchedulerLoop mirrors RunDepositSweepSchedulerLoop exactly.
func RunCashbackSchedulerLoop(ctx context.Context, pool *db.Pool, logger *slog.Logger, interval time.Duration) {
	runSchedulerLoop(ctx, logger, interval, "bonus cashback scheduler", func() error { return RunCashbackSweep(ctx, pool, logger) })
}

// --- Expiry sweep wiring ---

// RunExpirySweep runs RunExpirySweepForTenant for every active tenant,
// identical isolation posture to RunDepositSweep.
func RunExpirySweep(ctx context.Context, pool *db.Pool, logger *slog.Logger) error {
	tenantIDs, err := allActiveTenantIDs(ctx, pool)
	if err != nil {
		return fmt.Errorf("bonus: expiry sweep: list tenants: %w", err)
	}
	asOf := time.Now().UTC()
	for _, tenantID := range tenantIDs {
		var outcome ExpirySweepOutcome
		acquired, err := tryAdvisoryLockedTenantJob(ctx, pool, tenantID, "bonus_expiry_sweep", func(ctx context.Context, tx pgx.Tx) error {
			var runErr error
			outcome, runErr = RunExpirySweepForTenant(ctx, tx, tenantID, systemSchedulerActorID, asOf)
			if runErr != nil {
				return runErr
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_expiry_sweep.tick",
				TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeSuccess,
				Metadata: map[string]any{"grants_examined": outcome.GrantsExamined, "grants_terminated": outcome.GrantsTerminated, "grants_deferred": outcome.GrantsDeferred},
			})
		})
		if err != nil {
			if logger != nil {
				logger.Error("bonus expiry sweep: tenant tick failed", "tenant_id", tenantID, "error", err)
			}
			if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_expiry_sweep.tick_failed",
					TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
					Metadata: map[string]any{"error": err.Error()},
				})
			}); auditErr != nil && logger != nil {
				logger.Error("bonus expiry sweep: failed to audit tenant failure", "tenant_id", tenantID, "error", auditErr)
			}
			continue
		}
		if logger != nil && acquired && outcome.GrantsExamined > 0 {
			logger.Info("bonus expiry sweep: tenant tick complete", "tenant_id", tenantID,
				"grants_examined", outcome.GrantsExamined, "grants_terminated", outcome.GrantsTerminated, "grants_deferred", outcome.GrantsDeferred)
		}
	}
	return nil
}

// RunExpirySweepSchedulerLoop mirrors RunDepositSweepSchedulerLoop exactly.
func RunExpirySweepSchedulerLoop(ctx context.Context, pool *db.Pool, logger *slog.Logger, interval time.Duration) {
	runSchedulerLoop(ctx, logger, interval, "bonus expiry sweep", func() error { return RunExpirySweep(ctx, pool, logger) })
}

// runSchedulerLoop is the one shared "run immediately, then every
// interval, with panic recovery" shape all three jobs above use -
// factored out once here rather than copy-pasted three times, unlike
// internal/reconciliation.RunSchedulerLoop's own single-job version
// (which has no sibling job in that package to share this with).
func runSchedulerLoop(ctx context.Context, logger *slog.Logger, interval time.Duration, jobName string, run func() error) {
	runOnce := func() {
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error(jobName+": recovered from panic", "panic", r)
			}
		}()
		if err := run(); err != nil && logger != nil {
			logger.Error(jobName+": tick failed", "error", err)
		}
	}

	runOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
