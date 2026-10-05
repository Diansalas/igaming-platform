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

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
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

// TryRunSportsbookSettlementForTenant is TryRunLedgerVsProjectionForTenant
// for the sportsbook_settlement stream (ADR 0088 §8): same
// transaction-scoped advisory-lock discipline, under a stream-specific
// key so the two streams never contend with each other, only with a
// concurrent sweep of the SAME stream for the same tenant.
func TryRunSportsbookSettlementForTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time, source statement.SportsbookSettlementSource) (run Run, mismatches []Mismatch, acquired bool, err error) {
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtextextended('reconciliation:sportsbook_settlement:' || $1::text, 0))`,
		tenantID,
	).Scan(&acquired); err != nil {
		return Run{}, nil, false, fmt.Errorf("reconciliation: acquire tenant sportsbook advisory lock: %w", err)
	}
	if !acquired {
		return Run{}, nil, false, nil
	}
	run, mismatches, err = RunSportsbookSettlement(ctx, tx, tenantID, periodStart, periodEnd, source)
	return run, mismatches, true, err
}

// TryRunCasinoConsistencyForTenant is TryRunLedgerVsProjectionForTenant for
// the casino_consistency stream (Stage 10.3 W2b, CAS-RECON-1): the same
// transaction-scoped advisory-lock discipline under its own stream key
// ('reconciliation:casino_consistency:<tenant>'), so a concurrent sweep of
// the SAME stream for the same tenant serializes (the loser is skipped and
// records nothing but its audited "skipped" attempt), while the other
// streams never contend with it.
func TryRunCasinoConsistencyForTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time) (run Run, mismatches []Mismatch, metrics CasinoMetrics, acquired bool, err error) {
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtextextended('reconciliation:casino_consistency:' || $1::text, 0))`,
		tenantID,
	).Scan(&acquired); err != nil {
		return Run{}, nil, CasinoMetrics{}, false, fmt.Errorf("reconciliation: acquire tenant casino advisory lock: %w", err)
	}
	if !acquired {
		return Run{}, nil, CasinoMetrics{}, false, nil
	}
	run, mismatches, metrics, err = RunCasinoConsistency(ctx, tx, tenantID, periodStart, periodEnd)
	return run, mismatches, metrics, true, err
}

// TryRunCasinoStatementForTenant is TryRunLedgerVsProjectionForTenant for
// the casino_statement stream (Stage 10.3 W3a, CAS-RECON-STMT-1): the same
// transaction-scoped advisory-lock discipline under its own stream key
// ('reconciliation:casino_statement:<tenant>'), so a concurrent sweep of
// the SAME stream for the same tenant serializes (the loser is skipped and
// records nothing but its audited "skipped" attempt), while the other
// streams never contend with it. tx must be REPEATABLE READ
// (db.Pool.WithTenantSnapshot); RunCasinoStatement enforces it.
func TryRunCasinoStatementForTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time, source statement.CasinoStatementSource) (run Run, mismatches []Mismatch, info CasinoStatementInfo, acquired bool, err error) {
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtextextended('reconciliation:casino_statement:' || $1::text, 0))`,
		tenantID,
	).Scan(&acquired); err != nil {
		return Run{}, nil, CasinoStatementInfo{}, false, fmt.Errorf("reconciliation: acquire tenant casino statement advisory lock: %w", err)
	}
	if !acquired {
		return Run{}, nil, CasinoStatementInfo{}, false, nil
	}
	run, mismatches, info, err = RunCasinoStatement(ctx, tx, tenantID, periodStart, periodEnd, source)
	return run, mismatches, info, true, err
}

// TryRunPaymentStatementForTenant is TryRunLedgerVsProjectionForTenant for
// the payment_statement stream's MATCH phase (PRH-I5, ADR 0095 §12.1 step
// 3): the same transaction-scoped advisory-lock discipline under its own
// stream key ('reconciliation:payment_statement:<tenant>'). tx must be
// REPEATABLE READ (db.Pool.WithTenantSnapshot); RunPaymentStatement
// enforces it. The fetch and ingest phases never run under this lock or
// in this transaction (ReconcilePaymentStatementForTenant).
func TryRunPaymentStatementForTenant(ctx context.Context, tx pgx.Tx, tenantID, importID uuid.UUID, opts PaymentStatementOptions) (run Run, mismatches []Mismatch, info PaymentStatementInfo, acquired bool, err error) {
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtextextended('reconciliation:payment_statement:' || $1::text, 0))`,
		tenantID,
	).Scan(&acquired); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, false, fmt.Errorf("reconciliation: acquire tenant payment statement advisory lock: %w", err)
	}
	if !acquired {
		return Run{}, nil, PaymentStatementInfo{}, false, nil
	}
	run, mismatches, info, err = RunPaymentStatement(ctx, tx, tenantID, importID, opts)
	return run, mismatches, info, true, err
}

// SweepOutcome is one tenant's result from a single RunSweep tick.
type SweepOutcome struct {
	TenantID uuid.UUID
	// Skipped is true when another sweep already held this tenant's
	// advisory lock - not a failure, just lock contention.
	Skipped bool
	Run     Run
	Err     error
	// Sportsbook is the same tenant's sportsbook_settlement stream result
	// (ADR 0088 §8), run after ledger_vs_projection in its OWN
	// tenant-scoped transaction so one stream's failure never discards the
	// other's recorded evidence.
	Sportsbook StreamOutcome
	// Casino is the same tenant's casino_consistency stream result (Stage
	// 10.3 W2b, CAS-RECON-1), run after sportsbook_settlement in its OWN
	// tenant-scoped transaction, for the same reason.
	Casino StreamOutcome
	// CasinoStatement is the same tenant's casino_statement stream result
	// (Stage 10.3 W3a, CAS-RECON-STMT-1), run after casino_consistency in
	// its OWN tenant-scoped REPEATABLE READ transaction, for the same
	// reason. With today's MOCK source a clean result is tautological.
	CasinoStatement StreamOutcome
	// PaymentStatement is the same tenant's payment_statement stream result
	// (PRH-I5, ADR 0095 §12), one per registered payment statement source in
	// the order given, each run after casino_statement: fetch with no
	// transaction held, ingest in its own short transaction, match in its
	// own REPEATABLE READ transaction. With today's MOCK source it is a
	// MOCK result (the MockProvider's own records, not the platform DB).
	PaymentStatement []StreamOutcome
	// ObservationOnly is true for a tenant whose status is not 'active' (PRH-2
	// R3, H-W1): only the payment_statement stream ran for it, in evidence-only
	// mode (see observeNonActiveTenants); every field except PaymentStatement
	// and TenantStatus is the zero value. TenantStatus is the status read at
	// selection ("suspended", "closed"); empty for an active tenant.
	ObservationOnly bool
	TenantStatus    string
}

// StreamOutcome is one additional stream's per-tenant result.
type StreamOutcome struct {
	Skipped bool
	Run     Run
	Err     error
}

// allTenantIDs reads every ACTIVE tenant id via a platform-scoped
// connection (activeTenantIDs with no restriction).
func allTenantIDs(ctx context.Context, pool *db.Pool) ([]uuid.UUID, error) {
	return activeTenantIDs(ctx, pool, nil)
}

// activeTenantIDs reads the ids of active tenants via a platform-scoped
// connection, restricted to only when only is non-nil (a nil only means
// "every active tenant"; an empty non-nil only means "none"). Result
// order is deterministic (by id).
//
// This is the one legitimate cross-tenant read in this package: the
// sweep exists to iterate tenants so it can reconcile each one
// individually, and it never reads a tenant-owned row without first
// opening that tenant's own db.Pool.WithTenant scope (docs/decisions/0019
// "RLS and tenancy shape" - reconciliation_runs/reconciliation_mismatches
// themselves are tenant-scoped-only, no dual-scope policy, matching
// migration 0027's own comment).
func activeTenantIDs(ctx context.Context, pool *db.Pool, only []uuid.UUID) ([]uuid.UUID, error) {
	if only != nil && len(only) == 0 {
		return nil, nil
	}
	var ids []uuid.UUID
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Specialist review (architect): a suspended/closed tenant has no
		// legitimate reason to be swept - reconciling a closed tenant's
		// (presumably frozen) books every hour forever is pure waste, not
		// a safety measure. The same filter applies to a tenant-scoped
		// sweep (RunSweepTenants): naming a suspended/closed tenant does
		// not sweep it.
		// (PRH-2 R3 / H-W1: that decision stands for the full stream set; only
		// the payment_statement stream observes non-active tenants, evidence
		// only - observeNonActiveTenants.)
		rows, err := tx.Query(ctx,
			`SELECT id FROM tenants WHERE status = 'active' AND ($1::uuid[] IS NULL OR id = ANY($1::uuid[])) ORDER BY id`,
			only)
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

// RunSweep executes one ledger-vs-projection reconciliation attempt (and,
// after it, one sportsbook_settlement attempt - ADR 0088 §8) for
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
//
// casSource is the casino_statement stream's statement source (Stage 10.3
// W3a; in production today casino.MockStatementSource - MOCK). A nil
// casSource fails that stream's run closed (audited as a failure), never
// "clean".
//
// Cost: serial, O(active tenants x each tenant's full history) per call -
// every stream recomputes its whole population. Registered as
// CAS-RECON-SCALE-1 (docs/governance/task-registry.md); not redesigned
// here. RunSweepTenants runs the same body for named tenants only.
func RunSweep(ctx context.Context, pool *db.Pool, logger *slog.Logger, periodStart, periodEnd time.Time, sbSource statement.SportsbookSettlementSource, casSource statement.CasinoStatementSource, paySources ...statement.PaymentStatementSource) ([]SweepOutcome, error) {
	tenantIDs, err := allTenantIDs(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: list tenants for sweep: %w", err)
	}
	outcomes := sweepTenants(ctx, pool, logger, tenantIDs, periodStart, periodEnd, sbSource, casSource, paySources)
	// PRH-2 R3 (H-W1): non-active tenants are observed by the payment_statement
	// stream only. A failure to list them is returned, never swallowed; the
	// active outcomes are still returned with it.
	observed, obsErr := observeNonActiveTenants(ctx, pool, logger, nil, outcomes, periodStart, periodEnd, paySources)
	if obsErr != nil {
		return append(outcomes, observed...), fmt.Errorf("reconciliation: list non-active tenants for observation: %w", obsErr)
	}
	return append(outcomes, observed...), nil
}

// RunSweepTenants is RunSweep restricted to the named tenants: exactly the
// same per-tenant streams, locks, audit records, logging and failure
// isolation, over the ACTIVE tenants among tenantIDs (duplicates are swept
// once; an unknown id yields no SweepOutcome). A suspended or closed id among
// them gets RunSweep's evidence-only payment_statement observation
// (observeNonActiveTenants) when at least one payment statement source is
// given, and nothing otherwise. It is the operational entry point for an on-demand
// re-run of one tenant's reconciliation (for example after a provider
// redelivery, or to confirm a P1 is resolved) without paying for every
// other tenant's full sweep, and it is what the integration tests use so
// their runtime does not grow with every tenant other tests have left on
// the shared database. tenantIDs is a server-side, internal input - it is
// never taken from a client request. A nil or empty tenantIDs sweeps
// nothing (it never means "all"; that is RunSweep).
func RunSweepTenants(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantIDs []uuid.UUID, periodStart, periodEnd time.Time, sbSource statement.SportsbookSettlementSource, casSource statement.CasinoStatementSource, paySources ...statement.PaymentStatementSource) ([]SweepOutcome, error) {
	if len(tenantIDs) == 0 {
		return []SweepOutcome{}, nil
	}
	ids, err := activeTenantIDs(ctx, pool, tenantIDs)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: list tenants for scoped sweep: %w", err)
	}
	outcomes := sweepTenants(ctx, pool, logger, ids, periodStart, periodEnd, sbSource, casSource, paySources)
	observed, obsErr := observeNonActiveTenants(ctx, pool, logger, tenantIDs, outcomes, periodStart, periodEnd, paySources)
	if obsErr != nil {
		return append(outcomes, observed...), fmt.Errorf("reconciliation: list non-active tenants for scoped observation: %w", obsErr)
	}
	return append(outcomes, observed...), nil
}

// nonActiveTenant is one tenant whose status is anything but 'active'.
type nonActiveTenant struct {
	ID     uuid.UUID
	Status string
}

// nonActiveTenants lists the tenants whose status is NOT 'active' (today
// 'suspended' and 'closed'; the predicate is `<> 'active'`, so a status added
// later is observed too - fail-safe because observation is read-only),
// restricted to only when only is non-nil (a nil only means "every non-active
// tenant"; an empty non-nil only means "none"). Deterministic order (by id). It
// is the platform-scoped cross-tenant read activeTenantIDs documents; nothing
// tenant-owned is read without that tenant's own db.Pool.WithTenant scope.
func nonActiveTenants(ctx context.Context, pool *db.Pool, only []uuid.UUID) ([]nonActiveTenant, error) {
	if only != nil && len(only) == 0 {
		return nil, nil
	}
	var out []nonActiveTenant
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, status FROM tenants WHERE status <> 'active' AND ($1::uuid[] IS NULL OR id = ANY($1::uuid[])) ORDER BY id`,
			only)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t nonActiveTenant
			if err := rows.Scan(&t.ID, &t.Status); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// observeNonActiveTenants is the PRH-2 R3 / H-W1 observation sweep (OWNER
// DECISION 2026-10-05; ADR 0095 40.1, ADR 0101 28, ADR 0107 pointer).
//
// A suspended or closed tenant is not reconciled by the ordinary streams, but
// its provider-side money movements, parked captures, declared-paid payouts
// and M2 standing findings must not become invisible merely because the tenant
// stopped being active: R-5 still lets platform_acting staff resolve a closed
// tenant's player funds, so the detective control has to keep running.
// Therefore, for every non-active tenant and every given payment statement
// source, and ONLY for the payment_statement stream, it runs
// ReconcilePaymentStatementForTenant with PaymentStatementOptions
// .ObservationOnlyStatus set (recorded in the run's audit metadata as
// non_active_tenant_observation=true and tenant_status).
//
// GUARANTEE (pinned by TestR3_*): this path is read plus findings/alerts only.
// It posts no ledger transaction or entry, changes no attempt, withdrawal,
// deposit intent or receipt, creates no manual resolution, and calls no
// provider outbound except the statement READ itself (PaymentStatementSource
// .Fetch, a read-only gate call); it cannot QueryStatus, Deposit or Withdraw
// because this package imports no payment orchestrator or adapter. It does not
// change the payments sweeper, which stays RESOLUTION-ONLY for these tenants,
// and it does not unlock, create or execute any resolution: M2 on a closed
// tenant stays gated by the platform_acting + four-eyes path.
//
// The other streams (ledger_vs_projection, sportsbook_settlement,
// casino_consistency, casino_statement) are deliberately NOT run for a
// non-active tenant (deferred; none of them can make provider-side money
// disappear). With no sources it returns nothing. One tenant's failure is
// recorded on its own outcome and never stops the others.
func observeNonActiveTenants(ctx context.Context, pool *db.Pool, logger *slog.Logger, only []uuid.UUID, alreadySwept []SweepOutcome, periodStart, periodEnd time.Time, paySources []statement.PaymentStatementSource) ([]SweepOutcome, error) {
	if len(paySources) == 0 {
		return nil, nil
	}
	tenants, err := nonActiveTenants(ctx, pool, only)
	if err != nil {
		return nil, err
	}
	// A tenant that was active when the ordinary sweep selected it and was
	// closed while that sweep ran has already been reconciled this tick (by the
	// full stream set); it is not observed a second time in the same call.
	swept := make(map[uuid.UUID]bool, len(alreadySwept))
	for _, o := range alreadySwept {
		swept[o.TenantID] = true
	}
	outcomes := make([]SweepOutcome, 0, len(tenants))
	for _, t := range tenants {
		if swept[t.ID] {
			continue
		}
		outcome := SweepOutcome{TenantID: t.ID, ObservationOnly: true, TenantStatus: t.Status}
		for _, src := range paySources {
			outcome.PaymentStatement = append(outcome.PaymentStatement,
				ReconcilePaymentStatementForTenant(ctx, pool, logger, t.ID, periodStart, periodEnd, src,
					PaymentStatementOptions{ObservationOnlyStatus: t.Status}))
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// observationMetadata marks an audit record written by the non-active-tenant
// observation sweep so staff can see the tenant was not active when the run
// happened. A run for an active tenant is returned unchanged.
func observationMetadata(opts PaymentStatementOptions, m map[string]any) map[string]any {
	if opts.ObservationOnlyStatus != "" {
		m["non_active_tenant_observation"] = true
		m["tenant_status"] = opts.ObservationOnlyStatus
	}
	return m
}

// sweepTenants is the per-tenant body shared by RunSweep and
// RunSweepTenants.
func sweepTenants(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantIDs []uuid.UUID, periodStart, periodEnd time.Time, sbSource statement.SportsbookSettlementSource, casSource statement.CasinoStatementSource, paySources []statement.PaymentStatementSource) []SweepOutcome {
	outcomes := make([]SweepOutcome, 0, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		outcome := SweepOutcome{TenantID: tenantID}

		pending, txErr := alerting.InTx(ctx, alerting.NewTenantRunner(pool, tenantID), func(ctx context.Context, tx pgx.Tx) error {
			run, ledgerMismatches, acquired, err := TryRunLedgerVsProjectionForTenant(ctx, tx, tenantID, periodStart, periodEnd)
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
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run",
				TargetType: "reconciliation_run", TargetID: run.ID.String(), Outcome: audit.OutcomeSuccess,
				Metadata: map[string]any{
					"stream": string(StreamLedgerVsProjection), "skipped_lock_contention": !acquired,
					"status": status,
				},
			}); err != nil {
				return err
			}
			// ADR 0102 row 3 (+ K2 unlinked adjustments, C-K2-1): durable P1,
			// savepoint-guarded, after the audit row, inside this run tx.
			return raiseLedgerRunAlerts(ctx, tx, tenantID, run, acquired, ledgerMismatches)
		})
		outcome.Err = txErr
		pending.Flush(ctx) // post-commit detached retry of any swallowed raise; nil-safe

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
			raiseRunFailed(ctx, pool, tenantID, string(StreamLedgerVsProjection), "", "run", txErr) // row 14
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

		// ADR 0088 §8: the sportsbook stream runs after
		// ledger_vs_projection, regardless of its outcome.
		outcome.Sportsbook = runSportsbookStreamForTenant(ctx, pool, logger, tenantID, periodStart, periodEnd, sbSource)
		// Stage 10.3 W2b (CAS-RECON-1): casino_consistency runs after
		// sportsbook_settlement, regardless of either earlier outcome.
		outcome.Casino = runCasinoStreamForTenant(ctx, pool, logger, tenantID, periodStart, periodEnd)
		// Stage 10.3 W3a (CAS-RECON-STMT-1): casino_statement runs after
		// casino_consistency, regardless of any earlier outcome.
		outcome.CasinoStatement = runCasinoStatementStreamForTenant(ctx, pool, logger, tenantID, periodStart, periodEnd, casSource)
		// PRH-I5 (ADR 0095 §12): payment_statement, once per registered
		// source, regardless of any earlier outcome.
		for _, src := range paySources {
			outcome.PaymentStatement = append(outcome.PaymentStatement,
				ReconcilePaymentStatementForTenant(ctx, pool, logger, tenantID, periodStart, periodEnd, src, PaymentStatementOptions{}))
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// runSportsbookStreamForTenant runs the sportsbook_settlement stream for
// one tenant against source (the MOCK statement source in production
// today), with exactly the audit/log
// discipline RunSweep applies to ledger_vs_projection: every attempt is
// audited, a failure is audited in a fresh transaction, a mismatch is
// logged at Error level. The statement source label (which says MOCK) is
// carried in the audit metadata and log fields (ADR 0088 §8.4).
func runSportsbookStreamForTenant(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, periodStart, periodEnd time.Time, source statement.SportsbookSettlementSource) StreamOutcome {
	var out StreamOutcome
	stream := string(StreamSportsbookSettlement)
	label := "<none>"
	if source != nil {
		label = source.Label()
	}

	pending, txErr := alerting.InTx(ctx, alerting.NewTenantRunner(pool, tenantID), func(ctx context.Context, tx pgx.Tx) error {
		run, sbMismatches, acquired, err := TryRunSportsbookSettlementForTenant(ctx, tx, tenantID, periodStart, periodEnd, source)
		out.Run, out.Skipped = run, !acquired
		if err != nil {
			return err
		}
		status := string(run.Status)
		if !acquired {
			status = "skipped"
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run",
			TargetType: "reconciliation_run", TargetID: run.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"stream": stream, "skipped_lock_contention": !acquired,
				"status": status, "statement_source": label,
			},
		}); err != nil {
			return err
		}
		// ADR 0102 row 4.
		return raiseInTxMismatch(ctx, tx, alerting.KindReconciliationSportsbookSettlement, tenantID, "stream:"+stream,
			run, acquired, len(sbMismatches), map[string]alerting.AttrValue{"statement_source": label})
	})
	out.Err = txErr
	pending.Flush(ctx)

	switch {
	case txErr != nil:
		if logger != nil {
			logger.Error("reconciliation sweep: tenant run failed", "tenant_id", tenantID, "stream", stream, "error", txErr)
		}
		if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run_failed",
				TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
				Metadata: map[string]any{"stream": stream, "error": txErr.Error()},
			})
		}); auditErr != nil && logger != nil {
			logger.Error("reconciliation sweep: failed to audit tenant failure", "tenant_id", tenantID, "stream", stream, "error", auditErr)
		}
		raiseRunFailed(ctx, pool, tenantID, stream, "", "run", txErr) // row 14
	case out.Run.Status == StatusMismatchesFound:
		if logger != nil {
			logger.Error("reconciliation sweep: MISMATCH FOUND", "tenant_id", tenantID, "stream", stream,
				"run_id", out.Run.ID, "statement_source", label)
		}
	case logger != nil:
		logger.Info("reconciliation sweep: tenant run complete", "tenant_id", tenantID, "stream", stream,
			"skipped_lock_contention", out.Skipped, "status", string(out.Run.Status), "statement_source", label)
	}
	return out
}

// runCasinoStreamForTenant runs the casino_consistency stream for one
// tenant with exactly runSportsbookStreamForTenant's audit/log discipline:
// every attempt (clean, mismatches found, lock-skipped) is audited with
// the run's metrics (ageing cash rounds, tombstones, rejections - metrics,
// never mismatches); a failure is audited in a fresh transaction; a
// mismatch is logged at Error level ("MISMATCH FOUND", P1). It never
// corrects anything and never touches a balance.
func runCasinoStreamForTenant(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, periodStart, periodEnd time.Time) StreamOutcome {
	var out StreamOutcome
	stream := string(StreamCasinoConsistency)
	var mismatchCount int

	pending, txErr := alerting.InTx(ctx, alerting.NewTenantRunner(pool, tenantID), func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, metrics, acquired, err := TryRunCasinoConsistencyForTenant(ctx, tx, tenantID, periodStart, periodEnd)
		out.Run, out.Skipped = run, !acquired
		mismatchCount = len(mismatches)
		if err != nil {
			return err
		}
		status := string(run.Status)
		if !acquired {
			status = "skipped"
		}
		metadata := map[string]any{
			"stream": stream, "skipped_lock_contention": !acquired, "status": status,
			"mismatches": len(mismatches),
		}
		if acquired {
			for k, v := range metrics.AuditMetadata() {
				metadata[k] = v
			}
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run",
			TargetType: "reconciliation_run", TargetID: run.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: metadata,
		}); err != nil {
			return err
		}
		// ADR 0102 row 5.
		return raiseInTxMismatch(ctx, tx, alerting.KindReconciliationCasinoConsistency, tenantID, "stream:"+stream,
			run, acquired, len(mismatches), nil)
	})
	out.Err = txErr
	pending.Flush(ctx)

	switch {
	case txErr != nil:
		if logger != nil {
			logger.Error("reconciliation sweep: tenant run failed", "tenant_id", tenantID, "stream", stream, "error", txErr)
		}
		defer raiseRunFailed(ctx, pool, tenantID, stream, "", "run", txErr) // row 14, after the failure audit below
		if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run_failed",
				TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
				Metadata: map[string]any{"stream": stream, "error": txErr.Error()},
			})
		}); auditErr != nil && logger != nil {
			logger.Error("reconciliation sweep: failed to audit tenant failure", "tenant_id", tenantID, "stream", stream, "error", auditErr)
		}
	case out.Run.Status == StatusMismatchesFound:
		// CLAUDE.md: any non-zero drift is a P1 incident.
		if logger != nil {
			logger.Error("reconciliation sweep: MISMATCH FOUND", "tenant_id", tenantID, "stream", stream,
				"run_id", out.Run.ID, "mismatches", mismatchCount)
		}
	case logger != nil:
		logger.Info("reconciliation sweep: tenant run complete", "tenant_id", tenantID, "stream", stream,
			"skipped_lock_contention", out.Skipped, "status", string(out.Run.Status))
	}
	return out
}

// runCasinoStatementStreamForTenant runs the casino_statement stream for
// one tenant against source with exactly runSportsbookStreamForTenant's
// audit/log discipline: every attempt (clean, mismatches found,
// lock-skipped) is audited with the statement source label (which says
// MOCK today) and the statement's shape; a failure (including a nil
// source) is audited in a fresh transaction; a mismatch is logged at Error
// level ("MISMATCH FOUND", P1) with the label. Its transaction is opened
// with WithTenantSnapshot (REPEATABLE READ) because the statement and the
// ledger are read in separate statements. It never corrects anything and
// never touches a balance.
func runCasinoStatementStreamForTenant(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, periodStart, periodEnd time.Time, source statement.CasinoStatementSource) StreamOutcome {
	var out StreamOutcome
	stream := string(StreamCasinoStatement)
	label := "<none>"
	if source != nil {
		label = source.Label()
	}
	var mismatchCount int

	txErr := pool.WithTenantSnapshot(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, info, acquired, err := TryRunCasinoStatementForTenant(ctx, tx, tenantID, periodStart, periodEnd, source)
		out.Run, out.Skipped = run, !acquired
		mismatchCount = len(mismatches)
		if err != nil {
			return err
		}
		status := string(run.Status)
		if !acquired {
			status = "skipped"
		}
		metadata := map[string]any{
			"stream": stream, "skipped_lock_contention": !acquired, "status": status,
			"mismatches": len(mismatches), "statement_source": label,
		}
		if acquired {
			for k, v := range info.AuditMetadata() {
				metadata[k] = v
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run",
			TargetType: "reconciliation_run", TargetID: run.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: metadata,
		})
	})
	out.Err = txErr

	switch {
	case txErr != nil:
		if logger != nil {
			logger.Error("reconciliation sweep: tenant run failed", "tenant_id", tenantID, "stream", stream,
				"error", txErr, "statement_source", label)
		}
		defer raiseRunFailed(ctx, pool, tenantID, stream, "", "run", txErr) // row 14, after the failure audit below
		if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run_failed",
				TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
				Metadata: map[string]any{"stream": stream, "error": txErr.Error(), "statement_source": label},
			})
		}); auditErr != nil && logger != nil {
			logger.Error("reconciliation sweep: failed to audit tenant failure", "tenant_id", tenantID, "stream", stream, "error", auditErr)
		}
	case out.Run.Status == StatusMismatchesFound:
		// CLAUDE.md: any non-zero drift is a P1 incident.
		if logger != nil {
			logger.Error("reconciliation sweep: MISMATCH FOUND", "tenant_id", tenantID, "stream", stream,
				"run_id", out.Run.ID, "mismatches", mismatchCount, "statement_source", label)
		}
		// ADR 0102 row 6 (REPEATABLE READ site): post-commit detached only.
		// The run and its mismatch rows are already committed; a persistent
		// raise failure cannot change that (LF test 6).
		raisePostCommitMismatch(ctx, pool, alerting.KindReconciliationCasinoStatement, tenantID, "stream:"+stream,
			out.Run, mismatchCount, map[string]alerting.AttrValue{"statement_source": label})
	case logger != nil:
		logger.Info("reconciliation sweep: tenant run complete", "tenant_id", tenantID, "stream", stream,
			"skipped_lock_contention", out.Skipped, "status", string(out.Run.Status), "statement_source", label)
	}
	return out
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
func RunSchedulerLoop(ctx context.Context, pool *db.Pool, logger *slog.Logger, interval time.Duration, sbSource statement.SportsbookSettlementSource, casSource statement.CasinoStatementSource, paySources ...statement.PaymentStatementSource) {
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
		if _, err := RunSweep(ctx, pool, logger, now.Add(-interval), now, sbSource, casSource, paySources...); err != nil && logger != nil {
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

// ReconcilePaymentStatementForTenant runs the payment_statement stream for
// one tenant and one source, in ADR 0095 §12.1's three phases, never one
// transaction:
//
//  1. fetch  - FetchPaymentStatement with NO transaction held (INV-IO-1);
//  2. ingest - IngestPaymentStatement in its own short WithTenant tx;
//  3. match  - TryRunPaymentStatementForTenant in its own WithTenantSnapshot
//     (REPEATABLE READ) tx, with the run's audit record in the same tx.
//
// It has runCasinoStatementStreamForTenant's audit/log discipline: every
// attempt (clean, mismatches found, lock-skipped) is audited with the
// source label, is_mock, import id, coverage window and line count; a
// failure in ANY phase (including a nil source, a refused oversized or
// invalid statement, or a fetch error) is a P1: audited in a fresh
// transaction with the phase named, and logged at Error level. A refused
// statement stores nothing. It never corrects anything and never touches
// a balance, the ledger, an attempt, an intent or a receipt.
func ReconcilePaymentStatementForTenant(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, periodStart, periodEnd time.Time, source statement.PaymentStatementSource, opts PaymentStatementOptions) StreamOutcome {
	var out StreamOutcome
	stream := string(StreamPaymentStatement)
	label, provider := "<none>", "<none>"
	if source != nil {
		label, provider = source.Label(), source.ProviderID()
	}
	fail := func(phase string, err error) StreamOutcome {
		out.Err = fmt.Errorf("reconciliation: payment_statement %s phase: %w", phase, err)
		if logger != nil {
			logger.Error("reconciliation sweep: tenant run failed (P1)", "tenant_id", tenantID, "stream", stream,
				"phase", phase, "error", out.Err, "statement_source", label, "provider_id", provider)
		}
		if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run_failed",
				TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
				Metadata: observationMetadata(opts, map[string]any{"stream": stream, "phase": phase, "error": out.Err.Error(),
					"statement_source": label, "provider_id": provider, "severity": "P1"}),
			})
		}); auditErr != nil && logger != nil {
			logger.Error("reconciliation sweep: failed to audit tenant failure", "tenant_id", tenantID, "stream", stream, "error", auditErr)
		}
		raiseRunFailed(ctx, pool, tenantID, stream, provider, phase, err) // ADR 0102 row 14
		return out
	}

	// Phase 1: fetch, no transaction held.
	stmt, err := FetchPaymentStatement(ctx, source, tenantID, periodStart, periodEnd, opts)
	if err != nil {
		return fail("fetch", err)
	}
	fetchedAt := time.Now().UTC()

	// Phase 2: ingest.
	var importID uuid.UUID
	var reused bool
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		importID, reused, err = IngestPaymentStatement(ctx, tx, tenantID, source, stmt, fetchedAt)
		return err
	}); err != nil {
		return fail("ingest", err)
	}

	// Phase 3: match.
	var mismatchCount int
	if err := pool.WithTenantSnapshot(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, info, acquired, err := TryRunPaymentStatementForTenant(ctx, tx, tenantID, importID, opts)
		out.Run, out.Skipped = run, !acquired
		mismatchCount = len(mismatches)
		if err != nil {
			return err
		}
		status := string(run.Status)
		if !acquired {
			status = "skipped"
		}
		info.ImportReused = reused
		metadata := map[string]any{
			"stream": stream, "skipped_lock_contention": !acquired, "status": status,
			"mismatches": len(mismatches), "statement_source": label, "provider_id": provider,
			"import_id": importID.String(),
		}
		if acquired {
			for k, v := range info.AuditMetadata() {
				metadata[k] = v
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.sweep_run",
			TargetType: "reconciliation_run", TargetID: run.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: observationMetadata(opts, metadata),
		})
	}); err != nil {
		out.Run = Run{}
		return fail("match", err)
	}

	switch {
	case out.Run.Status == StatusMismatchesFound:
		// CLAUDE.md: any non-zero drift is a P1 incident.
		if logger != nil {
			logger.Error("reconciliation sweep: MISMATCH FOUND", "tenant_id", tenantID, "stream", stream,
				"run_id", out.Run.ID, "mismatches", mismatchCount, "statement_source", label, "provider_id", provider)
		}
		// ADR 0102 row 7 (REPEATABLE READ site): post-commit detached only.
		raisePostCommitMismatch(ctx, pool, alerting.KindReconciliationPaymentStatement, tenantID,
			"stream:"+stream+":provider:"+provider, out.Run, mismatchCount,
			map[string]alerting.AttrValue{"statement_source": label, "import_id": importID.String()})
	case logger != nil:
		logger.Info("reconciliation sweep: tenant run complete", "tenant_id", tenantID, "stream", stream,
			"skipped_lock_contention", out.Skipped, "status", string(out.Run.Status), "statement_source", label, "provider_id", provider)
	}
	return out
}
