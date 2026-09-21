//go:build integration

// Stage 9 (Production Readiness), migration 0082 sections 1.3 and 1.4.
//
// These two tables are the evidence CLAUDE.md's "reconciliation-capable,
// auditable" claim and its "any non-zero drift is a P1 incident" rule
// rest on. A reconciliation result that can be edited after the fact is
// not evidence - a clean/mismatches_found verdict, the period it covers,
// and the expected-vs-actual pair are the entire content of the claim
// "we checked, and here is what we found". Before migration 0082 neither
// table had any immutability trigger or TRUNCATE deny, and both have a
// FOR ALL RLS policy, so an ordinary tenant-scoped UPDATE could turn a
// recorded P1 into something that looks like it never happened.
//
// The two tables get deliberately DIFFERENT guards, and both halves are
// tested: reconciliation_runs is totally append-only (no code path ever
// updates one), while reconciliation_mismatches freezes only the drift
// finding itself and leaves the investigation/resolution columns writable
// so ResolveMismatch keeps working.
package reconciliation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedDriftedRun produces one reconciliation_runs row and at least one
// reconciliation_mismatches row by running the real sweep against a
// fixture whose projection has been made to disagree with the ledger.
func seedDriftedRun(t *testing.T, pool *db.Pool, f fixture) (runID, mismatchID uuid.UUID) {
	t.Helper()

	// Corrupt the PROJECTION only, never the ledger, so the sweep has
	// genuine drift to find - the same injection this package's existing
	// TestRunLedgerVsProjection_DetectsInjectedDrift uses. The projection
	// is an explicitly rebuildable cache (migration 0023), so writing to
	// it in a test is not a ledger mutation.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE wallet_balance_projection SET credit_total = credit_total + 500 WHERE ledger_account_id = $1`,
			f.cashAccountID)
		return err
	}); err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, err := RunLedgerVsProjection(ctx, tx, f.tenantID,
			time.Now().Add(-time.Hour), time.Now())
		if err != nil {
			return err
		}
		if len(mismatches) == 0 {
			t.Fatal("expected the seeded drift to produce at least one mismatch row")
		}
		runID, mismatchID = run.ID, mismatches[0].ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed drifted reconciliation run: %v", err)
	}
	return runID, mismatchID
}

// TestMigration0082_ReconciliationRunsAppendOnly: a run is a point-in-time
// verdict. Rewriting its status from 'mismatches_found' to 'clean', or
// moving the period it claims to cover, would let a P1 be made to look
// like it never happened - which is worse than the drift itself, because
// the drift is at least detectable.
func TestMigration0082_ReconciliationRunsAppendOnly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	runID, _ := seedDriftedRun(t, pool, f)

	for _, stmt := range []string{
		`UPDATE reconciliation_runs SET status = 'clean' WHERE id = $1`,
		`UPDATE reconciliation_runs SET period_start = now() - interval '10 years' WHERE id = $1`,
		`UPDATE reconciliation_runs SET stream = 'something_else' WHERE id = $1`,
		`UPDATE reconciliation_runs SET run_at = now() - interval '10 years' WHERE id = $1`,
		`DELETE FROM reconciliation_runs WHERE id = $1`,
	} {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, runID)
			return err
		})
		if err == nil {
			t.Fatalf("expected reconciliation_runs_immutable to reject %q, got nil error", stmt)
		}
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("expected ledger_deny_mutation's own append-only message for %q, got: %v", stmt, err)
		}
	}

	// The verdict survived intact.
	var status string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM reconciliation_runs WHERE id = $1`, runID).Scan(&status)
	}); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if status != string(StatusMismatchesFound) {
		t.Fatalf("expected the recorded verdict to survive, got status %q", status)
	}
}

// TestMigration0082_ReconciliationMismatchesEvidenceFrozen pins the drift
// finding itself: which run found it, the key it was found under, what
// was expected, what was actually there, what kind of mismatch it is, and
// when. Those are the P1.
func TestMigration0082_ReconciliationMismatchesEvidenceFrozen(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	_, mismatchID := seedDriftedRun(t, pool, f)

	cases := []struct {
		name string
		set  string
		arg  any
	}{
		{"expected_value", `expected_value = '0'`, nil},
		{"actual_value", `actual_value = '0'`, nil},
		{"reconciliation_key", `reconciliation_key = 'some-other-account'`, nil},
		{"mismatch_kind", `mismatch_kind = 'missing_projection'`, nil},
		{"reconciliation_run_id", `reconciliation_run_id = $2`, uuid.New()},
		{"tenant_id", `tenant_id = $2`, uuid.New()},
		{"created_at", `created_at = now() - interval '1 year'`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				sql := `UPDATE reconciliation_mismatches SET ` + tc.set + ` WHERE id = $1`
				if tc.arg != nil {
					_, err := tx.Exec(ctx, sql, mismatchID, tc.arg)
					return err
				}
				_, err := tx.Exec(ctx, sql, mismatchID)
				return err
			})
			if err == nil {
				t.Fatalf("expected reconciliation_mismatches_immutable_fields to reject %q, got nil error", tc.set)
			}
			if !strings.Contains(err.Error(), "immutable after insert") {
				t.Fatalf("expected the trigger's own immutability message, got: %v", err)
			}
		})
	}
}

// TestMigration0082_ReconciliationMismatchesStillResolvable is the other
// half: the guard is column-level precisely because ResolveMismatch is a
// legitimate, required write path. A total append-only guard here would
// have made every recorded drift permanently un-investigable, so the
// allowance is asserted through the real API, not a hand-written UPDATE.
func TestMigration0082_ReconciliationMismatchesStillResolvable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	_, mismatchID := seedDriftedRun(t, pool, f)

	resolver := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ResolveMismatch(ctx, tx, mismatchID, resolver, "investigated; projection rebuilt", nil)
	}); err != nil {
		t.Fatalf("expected ResolveMismatch to remain permitted after migration 0082, got: %v", err)
	}

	var status, note string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT investigation_status, resolution_note FROM reconciliation_mismatches WHERE id = $1`, mismatchID,
		).Scan(&status, &note)
	}); err != nil {
		t.Fatalf("re-read mismatch: %v", err)
	}
	if status != "resolved" || note == "" {
		t.Fatalf("expected the resolution to be recorded, got status=%q note=%q", status, note)
	}
}

func TestMigration0082_ReconciliationDenyTruncate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	seedDriftedRun(t, pool, f)

	for _, table := range []string{"reconciliation_mismatches", "reconciliation_runs"} {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE `+table+` CASCADE`)
			return err
		})
		if err == nil {
			t.Fatalf("expected the deny-truncate trigger on %s to reject TRUNCATE, got nil error", table)
		}
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("expected ledger_deny_mutation's own append-only message for %s, got: %v", table, err)
		}
	}
}
