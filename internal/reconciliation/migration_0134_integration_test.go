//go:build integration

// ADR 0113 slice 3, migration 0134 (the two check-D mismatch kinds) on THROWAWAY scratch databases: a
// whole-schema up/down/up snapshot (the down restores 0115's CHECK byte for byte) and the fail-loud
// down (WD099) while a finding of a new kind exists (findings are never deleted to make a down pass).
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

	"github.com/Diansalas/igaming-platform/internal/db"
)

const migration0134Version = 134

func m0134Snapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || pg_get_triggerdef(t.oid)
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || pg_get_constraintdef(oid)
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL SELECT 'table:' || c.relname || ':' || c.relrowsecurity || ':' || c.relforcerowsecurity
			  FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r'
			UNION ALL SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc)
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL SELECT 'index:' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public') s`).Scan(&snap)
	}); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestMigration0134_UpDownUp_WholeSchema_DownFailsLoudOnFindings(t *testing.T) {
	ctx := context.Background()
	pool, dir := payScratchThrough(t, migration0134Version)
	snap134 := m0134Snapshot(t, pool)
	var def string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'reconciliation_mismatches_mismatch_kind_check'`).Scan(&def)
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"withdrawal_hold_open_mismatch", "withdrawal_request_ledger_link_invalid", "pay_declared_paid_compensated_but_paid"} {
		if !strings.Contains(def, "'"+k+"'") {
			t.Fatalf("0134 CHECK must admit %s (0115's list plus two): %s", k, def)
		}
	}
	if rolled, err := pool.MigrateDown(ctx, dir, 1); err != nil || len(rolled) != 1 || rolled[0] != migration0134Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	snap133 := m0134Snapshot(t, pool)
	if snap133 == snap134 || strings.Contains(snap133, "withdrawal_hold_open_mismatch") {
		t.Fatal("down must remove the two kinds")
	}
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got := m0134Snapshot(t, pool); got != snap134 {
		t.Fatal("up after down differs from the first up")
	}

	// A finding of a new kind exists: the down refuses (WD099), changes nothing, and the row stays.
	f := seedFixture(t, pool)
	run := Run{TenantID: f.tenantID, Stream: StreamLedgerVsProjection, PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now(), RunAt: time.Now(), Status: StatusMismatchesFound}
	run.ID = uuid.New()
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return persistRun(ctx, tx, run, []Mismatch{{ID: uuid.New(), TenantID: f.tenantID, ReconciliationKey: "k", ExpectedValue: "e", ActualValue: "a",
			MismatchKind: MismatchKindWithdrawalHoldOpenMismatch, InvestigationStatus: investigationStatusOpen}})
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "WD099" {
		t.Fatalf("down with a withdrawal-hold finding must refuse WD099, got %v", err)
	}
	if got := m0134Snapshot(t, pool); got != snap134 {
		t.Fatal("a refused down must change nothing")
	}
	var n int
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, run.ID).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("the finding must stay: %d err=%v", n, err)
	}
}
