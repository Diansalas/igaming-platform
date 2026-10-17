//go:build integration

package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2 fix round, LFF2-5 (b) / SRF2-3 for F3b (R9): a clearing line must come from an import whose seal verifies (with
// keys), lie inside its import's coverage and not after the run's coverage end.

// r9ForgeReturnImport writes, in the world's tenant session, an UNSEALED MOCK import whose only line is a `reversed`
// payout line on ref (amount 500, EUR) dated at, with coverage [now - 1 h, coverageEnd].
func r9ForgeReturnImport(w *k3World, ref string, at, coverageEnd time.Time) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		imp := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports
			(id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
			VALUES ($1, $2, $3, 'MOCK forged return', true, now() - interval '1 hour', $4, 1, $5, now())`, imp, w.f.tenantID, w.provider, coverageEnd, make([]byte, 32)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO payment_statement_lines (id, tenant_id, import_id, line_no, provider_id, kind, provider_reference, status, amount, asset_code, occurred_at)
			VALUES ($1, $2, $3, 0, $4, 'payout', $5, 'reversed', 500, 'EUR', $6)`, uuid.New(), w.f.tenantID, imp, w.provider, ref, at)
		return err
	})
}

// A forged return line dated 50 years ahead, planted BEFORE the PSP's succeeded line, used to suppress R9 for good
// (LFF2-5 (b) probe). A line dated after the run's coverage end is never clearing evidence: R9 is raised.
func TestF3_R9_ForgedFutureReturnLineDoesNotSuppress(t *testing.T) {
	_, w, wr, ref := r9Branch(t, 500)
	future := time.Now().AddDate(50, 0, 0)
	r9ForgeReturnImport(w, ref, future, future.Add(time.Hour))
	if got := r9Findings(w.stmtRun(w.source(false, r9Line(w, ref, statement.PaymentStatusSucceeded, 500, time.Now()))), wr.ID); len(got) != 1 {
		t.Fatalf("a future-dated forged return must not suppress R9: %s", render(got))
	}
}

// With keys: an in-coverage forged return line in an UNSEALED import is not clearing evidence; the stream's own
// sealed return line is.
func TestF3_R9_WithKeysOnlyASealedReturnClears(t *testing.T) {
	_, w, wr, ref := r9Branch(t, 500)
	keys := f2eKeys(t)
	run := func(lines ...statement.PaymentStatementLine) []reconciliation.Mismatch {
		t.Helper()
		src := w.source(false, lines...)
		stmt, err := reconciliation.FetchPaymentStatement(context.Background(), src, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), reconciliation.PaymentStatementOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var importID uuid.UUID
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			importID, _, err = reconciliation.IngestPaymentStatementSealed(ctx, tx, w.f.tenantID, src, stmt, time.Now(), keys)
			return err
		})
		var ms []reconciliation.Mismatch
		if err := w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			_, ms, _, err = reconciliation.RunPaymentStatement(ctx, tx, w.f.tenantID, importID, reconciliation.PaymentStatementOptions{ImportKeys: keys})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ms
	}
	t0 := time.Now().Add(-10 * time.Minute)
	if got := r9Findings(run(r9Line(w, ref, statement.PaymentStatusSucceeded, 500, t0)), wr.ID); len(got) != 1 {
		t.Fatal("setup: raised")
	}
	r9ForgeReturnImport(w, ref, time.Now().Add(-time.Minute), time.Now())
	if got := r9Findings(run(), wr.ID); len(got) != 1 {
		t.Fatal("an unsealed forged return must not clear R9 when the process has keys")
	}
	if got := r9Findings(run(r9Line(w, ref, statement.PaymentStatusReversed, 500, time.Now().Add(-30*time.Second))), wr.ID); len(got) != 0 {
		t.Fatalf("the stream's own sealed return line clears: %s", render(got))
	}
}
