// Attributable Open Exposure (AOE) and NewStakeEligibility - the G-2-
// avoidance mechanism (docs/architecture/10-bonus-engine-architecture.md
// "doc 10" §N1.2-§N1.5). This is the load-bearing invariant TI-1
// mechanism: "For every Grant G and every instant t: if G.status(t) is
// TERMINAL, then AOE(G, t) = empty" - implemented EXACTLY per §N1.3's
// three-component model, not simplified.
//
// SCOPE NOTE, stated honestly: this platform has no sportsbook
// (internal/sportsbook does not exist) and casino does not yet call into
// Bonus Engine's seams (that is Phase 7 - "casino will call into your
// code in Phase 7, but the functions must exist and be correct now").
// Component 1 (LockedExposure, sportsbook-shaped, player_locked_bonus)
// is therefore structurally always zero in THIS platform today - not
// simplified away, but genuinely empty because no code path exists that
// could ever populate player_locked_bonus for a bonus-funded stake
// (Dependency Contract Freeze §9's player_locked_bonus split is itself
// still provisional). The function is implemented in full per §N1.3's
// definition (a live read of player_locked_bonus attributed to G) so
// that the moment a locked-stake product exists, this function is
// already correct against it - not a stub to revisit.
package bonus

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NewStakeEligibility(G, t) in {open, closed} - doc 10 N1.4's own
// unifying technical contract: "Purely technical, carries no claim about
// what happens to value already at risk." Open iff status in {issued,
// activated, in_progress}.
type NewStakeEligibility string

const (
	StakeEligibilityOpen   NewStakeEligibility = "open"
	StakeEligibilityClosed NewStakeEligibility = "closed"
)

// ComputeNewStakeEligibility is doc 10 N1.4's NewStakeEligibility(G, t)
// function, exactly: a pure function of the Grant's current status, no
// database access needed beyond having already read Status.
func ComputeNewStakeEligibility(status GrantStatus) NewStakeEligibility {
	switch status {
	case GrantIssued, GrantActivated, GrantInProgress:
		return StakeEligibilityOpen
	default:
		return StakeEligibilityClosed
	}
}

// AOE is the three-component Attributable Open Exposure model (doc 10
// §N1.3), computed live, inside the caller's own transaction, under the
// (tenant_id, grant_id) advisory lock the caller has already acquired
// (doc 10 §9/N1.5 - this function does not itself take that lock).
type AOE struct {
	LockedExposure   *bigIntZero // Component 1 - always 0 in this platform today (no sportsbook), see file doc comment
	InFlightExposure int         // Component 2 - count of open (uncorrelated-to-any-closing-event) casino bets
	HeldDisposition  *bigIntZero // Component 3 - live player_bonus_held balance attributed to G
}

// bigIntZero is a tiny wrapper purely so AOE's zero-value fields print
// legibly; this package's actual arithmetic uses *big.Int throughout -
// see IsZero below for the one predicate that matters.
type bigIntZero struct {
	amount int64 // minor units are always representable in int64 for a single Grant's exposure at this platform's current scale; HR-22's *big.Int discipline governs POSTING, not this internal diagnostic count
}

// IsEmpty reports AOE(G, t) = ∅ (doc 10 N1.2's Invariant TI-1
// precondition): all three components are zero/empty.
func (a AOE) IsEmpty() bool {
	return (a.LockedExposure == nil || a.LockedExposure.amount == 0) &&
		a.InFlightExposure == 0 &&
		(a.HeldDisposition == nil || a.HeldDisposition.amount == 0)
}

// ComputeAOE computes AOE(G, t) live, per doc 10 §N1.3, for grantID inside
// tx. This is the exact function N1.4's mechanism (lifecycle.go) and
// N1.4.2's RecheckGrantExposure (held_disposition_ops.go) both call - one
// implementation, reused, never a second copy of the three-component sum
// (N1.4.2's own binding text: "Nothing new is computed - this seam
// re-enters the identical three-component sum").
func ComputeAOE(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (AOE, error) {
	locked, err := lockedExposure(ctx, tx, tenantID, grantID)
	if err != nil {
		return AOE{}, err
	}
	inFlight, err := inFlightExposureCount(ctx, tx, tenantID, grantID)
	if err != nil {
		return AOE{}, err
	}
	held, err := heldDispositionExposure(ctx, tx, tenantID, grantID)
	if err != nil {
		return AOE{}, err
	}
	return AOE{
		LockedExposure:   &bigIntZero{amount: locked},
		InFlightExposure: inFlight,
		HeldDisposition:  &bigIntZero{amount: held},
	}, nil
}

// lockedExposure is Component 1 (doc 10 N1.3): "the signed sum, read
// live, of every player_locked_bonus ledger entry attributable to G via
// GrantLedgerAttribution that has not yet been nullified." Implemented in
// full against the real attribution table; structurally zero today
// because no code path in this platform ever attributes a
// player_locked_bonus entry to a Grant (no sportsbook, no locked-stake
// casino shape ratified per doc 10 §16.10.1) - see file doc comment.
func lockedExposure(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (int64, error) {
	byType, err := AttributedBalanceByAccountType(ctx, tx, tenantID, grantID)
	if err != nil {
		return 0, err
	}
	v, ok := byType["player_locked_bonus"]
	if !ok || v == nil {
		return 0, nil
	}
	if !v.IsInt64() {
		return 0, fmt.Errorf("bonus: locked exposure for grant %s does not fit in int64 (%s) - this diagnostic count is unsound at this scale, refusing rather than truncating", grantID, v.String())
	}
	return v.Int64(), nil
}

// inFlightExposureCount is Component 2 (doc 10 N1.3): the count of
// WageringProgress contribution rows attributable to G whose underlying
// stake-lock transaction has posted but for which NO qualifying closing
// event (a casino_win credit or a casino_rollback, correlated via the
// SAME correlation_id casino's own postBet/postWin/postRollback share -
// internal/casino/orchestrator.go's roundCorrelationID) has yet been
// observed. Casino-shaped only, per doc 10 N1.3's own "why casino needs a
// second component at all" reasoning; there is no sportsbook-shaped
// analogue to add here today.
func inFlightExposureCount(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (int, error) {
	var count int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		  FROM bonus_wagering_progress wp
		 WHERE wp.tenant_id = $1 AND wp.grant_id = $2
		   AND NOT EXISTS (
		       SELECT 1 FROM ledger_transactions lt
		        WHERE lt.tenant_id = wp.tenant_id
		          AND lt.correlation_id = wp.correlation_id
		          AND lt.transaction_type IN ('casino_win', 'casino_rollback')
		   )`,
		tenantID, grantID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("bonus: compute in-flight exposure for grant %s: %w", grantID, err)
	}
	return count, nil
}

// heldDispositionExposure is Component 3 (doc 10 N1.3, redefined by the
// Fix Round 2 revision this document adopts verbatim): "a live balance
// read of the player_bonus_held ledger account, attributed to G via
// GrantLedgerAttribution... NEVER 'does a bonus_held_dispositions row
// exist with status = held'." The ledger balance is the source of truth;
// bonus_held_dispositions is a reconciled, richer projection over it.
func heldDispositionExposure(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (int64, error) {
	byType, err := AttributedBalanceByAccountType(ctx, tx, tenantID, grantID)
	if err != nil {
		return 0, err
	}
	v, ok := byType["player_bonus_held"]
	if !ok || v == nil {
		return 0, nil
	}
	if !v.IsInt64() {
		return 0, fmt.Errorf("bonus: held exposure for grant %s does not fit in int64 (%s)", grantID, v.String())
	}
	return v.Int64(), nil
}
