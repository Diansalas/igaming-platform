package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
)

// F2e / H-W1 second half (LF-A11-8): an M2 force-resolution ("declare paid" / "declare not paid") rests on the
// payment_statement stream to catch a wrong declaration afterwards (the M2 standing kinds). A registered source
// is not enough: the stream for that provider must actually be WORKING. So both M2 kinds are refused, at
// submission and again at execution, unless the provider's payment_statement reconciliation is CURRENT:
//
//   - its newest run that the stream itself recorded (a reconciliation_runs row of stream payment_statement for
//     (tenant, provider) with run_at <= now and a backing import: in a process with import seal keys, a backing
//     import whose seal VERIFIES, exactly the SR11-10 rule, so a SQL-only forger cannot mint the evidence) exists;
//   - that run is NOT truncated (a truncated run is a backlog, never a clean claim);
//   - no payment_statement failure for the provider was audited after it (reconciliation.sweep_run_failed with
//     the provider's id: a failed fetch, ingest, match, cursor, a missing or ineligible source); and
//   - it is not older than the evidence age (DefaultStatementEvidenceMaxAge; WithStatementEvidenceMaxAge).
//
// The evidence is read in its OWN short read-only tenant transaction (the system view of the tenant, the same
// one the stream writes), because a platform-acting session cannot read audit_log (0112 acting fence). A forged
// failure row can only REFUSE (fail closed). Everything else about M2 - the proof-bound request and approvals,
// four-eyes, the registered-source rule, the database guard - is unchanged; this gate only adds a refusal.

// DefaultStatementEvidenceMaxAge is how old the last successful payment_statement run may be (the sweep runs
// hourly by default; 6 h matches PaymentStatementBacklog).
const DefaultStatementEvidenceMaxAge = 6 * time.Hour

// statementEvidenceRunsExamined bounds the run rows the gate inspects (newest first) for a verifying one.
const statementEvidenceRunsExamined = 16

// ErrResolutionReconciliationNotCurrent: the provider's payment_statement reconciliation is not current (no
// successful run, the last run failed or was truncated, or it is stale). Closed token
// force_resolve_precondition_failed (HTTP 409).
var ErrResolutionReconciliationNotCurrent = errors.New("payments: the provider's payment statement reconciliation is not current")

// resolutionRefusedReconNotCurrent is the refused_at_execution code of the same rule at execution.
const resolutionRefusedReconNotCurrent = "reconciliation_not_current"

// The closed reasons (audit/refusal detail only).
const (
	reconEvidenceNoRun     = "no_successful_run"
	reconEvidenceFailed    = "last_run_failed"
	reconEvidenceTruncated = "last_run_truncated"
	reconEvidenceStale     = "last_run_stale"
)

// WithStatementEvidenceMaxAge sets the evidence age (<= 0 restores the default).
func (s *ManualResolutionService) WithStatementEvidenceMaxAge(d time.Duration) *ManualResolutionService {
	s.evidenceMaxAge = d
	return s
}

func (s *ManualResolutionService) statementEvidenceMaxAge() time.Duration {
	if s.evidenceMaxAge <= 0 {
		return DefaultStatementEvidenceMaxAge
	}
	return s.evidenceMaxAge
}

// statementEvidenceRefusal returns "" when the provider's payment_statement reconciliation is current for the
// tenant, else one of the closed reasons. An unreadable state is an error (the caller fails closed).
func (s *ManualResolutionService) statementEvidenceRefusal(ctx context.Context, tenantID uuid.UUID, providerID *string) (string, error) {
	if s == nil || s.pool == nil || providerID == nil || *providerID == "" {
		return reconEvidenceNoRun, nil
	}
	reason := ""
	err := s.pool.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reason, err = statementEvidenceRefusalInTx(ctx, tx, s.importKeys, tenantID, *providerID, s.statementEvidenceMaxAge())
		return err
	})
	if err != nil {
		return "", fmt.Errorf("payments: read payment statement reconciliation evidence: %w", err)
	}
	return reason, nil
}

func statementEvidenceRefusalInTx(ctx context.Context, tx pgx.Tx, keys *payoutinstrument.Keys, tenantID uuid.UUID, providerID string, maxAge time.Duration) (string, error) {
	type runRow struct {
		id        uuid.UUID
		runAt     time.Time
		truncated bool
		imports   []uuid.UUID
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.run_at, COALESCE(r.window_truncated, false),
		       ARRAY(SELECT i.id FROM payment_statement_imports i
		              WHERE i.tenant_id = r.tenant_id AND i.provider_id = r.provider_id AND i.coverage_end = r.coverage_end
		                AND ($4 OR (i.import_seal IS NOT NULL AND i.seal_kid IS NOT NULL AND i.imported_by_service = $5))
		              ORDER BY i.fetched_at DESC, i.id LIMIT 4)
		  FROM reconciliation_runs r
		 WHERE r.tenant_id = $1 AND r.stream = 'payment_statement' AND r.provider_id = $2
		   AND r.coverage_end IS NOT NULL AND r.run_at <= now()
		 ORDER BY r.run_at DESC, r.id DESC
		 LIMIT $3`, tenantID, providerID, statementEvidenceRunsExamined, keys == nil, importedByServicePaymentStatement)
	if err != nil {
		return "", err
	}
	var runs []runRow
	for rows.Next() {
		var r runRow
		if err := rows.Scan(&r.id, &r.runAt, &r.truncated, &r.imports); err != nil {
			rows.Close()
			return "", err
		}
		runs = append(runs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	var last *runRow
	for i := range runs {
		r := &runs[i]
		if keys == nil {
			if len(r.imports) > 0 { // the A11 rule: any backing import (no keys in this process)
				last = r
			}
		} else {
			for _, id := range r.imports {
				if payoutinstrument.VerifyStatementImportInTx(ctx, tx, keys, tenantID, id) == nil {
					last = r
					break
				}
			}
		}
		if last != nil {
			break
		}
	}
	if last == nil {
		return reconEvidenceNoRun, nil
	}
	if last.truncated {
		return reconEvidenceTruncated, nil
	}
	var failedAfter, stale bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM audit_log
		                WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
		                  AND metadata->>'stream' = 'payment_statement' AND metadata->>'provider_id' = $2
		                  AND created_at > $3),
		       $3 < now() - make_interval(secs => $4)`,
		tenantID, providerID, last.runAt, maxAge.Seconds()).Scan(&failedAfter, &stale); err != nil {
		return "", err
	}
	switch {
	case failedAfter:
		return reconEvidenceFailed, nil
	case stale:
		return reconEvidenceStale, nil
	}
	return "", nil
}

// importedByServicePaymentStatement restates reconciliation.ImportedByServicePaymentStatement (this package
// cannot import internal/reconciliation; pinned equal by a test).
const importedByServicePaymentStatement = "reconciliation.payment_statement"
