package reconciliation

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// ADR 0102 I-wire rows 3-7 and 14. Every reconciliation P1 raises a durable
// alert in addition to the existing log line (logs are retained).
//
//   - READ COMMITTED run transactions (drift, sportsbook, casino
//     consistency): alerting.RaiseGuarded inside an alerting.InTx closure,
//     flushed after the commit (rows 3-5).
//   - REPEATABLE READ sites (casino_statement, payment_statement match): NO
//     alert statement inside the snapshot (SR-5, LF C-2); after the nil
//     commit, alerting.RaisePostCommit opens a fresh READ COMMITTED
//     tenant transaction (rows 6-7, LF test 6).
//   - Failure path (the run transaction rolled back): a detached raise of
//     reconciliation.run_failed (row 14, RECON-RUN-FAILED-ALERT-1).
//
// Discriminators are stable "stream:" keys; the run id is an attribute only
// (LF test 7), so a persisting condition is ONE open alert with growing
// occurrences. None of this can fail a run: the run and its mismatch rows are
// already durable (committed) or are the thing being reported as failed.

// Stream discriminators (stable, server-side, charset-safe).
const (
	discLedgerVsProjection = "stream:ledger_vs_projection"
	// discLedgerUnlinkedManualAdjustment is the K2 detective finding (ADR
	// 0100 section 12, C-K2-1). It deliberately uses a distinct discriminator
	// under the drift Kind so it is never presented as projection drift: it is
	// a governance breach (a manual adjustment posted outside the governed
	// request path), see the runbook.
	discLedgerUnlinkedManualAdjustment = "stream:ledger_unlinked_manual_adjustment"
)

// splitLedgerMismatches separates the K2 unlinked-manual-adjustment findings
// from true projection drift within one ledger_vs_projection run.
func splitLedgerMismatches(ms []Mismatch) (drift, unlinked int) {
	for _, m := range ms {
		if m.MismatchKind == MismatchKindLedgerUnlinkedManualAdjustment {
			unlinked++
		} else {
			drift++
		}
	}
	return drift, unlinked
}

func mismatchAlert(kind alerting.Kind, tenantID uuid.UUID, discriminator string, runID uuid.UUID, count int, extra map[string]alerting.AttrValue) alerting.Alert {
	attrs := map[string]alerting.AttrValue{"run_id": runID.String(), "mismatch_count": count}
	for k, v := range extra {
		attrs[k] = v
	}
	return alerting.Alert{Kind: kind, SubjectTenantID: tenantID, Discriminator: discriminator, Attributes: attrs}
}

// raiseLedgerRunAlerts raises the in-tx P1s for one ledger_vs_projection run:
// drift under discLedgerVsProjection and K2 unlinked adjustments under
// discLedgerUnlinkedManualAdjustment. Must be called inside the InTx closure,
// as the last statement group before the closure returns.
func raiseLedgerRunAlerts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, run Run, acquired bool, ms []Mismatch) error {
	if !acquired || run.Status != StatusMismatchesFound {
		return nil
	}
	drift, unlinked := splitLedgerMismatches(ms)
	if drift > 0 {
		if err := alerting.RaiseGuarded(ctx, tx, mismatchAlert(alerting.KindReconciliationLedgerProjectionDrift, tenantID, discLedgerVsProjection, run.ID, drift, nil)); err != nil {
			return err
		}
	}
	if unlinked > 0 {
		if err := alerting.RaiseGuarded(ctx, tx, mismatchAlert(alerting.KindReconciliationLedgerProjectionDrift, tenantID, discLedgerUnlinkedManualAdjustment, run.ID, unlinked, nil)); err != nil {
			return err
		}
	}
	return nil
}

// raiseInTxMismatch raises a single in-tx mismatch P1 (sportsbook, casino
// consistency) when the run found mismatches.
func raiseInTxMismatch(ctx context.Context, tx pgx.Tx, kind alerting.Kind, tenantID uuid.UUID, disc string, run Run, acquired bool, count int, extra map[string]alerting.AttrValue) error {
	if !acquired || run.Status != StatusMismatchesFound {
		return nil
	}
	return alerting.RaiseGuarded(ctx, tx, mismatchAlert(kind, tenantID, disc, run.ID, count, extra))
}

// raisePostCommitMismatch is the REPEATABLE READ sites' raise: after the nil
// commit, in a fresh READ COMMITTED tenant transaction, bounded retry then the
// terminal fallback (all inside alerting.RaisePostCommit). The error is
// logged inside alerting; the run result is never affected.
func raisePostCommitMismatch(ctx context.Context, pool *db.Pool, kind alerting.Kind, tenantID uuid.UUID, disc string, run Run, count int, extra map[string]alerting.AttrValue) {
	_ = alerting.RaisePostCommit(ctx, alerting.NewTenantRunner(pool, tenantID), mismatchAlert(kind, tenantID, disc, run.ID, count, extra))
}

// sqlstateClassOf returns the two-character SQLSTATE class of err, or "58"
// (the system-error class, the same stand-in alerting uses) when err is not a
// PG error. Never the error text (SR-3).
func sqlstateClassOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		return pgErr.Code[:2]
	}
	return "58"
}

// raiseRunFailed is ADR 0102 row 14: the run transaction rolled back, so the
// P1 is a detached raise in a fresh transaction of the originating tenant
// scope. providerID is empty except for payment_statement.
//
// A run that failed only because the sweep's own context was cancelled
// (graceful shutdown mid-sweep) is NOT a failed run: it raises nothing, or
// every remaining tenant and stream would page a false P1 at shutdown (LF F3).
// Any other failure raises, including a deadline expiry.
func raiseRunFailed(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, stream, providerID, phase string, runErr error) {
	if ctx.Err() != nil && errors.Is(runErr, context.Canceled) {
		return
	}
	disc := "stream:" + stream
	if providerID != "" && providerID != "<none>" {
		disc += ":provider:" + providerID
	}
	_ = alerting.RaiseDetached(ctx, alerting.NewTenantRunner(pool, tenantID), alerting.Alert{
		Kind: alerting.KindReconciliationRunFailed, SubjectTenantID: tenantID, Discriminator: disc,
		Attributes: map[string]alerting.AttrValue{"stream": stream, "phase": phase, "sqlstate_class": sqlstateClassOf(runErr)},
	})
}
