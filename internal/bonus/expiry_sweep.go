// Grant-expiry sweep (Stage 4H-B1 Wave 3 Phase 3, docs/governance/
// wave-3-reconnaissance.md gap-list item 7's own dependency-map entry:
// "Grant-level expiry (a field to expire against + a sweep)"). Migration
// 0070 (Phase 2, backend) added the field; commit 94257ec (Phase 3,
// recovered) wired ActivateGrant to populate it. This file is the
// missing sweep: finds every open Grant past its own expires_at and
// terminates it via the EXISTING TerminateGrant machinery - never a
// parallel termination path, per the dispatch's own explicit
// instruction.
package bonus

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExpirySweepReasonCode is the ReasonCode every expiry termination this
// sweep performs carries - a single, stable, machine-parseable string a
// disputing player's Progress trail (doc 10 §10's own completeness
// mandate) shows verbatim as the reason their Grant was forfeited/
// expired, never a free-form message that could drift between calls.
const ExpirySweepReasonCode = "wagering_time_limit_elapsed"

// ExpirySweepOutcome summarizes one RunExpirySweepForTenant call.
type ExpirySweepOutcome struct {
	GrantsExamined   int
	GrantsTerminated int
	GrantsDeferred   int // TerminateGrant returned pending_settlement (open exposure) rather than a direct terminal status
}

// RunExpirySweepForTenant finds every Grant for tenantID whose
// expires_at has passed asOf (asOf is the caller's own clock_timestamp()
// read - doc 10 §2's already-binding "never now()" rule, kept a pure
// input here for testability) and terminates it via TerminateGrant with
// TerminalResolutionExpired - the EXISTING mechanism (lifecycle.go),
// which itself already handles the AOE-non-empty case correctly
// (deferring to pending_settlement rather than a direct terminal write,
// N1.4) with NO changes needed here. Every terminated Grant's own
// Progress trail already records WHY (TerminateGrant's own
// AppendGrantProgress call, carrying this file's ExpirySweepReasonCode)
// - satisfying doc 10 §10's "sufficient to show a disputing player
// exactly why a bonus was forfeited" mandate without any new machinery.
//
// This function runs every Grant's own termination inside the SAME
// transaction (tx, the caller's own tenant-scoped transaction, mirroring
// RunDepositSweepForTenant/RunCashbackSchedulerForTenant's identical
// shape) - a single Grant's termination failing (a transient DB error, a
// genuinely illegal-transition race against a concurrent conversion)
// therefore aborts this tenant's WHOLE tick, rolling back every
// termination this tick already performed. This is safe, never lossy:
// ListExpirableGrants' own status filter means an already-terminated
// Grant (from a PRIOR successful tick) is never re-selected, so the next
// tick simply retries the entire remaining backlog from scratch - never
// a partial, silently-abandoned termination.
func RunExpirySweepForTenant(ctx context.Context, tx pgx.Tx, tenantID, systemActorID uuid.UUID, asOf time.Time) (ExpirySweepOutcome, error) {
	var outcome ExpirySweepOutcome
	grants, err := ListExpirableGrants(ctx, tx, tenantID, asOf)
	if err != nil {
		return outcome, err
	}

	for _, g := range grants {
		outcome.GrantsExamined++
		terminated, err := TerminateGrant(ctx, tx, tenantID, g.ID, TerminateGrantParams{
			Resolution: TerminalResolutionExpired, ReasonCode: ExpirySweepReasonCode,
			ActorType: ActorSystem, ActorID: systemActorID, TriggerType: TriggerAutomatedRuleEvaluation,
		})
		if err != nil {
			return outcome, fmt.Errorf("bonus: expiry sweep: terminate grant %s: %w", g.ID, err)
		}
		if terminated.Status == GrantPendingSettlement {
			outcome.GrantsDeferred++
		} else {
			outcome.GrantsTerminated++
		}
	}
	return outcome, nil
}
