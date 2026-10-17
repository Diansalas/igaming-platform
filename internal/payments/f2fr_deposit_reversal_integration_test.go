//go:build integration

package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2 fix round, LFF2-5 / SRF2-3 for the persisted deposit-reversal lookup: with keys, a persisted reversal line
// clears a captured-unposted finding only if its import's seal verifies.

// stmtRunKeyed is stmtRun with import seal keys: a sealed ingest and a keyed match.
func (w *k3World) stmtRunKeyed(keys *payoutinstrument.Keys, src statement.PaymentStatementSource) []reconciliation.Mismatch {
	w.t.Helper()
	stmt, err := reconciliation.FetchPaymentStatement(context.Background(), src, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), reconciliation.PaymentStatementOptions{})
	if err != nil {
		w.t.Fatalf("FetchPaymentStatement: %v", err)
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
		w.t.Fatalf("RunPaymentStatement: %v", err)
	}
	return ms
}

func TestF2fr_WithKeysOnlyASealedDepositReversalClears(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	keys := f2eKeys(t)
	a := w.disputedDeposit(5000)
	ref := *a.ProviderReference
	w.requireOne(w.stmtRunKeyed(keys, w.source(false, w.depositLine(ref, a.MerchantReference, statement.PaymentStatusSucceeded, 5000))),
		reconciliation.MismatchKindPayCapturedUnposted, a.ID, "setup: raised")

	// The forgery: an UNSEALED MOCK import whose only line reverses ref, inside its coverage.
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		imp := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports
			(id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
			VALUES ($1, $2, $3, 'MOCK forged reversal', true, now() - interval '1 hour', now(), 1, $4, now())`, imp, w.f.tenantID, w.provider, make([]byte, 32)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO payment_statement_lines (id, tenant_id, import_id, line_no, provider_id, kind, provider_reference,
			original_provider_reference, status, amount, asset_code, occurred_at)
			VALUES ($1, $2, $3, 0, $4, 'deposit_reversal', $5, $6, 'succeeded', 5000, 'EUR', now() - interval '1 minute')`,
			uuid.New(), w.f.tenantID, imp, w.provider, "forged-rev-"+uuid.NewString(), ref)
		return err
	})
	w.requireOne(w.stmtRunKeyed(keys, w.source(false)), reconciliation.MismatchKindPayCapturedUnposted, a.ID,
		"an unsealed forged reversal must not clear when the process has keys")

	rev := w.depositLine("f2fr-rev-"+uuid.NewString(), "", statement.PaymentStatusSucceeded, 5000)
	rev.Kind = statement.PaymentLineDepositReversal
	rev.OriginalProviderReference = ref
	w.requireNone(w.stmtRunKeyed(keys, w.source(false, rev)), reconciliation.MismatchKindPayCapturedUnposted, a.ID, "the stream's own sealed reversal clears")
	w.requireNone(w.stmtRunKeyed(keys, w.source(false)), reconciliation.MismatchKindPayCapturedUnposted, a.ID, "and stays cleared (persisted, sealed)")
}
