package reconciliation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Read-only accessors for the staff reconciliation views (Stage 10.3 W2b,
// CAS-RECON-1). Every function here is a plain SELECT under the caller's
// tenant-scoped transaction (reconciliation_runs/_mismatches are
// tenant-only FORCE RLS, migration 0027). Nothing here resolves, edits or
// annotates a mismatch - ResolveMismatch has no HTTP caller and mismatch
// resolution/compensation belongs to LEDGER-MANUAL-ADJ-4EYES-1 (NOT
// IMPLEMENTED).

// CasinoConsistencyMismatchKinds are the kinds the casino_consistency
// stream records.
var CasinoConsistencyMismatchKinds = []MismatchKind{
	MismatchKindCasRoundBinding, MismatchKindCasPostingShape, MismatchKindCasOrphanWin,
	MismatchKindCasRollbackLinkage, MismatchKindCasTombstoneConflict,
	MismatchKindCasUnpostedEvent, MismatchKindCasTombstoneLateOrigin,
}

// StoredMismatch is a reconciliation_mismatches row as read back.
type StoredMismatch struct {
	Mismatch
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// ListRunsForStream reads one page of the tenant's runs of stream, newest
// first, plus the total count.
func ListRunsForStream(ctx context.Context, tx pgx.Tx, stream Stream, limit, offset int) ([]Run, int, error) {
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE stream = $1`, string(stream)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("reconciliation: count runs: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, stream, period_start, period_end, run_at, status
		  FROM reconciliation_runs
		 WHERE stream = $1
		 ORDER BY run_at DESC, id
		 LIMIT $2 OFFSET $3`, string(stream), limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("reconciliation: list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var s, st string
		if err := rows.Scan(&r.ID, &r.TenantID, &s, &r.PeriodStart, &r.PeriodEnd, &r.RunAt, &st); err != nil {
			return nil, 0, fmt.Errorf("reconciliation: scan run: %w", err)
		}
		r.Stream, r.Status = Stream(s), Status(st)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// ListMismatchesOfKinds reads one page of the tenant's mismatches whose
// kind is in kinds, newest first, optionally filtered by
// investigation_status ("" = any), plus the total count.
func ListMismatchesOfKinds(ctx context.Context, tx pgx.Tx, kinds []MismatchKind, status string, limit, offset int) ([]StoredMismatch, int, error) {
	k := make([]string, len(kinds))
	for i, v := range kinds {
		k[i] = string(v)
	}
	var total int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM reconciliation_mismatches
		 WHERE mismatch_kind = ANY($1) AND ($2 = '' OR investigation_status = $2)`, k, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("reconciliation: count mismatches: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value,
		       mismatch_kind, investigation_status, created_at, resolved_at
		  FROM reconciliation_mismatches
		 WHERE mismatch_kind = ANY($1) AND ($2 = '' OR investigation_status = $2)
		 ORDER BY created_at DESC, id
		 LIMIT $3 OFFSET $4`, k, status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("reconciliation: list mismatches: %w", err)
	}
	defer rows.Close()
	var out []StoredMismatch
	for rows.Next() {
		var m StoredMismatch
		var kind string
		if err := rows.Scan(&m.ID, &m.TenantID, &m.RunID, &m.ReconciliationKey, &m.ExpectedValue, &m.ActualValue,
			&kind, &m.InvestigationStatus, &m.CreatedAt, &m.ResolvedAt); err != nil {
			return nil, 0, fmt.Errorf("reconciliation: scan mismatch: %w", err)
		}
		m.MismatchKind = MismatchKind(kind)
		out = append(out, m)
	}
	return out, total, rows.Err()
}
