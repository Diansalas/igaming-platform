package reconciliation

import (
	"context"
	"errors"
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

// CasinoStatementMismatchKinds are the kinds the casino_statement stream
// records (Stage 10.3 W3a, CAS-RECON-STMT-1).
var CasinoStatementMismatchKinds = []MismatchKind{MismatchKindCasMockStatement}

// CasinoStreams are the casino reconciliation streams, in sweep order.
var CasinoStreams = []Stream{StreamCasinoConsistency, StreamCasinoStatement}

// CasinoMismatchKindsForStream returns the mismatch kinds of one casino
// stream, or of both when stream is "". ok is false for any other value.
func CasinoMismatchKindsForStream(stream Stream) (kinds []MismatchKind, ok bool) {
	switch stream {
	case "":
		out := append([]MismatchKind{}, CasinoConsistencyMismatchKinds...)
		return append(out, CasinoStatementMismatchKinds...), true
	case StreamCasinoConsistency:
		return CasinoConsistencyMismatchKinds, true
	case StreamCasinoStatement:
		return CasinoStatementMismatchKinds, true
	}
	return nil, false
}

// StoredMismatch is a reconciliation_mismatches row as read back.
type StoredMismatch struct {
	Mismatch
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// ListRunsForStreams reads one page of the tenant's runs of the given
// streams, newest first, plus the total count.
func ListRunsForStreams(ctx context.Context, tx pgx.Tx, streams []Stream, limit, offset int) ([]Run, int, error) {
	s := make([]string, len(streams))
	for i, v := range streams {
		s[i] = string(v)
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE stream = ANY($1)`, s).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("reconciliation: count runs: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, stream, period_start, period_end, run_at, status
		  FROM reconciliation_runs
		 WHERE stream = ANY($1)
		 ORDER BY run_at DESC, id
		 LIMIT $2 OFFSET $3`, s, limit, offset)
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

// StaffViewStreams are the streams the ADR 0114 R2 staff views serve (slice S4): the streams
// that had no staff read route. The casino streams keep their own route.
var StaffViewStreams = []Stream{StreamPaymentStatement, StreamLedgerVsProjection, StreamSportsbookSettlement}

// LedgerVsProjectionMismatchKinds are the kinds the ledger_vs_projection stream records.
var LedgerVsProjectionMismatchKinds = []MismatchKind{
	MismatchKindMissingProjection, MismatchKindBalanceMismatch, MismatchKindLedgerUnlinkedManualAdjustment,
	// ADR 0113 s4.4 check D (withdrawal hold, invariant #12): recorded in the ledger_vs_projection run (G5).
	MismatchKindWithdrawalHoldOpenMismatch, MismatchKindWithdrawalRequestLedgerLinkInvalid,
}

// PaymentStatementMismatchKinds are the kinds the payment_statement stream records.
var PaymentStatementMismatchKinds = []MismatchKind{
	MismatchKindPayMissingPlatformRecord, MismatchKindPayMissingProviderRecord, MismatchKindPayAmountMismatch,
	MismatchKindPayAssetMismatch, MismatchKindPayReferenceMismatch, MismatchKindPayStatusMismatch,
	MismatchKindPayDuplicate, MismatchKindPayUnresolved, MismatchKindPayCapturedUnposted,
	MismatchKindPayDeclaredPaidUnconfirmed, MismatchKindPayDeclaredNotPaidButPaid,
	MismatchKindPayDeclaredPaidCompensatedButPaid,
}

// SportsbookSettlementMismatchKinds are the kinds the sportsbook_settlement stream records.
var SportsbookSettlementMismatchKinds = []MismatchKind{
	MismatchKindSBLocked, MismatchKindSBBetNet, MismatchKindSBOrphanLedger, MismatchKindSBOrphanHistory,
	MismatchKindSBStatus, MismatchKindSBMockStatement,
}

// StaffViewMismatchKinds returns the closed kind vocabulary of one staff-view stream. ok is
// false for any other stream.
func StaffViewMismatchKinds(stream Stream) (kinds []MismatchKind, ok bool) {
	switch stream {
	case StreamPaymentStatement:
		return PaymentStatementMismatchKinds, true
	case StreamLedgerVsProjection:
		return LedgerVsProjectionMismatchKinds, true
	case StreamSportsbookSettlement:
		return SportsbookSettlementMismatchKinds, true
	}
	return nil, false
}

// StaffMismatchFilter narrows ListMismatchesForStream. Every field is optional except Stream.
type StaffMismatchFilter struct {
	Stream Stream
	// Kind and Status are matched exactly when non-empty (validated by the caller against the
	// closed vocabularies).
	Kind   MismatchKind
	Status string
	// SubjectID, when non-empty, must be a canonical lower-case UUID (the caller validates it):
	// it matches a mismatch whose reconciliation_key names that id as the whole key or as one of
	// the subject tokens (attempt=, deposit_intent=, receipt=, bet=, wallet=, ledger_transaction=).
	SubjectID string
}

// ErrBadSubjectID is a StaffMismatchFilter.SubjectID that is not a canonical lower-case UUID.
var ErrBadSubjectID = errors.New("reconciliation: subject id must be a canonical lower-case UUID")

// ListMismatchesForStream reads one page of the tenant's mismatches of ONE stream (taken from
// the owning run's stream, not from the mismatch kind), newest first, with the total count. A
// plain SELECT under the caller's tenant-scoped transaction.
func ListMismatchesForStream(ctx context.Context, tx pgx.Tx, f StaffMismatchFilter, limit, offset int) ([]StoredMismatch, int, error) {
	where := `
		 FROM reconciliation_mismatches m
		 JOIN reconciliation_runs r ON r.id = m.reconciliation_run_id AND r.tenant_id = m.tenant_id
		WHERE r.stream = $1
		  AND ($2 = '' OR m.mismatch_kind = $2)
		  AND ($3 = '' OR m.investigation_status = $3)
		  AND ($4 = '' OR m.reconciliation_key = $4 OR (
		       m.reconciliation_key !~ '^(statement|ledger):'
		       AND EXISTS (
		           SELECT 1
		             FROM (SELECT regexp_replace(m.reconciliation_key, '` + FreeTextTokenSQL + `', '') AS p) x,
		                  unnest(ARRAY['attempt', 'deposit_intent', 'receipt', 'bet', 'wallet', 'ledger_transaction']) AS tok
		            WHERE x.p ~ ('(^| )' || tok || '=' || $4 || '( |$)')
		              AND (SELECT count(*) FROM regexp_matches(x.p, '(^| )' || tok || '=', 'g')) = 1))
		       OR (r.stream = 'payment_statement' AND EXISTS (
		           SELECT 1
		             FROM (SELECT regexp_replace(m.expected_value, '` + FreeTextTokenSQL + `', '') AS p) x
		            WHERE x.p ~ ('(^| )attempt=' || $4 || '( |$)')
		              AND (SELECT count(*) FROM regexp_matches(x.p, '(^| )attempt=', 'g')) = 1)))`
	if f.SubjectID != "" && !IsCanonicalUUID(f.SubjectID) {
		return nil, 0, ErrBadSubjectID
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*)`+where, string(f.Stream), string(f.Kind), f.Status, f.SubjectID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("reconciliation: count staff-view mismatches: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.tenant_id, m.reconciliation_run_id, m.reconciliation_key, m.expected_value, m.actual_value,
		       m.mismatch_kind, m.investigation_status, m.created_at, m.resolved_at`+where+`
		 ORDER BY m.created_at DESC, m.id
		 LIMIT $5 OFFSET $6`, string(f.Stream), string(f.Kind), f.Status, f.SubjectID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("reconciliation: list staff-view mismatches: %w", err)
	}
	defer rows.Close()
	var out []StoredMismatch
	for rows.Next() {
		var m StoredMismatch
		var kind string
		if err := rows.Scan(&m.ID, &m.TenantID, &m.RunID, &m.ReconciliationKey, &m.ExpectedValue, &m.ActualValue,
			&kind, &m.InvestigationStatus, &m.CreatedAt, &m.ResolvedAt); err != nil {
			return nil, 0, fmt.Errorf("reconciliation: scan staff-view mismatch: %w", err)
		}
		m.MismatchKind = MismatchKind(kind)
		out = append(out, m)
	}
	return out, total, rows.Err()
}
