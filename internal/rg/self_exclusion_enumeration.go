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
// rather than nothing at all. FindMissingEnumerationRuns and
// FindStalledEnumerationRuns are the two reconciliation queries built on
// top of that record - together PARTIALLY IMPLEMENTED, covering three
// named failure modes (never started / wrongly-scoped caller / stalled);
// see each function's own doc comment for exactly what it can and cannot
// detect today.
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

// Sentinel errors for the reconciliation-scope assertion below - mirrors
// internal/risk/evaluator.go's ErrPlayerScopedConnection/
// ErrTenantScopeMismatch naming and reasoning exactly (Stage 4H-B0-R6 fix
// 3(a) deliberately reuses that pattern rather than inventing a third
// one).
var (
	// ErrPlayerScopedConnection is returned when the transaction passed to
	// a reconciliation function has app.player_account_id set - it must
	// not, since self_exclusion_enumeration_runs and player_restrictions
	// carry no legitimate player-facing access path.
	ErrPlayerScopedConnection = errors.New("rg: transaction is player-scoped, not tenant-scoped")
	// ErrTenantScopeMismatch is returned when the transaction's own
	// app.tenant_id GUC is unset, or set to a different tenant than the
	// caller explicitly asked to reconcile.
	ErrTenantScopeMismatch = errors.New("rg: transaction tenant scope does not match the requested tenant")
)

// verifyReconciliationScope proves tx is scoped exactly the way
// FindMissingEnumerationRuns/FindStalledEnumerationRuns' own documented
// precondition requires (db.WithTenant(tenantID, ...)) BEFORE either
// query runs, so a wrongly-scoped transaction fails loudly instead of
// silently returning an empty gap list - code-reviewer's Stage 4H-B0-R6
// finding: this is the exact fail-open shape
// internal/risk/evaluator.go's verifyConnectionScope was built to close
// this same stage, mirrored here rather than promoted to shared
// infrastructure (that would be an architect-owned change per
// docs/governance/change-control.md, same reasoning risk's own doc
// comment gives for keeping its copy package-local).
func verifyReconciliationScope(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	var scopedTenant, scopedPlayer *string
	err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), ''), NULLIF(current_setting('app.player_account_id', true), '')`,
	).Scan(&scopedTenant, &scopedPlayer)
	if err != nil {
		return fmt.Errorf("rg: read connection scope: %w", err)
	}
	if scopedPlayer != nil {
		return fmt.Errorf("%w: app.player_account_id is set", ErrPlayerScopedConnection)
	}
	if scopedTenant == nil {
		return fmt.Errorf("%w: app.tenant_id is not set on this transaction", ErrTenantScopeMismatch)
	}
	parsed, err := uuid.Parse(*scopedTenant)
	if err != nil {
		return fmt.Errorf("%w: app.tenant_id %q is not a uuid", ErrTenantScopeMismatch, *scopedTenant)
	}
	if parsed != tenantID {
		return fmt.Errorf("%w: transaction is scoped to %s, requested reconciliation is for %s", ErrTenantScopeMismatch, parsed, tenantID)
	}
	return nil
}

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
// which THREE failure modes it covers and which it does not (Stage
// 4H-B0-R6 fix 3 broadened this from an earlier two-case framing that
// read as exhaustive when it wasn't):
//
//  1. Never started: a currently-effective self-exclusion restriction
//     affecting a person who holds a player_account in the CALLER's
//     tenant for which NO self_exclusion_enumeration_runs row exists at
//     all for that (restriction, tenant) pair - a dropped event, no
//     record whatsoever. This is what the query below actually detects.
//  2. Wrongly-scoped caller (fix 3(a), code-reviewer finding): tx MUST be
//     db.WithTenant(tenantID, ...)-scoped, matching self_exclusion_
//     enumeration_runs' and player_accounts' own RLS - self_exclusion_
//     enumeration_runs and player_accounts are both tenant-isolated, so
//     this check is necessarily run once per tenant, mirroring the
//     platform's own isolation model rather than a fictional single
//     global query. Before this fix, a wrongly-scoped transaction (no
//     tenant set, or player-scoped) satisfied RLS by returning zero rows
//     from the join and this function returned (nil, nil) - an empty gap
//     list indistinguishable from "genuinely nothing is missing." tenantID
//     is now a required parameter and verifyReconciliationScope asserts
//     the transaction is actually scoped to it before the query runs, the
//     same fail-loud pattern internal/risk/evaluator.go's
//     verifyConnectionScope established this same stage.
//  3. Stalled (fix 3(b), security finding): a run that WAS created
//     (`pending`) and then never progressed - crashed before
//     StartEnumerationRun, or stuck `in_progress`, or `failed` - all
//     satisfy "a row exists" and are invisible to THIS function, even
//     though migration 0043's partial index
//     (idx_self_exclusion_enumeration_runs_incomplete) exists
//     specifically to make them cheap to find. See the sibling function
//     FindStalledEnumerationRuns below, which this fix adds specifically
//     to close this case - this function alone does not cover it.
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
func FindMissingEnumerationRuns(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]EnumerationGap, error) {
	if err := verifyReconciliationScope(ctx, tx, tenantID); err != nil {
		return nil, err
	}
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

// StalledEnumerationRun is one reconciliation finding from
// FindStalledEnumerationRuns - a run row that exists but has not reached
// 'completed' within the caller-supplied age threshold.
type StalledEnumerationRun struct {
	ID             uuid.UUID
	RestrictionID  uuid.UUID
	PersonID       uuid.UUID
	DispatchStatus DispatchStatus
	CreatedAt      time.Time
	StartedAt      *time.Time
	FailureReason  string
}

// FindStalledEnumerationRuns is fix 3(b)'s reconciliation query: it
// reports every self_exclusion_enumeration_runs row for tenantID whose
// dispatch_status is NOT 'completed' and whose created_at is older than
// olderThan - a listener that crashed before StartEnumerationRun (stuck
// 'pending'), one that hung mid-run (stuck 'in_progress'), or one that
// reached 'failed' and was never retried. All three "a row exists but
// enumeration never actually finished" cases are invisible to
// FindMissingEnumerationRuns above, which only detects "no row at all."
//
// olderThan is deliberately a caller-supplied parameter, never a
// hardcoded duration: how long a run may legitimately sit 'pending'/
// 'in_progress' before it counts as stalled is an operational/business
// tuning question (how fast the eventual listener is expected to run),
// not a fact this package can assert on its own. A caller with no
// specific requirement yet can pass a conservative default (e.g. 15
// minutes) at the call site, not baked in here.
//
// Uses migration 0043's idx_self_exclusion_enumeration_runs_incomplete
// partial index (WHERE dispatch_status <> 'completed') - the index this
// stage's security review found had zero Go readers before this fix.
//
// Same scope precondition as FindMissingEnumerationRuns: tx must be
// db.WithTenant(tenantID, ...)-scoped, asserted the identical way.
func FindStalledEnumerationRuns(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, olderThan time.Duration) ([]StalledEnumerationRun, error) {
	if err := verifyReconciliationScope(ctx, tx, tenantID); err != nil {
		return nil, err
	}
	if olderThan <= 0 {
		return nil, fmt.Errorf("%w: older_than must be a positive duration", ErrInvalidInput)
	}
	rows, err := tx.Query(ctx,
		`SELECT id, restriction_id, person_id, dispatch_status, created_at, started_at, failure_reason
		 FROM self_exclusion_enumeration_runs
		 WHERE tenant_id = $1
		   AND dispatch_status <> 'completed'
		   AND created_at < clock_timestamp() - $2::interval
		 ORDER BY created_at`,
		tenantID, fmt.Sprintf("%d seconds", int64(olderThan.Seconds())),
	)
	if err != nil {
		return nil, fmt.Errorf("rg: find stalled enumeration runs: %w", err)
	}
	defer rows.Close()
	var out []StalledEnumerationRun
	for rows.Next() {
		var s StalledEnumerationRun
		var status string
		var failureReason *string
		if err := rows.Scan(&s.ID, &s.RestrictionID, &s.PersonID, &status, &s.CreatedAt, &s.StartedAt, &failureReason); err != nil {
			return nil, fmt.Errorf("rg: scan stalled enumeration run: %w", err)
		}
		s.DispatchStatus = DispatchStatus(status)
		if failureReason != nil {
			s.FailureReason = *failureReason
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
