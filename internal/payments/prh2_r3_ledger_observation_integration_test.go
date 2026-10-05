//go:build integration

// PRH-2 R3 / H-W1, ledger-finance C-2 (owner-approved extension of the
// observation): a suspended or closed tenant's ledger STILL MOVES - through
// staff M2 under platform_acting four-eyes (a withdrawal_failed posting) and
// through the payments sweeper's resolution-only evidence application - so the
// hourly ledger_vs_projection drift check (CLAUDE.md) must keep covering it.
// The stream is an internal read: no credential, no outbound call, no write
// besides reconciliation_runs/mismatches, audit_log and alerts.
package payments

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// An M2 posting made on a NON-ACTIVE tenant through the staff path is covered
// by the observation's ledger run (clean first), and an injected projection
// drift on an account that posting touched raises the drift finding and the P1
// alert, exactly as for an active tenant - while the observation itself writes
// nothing money-affecting and never repairs the projection.
func TestR3_NonActiveTenant_LedgerRunCoversStaffM2PostingAndDetectsDrift(t *testing.T) {
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			_, a := w.ambiguousPayout(100)
			w.setTenantStatus(status)

			// Staff M2 on the non-active tenant (platform_acting, four-eyes).
			r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
			out, err := w.decide(w.acting2, r, ResolutionApprove)
			if err != nil || !out.Executed {
				t.Fatalf("setup: staff M2 on a %s tenant did not execute: %v %+v", status, err, out)
			}
			if n := w.ledgerTxCount("withdrawal_failed"); n != 1 {
				t.Fatalf("setup: want one withdrawal_failed posting, got %d", n)
			}
			var touched uuid.UUID
			w.tx(func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT e.ledger_account_id FROM ledger_entries e
					JOIN ledger_transactions t ON t.id = e.ledger_transaction_id AND t.tenant_id = e.tenant_id
					WHERE e.tenant_id = $1 AND t.transaction_type = 'withdrawal_failed'
					ORDER BY e.id LIMIT 1`, w.f.tenantID).Scan(&touched)
			})

			// 1. Clean observation of the post-M2 ledger.
			before := w.r3Snapshot()
			o := r3Outcome(t, w.r3Sweep([]uuid.UUID{w.f.tenantID}), w.f.tenantID)
			if !o.ObservationOnly || o.TenantStatus != status || o.Err != nil || o.Run.Stream != reconciliation.StreamLedgerVsProjection || o.Run.Status != reconciliation.StatusClean {
				t.Fatalf("want a clean ledger observation of the %s tenant, got %+v", status, o)
			}
			if m := w.r3AuditSweepRun(o.Run.ID); m["non_active_tenant_observation"] != true || m["tenant_status"] != status || m["stream"] != "ledger_vs_projection" {
				t.Fatalf("ledger run audit: %v", m)
			}
			r3RequireNoMoneyEffect(t, before, w.r3Snapshot())

			// 2. Drift injected on the account the M2 posting touched.
			w.tx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET credit_total = credit_total + 999 WHERE ledger_account_id = $1`, touched)
				return err
			})
			var driftBefore int64
			w.tx(func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT credit_total::bigint FROM wallet_balance_projection WHERE ledger_account_id = $1`, touched).Scan(&driftBefore)
			})
			snap := w.r3Snapshot() // taken AFTER the injection: the sweep must change nothing from here
			o = r3Outcome(t, w.r3Sweep([]uuid.UUID{w.f.tenantID}), w.f.tenantID)
			if o.Err != nil || o.Run.Status != reconciliation.StatusMismatchesFound {
				t.Fatalf("injected drift on the %s tenant was not detected: %+v", status, o)
			}
			var drift int
			w.tx(func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE tenant_id = $1 AND reconciliation_run_id = $2`, w.f.tenantID, o.Run.ID).Scan(&drift)
			})
			if drift == 0 {
				t.Fatal("the ledger run reported mismatches_found but persisted no mismatch row")
			}
			if n := w.r3Alerts(alerting.KindReconciliationLedgerProjectionDrift); n == 0 {
				t.Fatalf("no %s alert was raised for the drifted %s tenant", alerting.KindReconciliationLedgerProjectionDrift, status)
			}
			// Detection never repairs: the projection still carries the drift,
			// and nothing else money-affecting changed.
			var driftAfter int64
			w.tx(func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT credit_total::bigint FROM wallet_balance_projection WHERE ledger_account_id = $1`, touched).Scan(&driftAfter)
			})
			if driftAfter != driftBefore {
				t.Fatalf("the observation changed the projection: %d -> %d", driftBefore, driftAfter)
			}
			after := w.r3Snapshot()
			if snap.projDebit != after.projDebit || snap.projCredit != after.projCredit {
				t.Fatal("projection totals changed during the observation")
			}
			snap.projDebit, snap.projCredit = 0, 0
			after.projDebit, after.projCredit = 0, 0
			r3RequireNoMoneyEffect(t, snap, after)
		})
	}
}

// Failure isolation of the ledger observation: one non-active tenant's failing
// ledger run (forced by a scratch-database-only CHECK on reconciliation_runs)
// is recorded on its own outcome, audited with the observation flag and alerted
// as a platform run_failed; the other non-active tenant is still observed.
func TestR3_NonActiveTenant_LedgerRunFailureIsIsolatedAndVisible(t *testing.T) {
	a := newK3World(t, k3Opts{base: 1})
	b := newK3WorldOn(t, a.pool, k3Opts{base: 1})
	if b.f.tenantID.String() < a.f.tenantID.String() {
		a, b = b, a // the failing tenant is the FIRST in observation order
	}
	a.setTenantStatus("closed")
	b.setTenantStatus("closed")
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE reconciliation_runs ADD CONSTRAINT r3_force_ledger_failure
			CHECK (stream <> 'ledger_vs_projection' OR tenant_id <> '%s') NOT VALID`, a.f.tenantID))
		return err
	}); err != nil {
		t.Fatalf("setup (scratch database only): %v", err)
	}

	outs := a.r3Sweep([]uuid.UUID{a.f.tenantID, b.f.tenantID})
	oa, ob := r3Outcome(t, outs, a.f.tenantID), r3Outcome(t, outs, b.f.tenantID)
	if !oa.ObservationOnly || oa.Err == nil {
		t.Fatalf("closed tenant A's forced ledger failure was not recorded on its outcome: %+v", oa)
	}
	var meta map[string]any
	a.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed' AND metadata->>'stream' = 'ledger_vs_projection' ORDER BY created_at DESC LIMIT 1`, a.f.tenantID).Scan(&meta)
	})
	if meta["non_active_tenant_observation"] != true || meta["tenant_status"] != "closed" {
		t.Fatalf("the ledger failure audit lacks the observation flag: %v", meta)
	}
	if n := a.r3Alerts(alerting.KindReconciliationRunFailed); n == 0 {
		t.Fatal("no run_failed alert for the failed non-active ledger run")
	}
	if ob.Err != nil || ob.Run.ID == uuid.Nil || ob.Run.Stream != reconciliation.StreamLedgerVsProjection {
		t.Fatalf("closed tenant B was not observed after A failed: %+v", ob)
	}
	if n := b.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND stream = 'ledger_vs_projection'`, b.f.tenantID); n != 1 {
		t.Fatalf("B's ledger run must exist in B's own scope exactly once, got %d", n)
	}
	if n := a.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND stream = 'ledger_vs_projection'`, a.f.tenantID); n != 0 {
		t.Fatalf("A's failed ledger run must have rolled back, found %d", n)
	}
}

// An idempotent re-run: observing the same non-active tenant twice records one
// run each, with the same (clean) result and no money effect.
func TestR3_NonActiveTenant_LedgerObservationIsIdempotent(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.setTenantStatus("closed")
	before := w.r3Snapshot()
	for i := 0; i < 2; i++ {
		o := r3Outcome(t, w.r3Sweep([]uuid.UUID{w.f.tenantID}), w.f.tenantID)
		if o.Err != nil || o.Run.Status != reconciliation.StatusClean {
			t.Fatalf("run %d: %+v", i, o)
		}
	}
	if n := w.countRows(`SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND stream = 'ledger_vs_projection'`, w.f.tenantID); n != 2 {
		t.Fatalf("want 2 ledger runs, got %d", n)
	}
	r3RequireNoMoneyEffect(t, before, w.r3Snapshot())
}
