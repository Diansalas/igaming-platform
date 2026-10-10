//go:build integration

// ADR 0112 slice 2 ledger-finance condition: RunLedgerVsProjection must treat a
// NEVER-POSTED account (no ledger_entries, zero totals, NO projection row) as clean,
// consistent with its own comment, while every real drift stays detected:
//   - never-posted account, no projection row             -> clean (the fixed false positive);
//   - entries exist, projection row missing                -> missing_projection;
//   - projection row exists, no entries, zero totals       -> clean (defined behaviour);
//   - projection row exists, no entries, non-zero totals   -> balance_mismatch.
package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func npRun(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (Run, []Mismatch) {
	t.Helper()
	var run Run
	var mismatches []Mismatch
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, mismatches, err = RunLedgerVsProjection(ctx, tx, tenantID, time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("RunLedgerVsProjection: %v", err)
	}
	return run, mismatches
}

// npNeverPostedAccount resolves an account the way a sportsbook settlement event resolves
// house_gaming before it knows whether it will post (a void of an open bet never does).
func npNeverPostedAccount(t *testing.T, pool *db.Pool, f fixture, accountType ledger.AccountType) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, accountType, "EUR")
		return err
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	return id
}

func npProjectionExists(t *testing.T, pool *db.Pool, tenantID, accountID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM wallet_balance_projection WHERE ledger_account_id = $1`, accountID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestRunLedgerVsProjection_NeverPostedAccountWithoutProjection_IsClean(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	house := npNeverPostedAccount(t, pool, f, ledger.AccountHouseGaming)
	if npProjectionExists(t, pool, f.tenantID, house) {
		t.Fatal("precondition: a never-posted account has no projection row")
	}
	run, mismatches := npRun(t, pool, f.tenantID)
	if run.Status != StatusClean || len(mismatches) != 0 {
		t.Fatalf("a never-posted account must be clean, got %s %+v", run.Status, mismatches)
	}
}

func TestRunLedgerVsProjection_EntriesWithoutProjection_IsMismatch(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM wallet_balance_projection WHERE ledger_account_id = $1`, f.cashAccountID)
		return err
	}); err != nil {
		t.Fatalf("delete projection: %v", err)
	}
	run, mismatches := npRun(t, pool, f.tenantID)
	if run.Status != StatusMismatchesFound || len(mismatches) != 1 ||
		mismatches[0].MismatchKind != MismatchKindMissingProjection || mismatches[0].ReconciliationKey != f.cashAccountID.String() {
		t.Fatalf("entries without a projection row must be one missing_projection, got %s %+v", run.Status, mismatches)
	}
}

func TestRunLedgerVsProjection_ProjectionWithoutEntries(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	house := npNeverPostedAccount(t, pool, f, ledger.AccountHouseGaming)
	// A zero projection row for an account with no entries (what RebuildProjectionRow
	// writes for it) is consistent: clean.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.RebuildProjectionRow(ctx, tx, house)
		return err
	}); err != nil {
		t.Fatalf("rebuild projection row: %v", err)
	}
	if !npProjectionExists(t, pool, f.tenantID, house) {
		t.Fatal("precondition: the zero projection row exists")
	}
	run, mismatches := npRun(t, pool, f.tenantID)
	if run.Status != StatusClean || len(mismatches) != 0 {
		t.Fatalf("a zero projection row with no entries must be clean, got %s %+v", run.Status, mismatches)
	}
	// The same row with non-zero totals but still no entries is drift: balance_mismatch.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET debit_total = 7 WHERE ledger_account_id = $1`, house)
		return err
	}); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	run, mismatches = npRun(t, pool, f.tenantID)
	if run.Status != StatusMismatchesFound || len(mismatches) != 1 ||
		mismatches[0].MismatchKind != MismatchKindBalanceMismatch || mismatches[0].ReconciliationKey != house.String() {
		t.Fatalf("a non-zero projection row with no entries must be one balance_mismatch, got %s %+v", run.Status, mismatches)
	}
}
