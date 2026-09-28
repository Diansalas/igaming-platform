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

// runCleanExceptUnlinkedFixtures reports whether run's only mismatches (if
// any) are ledger_unlinked_manual_adjustment rows.
func runCleanExceptUnlinkedFixtures(t *testing.T, pool *db.Pool, tenantID uuid.UUID, run Run) bool {
	t.Helper()
	if run.Status == StatusClean {
		return true
	}
	var other int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches
			WHERE reconciliation_run_id = $1 AND mismatch_kind <> 'ledger_unlinked_manual_adjustment'`, run.ID).Scan(&other)
	}); err != nil {
		t.Fatalf("read run mismatches: %v", err)
	}
	return other == 0
}
