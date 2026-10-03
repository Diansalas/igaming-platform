//go:build integration

package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// In-tx sites (rows 3-5): an injected raise failure INSIDE the run transaction
// never aborts the run. With a failure that only affects the in-tx raise, the
// post-commit detached retry (Pending.Flush) persists the alert; with a
// persistent failure the run and its mismatch rows still commit.
func TestIWire_Recon_InTxRaiseFailureNeverAbortsRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      alertinject.Mode
		wantAlert bool
	}{{"in_tx_only_detached_retry_persists", alertinject.InTxOnly, true}, {"persistent_run_still_commits", alertinject.Persistent, false}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			f := seedFixture(t, pool)
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET credit_total = credit_total + 999 WHERE ledger_account_id = $1`, f.cashAccountID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			alertinject.Install(t, pool, f.tenantID, tc.mode, "P0001")
			now := time.Now()
			out, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now,
				sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
			if err != nil {
				t.Fatal(err)
			}
			o := findOutcome(t, out, f)
			if o.Err != nil || o.Run.Status != StatusMismatchesFound {
				t.Fatalf("the run must commit with its mismatch despite the alert failure: %+v", o)
			}
			var n int
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, o.Run.ID).Scan(&n)
			}); err != nil || n == 0 {
				t.Fatalf("mismatch rows must be durable: n=%d err=%v", n, err)
			}
			got := alertinject.Find(alertinject.ForSubject(t, pool, f.tenantID), string(alerting.KindReconciliationLedgerProjectionDrift))
			if tc.wantAlert && len(got) != 1 {
				t.Fatalf("the post-commit detached retry must persist the P1, got %+v", got)
			}
			if !tc.wantAlert && len(got) != 0 {
				t.Fatalf("persistent failure: no tenant-visible drift alert expected, got %+v", got)
			}
		})
	}
}
