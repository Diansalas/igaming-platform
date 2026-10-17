package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2 (platform-freeze work list): payment_statement stream integrity.
//
// SR11-1 - the SEALED cursor. The continuation cursor of a (tenant, provider) pair selects what is fetched, so a
// forged cursor silently blinds the pair. The runtime role may INSERT both reconciliation_runs and
// payment_statement_imports, so neither row can be trusted on its own. The application therefore seals BOTH in their
// own INSERTs with HMAC keys that never enter the database (payoutinstrument.Keys):
//
//   - the IMPORT seal (since 0125): import id, tenant, provider, source label, is_mock, coverage window, content and
//     lines digest, fetched_at, importing service;
//   - the RUN seal (migration 0168, SRF2-1 / LFF2-1): run id, tenant, provider, stream, the import the run matched
//     (run_import_id), the import's coverage window, window_truncated, run_at.
//
// With keys (a verifier) the cursor is the coverage_end of the newest payment_statement run row of the pair
// (run_at and coverage_end not in the future) whose RUN seal verifies and whose named import was written by this
// stream and THIS source (imported_by_service, source_label, is_mock), has the same coverage window, and whose
// IMPORT seal verifies over the import row and all of its stored lines. A forged run row - also one naming a genuine
// sealed import whose own match never committed (a match skipped by holding the stream's advisory lock, SRF2-1) - is
// never a cursor. Candidates are read as ONE stream (a server-side cursor executed once, SRF2R-1), newest first,
// checking the run seal in Go (one HMAC, no extra read). Forged rows cost a linear scan and are reported; they can
// never move the cursor. A flood larger than CursorScanBudget rows stops THIS read loud
// (ErrPaymentCursorScanBudgetExceeded -> P1 cursor_flood) instead of making the sweep hang: nothing is trusted and
// nothing is fetched, and the next run retries (SRF2-4 / LFF2-4 / SRF2R-1; removal of the forged rows is the
// owner-authorised runbook procedure).
//
// Run rows that do not back the cursor and are newer than it (coverage_end above it, or run_at after the cursor
// run's) are reported by the durable P1 cursor_anomaly, whose discriminator carries their count and a digest of their
// ids (SR11-11). The report reads at most anomalyRowsRead rows plus one count(*).
//
// Without keys (a non-production process with no PAYOUT_INSTRUMENT_KEYS: nothing is sealed) the A11 rule applies
// unchanged (any backing import). Production cannot reach that mode with a registered source:
// VerifyPaymentCursorSealing refuses the start.

const (
	// cursorRunsPerPage is the batch size of the streaming candidate read (one server-side cursor, SRF2R-1).
	cursorRunsPerPage = 512
	// anomalyRowsRead bounds the rows the anomaly report reads (the count is a separate count(*)).
	anomalyRowsRead = 1000
	// cursorAnomalyDiscMax mirrors the alerts.discriminator CHECK (migration 0110).
	cursorAnomalyDiscMax = 160
)

// MatchStallAfter is how long a sealed import of the pair may sit above the trusted cursor (fetched, but its match
// never committed: lock contention, a stale cursor or a failed match) before the stream raises the durable P1
// match_stalled (SRF2-1 (c)): holding the stream's advisory lock becomes a visible stall, never a quiet one.
const MatchStallAfter = 30 * time.Minute

// matchStallAfter is MatchStallAfter; a variable ONLY so a test can shorten it.
var matchStallAfter = MatchStallAfter

// CursorScanBudget bounds the candidate run rows ONE sealed cursor read may scan (SRF2R-1). A genuine pair finds its
// cursor on the first row (the newest verifying run); rows scanned before it are run rows whose seal does not verify
// or whose import does not, i.e. a flood. Past the budget the read stops with ErrPaymentCursorScanBudgetExceeded: no
// cursor is trusted (never a fail-open zero or partial cursor), the run fails loud (P1 phase cursor_flood) and the
// next run retries. The same budget bounds the stall check's progress read.
const CursorScanBudget = 50000

// cursorScanBudget is CursorScanBudget; a variable ONLY so a test can shorten it.
var cursorScanBudget = CursorScanBudget

// ErrPaymentCursorScanBudgetExceeded: a sealed cursor read scanned CursorScanBudget candidate run rows without
// finding a verifying cursor (SRF2R-1, a flood of forged sealed-looking run rows). The run fails as cursor_flood.
var ErrPaymentCursorScanBudgetExceeded = errors.New("reconciliation: payment statement cursor scan budget exceeded (run row flood)")

// streamVerifier verifies both seals the cursor rests on (payoutinstrument.Keys implements it).
type streamVerifier interface {
	statement.ImportSealVerifier
	statement.RunSealVerifier
}

// cursorBasis is what a cursor read needs to know about the source whose cursor it is.
type cursorBasis struct {
	// verifier is nil in the A11 fallback mode (no keys in this process).
	verifier streamVerifier
	label    string
	isMock   bool
}

// cursorState is one consistent read of a pair's cursor and of the run rows that do not back it.
type cursorState struct {
	cursor time.Time
	// cursorRunAt / cursorRunID identify the run row the cursor rests on (sealed mode only).
	cursorRunAt time.Time
	cursorRunID uuid.UUID
	// unbacked counts the run rows of the pair newer than the cursor that do not back it.
	unbacked int
	// unbackedDigest is the first 16 hex characters of SHA-256 over the sorted ids of those rows ("" when none).
	unbackedDigest string
	// newestUnbacked is the unbacked row with the greatest (coverage_end, id) (uuid.Nil when none).
	newestUnbacked uuid.UUID
	// sealed reports whether the cursor was read with seal verification (false = A11 fallback).
	sealed bool
	// rejected counts the sealed-looking candidate run rows whose run seal did not verify (a flood indicator).
	rejected int
	// scanned counts every candidate run row the sealed read scanned (bounded by cursorScanBudget).
	scanned int
}

// importSealerFor returns the keys the stream seals imports with: opts.ImportKeys, else the process-wide sealer.
func importSealerFor(opts PaymentStatementOptions) statement.ImportSealer {
	if opts.ImportKeys != nil {
		return opts.ImportKeys
	}
	return DefaultImportSealer()
}

// runSealerFor returns the run sealer: opts.ImportKeys, else the process-wide sealer when it can seal runs.
func runSealerFor(opts PaymentStatementOptions) statement.RunSealer {
	if opts.ImportKeys != nil {
		return opts.ImportKeys
	}
	if s, ok := DefaultImportSealer().(statement.RunSealer); ok {
		return s
	}
	return nil
}

// importVerifierFor returns the verifier of both seals: opts.ImportKeys, else the process-wide sealer when it can
// verify both (payoutinstrument.Keys); nil = the A11 fallback.
func importVerifierFor(opts PaymentStatementOptions) streamVerifier {
	if opts.ImportKeys != nil {
		return opts.ImportKeys
	}
	if v, ok := DefaultImportSealer().(streamVerifier); ok {
		return v
	}
	return nil
}

// DefaultCursorVerifierAvailable reports whether the process-wide sealer can also verify imports and runs, i.e.
// whether the scoped sweep reads SEALED cursors. cmd/platform-api refuses production startup otherwise.
func DefaultCursorVerifierAvailable() bool {
	_, ok := DefaultImportSealer().(streamVerifier)
	return ok
}

// ErrPaymentCursorSealingUnavailable: production with a registered payment statement source but no seal verifier.
// The scoped sweep would fall back to the A11 (forgeable) cursor; startup is refused instead.
var ErrPaymentCursorSealingUnavailable = errors.New("reconciliation: payment statement sources are registered but no import seal keys are configured (the continuation cursor cannot be verified; PAYOUT_INSTRUMENT_KEYS)")

// VerifyPaymentCursorSealing is the startup gate (F2 / SR11-10): in production, a non-empty set of registered
// payment statement sources requires the process-wide sealer to verify import and run seals as well.
func VerifyPaymentCursorSealing(production bool, registeredSources int) error {
	if production && registeredSources > 0 && !DefaultCursorVerifierAvailable() {
		return ErrPaymentCursorSealingUnavailable
	}
	return nil
}

func cursorBasisFor(src statement.PaymentStatementSource, opts PaymentStatementOptions) cursorBasis {
	b := cursorBasis{verifier: importVerifierFor(opts)}
	if src != nil {
		b.label, b.isMock = src.Label(), isSyntheticSource(src)
	}
	return b
}

// readPaymentCursorState reads the cursor and its anomaly evidence in ONE read-only tenant transaction.
func readPaymentCursorState(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, providerID string, b cursorBasis) (cursorState, error) {
	var st cursorState
	err := pool.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = queryPaymentCursorState(ctx, tx, tenantID, providerID, b)
		return err
	})
	if err != nil {
		return cursorState{}, err
	}
	return st, nil
}

// sealedRun is one candidate run row as the run seal reads it.
type sealedRun struct {
	in        statement.RunSealInput
	seal, kid *string
	// sealOK is set by sealedRunBatch: the run seal verifies.
	sealOK bool
}

// runSealVerifies reports whether r's stored run seal verifies under v.
func runSealVerifies(v statement.RunSealVerifier, r sealedRun) bool {
	if v == nil || r.seal == nil || r.kid == nil {
		return false
	}
	return v.VerifyPaymentRun(r.in, *r.kid, *r.seal) == nil
}

// queryPaymentCursorState is the cursor read inside tx (the stream's read-only pre-fetch transaction, and again
// under the advisory lock in the match transaction's snapshot). now() is the transaction start.
func queryPaymentCursorState(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, b cursorBasis) (cursorState, error) {
	st := cursorState{sealed: b.verifier != nil}
	if b.verifier == nil {
		// A11 fallback: any backing import (same tenant, provider and coverage_end).
		var prev *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT max(r.coverage_end) FROM reconciliation_runs r
			  WHERE r.tenant_id = $1 AND r.stream = $2 AND r.provider_id = $3 AND r.coverage_end IS NOT NULL
			    AND r.run_at <= now() AND r.coverage_end <= now()
			    AND EXISTS (SELECT 1 FROM payment_statement_imports i
			                 WHERE i.tenant_id = r.tenant_id AND i.provider_id = r.provider_id AND i.coverage_end = r.coverage_end)`,
			tenantID, string(StreamPaymentStatement), providerID).Scan(&prev); err != nil {
			return cursorState{}, fmt.Errorf("reconciliation: read payment statement cursor: %w", err)
		}
		if prev != nil {
			st.cursor = prev.UTC().Truncate(time.Microsecond)
		}
		return st, anomalyRows(ctx, tx, tenantID, providerID, &st, nil)
	}

	// Sealed mode: stream the sealed-looking run rows, newest first, through ONE server-side cursor (SRF2R-1: the
	// query is executed once, never re-issued per page), verify each run seal in Go, and check the imports of the
	// verified ones. At most cursorScanBudget rows are scanned: past that the read stops with
	// ErrPaymentCursorScanBudgetExceeded (no cursor is trusted, nothing is fetched; the run fails loud as cursor_flood
	// and the next run retries). See the file comment.
	cur, err := db.DeclareCursor(ctx, tx, "srf2r_payment_cursor_candidates",
		`SELECT r.id, r.run_import_id, r.period_start, r.coverage_end, COALESCE(r.window_truncated, false), r.run_at, r.run_seal, r.run_seal_kid
		   FROM reconciliation_runs r
		  WHERE r.tenant_id = $1 AND r.stream = $2 AND r.provider_id = $3 AND r.run_seal IS NOT NULL
		    AND r.run_at <= now() AND r.coverage_end <= now()
		  ORDER BY r.coverage_end DESC, r.run_at DESC, r.id DESC`, tenantID, string(StreamPaymentStatement), providerID)
	if err != nil {
		return cursorState{}, fmt.Errorf("reconciliation: read payment statement cursor candidates: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	for {
		batch, err := sealedRunBatch(ctx, cur, tenantID, providerID, b.verifier)
		if err != nil {
			return cursorState{}, err
		}
		for _, r := range batch {
			st.scanned++
			if !r.sealOK {
				st.rejected++
				continue
			}
			ok, err := runImportVerifies(ctx, tx, tenantID, providerID, r, b)
			if err != nil {
				return cursorState{}, err
			}
			if ok {
				st.cursor = r.in.CoverageEnd.UTC().Truncate(time.Microsecond)
				st.cursorRunAt, st.cursorRunID = r.in.RunAt.UTC().Truncate(time.Microsecond), r.in.RunID
				if err := cur.Close(ctx); err != nil {
					return cursorState{}, fmt.Errorf("reconciliation: close payment statement cursor candidates: %w", err)
				}
				return st, anomalyRows(ctx, tx, tenantID, providerID, &st, b.verifier)
			}
		}
		if len(batch) < cursorRunsPerPage {
			if err := cur.Close(ctx); err != nil {
				return cursorState{}, fmt.Errorf("reconciliation: close payment statement cursor candidates: %w", err)
			}
			return st, anomalyRows(ctx, tx, tenantID, providerID, &st, b.verifier)
		}
		if st.scanned >= cursorScanBudget {
			return st, fmt.Errorf("%w: %d candidate run rows scanned without a verifying cursor (%d run seals did not verify)",
				ErrPaymentCursorScanBudgetExceeded, st.scanned, st.rejected)
		}
	}
}

// sealedRunBatch fetches the next batch (at most cursorRunsPerPage rows) of a candidate cursor, in order, each with
// whether its run seal verifies under v.
func sealedRunBatch(ctx context.Context, cur *db.ServerCursor, tenantID uuid.UUID, providerID string, v statement.RunSealVerifier) ([]sealedRun, error) {
	rows, err := cur.Fetch(ctx, cursorRunsPerPage)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: read payment statement run candidates: %w", err)
	}
	defer rows.Close()
	var batch []sealedRun
	for rows.Next() {
		r := sealedRun{in: statement.RunSealInput{TenantID: tenantID, ProviderID: providerID, Stream: string(StreamPaymentStatement)}}
		if err := rows.Scan(&r.in.RunID, &r.in.ImportID, &r.in.CoverageStart, &r.in.CoverageEnd, &r.in.WindowTruncated, &r.in.RunAt, &r.seal, &r.kid); err != nil {
			return nil, err
		}
		r.sealOK = runSealVerifies(v, r)
		batch = append(batch, r)
	}
	return batch, rows.Err()
}

// runImportVerifies reports whether the import a (run-seal-verified) run names was written by this stream and source,
// carries the run's coverage window, and has an import seal that verifies over the import and all of its lines.
func runImportVerifies(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, r sealedRun, b cursorBasis) (bool, error) {
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payment_statement_imports
		                 WHERE id = $1 AND tenant_id = $2 AND provider_id = $3 AND coverage_start = $4 AND coverage_end = $5
		                   AND import_seal IS NOT NULL AND seal_kid IS NOT NULL
		                   AND imported_by_service = $6 AND source_label = $7 AND is_mock = $8)`,
		r.in.ImportID, tenantID, providerID, r.in.CoverageStart, r.in.CoverageEnd, ImportedByServicePaymentStatement, b.label, b.isMock).Scan(&ok); err != nil {
		return false, fmt.Errorf("reconciliation: read cursor backing import: %w", err)
	}
	if !ok {
		return false, nil
	}
	return verifyImportSealInTx(ctx, tx, b.verifier, tenantID, r.in.ImportID) == nil, nil
}

// anomalyRows fills st's anomaly evidence: the run rows of the pair that do not back the cursor and are newer than
// it. Sealed mode (v != nil): every row other than the cursor's own whose coverage_end is above the cursor or whose
// run_at is after the cursor run's (so a COPIED row with run_at = now() at an old coverage_end, SRF2-2, is reported
// too), minus any row whose run seal verifies (a genuine re-match of a window). A11 fallback (v == nil): the rows
// above the cursor with no backing import at all (the A11 rule). The count is one count(*); at most anomalyRowsRead
// rows are read for the digest and the newest id (SRF2-4 / LFF2-4: bounded).
func anomalyRows(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, st *cursorState, v streamVerifier) error {
	var floor, after *time.Time
	if !st.cursor.IsZero() {
		c := st.cursor
		floor = &c
	}
	if v != nil && !st.cursorRunAt.IsZero() {
		a := st.cursorRunAt
		after = &a
	}
	const where = `r.tenant_id = $1 AND r.stream = $2 AND r.provider_id = $3 AND r.coverage_end IS NOT NULL AND r.id <> $4
		    AND (($5::timestamptz IS NULL OR r.coverage_end > $5) OR ($6::timestamptz IS NOT NULL AND r.run_at > $6))
		    AND ($7 OR NOT EXISTS (SELECT 1 FROM payment_statement_imports i
		                            WHERE i.tenant_id = r.tenant_id AND i.provider_id = r.provider_id AND i.coverage_end = r.coverage_end))`
	args := []any{tenantID, string(StreamPaymentStatement), providerID, st.cursorRunID, floor, after, v != nil}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs r WHERE `+where, args...).Scan(&total); err != nil {
		return fmt.Errorf("reconciliation: count unbacked payment statement cursor rows: %w", err)
	}
	if total == 0 {
		return nil
	}
	cols := `r.id, r.run_import_id, r.period_start, r.coverage_end, COALESCE(r.window_truncated, false), r.run_at, r.run_seal, r.run_seal_kid`
	if v == nil {
		// The A11 fallback reads no 0168 column (it also runs against schemas below 0168).
		cols = `r.id, NULL::uuid, r.period_start, r.coverage_end, false, r.run_at, NULL::text, NULL::text`
	}
	rows, err := tx.Query(ctx,
		`SELECT `+cols+`
		   FROM reconciliation_runs r WHERE `+where+`
		  ORDER BY r.coverage_end DESC, r.id DESC
		  LIMIT `+strconv.Itoa(anomalyRowsRead), args...)
	if err != nil {
		return fmt.Errorf("reconciliation: read unbacked payment statement cursor rows: %w", err)
	}
	defer rows.Close()
	var ids []string
	genuine := 0
	for rows.Next() {
		r := sealedRun{in: statement.RunSealInput{TenantID: tenantID, ProviderID: providerID, Stream: string(StreamPaymentStatement)}}
		var imp *uuid.UUID
		if err := rows.Scan(&r.in.RunID, &imp, &r.in.CoverageStart, &r.in.CoverageEnd, &r.in.WindowTruncated, &r.in.RunAt, &r.seal, &r.kid); err != nil {
			return err
		}
		if imp != nil {
			r.in.ImportID = *imp
		}
		if v != nil && runSealVerifies(v, r) {
			genuine++ // a sealed run of this process (e.g. a re-matched window): not an anomaly
			continue
		}
		if st.newestUnbacked == uuid.Nil {
			st.newestUnbacked = r.in.RunID
		}
		ids = append(ids, r.in.RunID.String())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	st.unbacked = total - genuine
	if len(ids) > 0 {
		sort.Strings(ids)
		h := sha256.New()
		h.Write([]byte(strconv.Itoa(st.unbacked)))
		h.Write([]byte{0})
		for _, id := range ids {
			h.Write([]byte(id))
			h.Write([]byte{0})
		}
		st.unbackedDigest = hex.EncodeToString(h.Sum(nil))[:16]
	}
	return nil
}

// errImportSealMismatch is verifyImportSealInTx's refusal (never surfaced: a candidate that does not verify is
// simply not a cursor).
var errImportSealMismatch = errors.New("reconciliation: import seal does not verify")

// verifyImportSealInTx re-reads import importID and ALL its lines (line_no order) in tx and verifies the stored
// seal with v. It is the reconciliation-side twin of payoutinstrument.VerifyStatementImportInTx (same canonical
// input, statement.ImportCanon / ImportLinesDigest; parity pinned by a test), written against the verifier
// interface so this package stays free of key material.
func verifyImportSealInTx(ctx context.Context, tx pgx.Tx, v statement.ImportSealVerifier, tenantID, importID uuid.UUID) error {
	if v == nil {
		return errImportSealMismatch
	}
	in := statement.ImportSealInput{ImportID: importID, TenantID: tenantID}
	var seal, kid, service *string
	if err := tx.QueryRow(ctx, `
		SELECT provider_id, source_label, is_mock, coverage_start, coverage_end, content_digest, line_count, fetched_at,
		       payout_lines_carry_merchant_reference, imported_by_service, import_seal, seal_kid
		  FROM payment_statement_imports WHERE id = $1 AND tenant_id = $2`, importID, tenantID).
		Scan(&in.ProviderID, &in.SourceLabel, &in.IsMock, &in.CoverageStart, &in.CoverageEnd, &in.ContentDigest, &in.LineCount,
			&in.FetchedAt, &in.PayoutLinesCarryMerchantReference, &service, &seal, &kid); err != nil {
		return fmt.Errorf("reconciliation: read import %s for seal verification: %w", importID, err)
	}
	if seal == nil || kid == nil || service == nil {
		return errImportSealMismatch
	}
	in.ImportedByService = *service
	rows, err := tx.Query(ctx, `
		SELECT line_no, provider_id, kind, provider_reference, merchant_reference, original_provider_reference, settlement_reference,
		       status, amount::text, asset_code, occurred_at
		  FROM payment_statement_lines WHERE tenant_id = $1 AND import_id = $2 ORDER BY line_no
		  LIMIT $3`, tenantID, importID, statement.MaxPaymentStatementLines+1)
	if err != nil {
		return fmt.Errorf("reconciliation: read import lines for seal verification: %w", err)
	}
	var lines []statement.ImportLineCanon
	for rows.Next() {
		var l statement.ImportLineCanon
		if err := rows.Scan(&l.LineNo, &l.ProviderID, &l.Kind, &l.ProviderReference, &l.MerchantReference, &l.OriginalProviderReference,
			&l.SettlementReference, &l.Status, &l.Amount, &l.AssetCode, &l.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lines) != in.LineCount {
		return errImportSealMismatch
	}
	in.LinesDigest = statement.ImportLinesDigest(lines)
	if err := v.VerifyStatementImport(in, *kid, *seal); err != nil {
		return errImportSealMismatch
	}
	return nil
}

// cursorAnomalyPhase is the alert discriminator tail of SR11-1/SR11-11 (the phase attribute stays cursor_anomaly): "cursor_anomaly:n<count>:<digest>" when it fits the
// discriminator bound, else "cursor_anomaly:<digest>" (the digest alone still changes whenever the set of
// unbacked rows changes). A new forgery therefore opens a NEW alert (it pages again); the same, unchanged set
// keeps one alert with growing occurrences.
func cursorAnomalyPhase(providerID string, st cursorState) string {
	base := "stream:" + string(StreamPaymentStatement) + ":provider:" + discPart(providerID) + ":"
	long := "cursor_anomaly:n" + strconv.Itoa(st.unbacked) + ":" + st.unbackedDigest
	if len(base)+len(long) <= cursorAnomalyDiscMax {
		return long
	}
	short := "cursor_anomaly:" + st.unbackedDigest
	if len(base)+len(short) <= cursorAnomalyDiscMax {
		return short
	}
	return "cursor_anomaly:" + st.unbackedDigest[:8]
}

// paymentSettleLag is the provider's settle lag (F2 / LF-A11-2): the source's declaration when it makes one
// (validated), else DefaultStatementSettleLag for a real source and 0 for a Synthetic (MOCK) one.
func paymentSettleLag(src statement.PaymentStatementSource) (time.Duration, error) {
	if d, ok := src.(statement.SettleLagDeclarer); ok {
		lag := d.StatementSettleLag()
		if lag < 0 || lag > statement.MaxStatementSettleLag {
			return 0, fmt.Errorf("%w: settle lag %s outside [0, %s]", ErrPaymentStatementInvalid, lag, statement.MaxStatementSettleLag)
		}
		return lag, nil
	}
	if isSyntheticSource(src) {
		return 0, nil
	}
	return statement.DefaultStatementSettleLag, nil
}

// ValidatePaymentSettleLag is the registry's startup check of a source's declared settle lag.
func ValidatePaymentSettleLag(src statement.PaymentStatementSource) error {
	_, err := paymentSettleLag(src)
	return err
}

// reportCursorAnomaly is the SR11-1 / SR11-11 report: an error log, an audit row
// (reconciliation.payment_cursor_anomaly: count, newest unbacked run id, digest, whether the cursor was read
// sealed) and the durable P1 reconciliation.run_failed whose phase (and so discriminator) carries the count and
// the digest of the unbacked rows. The runbook compares the audit rows' digests to tell a new forgery from a
// known one.
func reportCursorAnomaly(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, provider, obsStatus string, st cursorState) {
	stream := string(StreamPaymentStatement)
	shown := discPart(provider)
	if logger != nil {
		logger.Error("reconciliation sweep: payment statement cursor ANOMALY (P1): run rows not backed by a verifying import were ignored",
			"tenant_id", tenantID, "stream", stream, "provider_id", shown, "rows", st.unbacked,
			"newest_unbacked_run_id", st.newestUnbacked, "unbacked_digest", st.unbackedDigest, "sealed_cursor", st.sealed)
	}
	if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.payment_cursor_anomaly",
			TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
			Metadata: observationMetadata(obsStatus, map[string]any{"stream": stream, "provider_id": shown,
				"unbacked_rows": st.unbacked, "newest_unbacked_run_id": st.newestUnbacked.String(), "unbacked_digest": st.unbackedDigest,
				"sealed_cursor": st.sealed, "cursor": st.cursor.UTC().Format(time.RFC3339Nano), "severity": "P1"}),
		})
	}); auditErr != nil && logger != nil {
		logger.Error("reconciliation sweep: failed to audit the cursor anomaly", "tenant_id", tenantID, "stream", stream, "error", auditErr)
	}
	raiseRunNotReconciledAs(ctx, pool, tenantID, stream, provider, "cursor_anomaly", cursorAnomalyPhase(provider, st))
}

// matchStallLockWait bounds how long checkMatchStall waits for the stream's advisory lock to be released (by
// another instance's match in progress) before it re-evaluates and raises (SRF2R-2). A variable ONLY so a test can
// shorten it.
var matchStallLockWait = 15 * time.Second

// matchStallLockPoll is the polling interval of that wait.
const matchStallLockPoll = 50 * time.Millisecond

// stallEvidence is one evaluation of the stall rule.
type stallEvidence struct {
	n      int
	oldest *time.Time
	// progressAt is the run_at of the newest committed run of this source that the rule accepts as progress (zero
	// when none); budgetHit reports that the progress read stopped at cursorScanBudget (then nothing counts as
	// progress: fail closed, the stall is raised).
	progressAt time.Time
	budgetHit  bool
}

// checkMatchStall reports a stalled pair (SRF2-1 (c), SRF2R-2). It runs at the END of every continuation sweep of the
// pair (after this sweep's own match, whatever its outcome). An import of THIS source (sealed, when the cursor is read
// sealed) whose coverage_end lies above the trusted cursor read at the start of the sweep, and that was fetched more
// than MatchStallAfter ago, is STALLED only when no committed run of this source has a run_at after the import's
// fetched_at: with keys, a run whose RUN seal verifies (only the application can write one) and whose import is of
// this source; without keys (non-production), a run row backed by an import of this source (the A11 rule). Any such
// run means the stream progressed after the fetch and the orphan window has been, or is being, re-covered: benign
// multi-instance contention is not a stall. When the rule still finds a stalled import, the check waits (at most
// matchStallLockWait, polling pg_locks, never taking the lock) for the stream's advisory lock to be released - another
// instance's match may be committing right now - and evaluates once more. A continuously held lock commits no run, so
// it still trips: durable P1 reconciliation.run_failed (phase match_stalled) plus an audit row.
func checkMatchStall(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, provider, obsStatus string, b cursorBasis, st cursorState) error {
	ev, err := evaluateMatchStall(ctx, pool, tenantID, provider, b, st)
	if err != nil || ev.n == 0 {
		return err
	}
	waited, err := waitStreamLockFree(ctx, pool, tenantID)
	if err != nil {
		return err
	}
	if ev, err = evaluateMatchStall(ctx, pool, tenantID, provider, b, st); err != nil || ev.n == 0 {
		return err
	}
	stream := string(StreamPaymentStatement)
	shown := discPart(provider)
	progress := ""
	if !ev.progressAt.IsZero() {
		progress = ev.progressAt.UTC().Format(time.RFC3339Nano)
	}
	if logger != nil {
		logger.Error("reconciliation sweep: payment statement match STALLED (P1): imports above the trusted cursor were never matched and no run committed since they were fetched",
			"tenant_id", tenantID, "stream", stream, "provider_id", shown, "imports", ev.n, "oldest_fetched_at", ev.oldest,
			"last_progress_run_at", progress, "progress_scan_budget_exceeded", ev.budgetHit, "lock_still_held", !waited)
	}
	if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.payment_match_stalled",
			TargetType: "tenant", TargetID: tenantID.String(), Outcome: audit.OutcomeFailure,
			Metadata: observationMetadata(obsStatus, map[string]any{"stream": stream, "provider_id": shown, "unmatched_imports": ev.n,
				"cursor": st.cursor.UTC().Format(time.RFC3339Nano), "last_progress_run_at": progress,
				"progress_scan_budget_exceeded": ev.budgetHit, "lock_still_held": !waited, "severity": "P1"}),
		})
	}); auditErr != nil && logger != nil {
		logger.Error("reconciliation sweep: failed to audit the match stall", "tenant_id", tenantID, "stream", stream, "error", auditErr)
	}
	raiseRunNotReconciled(ctx, pool, tenantID, stream, provider, "match_stalled")
	return nil
}

// evaluateMatchStall applies the stall rule once, in its own short read-only transaction.
func evaluateMatchStall(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, provider string, b cursorBasis, st cursorState) (stallEvidence, error) {
	var floor *time.Time
	if !st.cursor.IsZero() {
		c := st.cursor
		floor = &c
	}
	const candidates = `FROM payment_statement_imports
		  WHERE tenant_id = $1 AND provider_id = $2 AND imported_by_service = $3 AND source_label = $4 AND is_mock = $5
		    AND ($6::timestamptz IS NULL OR coverage_end > $6)
		    AND (NOT $7 OR import_seal IS NOT NULL)
		    AND fetched_at < now() - make_interval(secs => $8)`
	var ev stallEvidence
	err := pool.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		args := []any{tenantID, provider, ImportedByServicePaymentStatement, b.label, b.isMock, floor, b.verifier != nil, matchStallAfter.Seconds()}
		var minFetched *time.Time
		if err := tx.QueryRow(ctx, `SELECT min(fetched_at) `+candidates, args...).Scan(&minFetched); err != nil {
			return err
		}
		if minFetched == nil {
			return nil
		}
		var err error
		if ev.progressAt, ev.budgetHit, err = streamProgressAfter(ctx, tx, tenantID, provider, b, *minFetched); err != nil {
			return err
		}
		var progress *time.Time
		if !ev.progressAt.IsZero() {
			p := ev.progressAt
			progress = &p
		}
		return tx.QueryRow(ctx, `SELECT count(*), min(fetched_at) `+candidates+` AND ($9::timestamptz IS NULL OR fetched_at >= $9)`,
			append(args, progress)...).Scan(&ev.n, &ev.oldest)
	})
	if err != nil {
		return stallEvidence{}, fmt.Errorf("reconciliation: read unmatched payment statement imports: %w", err)
	}
	return ev, nil
}

// streamProgressAfter returns the run_at of the newest committed run of this source with run_at after `after` (and
// not in the future) that counts as progress (see checkMatchStall), or zero. With keys the rows are streamed newest
// first through one server-side cursor and the first whose run seal verifies wins; at most cursorScanBudget rows are
// scanned, past that budgetHit is true and zero is returned (fail closed: nothing counts as progress).
func streamProgressAfter(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, provider string, b cursorBasis, after time.Time) (time.Time, bool, error) {
	if b.verifier == nil {
		var at *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT max(r.run_at) FROM reconciliation_runs r
			  WHERE r.tenant_id = $1 AND r.stream = $2 AND r.provider_id = $3 AND r.coverage_end IS NOT NULL
			    AND r.run_at > $4 AND r.run_at <= now()
			    AND EXISTS (SELECT 1 FROM payment_statement_imports i
			                 WHERE i.tenant_id = r.tenant_id AND i.provider_id = r.provider_id AND i.coverage_end = r.coverage_end
			                   AND i.imported_by_service = $5 AND i.source_label = $6 AND i.is_mock = $7)`,
			tenantID, string(StreamPaymentStatement), provider, after, ImportedByServicePaymentStatement, b.label, b.isMock).Scan(&at); err != nil {
			return time.Time{}, false, err
		}
		if at == nil {
			return time.Time{}, false, nil
		}
		return *at, false, nil
	}
	cur, err := db.DeclareCursor(ctx, tx, "srf2r_payment_stall_progress",
		`SELECT r.id, r.run_import_id, r.period_start, r.coverage_end, COALESCE(r.window_truncated, false), r.run_at, r.run_seal, r.run_seal_kid
		   FROM reconciliation_runs r
		   JOIN payment_statement_imports i ON i.id = r.run_import_id AND i.tenant_id = r.tenant_id
		  WHERE r.tenant_id = $1 AND r.stream = $2 AND r.provider_id = $3 AND r.run_seal IS NOT NULL
		    AND r.run_at > $4 AND r.run_at <= now()
		    AND i.provider_id = $3 AND i.imported_by_service = $5 AND i.source_label = $6 AND i.is_mock = $7
		  ORDER BY r.run_at DESC, r.id DESC`,
		tenantID, string(StreamPaymentStatement), provider, after, ImportedByServicePaymentStatement, b.label, b.isMock)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = cur.Close(ctx) }()
	scanned := 0
	for {
		batch, err := sealedRunBatch(ctx, cur, tenantID, provider, b.verifier)
		if err != nil {
			return time.Time{}, false, err
		}
		for _, r := range batch {
			if r.sealOK {
				return r.in.RunAt, false, nil
			}
		}
		scanned += len(batch)
		if len(batch) < cursorRunsPerPage {
			return time.Time{}, false, nil
		}
		if scanned >= cursorScanBudget {
			return time.Time{}, true, nil
		}
	}
}

// waitStreamLockFree waits, at most matchStallLockWait, until no session holds the payment_statement stream's advisory
// lock of the tenant (read from pg_locks; the lock is never taken here, so no other instance's match is ever skipped
// because of this check). It reports whether the lock was observed free.
func waitStreamLockFree(ctx context.Context, pool *db.Pool, tenantID uuid.UUID) (bool, error) {
	deadline := time.Now().Add(matchStallLockWait)
	for {
		var held bool
		if err := pool.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_locks l
				                 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
				                   AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				                   AND ((l.classid::bigint << 32) | l.objid::bigint) = hashtextextended('reconciliation:payment_statement:' || $1::text, 0))`,
				tenantID).Scan(&held)
		}); err != nil {
			return false, fmt.Errorf("reconciliation: read the payment statement stream lock: %w", err)
		}
		if !held {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(matchStallLockPoll):
		}
	}
}

// reportMatchSkipped audits a skipped match (lock contention or a stale cursor) that left importID without a run.
func reportMatchSkipped(ctx context.Context, pool *db.Pool, logger *slog.Logger, tenantID uuid.UUID, provider, obsStatus string, importID uuid.UUID) {
	stream := string(StreamPaymentStatement)
	shown := discPart(provider)
	if logger != nil {
		logger.Warn("reconciliation sweep: payment statement match SKIPPED; the import stays unmatched (the next sweep re-fetches from the cursor)",
			"tenant_id", tenantID, "stream", stream, "provider_id", shown, "import_id", importID)
	}
	if auditErr := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "reconciliation.payment_match_skipped",
			TargetType: "payment_statement_import", TargetID: importID.String(), Outcome: audit.OutcomeFailure,
			Metadata: observationMetadata(obsStatus, map[string]any{"stream": stream, "provider_id": shown, "import_id": importID.String()}),
		})
	}); auditErr != nil && logger != nil {
		logger.Error("reconciliation sweep: failed to audit the skipped match", "tenant_id", tenantID, "stream", stream, "error", auditErr)
	}
}
