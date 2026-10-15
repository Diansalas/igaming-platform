// Command restorecheck is the application-level half of deploy/backup/verify.sh (A12, ADR 0122): it connects to a
// RESTORED database exactly as the application would (the non-owning igaming_runtime role, DATABASE_URL in the
// environment, never argv) and exercises the platform's OWN code against it:
//
//   - the platform's reconciliation stream ledger_vs_projection (internal/reconciliation.RunLedgerVsProjection) for every
//     tenant, which recomputes each account balance from ledger_entries and diffs it against the projection under the
//     real RLS and trigger regime. Any mismatch is drift, which CLAUDE.md defines as a P1 incident. NOTE: a run writes
//     one reconciliation_runs row per tenant (that is what the platform does), so verify.sh runs this AFTER the
//     data comparison;
//   - RLS is really enforced for the runtime role in the restored database (not merely flagged): without a tenant scope
//     it sees no tenant rows, and with a tenant scope it sees only that tenant's rows;
//   - the startup guard db.VerifyEnvironmentAnchor behaves as documented: a production process is refused on a restored
//     database whose anchor is unrecorded (a restore of a dev/test database, or a fresh cluster), and admitted once the
//     owner has recorded 'production'; a development process is never blocked.
//
// Output is line oriented ("CHK|name|PASS|detail") and the exit status is non-zero if any check fails.
//
//	DATABASE_URL=postgres://igaming_runtime:...@127.0.0.1:5498/db restorecheck -expect-anchor unrecorded
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

var failed int

func chk(name string, ok bool, detail string, a ...any) {
	st := "PASS"
	if !ok {
		st = "FAIL"
		failed++
	}
	fmt.Printf("CHK|%s|%s|%s\n", name, st, fmt.Sprintf(detail, a...))
}

func main() {
	expect := flag.String("expect-anchor", "unrecorded", "expected database_environment anchor: unrecorded | production | development | test")
	persist := flag.Bool("persist", false, "keep the reconciliation_runs rows (default: roll the run back)")
	flag.Parse()
	if err := run(*expect, *persist); err != nil {
		fmt.Fprintln(os.Stderr, "restorecheck:", err)
		os.Exit(2)
	}
	if failed > 0 {
		fmt.Printf("restorecheck: %d check(s) FAILED\n", failed)
		os.Exit(1)
	}
}

var errRollback = errors.New("restorecheck: deliberate rollback of the reconciliation run")

func run(expectAnchor string, persist bool) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL (the igaming_runtime credential) is required")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 4, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect as the runtime role: %w", err)
	}
	defer pool.Close()

	var role string
	var super, bypass bool
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user, r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).Scan(&role, &super, &bypass)
	}); err != nil {
		return err
	}
	if role != "igaming_runtime" || super || bypass {
		return fmt.Errorf("DATABASE_URL must be the non-owning igaming_runtime role (got %s super=%v bypassrls=%v)", role, super, bypass)
	}

	// tenants (platform scope read, as the reconciliation scheduler does)
	var tenants []uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM tenants ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			tenants = append(tenants, id)
		}
		return rows.Err()
	}); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	chk("restorecheck_tenants_visible", len(tenants) > 0, "tenants=%d", len(tenants))

	// RLS is enforced for the runtime role: no scope => no tenant rows; a scope => only that tenant's rows
	var unscoped int64
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&unscoped)
	}); err != nil {
		return fmt.Errorf("unscoped read: %w", err)
	}
	chk("runtime_role_unscoped_sees_no_ledger_rows", unscoped == 0, "visible_ledger_entries=%d", unscoped)
	if len(tenants) > 0 {
		var own, foreign int64
		if err := pool.WithTenant(ctx, tenants[0], func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&own); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id <> $1`, tenants[0]).Scan(&foreign)
		}); err != nil {
			return fmt.Errorf("scoped read: %w", err)
		}
		chk("runtime_role_scoped_sees_only_own_tenant", own > 0 && foreign == 0, "own_rows=%d foreign_rows=%d", own, foreign)
	}

	// the platform's own reconciliation, per tenant, with the runtime role
	now := time.Now().UTC()
	totalMismatch := 0
	for _, tid := range tenants {
		var mism int
		var status reconciliation.Status
		err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			run, ms, err := reconciliation.RunLedgerVsProjection(ctx, tx, tid, now.Add(-24*time.Hour), now)
			mism, status = len(ms), run.Status
			if err == nil && !persist {
				return errRollback
			}
			return err
		})
		if errors.Is(err, errRollback) {
			err = nil
		}
		if err != nil {
			chk("recon_ledger_vs_projection_"+tid.String(), false, "run failed: %v", err)
			continue
		}
		totalMismatch += mism
		chk("recon_ledger_vs_projection_"+tid.String(), mism == 0 && status == reconciliation.StatusClean, "status=%s mismatches=%d", status, mism)
	}
	chk("recon_ledger_vs_projection_total_drift", totalMismatch == 0, "tenants=%d drift_mismatches=%d (any non-zero drift is a P1 incident)", len(tenants), totalMismatch)

	// startup guard behaviour (ADR 0115 S1)
	anchor, err := pool.ReadEnvironmentAnchor(ctx)
	if err != nil {
		return fmt.Errorf("read anchor: %w", err)
	}
	got := anchor
	if got == "" {
		got = "unrecorded"
	}
	chk("environment_anchor_state", got == expectAnchor, "anchor=%s expected=%s", got, expectAnchor)
	_, prodErr := db.VerifyEnvironmentAnchor(ctx, "production", pool)
	switch expectAnchor {
	case "production":
		chk("startup_guard_production_process_admitted", prodErr == nil, "err=%v", prodErr)
	case "unrecorded":
		chk("startup_guard_production_process_refused_when_unrecorded", errors.Is(prodErr, db.ErrEnvironmentAnchorUnrecorded), "err=%v (re-provision: INSERT INTO database_environment (environment) VALUES ('production') as the owner, BEFORE starting platform-api in production)", prodErr)
	default:
		chk("startup_guard_production_process_refused_on_dev_or_test_anchor", errors.Is(prodErr, db.ErrEnvironmentAnchorMismatch), "err=%v", prodErr)
	}
	_, devErr := db.VerifyEnvironmentAnchor(ctx, "development", pool)
	chk("startup_guard_development_process_never_blocked", devErr == nil, "err=%v", devErr)
	return nil
}
