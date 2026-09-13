// Stage 3C hardening (directive item 4): operationalizes the
// ledger-vs-projection reconciliation stream Stage 3B only ever exercised
// from tests. Nothing in Stage 3B invoked RunLedgerVsProjection outside a
// test - this file is the scheduler/job mechanism that actually runs it,
// on every tenant, on a recurring cadence, safely.
package reconciliation

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

// TryRunLedgerVsProjectionForTenant wraps RunLedgerVsProjection with a
// transaction-scoped advisory lock keyed by tenantID, so two overlapping
// sweep attempts for the SAME tenant (adversarial test 8.K: "reconciliation
// runs concurrently twice") serialize instead of racing to insert
// duplicate Run/Mismatch rows for the same tick. The lock is released
// automatically at commit or rollback (the "xact" advisory lock family) -
// it can never be left stuck by a crashed process the way a session-level
// lock or an application-level mutex could be. acquired is false (with a
// zero Run, zero Mismatches, and a nil error) when another sweep already
// holds the lock for this tenant; the caller treats that as "skip this
// tick for this tenant", never as a failure.
func TryRunLedgerVsProjectionForTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time) (run Run, mismatches []Mismatch, acquired bool, err error) {
	// hashtextextended (not hashtext) deliberately: hashtext returns a
	// 32-bit int4, so with enough tenants two could hash to the same
	// advisory-lock key and silently skip each other's sweep (specialist
	// review: architect/ledger-finance). hashtextextended(_, 0) returns
	// the full 64-bit hash pg_try_advisory_xact_lock(bigint) accepts,
	// making an accidental collision astronomically less likely.
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtextextended('reconciliation:' || $1::text, 0))`,
		tenantID,
	).Scan(&acquired); err != nil {
		return Run{}, nil, false, fmt.Errorf("reconciliation: acquire tenant advisory lock: %w", err)
	}
	if !acquired {
		return Run{}, nil, false, nil
	}
	run, mismatches, err = RunLedgerVsProjection(ctx, tx, tenantID, periodStart, periodEnd)
	return run, mismatches, true, err
}

// SweepOutcome is one tenant's result from a single RunSweep tick.
type SweepOutcome struct {
	TenantID uuid.UUID
	// Skipped is true when another sweep already held this tenant's
	// advisory lock - not a failure, just lock contention.
	Skipped bool
	Run     Run
	Err     error
}

// allTenantIDs reads every tenant id via a platform-scoped connection.
// This is the one legitimate cross-tenant read in this package: the
// sweep exists to iterate tenants so it can reconcile each one
// individually, and it never reads a tenant-owned row without first
// opening that tenant's own db.Pool.WithTenant scope (docs/decisions/0019
// "RLS and tenancy shape" - reconciliation_runs/reconciliation_mismatches
// themselves are tenant-scoped-only, no dual-scope policy, matching
// migration 0027's own comment).
func allTenantIDs(ctx context.Context, pool *db.Pool) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Specialist review (architect): a suspended/closed tenant has no
		// legitimate reason to be swept - reconciling a closed tenant's
		// (presumably frozen) books every hour forever is pure waste, not
		// a safety measure.
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

// RunSweep executes one ledger-vs-projection reconciliation attempt for
// every tenant, isolating each tenant's outcome from every other
// tenant's - directive item 4's "failure-safe... tenant-aware... safe to
// retry". One tenant's error (a DB error mid-run, an unexpected
// exception) is recorded on its own SweepOutcome and never aborts the
// sweep for the remaining tenants. The sweep itself does nothing that
// cannot be safely repeated: it only ever inserts new
// ReconciliationRun/ReconciliationMismatch/audit_log rows and never
// mutates ledger or projection data - RunLedgerVsProjection's own
// invariant, unchanged here. Every attempt (success, lock-skipped, or
// failed) is recorded to audit_log with ActorSystem, so a sweep that ran
// but found nothing to do is exactly as observable as one that found
// mismatches.
func RunSweep(ctx context.Context, pool *db.Pool, logger *slog.Logger, periodStart, periodEnd time.Time) ([]SweepOutcome, error) {
	tenantIDs, err := allTenantIDs(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: list tenants for sweep: %w", err)
	}

	outcomes := make([]SweepOutcome, 0, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		outcome := SweepOutcome{TenantID: tenantID}

		txErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			run, _, acquired, err := TryRunLedgerVsProjectionForTenant(ctx, tx, tenantID, periodStart, periodEnd)
			outcome.Run, outcome.Skipped = run, !acquired
			if err != nil {
				return err
			}
			// Specialist review (architect/ledger-finance): when skipped
			// by lock contention, run is the zero value, so "status"
			// would otherwise be an empty string - indistinguishable at a
			// glance from a genuinely clean run whose Status field was
			// somehow blank. Record "skipped" explicitly instead.
			status := string(run.Status)
			if !acquired {
				status = "skipped"
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run",
				TargetType: "reconciliation_run", TargetID: run.ID.String(), Outcome: audit.OutcomeSuccess,
				Metadata: map[string]any{
					"stream": string(StreamLedgerVsProjection), "skipped_lock_contention": !acquired,
					"status": status,
				},
			})
		})
		outcome.Err = txErr
		outcomes = append(outcomes, outcome)

		switch {
		case txErr != nil:
			if logger != nil {
				logger.Error("reconciliation sweep: tenant run failed", "tenant_id", tenantID, "error", txErr)
			}
			// The failed attempt's own transaction (including any
			// Run/Mismatch rows it tried to insert, and the audit
			// record above) already rolled back in full - CLAUDE.md's
			// "no fake completion"/auditability rules mean the failure
			// itself must still become an observable, auditable fact,
			// not just a log line, so it is recorded here in a fresh
			// transaction, best-effort.
			if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run_failed",
					TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
					Metadata: map[string]any{"stream": string(StreamLedgerVsProjection), "error": txErr.Error()},
				})
			}); auditErr != nil && logger != nil {
				logger.Error("reconciliation sweep: failed to audit tenant failure", "tenant_id", tenantID, "error", auditErr)
			}
		case outcome.Run.Status == StatusMismatchesFound:
			// CLAUDE.md: "Any non-zero drift is a P1 incident." Logged at
			// Error level (specialist review: ledger-finance), not Info -
			// a mismatch found is not routine sweep telemetry, it is the
			// exact condition this job exists to surface. The
			// reconciliation_mismatches rows themselves are the durable
			// record; this is the runtime signal an operator/alerting
			// pipeline can act on without polling the table.
			if logger != nil {
				logger.Error("reconciliation sweep: MISMATCH FOUND", "tenant_id", tenantID, "run_id", outcome.Run.ID)
			}
		case logger != nil:
			logger.Info("reconciliation sweep: tenant run complete",
				"tenant_id", tenantID, "skipped_lock_contention", outcome.Skipped, "status", string(outcome.Run.Status))
		}
	}
	return outcomes, nil
}

// RunSchedulerLoop invokes RunSweep on a fixed interval until ctx is
// canceled - the scheduler/job mechanism directive item 4 requires
// ("the target remains hourly unless an existing approved architecture
// specifies otherwise" - reconciliation-model.md names no other cadence,
// so the default stays hourly; see cmd/platform-api/main.go's
// RECONCILIATION_INTERVAL_SECONDS for how it is configured). It runs one
// sweep immediately on start, then every interval thereafter: a process
// that was down across what would have been a scheduled tick should not
// have to wait a full interval after restart to catch up, and running an
// extra sweep is always safe (RunSweep's own idempotency). A sweep's own
// errors are logged and audited per-tenant inside RunSweep and never
// crash this loop or the calling process - reconciliation failing must
// never take down the platform it exists to protect.
func RunSchedulerLoop(ctx context.Context, pool *db.Pool, logger *slog.Logger, interval time.Duration) {
	runOnce := func() {
		// Specialist review (backend, P0): this loop runs in a bare `go`
		// statement (cmd/platform-api/main.go) with no equivalent of the
		// HTTP path's recoverMiddleware. Without this recover, a panic
		// anywhere in RunSweep - a future refactor's nil-pointer, a
		// driver-level panic - would crash the ENTIRE platform-api
		// process, not just reconciliation, defeating the very
		// "reconciliation failing must never take down the platform it
		// exists to protect" guarantee this package documents elsewhere.
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error("reconciliation sweep: recovered from panic", "panic", r)
			}
		}()
		now := time.Now().UTC()
		if _, err := RunSweep(ctx, pool, logger, now.Add(-interval), now); err != nil && logger != nil {
			logger.Error("reconciliation sweep: failed to list tenants", "error", err)
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
