//go:build integration

// F3 / A5 R9, migration 0167 (the pay_payout_reversal_tombstone_but_paid mismatch kind) on a THROWAWAY scratch
// database: a whole-schema up/down/up snapshot (the down restores 0134's CHECK) and the fail-loud down (WD099) while
// a finding of the new kind exists (findings are never deleted to make a down pass).
package reconciliation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const migration0167Version = 167

func TestMigration0167_UpDownUp_WholeSchema_DownFailsLoudOnFindings(t *testing.T) {
	ctx := context.Background()
	pool, dir := payScratchThrough(t, migration0167Version)
	snap167 := m0134Snapshot(t, pool)
	var def string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'reconciliation_mismatches_mismatch_kind_check'`).Scan(&def)
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []MismatchKind{MismatchKindPayPayoutReversalTombstoneButPaid, MismatchKindWithdrawalHoldOpenMismatch, MismatchKindPayDeclaredNotPaidButPaid} {
		if !strings.Contains(def, "'"+string(k)+"'") {
			t.Fatalf("0167 CHECK must admit %s (0134's list plus one): %s", k, def)
		}
	}
	if rolled, err := pool.MigrateDown(ctx, dir, 1); err != nil || len(rolled) != 1 || rolled[0] != migration0167Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	snapDown := m0134Snapshot(t, pool)
	if snapDown == snap167 || strings.Contains(snapDown, string(MismatchKindPayPayoutReversalTombstoneButPaid)) || !strings.Contains(snapDown, "withdrawal_request_ledger_link_invalid") {
		t.Fatal("down must remove exactly the new kind")
	}
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got := m0134Snapshot(t, pool); got != snap167 {
		t.Fatal("up after down differs from the first up")
	}

	f := seedFixture(t, pool)
	run := Run{ID: uuid.New(), TenantID: f.tenantID, Stream: StreamPaymentStatement, PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now(), RunAt: time.Now(), Status: StatusMismatchesFound}
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return persistRun(ctx, tx, run, []Mismatch{{ID: uuid.New(), TenantID: f.tenantID, ReconciliationKey: "k", ExpectedValue: "e", ActualValue: "a",
			MismatchKind: MismatchKindPayPayoutReversalTombstoneButPaid, InvestigationStatus: investigationStatusOpen}})
	}); err != nil {
		t.Fatalf("the new kind must be storable: %v", err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "WD099" {
		t.Fatalf("down with a finding of the new kind must refuse WD099, got %v", err)
	}
	if got := m0134Snapshot(t, pool); got != snap167 {
		t.Fatal("a refused down must change nothing")
	}
}
