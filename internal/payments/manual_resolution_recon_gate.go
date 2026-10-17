package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2e / H-W1 second half (LF-A11-8): an M2 force-resolution ("declare paid" / "declare not paid") rests on the
// payment_statement stream to catch a wrong declaration afterwards (the M2 standing kinds). A registered source
// is not enough: the stream for that provider must actually be WORKING. So both M2 kinds are refused, at
// submission and again at execution, unless the provider's payment_statement reconciliation is CURRENT:
//
//   - its newest stream-recorded run exists: with import seal keys (production), a reconciliation_runs row whose RUN
//     seal verifies (migration 0168: run id, tenant, provider, stream, import, coverage, window_truncated and run_at
//     are inside the seal) and whose import's seal verifies; a forged or copied run row (SRF2-2 / LFF2-2) is never
//     evidence. Without keys (non-production only): a run row with a backing import (the A11 rule);
//   - that run is NOT truncated (its sealed window_truncated);
//   - no payment_statement failure for the provider was audited after its sealed run_at and not after now() + a
//     5-minute clock-skew tolerance (LFF2-3: a future-dated audit row cannot block M2 for more than that);
//   - its sealed run_at is not older than DefaultStatementEvidenceMaxAge (WithStatementEvidenceMaxAge).
//
// The evidence is read BEFORE the M2 transaction opens (SRF2-5 / LFF2-7: no second pooled connection while the M2
// transaction holds one), in its own short read-only tenant (system) transaction, for every provider the tenant has
// attempts with, and carried into the transaction on the context. A platform-acting session cannot read audit_log
// (0112 acting fence), which is why it is never read inside the M2 session. A forged failure row can only refuse.
// Everything else about M2 (proof-bound request and approvals, four-eyes, the registered-source rule, the database
// guard) is unchanged; this gate only adds a refusal.

// DefaultStatementEvidenceMaxAge is how old the last successful payment_statement run may be (the sweep runs
// hourly by default; 6 h matches PaymentStatementBacklog).
const DefaultStatementEvidenceMaxAge = 6 * time.Hour

// failureAuditSkew is the clock-skew tolerance on a failure row's created_at (LFF2-3).
const failureAuditSkew = 5 * time.Minute

// evidenceRunsPerPage bounds one page of the streaming run read (keyset paging, no overall limit: forged rows cost
// a linear HMAC scan, they can never stop the read or become evidence).
const evidenceRunsPerPage = 512

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
	reconEvidenceUnread    = "evidence_not_read"
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

// reconEvidence is the pre-read verdict per provider id ("" = current). err is a read failure (applied only when an
// M2 needs it: fail closed).
type reconEvidence struct {
	reasons map[string]string
	err     error
}

type reconEvidenceKey struct{}

func withReconEvidence(ctx context.Context, ev *reconEvidence) context.Context {
	return context.WithValue(ctx, reconEvidenceKey{}, ev)
}

// statementEvidenceRefusal returns "" when the provider's reconciliation is current according to the evidence the
// caller read before the transaction, else a closed reason. No evidence on ctx (an entry point that did not pre-read)
// refuses; a pre-read error is returned (fail closed).
func statementEvidenceRefusal(ctx context.Context, providerID *string) (string, error) {
	ev, _ := ctx.Value(reconEvidenceKey{}).(*reconEvidence)
	if ev == nil {
		return reconEvidenceUnread, nil
	}
	if ev.err != nil {
		return "", ev.err
	}
	if providerID == nil || *providerID == "" {
		return reconEvidenceNoRun, nil
	}
	reason, ok := ev.reasons[*providerID]
	if !ok {
		return reconEvidenceNoRun, nil
	}
	return reason, nil
}

// preReadStatementEvidence reads the reconciliation-currency verdict of every provider the tenant has payment
// attempts with, in its own short read-only tenant transaction, BEFORE the M2 transaction (SRF2-5 / LFF2-7).
func (s *ManualResolutionService) preReadStatementEvidence(ctx context.Context, tenantID uuid.UUID) *reconEvidence {
	ev := &reconEvidence{reasons: map[string]string{}}
	if s == nil || s.pool == nil || tenantID == uuid.Nil {
		return ev
	}
	err := s.pool.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT provider_id FROM payment_attempts WHERE tenant_id = $1 AND provider_id IS NOT NULL`, tenantID)
		if err != nil {
			return err
		}
		var providers []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			providers = append(providers, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range providers {
			reason, err := statementEvidenceRefusalInTx(ctx, tx, s.importKeys, tenantID, p, s.statementEvidenceMaxAge())
			if err != nil {
				return err
			}
			ev.reasons[p] = reason
		}
		return nil
	})
	if err != nil {
		ev.err = fmt.Errorf("payments: read payment statement reconciliation evidence: %w", err)
	}
	return ev
}

// lastRun is the newest trusted run's sealed facts.
type lastRun struct {
	runAt     time.Time
	truncated bool
}

func statementEvidenceRefusalInTx(ctx context.Context, tx pgx.Tx, keys *payoutinstrument.Keys, tenantID uuid.UUID, providerID string, maxAge time.Duration) (string, error) {
	var last *lastRun
	var err error
	if keys == nil {
		last, err = newestBackedRun(ctx, tx, tenantID, providerID)
	} else {
		last, err = newestSealedRun(ctx, tx, keys, tenantID, providerID)
	}
	if err != nil {
		return "", err
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
		                  AND created_at > $3 AND created_at <= now() + make_interval(secs => $5)),
		       $3 < now() - make_interval(secs => $4)`,
		tenantID, providerID, last.runAt, maxAge.Seconds(), failureAuditSkew.Seconds()).Scan(&failedAfter, &stale); err != nil {
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

// newestBackedRun is the no-keys rule (non-production only): the newest run row with any backing import.
func newestBackedRun(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string) (*lastRun, error) {
	var r lastRun
	err := tx.QueryRow(ctx, `
		SELECT r.run_at, COALESCE(r.window_truncated, false)
		  FROM reconciliation_runs r
		 WHERE r.tenant_id = $1 AND r.stream = 'payment_statement' AND r.provider_id = $2
		   AND r.coverage_end IS NOT NULL AND r.run_at <= now()
		   AND EXISTS (SELECT 1 FROM payment_statement_imports i
		                WHERE i.tenant_id = r.tenant_id AND i.provider_id = r.provider_id AND i.coverage_end = r.coverage_end)
		 ORDER BY r.run_at DESC, r.id DESC
		 LIMIT 1`, tenantID, providerID).Scan(&r.runAt, &r.truncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// newestSealedRun is the keys rule: the newest run row (by run_at) whose run seal verifies and whose import - with
// the run's coverage window, written by the stream - has a seal that verifies. Read in keyset pages, newest first.
func newestSealedRun(ctx context.Context, tx pgx.Tx, keys *payoutinstrument.Keys, tenantID uuid.UUID, providerID string) (*lastRun, error) {
	var afterAt *time.Time
	var afterID *uuid.UUID
	for {
		rows, err := tx.Query(ctx, `
			SELECT r.id, r.run_import_id, r.period_start, r.coverage_end, COALESCE(r.window_truncated, false), r.run_at, r.run_seal, r.run_seal_kid
			  FROM reconciliation_runs r
			 WHERE r.tenant_id = $1 AND r.stream = 'payment_statement' AND r.provider_id = $2
			   AND r.run_seal IS NOT NULL AND r.run_at <= now()
			   AND ($3::timestamptz IS NULL OR (r.run_at, r.id) < ($3, $4::uuid))
			 ORDER BY r.run_at DESC, r.id DESC
			 LIMIT $5`, tenantID, providerID, afterAt, afterID, evidenceRunsPerPage)
		if err != nil {
			return nil, err
		}
		var verified []statement.RunSealInput
		n := 0
		for rows.Next() {
			in := statement.RunSealInput{TenantID: tenantID, ProviderID: providerID, Stream: "payment_statement"}
			var seal, kid string
			if err := rows.Scan(&in.RunID, &in.ImportID, &in.CoverageStart, &in.CoverageEnd, &in.WindowTruncated, &in.RunAt, &seal, &kid); err != nil {
				rows.Close()
				return nil, err
			}
			n++
			at, id := in.RunAt, in.RunID
			afterAt, afterID = &at, &id
			if keys.VerifyPaymentRun(in, kid, seal) == nil {
				verified = append(verified, in)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, in := range verified {
			var shaped bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_statement_imports
				WHERE id = $1 AND tenant_id = $2 AND provider_id = $3 AND coverage_start = $4 AND coverage_end = $5 AND imported_by_service = $6)`,
				in.ImportID, tenantID, providerID, in.CoverageStart, in.CoverageEnd, importedByServicePaymentStatement).Scan(&shaped); err != nil {
				return nil, err
			}
			if shaped && payoutinstrument.VerifyStatementImportInTx(ctx, tx, keys, tenantID, in.ImportID) == nil {
				return &lastRun{runAt: in.RunAt, truncated: in.WindowTruncated}, nil
			}
		}
		if n < evidenceRunsPerPage {
			return nil, nil
		}
	}
}

// importedByServicePaymentStatement restates reconciliation.ImportedByServicePaymentStatement (this package
// cannot import internal/reconciliation; pinned equal by a test).
const importedByServicePaymentStatement = "reconciliation.payment_statement"
