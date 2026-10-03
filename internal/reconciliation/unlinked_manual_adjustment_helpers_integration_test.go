//go:build integration

package reconciliation

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// PRH-2 K2 (ADR 0100 §12): several pre-K2 fixtures in this package fund
// their worlds with a manual_adjustment posted DIRECTLY through
// ledger.Post, outside any ledger_adjustment_requests row. After migration
// 0113's governed_since cutover the ledger_vs_projection run now - correctly
// - raises ledger_unlinked_manual_adjustment for each of them. Moving those
// fixtures to request-backed helpers is LEDGER-MANUAL-ADJ-LINK-1's
// deliverable (ADR 0100 §12, "the 19 test files"), not K2's; until then the
// "ledger_vs_projection must be clean" assertions below accept EXACTLY that
// kind and nothing else, so a real balance drift still fails them.

// ignoringUnlinkedFixtureAdjustments drops ledger_unlinked_manual_adjustment
// mismatches (fixture funding) and returns the rest.
func ignoringUnlinkedFixtureAdjustments(ms []Mismatch) []Mismatch {
	var out []Mismatch
	for _, m := range ms {
		if m.MismatchKind != MismatchKindLedgerUnlinkedManualAdjustment {
			out = append(out, m)
		}
	}
	return out
}

// cleanExceptUnlinkedFixtures decides IN MEMORY, from the run and the
// mismatches the same call returned (code review R-4: no re-read in a new
// transaction, which could pass vacuously): a clean run must carry no
// mismatches; a non-clean run must carry at least one, every one of them
// belonging to this run and of kind ledger_unlinked_manual_adjustment.
func cleanExceptUnlinkedFixtures(run Run, ms []Mismatch) bool {
	if run.ID == uuid.Nil {
		return false
	}
	if run.Status == StatusClean {
		return len(ms) == 0
	}
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if m.RunID != run.ID || m.MismatchKind != MismatchKindLedgerUnlinkedManualAdjustment {
			return false
		}
	}
	return true
}

// persistedCleanExceptUnlinkedFixtures is for callers that only hold the
// Run (RunSweepTenants returns no mismatches). It reads EVERY persisted
// mismatch row of the run and then applies the same in-memory rule, so a
// non-clean run whose rows the read cannot see fails (never vacuous).
func persistedCleanExceptUnlinkedFixtures(t *testing.T, pool *db.Pool, tenantID uuid.UUID, run Run) bool {
	t.Helper()
	var ms []Mismatch
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT reconciliation_run_id, mismatch_kind FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, run.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Mismatch
			var kind string
			if err := rows.Scan(&m.RunID, &kind); err != nil {
				return err
			}
			m.MismatchKind = MismatchKind(kind)
			ms = append(ms, m)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read run mismatches: %v", err)
	}
	return cleanExceptUnlinkedFixtures(run, ms)
}
