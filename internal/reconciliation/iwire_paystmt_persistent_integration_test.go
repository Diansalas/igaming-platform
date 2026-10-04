//go:build integration

package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// LF test 6 for payment_statement itself (LF I-wire F5): with a PERSISTENT
// failure injected into the alert path (P0001, then deadlock class 40P01), the
// payment_statement run and its mismatch rows still commit and the run result
// is unchanged; no tenant-visible alert can exist.
func TestIWire_Recon_PaymentStatementP1_PostCommit_RunCommitsUnderPersistentRaiseFailure(t *testing.T) {
	for _, code := range []string{"P0001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			w := newPayWorld(t)
			alertinject.Install(t, w.pool, w.f.tenantID, alertinject.Persistent, code)
			src := payFixedSource{provider: payProvA, stmt: wideCoverage(
				payLineFor(payProvA, "iw-unknown-1", "", statement.PaymentLineDeposit, statement.PaymentStatusSucceeded, 1))}
			out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src, PaymentStatementOptions{})
			if out.Err != nil || out.Run.Status != StatusMismatchesFound {
				t.Fatalf("the run must commit with mismatches regardless of the alert path: %+v", out)
			}
			var n int
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, out.Run.ID).Scan(&n)
			}); err != nil || n == 0 {
				t.Fatalf("mismatch rows must be durable: n=%d err=%v", n, err)
			}
			if got := alertinject.Find(alertinject.ForSubject(t, w.pool, w.f.tenantID), string(alerting.KindReconciliationPaymentStatement)); len(got) != 0 {
				t.Fatalf("no tenant-visible alert under a persistent failure, got %+v", got)
			}
		})
	}
}
