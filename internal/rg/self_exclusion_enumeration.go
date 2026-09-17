// Stage 4H-B0-R6, Workstream E: the per-self-exclusion-event completion
// record security finding S-9 requires (ADR 0034 §14.5,
// docs/security/security-architecture.md's Stage 4H-B0-R5 section).
//
// This file is honest about a real limit, per CLAUDE.md's "no fake
// completion": there is no sportsbook implementation yet, so nothing in
// this codebase can actually enumerate a Person's open bets or produce a
// real bets_in_scope_count. What is implemented here is the completion-
// record PRIMITIVE - a future sportsbook/rg listener (ADR 0034 §14.5's
// "listener on the self-exclusion commit event") records that it started,
// how many bets it found in scope, and how it finished, so a dropped
// event or a crashed listener leaves an explicit 'pending'/'failed' row
// rather than nothing at all. FindMissingEnumerationRuns is a
// reconciliation query built on top of that record - PARTIALLY
// IMPLEMENTED, see its own doc comment for exactly what it can and
// cannot detect today.
package rg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// DispatchStatus mirrors self_exclusion_enumeration_runs.dispatch_status.
type DispatchStatus string

const (
	DispatchPending    DispatchStatus = "pending"
	DispatchInProgress DispatchStatus = "in_progress"
	DispatchCompleted  DispatchStatus = "completed"
	DispatchFailed     DispatchStatus = "failed"
)

// EnumerationRun mirrors one self_exclusion_enumeration_runs row.
type EnumerationRun struct {
	ID               uuid.UUID
	RestrictionID    uuid.UUID
	TenantID         uuid.UUID
	PersonID         uuid.UUID
	PolicyAsOf       time.Time
	BetsInScopeCount int
	DispatchStatus   DispatchStatus
	StartedAt        *time.Time
	CompletedAt      *time.Time
	FailureReason    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const enumerationRunColumns = `id, restriction_id, tenant_id, person_id, policy_as_of, bets_in_scope_count,
	dispatch_status, started_at, completed_at, failure_reason, created_at, updated_at`

func scanEnumerationRun(row pgx.Row) (EnumerationRun, error) {
	var r EnumerationRun
	var status string
	var failureReason *string
	if err := row.Scan(
		&r.ID, &r.RestrictionID, &r.TenantID, &r.PersonID, &r.PolicyAsOf, &r.BetsInScopeCount,
		&status, &r.StartedAt, &r.CompletedAt, &failureReason, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return EnumerationRun{}, err
	}
	r.DispatchStatus = DispatchStatus(status)
	if failureReason != nil {
		r.FailureReason = *failureReason
	}
	return r, nil
}

// CreateEnumerationRunParams is CreateEnumerationRun's input. tx must be
// db.WithTenant(TenantID, ...)-scoped, matching migration 0043's
// tenant_isolation policy on self_exclusion_enumeration_runs.
type CreateEnumerationRunParams struct {
	RestrictionID uuid.UUID
	TenantID      uuid.UUID
	PersonID      uuid.UUID
	PolicyAsOf    time.Time
	ActorType     audit.ActorType
	ActorID       uuid.UUID
}

// CreateEnumerationRun records that enumeration/dispatch has BEGUN for
// (RestrictionID, TenantID) - status 'pending'. Idempotent: a retried
// call for the same (RestrictionID, TenantID) pair (migration 0043's
// UNIQUE constraint) returns the EXISTING row rather than erroring or
// creating a second, competing one - a dropped/retried listener
// invocation must never fork the completion record it is trying to
// produce.
func CreateEnumerationRun(ctx context.Context, tx pgx.Tx, params CreateEnumerationRunParams) (EnumerationRun, error) {
	if params.RestrictionID == uuid.Nil || params.TenantID == uuid.Nil || params.PersonID == uuid.Nil {
		return EnumerationRun{}, fmt.Errorf("%w: restriction_id, tenant_id, and person_id are required", ErrInvalidInput)
	}
	if params.PolicyAsOf.IsZero() {
		return EnumerationRun{}, fmt.Errorf("%w: policy_as_of is required", ErrInvalidInput)
	}

	id := uuid.New()
	tag, err := tx.Exec(ctx,
		`INSERT INTO self_exclusion_enumeration_runs (id, restriction_id, tenant_id, person_id, policy_as_of)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (restriction_id, tenant_id) DO NOTHING`,
		id, params.RestrictionID, params.TenantID, params.PersonID, params.PolicyAsOf,
	)
	if err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: insert enumeration run: %w", err)
	}

	lookupID := id
	created := tag.RowsAffected() > 0
	if !created {
		existing, err := scanEnumerationRun(tx.QueryRow(ctx,
			`SELECT `+enumerationRunColumns+` FROM self_exclusion_enumeration_runs WHERE restriction_id = $1 AND tenant_id = $2`,
			params.RestrictionID, params.TenantID,
		))
		if err != nil {
			return EnumerationRun{}, fmt.Errorf("rg: load existing enumeration run: %w", err)
		}
		return existing, nil
	}

	run, err := scanEnumerationRun(tx.QueryRow(ctx, `SELECT `+enumerationRunColumns+` FROM self_exclusion_enumeration_runs WHERE id = $1`, lookupID))
	if err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: reload enumeration run: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: params.ActorType, ActorID: params.ActorID,
		Action: "rg.self_exclusion_enumeration_run.created", TargetType: "self_exclusion_enumeration_run", TargetID: run.ID.String(),
		Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"restriction_id": params.RestrictionID.String(), "person_id": params.PersonID.String(),
			"policy_as_of": params.PolicyAsOf,
		},
	}); err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: audit enumeration run creation: %w", err)
	}
	return run, nil
}

// ErrEnumerationRunNotFound covers "no such run" / "not in the expected
// status for this transition."
var ErrEnumerationRunNotFound = errors.New("rg: enumeration run not found or not in the expected status")

// StartEnumerationRun transitions a 'pending' run to 'in_progress'.
func StartEnumerationRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID, actorType audit.ActorType, actorID uuid.UUID) (EnumerationRun, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE self_exclusion_enumeration_runs
		 SET dispatch_status = 'in_progress', started_at = clock_timestamp()
		 WHERE id = $1 AND dispatch_status = 'pending'`,
		runID,
	)
	if err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: start enumeration run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return EnumerationRun{}, ErrEnumerationRunNotFound
	}
	run, err := scanEnumerationRun(tx.QueryRow(ctx, `SELECT `+enumerationRunColumns+` FROM self_exclusion_enumeration_runs WHERE id = $1`, runID))
	if err != nil {
		return EnumerationRun{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: run.TenantID, ActorType: actorType, ActorID: actorID,
		Action: "rg.self_exclusion_enumeration_run.started", TargetType: "self_exclusion_enumeration_run", TargetID: run.ID.String(),
		Outcome: audit.OutcomeSuccess,
	}); err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: audit enumeration run start: %w", err)
	}
	return run, nil
}

// CompleteEnumerationRun marks a run 'completed' with the final
// bets-in-scope count - the S-9 completion proof. Callable directly from
// 'pending' (the common "found nothing in scope" fast path) or from
// 'in_progress'; started_at is backfilled to clock_timestamp() if not
// already set, satisfying migration 0043's own CHECK constraint (a
// non-pending row must carry a started_at).
func CompleteEnumerationRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID, betsInScopeCount int, actorType audit.ActorType, actorID uuid.UUID) (EnumerationRun, error) {
	if betsInScopeCount < 0 {
		return EnumerationRun{}, fmt.Errorf("%w: bets_in_scope_count must be non-negative", ErrInvalidInput)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE self_exclusion_enumeration_runs
		 SET dispatch_status = 'completed', bets_in_scope_count = $2,
		     started_at = COALESCE(started_at, clock_timestamp()), completed_at = clock_timestamp()
		 WHERE id = $1 AND dispatch_status IN ('pending', 'in_progress')`,
		runID, betsInScopeCount,
	)
	if err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: complete enumeration run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return EnumerationRun{}, ErrEnumerationRunNotFound
	}
	run, err := scanEnumerationRun(tx.QueryRow(ctx, `SELECT `+enumerationRunColumns+` FROM self_exclusion_enumeration_runs WHERE id = $1`, runID))
	if err != nil {
		return EnumerationRun{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: run.TenantID, ActorType: actorType, ActorID: actorID,
		Action: "rg.self_exclusion_enumeration_run.completed", TargetType: "self_exclusion_enumeration_run", TargetID: run.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: map[string]any{"bets_in_scope_count": betsInScopeCount},
	}); err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: audit enumeration run completion: %w", err)
	}
	return run, nil
}

// FailEnumerationRun marks a run 'failed' with a required reason -
// leaving an explicit, queryable failure record rather than the run
// simply never reaching 'completed'.
func FailEnumerationRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID, failureReason string, actorType audit.ActorType, actorID uuid.UUID) (EnumerationRun, error) {
	if failureReason == "" {
		return EnumerationRun{}, fmt.Errorf("%w: failure_reason is required", ErrInvalidInput)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE self_exclusion_enumeration_runs
		 SET dispatch_status = 'failed', failure_reason = $2
		 WHERE id = $1 AND dispatch_status IN ('pending', 'in_progress')`,
		runID, failureReason,
	)
	if err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: fail enumeration run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return EnumerationRun{}, ErrEnumerationRunNotFound
	}
	run, err := scanEnumerationRun(tx.QueryRow(ctx, `SELECT `+enumerationRunColumns+` FROM self_exclusion_enumeration_runs WHERE id = $1`, runID))
	if err != nil {
		return EnumerationRun{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: run.TenantID, ActorType: actorType, ActorID: actorID,
		Action: "rg.self_exclusion_enumeration_run.failed", TargetType: "self_exclusion_enumeration_run", TargetID: run.ID.String(),
		Outcome: audit.OutcomeFailure, Metadata: map[string]any{"failure_reason": failureReason},
	}); err != nil {
		return EnumerationRun{}, fmt.Errorf("rg: audit enumeration run failure: %w", err)
	}
	return run, nil
}

// EnumerationGap is one reconciliation finding from
// FindMissingEnumerationRuns.
type EnumerationGap struct {
	RestrictionID          uuid.UUID
	PersonID               uuid.UUID
	SelfExclusionEffective time.Time
}

// FindMissingEnumerationRuns is the reconciliation query security finding
// S-9 requires - PARTIALLY IMPLEMENTED, and this comment states exactly
// how. It detects: a currently-effective self-exclusion restriction
// affecting a person who holds a player_account in the CALLER's tenant
// (tx must be db.WithTenant(tenantID, ...)-scoped - self_exclusion_
// enumeration_runs and player_accounts are both tenant-isolated by RLS,
// so this check is necessarily run once per tenant, mirroring the
// platform's own isolation model rather than a fictional single global
// query) for which NO self_exclusion_enumeration_runs row exists at all
// for that (restriction, tenant) pair - i.e. enumeration for that tenant
// never even started, the "dropped event, no record at all" failure mode
// S-9 names.
//
// It deliberately does NOT, and cannot yet, detect the deeper case S-9
// also describes: a run that DID complete but whose reported bets_in_
// scope_count has no matching per-bet disposition record (ADR 0034
// §14.5's "one audit record per affected open bet"). That check requires
// a real sportsbook open-bet/per-bet-audit table, which does not exist
// in this codebase yet (this stage's directive explicitly forbids
// inventing one). Closing that gap is future sportsbook implementation
// work, tracked as an explicit follow-up, not silently implied to be
// covered here.
//
// Operational runbook (until a scheduled job calls this automatically):
// a compliance operator runs this once per active tenant on a recurring
// cadence (e.g. hourly, alongside the platform's other reconciliation
// jobs) and investigates any non-empty result as a possible dropped
// rg.status.changed event.
func FindMissingEnumerationRuns(ctx context.Context, tx pgx.Tx) ([]EnumerationGap, error) {
	rows, err := tx.Query(ctx,
		`SELECT pr.id, pr.person_id, pr.starts_at
		 FROM player_restrictions pr
		 JOIN player_accounts pa ON pa.person_id = pr.person_id
		 LEFT JOIN self_exclusion_enumeration_runs ser
		   ON ser.restriction_id = pr.id AND ser.tenant_id = pa.tenant_id
		 WHERE pr.restriction_type = 'self_exclusion'
		   AND pr.starts_at <= clock_timestamp()
		   AND (pr.ends_at IS NULL OR pr.ends_at > clock_timestamp())
		   AND ser.id IS NULL
		 ORDER BY pr.starts_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("rg: find missing enumeration runs: %w", err)
	}
	defer rows.Close()
	var out []EnumerationGap
	for rows.Next() {
		var g EnumerationGap
		if err := rows.Scan(&g.RestrictionID, &g.PersonID, &g.SelfExclusionEffective); err != nil {
			return nil, fmt.Errorf("rg: scan enumeration gap: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
