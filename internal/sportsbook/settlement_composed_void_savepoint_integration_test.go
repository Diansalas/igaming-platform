//go:build integration

// SB-T1-XMIN, service level (docs/plans/stage-10.1-planning-gate-proposal.md
// §J, test #17): the DB-level trigger tests in
// settlement_db_constraints_integration_test.go prove T-1's raw-SQL
// behavior directly. This file proves the fix actually unblocks a REAL
// driver shape: a caller that wraps the entire
// SimulateSettlementEvent(void-after-settlement) call in its own
// SAVEPOINT (a test-only wrapper, opened via pgx's tx.Begin(ctx) on an
// already-open db.Pool.WithTenant transaction - exactly how a future
// batch driver settling many bets per outer transaction, one SAVEPOINT
// per bet, would call it). insertSettlementRecord itself never opens a
// savepoint (settlement.go's doc comment); this test's savepoint is
// opened by the CALLER, around the whole service call, which is the
// documented future-risk shape ADR 0088 §3.3 and the SB-T1-XMIN analysis
// name.
//
// REQUIRED PRE-FIX EVIDENCE (docs/plans/stage-10.1-planning-gate-proposal.md
// §J item 17): this test was run, and shown FAILING, against the schema at
// migration 0091 (before migration 0093 existed) - see
// docs/plans/stage-10.1-planning/evidence/sb-t1-xmin-prefix-failure.txt.
package sportsbook

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestSettlementScenario_ComposedVoid_InsideOuterSavepoint_ServiceLevel
// settles a bet, then drives the void-after-settlement composed-void event
// through SimulateSettlementEvent INSIDE an outer SAVEPOINT (opened on the
// same top-level db.Pool.WithTenant transaction, released before the outer
// transaction commits). Before migration 0093, this fails end-to-end (the
// rollback leg's history row is inserted under the savepoint, so its xmin
// is the subtransaction's own xid, which T-1 rejects). After 0093, it
// must succeed end-to-end, exactly as the raw-SQL-level
// TestDBConstraints_T1_ComposedVoidCausation_SavepointRollbackIsAccepted
// proves at the trigger level - this test proves it at the real caller
// shape a future savepoint-taking driver would actually use.
func TestSettlementScenario_ComposedVoid_InsideOuterSavepoint_ServiceLevel(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	ev := voidEvent(betID, actor, "data_error")
	ev.TenantID = f.tenantID

	var res SettlementResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Test-only wrapper: opens a SAVEPOINT (pgx.Tx.Begin issues
		// SAVEPOINT/RELEASE SAVEPOINT/ROLLBACK TO SAVEPOINT when called on
		// an already-open transaction) around the WHOLE service call, not
		// just the history insert - the real driver shape this test
		// exists to prove, per the analysis's §5 recommendation.
		spTx, err := tx.Begin(ctx)
		if err != nil {
			return fmt.Errorf("open outer savepoint: %w", err)
		}
		res, err = SimulateSettlementEvent(ctx, spTx, ev)
		if err != nil {
			_ = spTx.Rollback(ctx)
			return fmt.Errorf("simulate composed void inside savepoint: %w", err)
		}
		if res.Rejected() {
			_ = spTx.Rollback(ctx)
			return fmt.Errorf("simulate composed void inside savepoint: rejected %s (alert=%v)", res.RejectionCode, res.Alert)
		}
		return spTx.Commit(ctx)
	})
	if err != nil {
		t.Fatalf("SB-T1-XMIN: expected a composed void driven inside an outer SAVEPOINT to succeed "+
			"end-to-end (migration 0093), got: %v", err)
	}
	if res.BetStatus != BetStatusVoid {
		t.Fatalf("bet status = %q, want void", res.BetStatus)
	}
	assertNets(t, pool, f, 0, 0, 0)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 3 {
		t.Fatalf("expected 3 history rows (settlement, rollback, void), got %d", len(hist))
	}
	rollback, void := hist[1], hist[2]
	if rollback.EventKind != settlementHistoryKindRollback {
		t.Fatalf("unexpected rollback row: %+v", rollback)
	}
	if void.EventKind != settlementHistoryKindVoid || void.CausationRecordID == nil || *void.CausationRecordID != rollback.ID {
		t.Fatalf("composed void must cite the savepoint-inserted rollback row: %+v", void)
	}
}
