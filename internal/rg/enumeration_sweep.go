// Stage 4H-B0-R7, Workstream D (directive item 4): wires
// FindMissingEnumerationRuns and FindStalledEnumerationRuns - both
// PARTIALLY IMPLEMENTED reconciliation queries from Stage 4H-B0-R6 - into
// an actual running scheduler. Stage 4H-B0-R6's own security finding was
// that FindStalledEnumerationRuns had zero callers in a running system,
// leaving the migration 0043 partial index it was built to use
// unqueried outside tests; this pass found the identical gap for
// FindMissingEnumerationRuns (also zero non-test callers) and closes both
// together, since they are the same class of finding and it would be
// inconsistent to fix one and knowingly leave the other unfixed once
// found.
//
// This mirrors internal/reconciliation/scheduler.go's own RunSweep/
// RunSchedulerLoop pattern deliberately rather than inventing a new
// shape - same per-tenant isolation (one tenant's error never aborts the
// sweep for the rest), same "record every attempt to audit_log, success
// or failure" rule, same panic-recovered ticker loop. It is a
// package-local, self-contained sweep (its own tenant-listing query, not
// a shared helper imported from internal/reconciliation) rather than a
// modification of that file: internal/reconciliation is
// `ledger-finance`-owned per docs/decisions/0019, and self_exclusion_
// enumeration_runs is an `identity-compliance`-owned table with no
// natural home in that package. Sharing the twelve-line tenant-listing
// query is not worth a cross-domain coupling or an architect-gated
// "promote to shared infrastructure" change for this - the identical
// reasoning verifyReconciliationScope's own doc comment already gives for
// keeping that check package-local rather than promoted to
// internal/risk's copy.
//
// What this file deliberately does NOT do: enumerate a Person's actual
// open bets, void anything, or dispatch to a sportsbook listener - no
// sportsbook implementation exists in this codebase yet (see this
// package's other files' own doc comments). This sweep only detects and
// audits the two gap shapes FindMissingEnumerationRuns/
// FindStalledEnumerationRuns already define; closing a detected gap is
// still the future listener's job.
package rg

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

// activeTenantIDsForEnumerationSweep lists every active tenant, exactly
// mirroring internal/reconciliation's own allTenantIDs (same query, same
// platform-scoped read, same "active only - a suspended/closed tenant has
// no legitimate reason to be swept" reasoning) - deliberately not shared
// code, per this file's own package doc comment.
func activeTenantIDsForEnumerationSweep(ctx context.Context, pool *db.Pool) ([]uuid.UUID, error) {
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

// EnumerationSweepOutcome is one tenant's result from a single
// RunEnumerationReconciliationSweep tick.
type EnumerationSweepOutcome struct {
	TenantID    uuid.UUID
	MissingRuns []EnumerationGap
	StalledRuns []StalledEnumerationRun
	// Err is set when this tenant's reconciliation attempt itself failed
	// (a DB error mid-query) - distinct from finding zero or more gaps,
	// which is a successful attempt with a (possibly empty) result.
	Err error
}

// StalledRunThresholdDefault is the conservative default age a run may
// sit 'pending'/'in_progress' before RunEnumerationReconciliationSweep
// treats it as stalled, used when the caller supplies zero. See
// FindStalledEnumerationRuns' own doc comment for why this is a tunable,
// not a hardcoded platform fact - 15 minutes is a starting operational
// default, not a claim about how fast a future listener is required to
// run.
const StalledRunThresholdDefault = 15 * time.Minute

// RunEnumerationReconciliationSweep runs both self-exclusion enumeration
// reconciliation queries (FindMissingEnumerationRuns,
// FindStalledEnumerationRuns) once for every active tenant, isolating
// each tenant's outcome from every other tenant's exactly like
// internal/reconciliation.RunSweep: one tenant's error is recorded on its
// own EnumerationSweepOutcome and never aborts the sweep for the
// remaining tenants. Every attempt (clean, gap(s) found, or failed) is
// recorded to audit_log with ActorSystem, so a sweep that ran and found
// nothing is exactly as observable as one that found a gap - the same
// property RunSweep's own doc comment states for its stream.
//
// stalledOlderThan is passed through to FindStalledEnumerationRuns; zero
// substitutes StalledRunThresholdDefault rather than erroring, since a
// scheduler loop calling this on a fixed interval should not have to
// re-derive that default at every call site.
func RunEnumerationReconciliationSweep(ctx context.Context, pool *db.Pool, logger *slog.Logger, stalledOlderThan time.Duration) ([]EnumerationSweepOutcome, error) {
	if stalledOlderThan <= 0 {
		stalledOlderThan = StalledRunThresholdDefault
	}

	tenantIDs, err := activeTenantIDsForEnumerationSweep(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("rg: list tenants for enumeration reconciliation sweep: %w", err)
	}

	outcomes := make([]EnumerationSweepOutcome, 0, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		outcome := EnumerationSweepOutcome{TenantID: tenantID}

		txErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			missing, err := FindMissingEnumerationRuns(ctx, tx, tenantID)
			if err != nil {
				return fmt.Errorf("find missing enumeration runs: %w", err)
			}
			stalled, err := FindStalledEnumerationRuns(ctx, tx, tenantID, stalledOlderThan)
			if err != nil {
				return fmt.Errorf("find stalled enumeration runs: %w", err)
			}
			outcome.MissingRuns, outcome.StalledRuns = missing, stalled

			missingIDs := make([]string, len(missing))
			for i, g := range missing {
				missingIDs[i] = g.RestrictionID.String()
			}
			stalledIDs := make([]string, len(stalled))
			for i, s := range stalled {
				stalledIDs[i] = s.ID.String()
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem,
				Action: "rg.self_exclusion_enumeration_reconciliation.sweep_run", TargetType: "tenant", TargetID: tenantID.String(),
				Outcome: audit.OutcomeSuccess,
				Metadata: map[string]any{
					"missing_count": len(missing), "missing_restriction_ids": missingIDs,
					"stalled_count": len(stalled), "stalled_run_ids": stalledIDs,
					"stalled_older_than_seconds": int64(stalledOlderThan.Seconds()),
				},
			})
		})
		outcome.Err = txErr
		outcomes = append(outcomes, outcome)

		switch {
		case txErr != nil:
			if logger != nil {
				logger.Error("enumeration reconciliation sweep: tenant run failed", "tenant_id", tenantID, "error", txErr)
			}
			// Mirrors RunSweep's own reasoning: the failed attempt's own
			// transaction (including the audit record above) already
			// rolled back in full, so the failure itself must still
			// become an observable, auditable fact - recorded here in a
			// fresh, best-effort transaction.
			if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem,
					Action: "rg.self_exclusion_enumeration_reconciliation.sweep_run_failed", TargetType: "tenant", TargetID: tenantID.String(),
					Outcome: audit.OutcomeFailure, Metadata: map[string]any{"error": txErr.Error()},
				})
			}); auditErr != nil && logger != nil {
				logger.Error("enumeration reconciliation sweep: failed to audit tenant failure", "tenant_id", tenantID, "error", auditErr)
			}
		case len(outcome.MissingRuns) > 0 || len(outcome.StalledRuns) > 0:
			// A dropped or stalled enumeration run means self-exclusion
			// enforcement for a real open bet may not have run at all -
			// this is a compliance-relevant finding, not routine sweep
			// telemetry, logged at Error level to match RunSweep's own
			// "mismatch found" precedent.
			if logger != nil {
				logger.Error("enumeration reconciliation sweep: gap found",
					"tenant_id", tenantID, "missing_count", len(outcome.MissingRuns), "stalled_count", len(outcome.StalledRuns))
			}
		case logger != nil:
			logger.Info("enumeration reconciliation sweep: tenant run complete", "tenant_id", tenantID)
		}
	}
	return outcomes, nil
}

// RunEnumerationReconciliationSchedulerLoop invokes
// RunEnumerationReconciliationSweep on a fixed interval until ctx is
// canceled, mirroring internal/reconciliation.RunSchedulerLoop exactly:
// one sweep immediately on start (a process down across a scheduled tick
// should not wait a full interval to catch up), then every interval
// thereafter, with the identical panic-recovery this goroutine needs for
// the identical reason - it runs in a bare `go` statement with no
// recoverMiddleware equivalent, so an unrecovered panic here would take
// down the whole platform-api process, not just this sweep.
func RunEnumerationReconciliationSchedulerLoop(ctx context.Context, pool *db.Pool, logger *slog.Logger, interval, stalledOlderThan time.Duration) {
	runOnce := func() {
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error("enumeration reconciliation sweep: recovered from panic", "panic", r)
			}
		}()
		if _, err := RunEnumerationReconciliationSweep(ctx, pool, logger, stalledOlderThan); err != nil && logger != nil {
			logger.Error("enumeration reconciliation sweep: failed to list tenants", "error", err)
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
