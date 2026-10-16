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
// SR11-1 via SR11-10 - the SEALED cursor. The continuation cursor of a (tenant, provider) pair selects what is
// fetched, so a forged cursor silently blinds the pair. A11 required a backing import (same tenant, provider and
// coverage_end); a SQL-only attacker could forge that import too. The platform already HMAC-seals every import in
// its INSERT (SetDefaultImportSealer; the seal covers import id, tenant, provider, source label, is_mock,
// coverage window, content and lines digest, fetched_at and the importing service), and the key never enters the
// database. With a seal verifier available the cursor is therefore:
//
//	max(coverage_end) over payment_statement run rows of the pair (run_at <= now, coverage_end <= now) whose
//	coverage_end equals that of a stored import of the same tenant and provider that
//	  - carries a seal and a kid,
//	  - was written by this stream (imported_by_service), by THIS source (source_label, is_mock), and
//	  - VERIFIES under the configured keys over the import row and all of its stored lines.
//
// Every run row above that cursor (whatever its run_at or coverage_end) is unbacked: ignored, and reported by the
// durable P1 cursor_anomaly, whose discriminator carries the unbacked-row count and a digest of their ids
// (SR11-11), so a LATER forgery pages again instead of being absorbed into an alert that is already open.
//
// Without a verifier (a non-production process with no PAYOUT_INSTRUMENT_KEYS: imports are then stored unsealed)
// the A11 rule applies unchanged (any backing import). Production cannot reach that mode with a registered
// source: cmd/platform-api refuses to start the scoped sweep without a verifier (VerifyPaymentCursorSealing).
//
// Bounds (an attacker must not be able to make the read unbounded): at most cursorCandidateLimit distinct
// coverage_end values are examined, newest first, with at most cursorImportsPerCandidate sealed imports each.
// When every examined candidate fails and the limit was reached the read FAILS (ErrPaymentCursorUnverifiable,
// a P1 failed run) rather than falling back to a zero cursor.

const (
	cursorCandidateLimit      = 64
	cursorImportsPerCandidate = 4
	// cursorAnomalyDiscMax mirrors the alerts.discriminator CHECK (migration 0110).
	cursorAnomalyDiscMax = 160
)

// ErrPaymentCursorUnverifiable: no examined cursor candidate is backed by an import whose seal verifies, and the
// candidate limit was reached. Fail closed: the run is a P1 failed run and nothing is fetched.
var ErrPaymentCursorUnverifiable = errors.New("reconciliation: payment statement cursor: no sealed, verifying backing import among the newest run rows")

// cursorBasis is what a cursor read needs to know about the source whose cursor it is.
type cursorBasis struct {
	// verifier is nil in the A11 fallback mode (no keys in this process).
	verifier statement.ImportSealVerifier
	label    string
	isMock   bool
}

// cursorState is one consistent read of a pair's cursor and of the run rows that do not back it.
type cursorState struct {
	cursor time.Time
	// unbacked counts the run rows of the pair above the cursor that no verifying import backs.
	unbacked int
	// unbackedDigest is the first 16 hex characters of SHA-256 over the sorted ids of those rows ("" when none).
	unbackedDigest string
	// newestUnbacked is the unbacked row with the greatest (coverage_end, id) (uuid.Nil when none).
	newestUnbacked uuid.UUID
	// sealed reports whether the cursor was read with seal verification (false = A11 fallback).
	sealed bool
}

// importKeysFor returns the keys the stream seals and verifies with: opts.ImportKeys, else the process-wide
// sealer (and, when it can verify, the process-wide verifier).
func importSealerFor(opts PaymentStatementOptions) statement.ImportSealer {
	if opts.ImportKeys != nil {
		return opts.ImportKeys
	}
	return DefaultImportSealer()
}

func importVerifierFor(opts PaymentStatementOptions) statement.ImportSealVerifier {
	if opts.ImportKeys != nil {
		return opts.ImportKeys
	}
	if v, ok := DefaultImportSealer().(statement.ImportSealVerifier); ok {
		return v
	}
	return nil
}

// DefaultCursorVerifierAvailable reports whether the process-wide sealer can also verify, i.e. whether the
// scoped sweep reads SEALED cursors (F2 / SR11-10). cmd/platform-api refuses production startup otherwise.
func DefaultCursorVerifierAvailable() bool {
	_, ok := DefaultImportSealer().(statement.ImportSealVerifier)
	return ok
}

// ErrPaymentCursorSealingUnavailable: production with a registered payment statement source but no import seal
// verifier. The scoped sweep would fall back to the A11 (forgeable) cursor; startup is refused instead.
var ErrPaymentCursorSealingUnavailable = errors.New("reconciliation: payment statement sources are registered but no import seal keys are configured (the continuation cursor cannot be verified; PAYOUT_INSTRUMENT_KEYS)")

// VerifyPaymentCursorSealing is the startup gate (F2 / SR11-10): in production, a non-empty set of registered
// payment statement sources requires the process-wide import sealer to verify seals as well.
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

// queryPaymentCursorState is the cursor read inside tx (the stream's read-only pre-fetch transaction, and again
// under the advisory lock in the match transaction's snapshot).
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
		return st, unbackedAbove(ctx, tx, tenantID, providerID, &st, true)
	}

	rows, err := tx.Query(ctx,
		`SELECT r.coverage_end FROM reconciliation_runs r
		  WHERE r.tenant_id = $1 AND r.stream = $2 AND r.provider_id = $3 AND r.coverage_end IS NOT NULL
		    AND r.run_at <= now() AND r.coverage_end <= now()
		  GROUP BY r.coverage_end
		  ORDER BY r.coverage_end DESC
		  LIMIT $4`, tenantID, string(StreamPaymentStatement), providerID, cursorCandidateLimit)
	if err != nil {
		return cursorState{}, fmt.Errorf("reconciliation: read payment statement cursor candidates: %w", err)
	}
	var candidates []time.Time
	for rows.Next() {
		var c time.Time
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return cursorState{}, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return cursorState{}, err
	}
	found := false
	for _, c := range candidates {
		ok, err := sealedBackingImport(ctx, tx, tenantID, providerID, c, b)
		if err != nil {
			return cursorState{}, err
		}
		if ok {
			st.cursor, found = c.UTC().Truncate(time.Microsecond), true
			break
		}
	}
	if !found && len(candidates) >= cursorCandidateLimit {
		return cursorState{}, fmt.Errorf("%w (%d candidates examined)", ErrPaymentCursorUnverifiable, len(candidates))
	}
	return st, unbackedAbove(ctx, tx, tenantID, providerID, &st, false)
}

// sealedBackingImport reports whether a stored import of the pair with coverage_end c was written by this stream
// and source and carries a seal that verifies over the import and all of its stored lines.
func sealedBackingImport(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, c time.Time, b cursorBasis) (bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM payment_statement_imports
		  WHERE tenant_id = $1 AND provider_id = $2 AND coverage_end = $3
		    AND import_seal IS NOT NULL AND seal_kid IS NOT NULL
		    AND imported_by_service = $4 AND source_label = $5 AND is_mock = $6
		  ORDER BY fetched_at DESC, id
		  LIMIT $7`, tenantID, providerID, c, ImportedByServicePaymentStatement, b.label, b.isMock, cursorImportsPerCandidate)
	if err != nil {
		return false, fmt.Errorf("reconciliation: read cursor backing imports: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, id := range ids {
		if verifyImportSealInTx(ctx, tx, b.verifier, tenantID, id) == nil {
			return true, nil
		}
	}
	return false, nil
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

// unbackedAbove fills st's anomaly evidence: the run rows of the pair above the cursor that do not back it. In
// the sealed mode that is EVERY row above the cursor (the cursor is the newest verifying candidate, and a row
// beyond now or beyond the candidate limit cannot be trusted either); in the A11 fallback it is the rows with no
// backing import at all (the A11 rule, unchanged).
func unbackedAbove(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, st *cursorState, fallback bool) error {
	var floor *time.Time
	if !st.cursor.IsZero() {
		c := st.cursor
		floor = &c
	}
	rows, err := tx.Query(ctx,
		`SELECT r.id, r.coverage_end FROM reconciliation_runs r
		  WHERE r.tenant_id = $1 AND r.stream = $2 AND r.provider_id = $3 AND r.coverage_end IS NOT NULL
		    AND ($4::timestamptz IS NULL OR r.coverage_end > $4)
		    AND (NOT $5 OR NOT EXISTS (SELECT 1 FROM payment_statement_imports i
		                                WHERE i.tenant_id = r.tenant_id AND i.provider_id = r.provider_id AND i.coverage_end = r.coverage_end))
		  ORDER BY r.coverage_end DESC, r.id DESC`,
		tenantID, string(StreamPaymentStatement), providerID, floor, fallback)
	if err != nil {
		return fmt.Errorf("reconciliation: read unbacked payment statement cursor rows: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id uuid.UUID
		var ce time.Time
		if err := rows.Scan(&id, &ce); err != nil {
			return err
		}
		if st.newestUnbacked == uuid.Nil {
			st.newestUnbacked = id
		}
		ids = append(ids, id.String())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	st.unbacked = len(ids)
	if len(ids) > 0 {
		sort.Strings(ids)
		h := sha256.New()
		for _, id := range ids {
			h.Write([]byte(id))
			h.Write([]byte{0})
		}
		st.unbackedDigest = hex.EncodeToString(h.Sum(nil))[:16]
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
