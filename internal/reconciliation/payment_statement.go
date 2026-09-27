package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// Payment statement reconciliation stream (PRH-I5; ADR 0095 §12, §13.3,
// §16.3; reconciliation-model.md §2.2 as amended by ledger-finance, LF-Q3).
//
// The wallet <-> PSP stream. Three phases, never one transaction:
//
//	1. Fetch  (FetchPaymentStatement)  NO transaction held. Provider I/O
//	          (INV-IO-1): refused under txscope.Held. The source resolves the
//	          tenant's own outbound credential through the payments
//	          provider-call gate. The result is validated in full - line
//	          cap (S95-C11), every field's length/charset, every line's
//	          provider equal to the source's provider (INV-IO-14) - and a
//	          statement that fails any check is refused whole: nothing is
//	          stored.
//	2. Ingest (IngestPaymentStatement) one short tenant transaction:
//	          payment_statement_imports + payment_statement_lines (migration
//	          0102, append-only by trigger), idempotent on (tenant, provider,
//	          label, coverage, content digest).
//	3. Match  (RunPaymentStatement)     REPEATABLE READ (required and
//	          enforced) under the per-tenant advisory lock: reads the STORED
//	          lines, payment_attempts, unresolved payment_provider_events
//	          and the ledger in one snapshot, and writes exactly one
//	          reconciliation_runs row plus its reconciliation_mismatches
//	          (persistRun). Nothing else - never the ledger, a balance, a
//	          projection, an attempt, an intent or a receipt (INV-IO-12).
//
// This is the CAS-STMT-IO-1 remediation applied to payments: the casino
// stream's source reads inside the run tx, which is correct only for a
// MOCK that renders DB rows; a payment source is provider I/O.
//
// Classification (ADR 0095 §12.3), every mismatch a P1, zero tolerance,
// amounts compared as big.Int:
//
//	pay_duplicate               >1 line per (provider, reference, kind) in
//	                            one import (reported once per key); one
//	                            attempt matched by more than one line; >1
//	                            succeeded attempt for one deposit intent.
//	pay_missing_platform_record a line resolving to no attempt of the same
//	                            provider (by provider reference, a payout's
//	                            settlement reference, or merchant
//	                            reference); a deposit_reversal line with no
//	                            ledger reversal and no tombstone; a ledger
//	                            deposit / withdrawal_completed of the
//	                            provider in the coverage window that maps to
//	                            no (or more than one) succeeded attempt
//	                            (LF95-C13).
//	pay_reference_mismatch      resolved by merchant reference only, while
//	                            the attempt holds a different provider
//	                            reference.
//	pay_asset_mismatch          asset differs (checked before amount).
//	pay_amount_mismatch         amount differs.
//	pay_status_mismatch         provider succeeded/reversed vs platform not
//	                            succeeded (the LF-C1(b) "provider-settled,
//	                            platform-unposted" case); platform succeeded
//	                            vs provider declined; a posted reversal vs a
//	                            provider reversal line still pending or
//	                            declined; a succeeded attempt without
//	                            exactly its one ledger posting (LF95-C13).
//	pay_missing_provider_record a succeeded attempt in the coverage window
//	                            that no line matches.
//	pay_unresolved              an attempt still submitting/pending/
//	                            ambiguous past the horizon (line absent,
//	                            pending or declined); a deferred verified
//	                            callback receipt still unresolved past the
//	                            horizon (LF95-C5). The ONLY age-gated
//	                            checks. Age is measured against the
//	                            import's coverage_end, never the wall
//	                            clock, so a run is deterministic for a
//	                            given import and database snapshot.
//
// Coverage window: platform records outside [coverage_start,
// coverage_end) are never flagged as missing (a restarted MOCK provider
// starts a new window, so pre-restart history is never a false P1). A
// line is always matched, whatever its time.
//
// Remediation is never through this stream (ADR 0095 §12.5): a
// pay_status_mismatch (provider succeeded) converges only through fresh
// provider evidence - a T17 re-verify request on the named attempt, then
// the payments sweeper's QueryStatus -> applyEvidence -> T7 posting under
// the (provider_id, provider_tx_id) unique constraint. The automatic
// payments-owned re-drive job (LF95-R1) is NOT IMPLEMENTED (deferred,
// see the ADR 0095 implementation record); an operator T17 is the path
// today. Every other kind escalates; any credit goes only through
// LEDGER-MANUAL-ADJ-4EYES-1 (BLOCKED).
//
// Interim exclusion (ADR 0095 §12.7.1, ledger-finance ruling on code
// review F1): until the payments cutover, a deposit / withdrawal_completed
// posting linked from an intent or withdrawal request with NO
// payment_attempts row (the live legacy path) is counted as
// legacy_unattempted instead of raising pay_missing_platform_record. It
// retires itself once every new intent/request carries an attempt.
//
// Disclosed limits:
//   - disputed attempts are excluded from status comparison: T10/T13t/T14
//     already made them a payments P1 with no automated remedy, and the
//     stream does not second-guess that terminal decision.
//   - declined attempts absent from the statement are not flagged
//     (whether a real statement lists declines is PROVIDER DEPENDENT).
//   - provider-succeeded vs platform-in-flight is NOT age-gated (ADR
//     §12.3 as written). A real, near-real-time source may need a short
//     grace for callback latency; PROVIDER DEPENDENT, decided with the
//     first real source.
//   - O(tenant history) per run, like every other stream
//     (CAS-RECON-SCALE-1).

// StreamPaymentStatement is the stream name on reconciliation_runs.
const StreamPaymentStatement Stream = "payment_statement"

// The payment_statement mismatch kinds (migration 0102).
const (
	MismatchKindPayMissingPlatformRecord MismatchKind = "pay_missing_platform_record"
	MismatchKindPayMissingProviderRecord MismatchKind = "pay_missing_provider_record"
	MismatchKindPayAmountMismatch        MismatchKind = "pay_amount_mismatch"
	MismatchKindPayAssetMismatch         MismatchKind = "pay_asset_mismatch"
	MismatchKindPayReferenceMismatch     MismatchKind = "pay_reference_mismatch"
	MismatchKindPayStatusMismatch        MismatchKind = "pay_status_mismatch"
	MismatchKindPayDuplicate             MismatchKind = "pay_duplicate"
	MismatchKindPayUnresolved            MismatchKind = "pay_unresolved"
)

// paymentAssetCodeRE is the asset code shape a statement line may carry
// (security C2): the Asset registry's codes are upper-case alphanumerics.
var paymentAssetCodeRE = regexp.MustCompile(`^[A-Z0-9]{1,16}$`)

// PaymentCoverageMaxClockSkew bounds how far past the fetch time a
// statement's CoverageEnd may lie (code review F5): a later end would move
// the unresolved horizon forward and flag in-flight attempts early.
const PaymentCoverageMaxClockSkew = 5 * time.Minute

// DefaultPaymentUnresolvedHorizon is the age past which an in-flight
// attempt or a deferred receipt becomes pay_unresolved. It equals
// payments.DefaultSettlementWindow (ADR 0095 §7.3; asserted equal by a
// test - this package cannot import internal/payments).
const DefaultPaymentUnresolvedHorizon = 24 * time.Hour

// PaymentStatementOptions tunes the stream. The zero value means the
// defaults.
type PaymentStatementOptions struct {
	// MaxLines is the per-import line cap (default and ceiling:
	// statement.MaxPaymentStatementLines, which migration 0102's CHECK
	// also enforces). A smaller value is for tests only.
	MaxLines int
	// UnresolvedHorizon defaults to DefaultPaymentUnresolvedHorizon.
	UnresolvedHorizon time.Duration
}

func (o PaymentStatementOptions) maxLines() int {
	if o.MaxLines <= 0 || o.MaxLines > statement.MaxPaymentStatementLines {
		return statement.MaxPaymentStatementLines
	}
	return o.MaxLines
}

func (o PaymentStatementOptions) horizon() time.Duration {
	if o.UnresolvedHorizon <= 0 {
		return DefaultPaymentUnresolvedHorizon
	}
	return o.UnresolvedHorizon
}

// Fetch/ingest refusals. Each is a P1 when it fires in the sweep (audited
// as a failed run and logged at Error level).
var (
	ErrPaymentFetchUnderTx        = errors.New("reconciliation: payment statement fetch refused: a database transaction is held (INV-IO-1)")
	ErrPaymentStatementTooLarge   = errors.New("reconciliation: payment statement refused: line count above the per-import cap (S95-C11)")
	ErrPaymentStatementInvalid    = errors.New("reconciliation: payment statement refused: invalid content")
	ErrPaymentStatementSourceNone = errors.New("reconciliation: payment_statement requires a statement source")
)

// syntheticComponent is providerkind.Synthetic, restated structurally so
// this package need not import providerkind (the stream only reads the
// marker, to set is_mock and demand "MOCK" in the label).
type syntheticComponent interface{ SyntheticComponent() }

func isSyntheticSource(src statement.PaymentStatementSource) bool {
	_, ok := src.(syntheticComponent)
	return ok
}

func validateSourceIdentity(src statement.PaymentStatementSource) error {
	if src == nil {
		return ErrPaymentStatementSourceNone
	}
	label := src.Label()
	if label == "" || len(label) > 256 || !utf8.ValidString(label) {
		return fmt.Errorf("%w: source label must be 1..256 bytes of UTF-8", ErrPaymentStatementInvalid)
	}
	if isSyntheticSource(src) && !strings.Contains(label, "MOCK") {
		return fmt.Errorf("%w: a synthetic source's label must contain MOCK", ErrPaymentStatementInvalid)
	}
	if err := providerref.Validate("provider_id", src.ProviderID()); err != nil {
		return fmt.Errorf("%w: source provider id: %w", ErrPaymentStatementInvalid, err)
	}
	return nil
}

// FetchPaymentStatement is phase 1: it calls source.Fetch with NO
// transaction held and validates the result in full. A refusal stores
// nothing. Times are truncated to microseconds (the database's
// precision) so the content digest of a re-fetch is stable.
func FetchPaymentStatement(ctx context.Context, source statement.PaymentStatementSource, tenantID uuid.UUID, periodStart, periodEnd time.Time, opts PaymentStatementOptions) (statement.PaymentStatement, error) {
	if txscope.Held(ctx) {
		return statement.PaymentStatement{}, ErrPaymentFetchUnderTx
	}
	if err := validateSourceIdentity(source); err != nil {
		return statement.PaymentStatement{}, err
	}
	providerID := source.ProviderID()
	stmt, err := source.Fetch(ctx, statement.PaymentFetchRequest{
		TenantID: tenantID, ProviderID: providerID, PeriodStart: periodStart, PeriodEnd: periodEnd,
	})
	if errors.Is(err, statement.ErrPaymentStatementBodyTooLarge) || errors.Is(err, statement.ErrPaymentStatementTooManyLines) {
		// Security C1: the source hit a streaming cap; same refusal as the
		// stream's own line cap (nothing stored, P1).
		return statement.PaymentStatement{}, fmt.Errorf("%w: source %q: %w", ErrPaymentStatementTooLarge, source.Label(), err)
	}
	if err != nil {
		return statement.PaymentStatement{}, fmt.Errorf("reconciliation: payment statement source %q: %w", source.Label(), err)
	}
	if n := len(stmt.Lines); n > opts.maxLines() {
		return statement.PaymentStatement{}, fmt.Errorf("%w: %d lines, cap %d (source %q)", ErrPaymentStatementTooLarge, n, opts.maxLines(), source.Label())
	}
	stmt.CoverageStart = stmt.CoverageStart.UTC().Truncate(time.Microsecond)
	stmt.CoverageEnd = stmt.CoverageEnd.UTC().Truncate(time.Microsecond)
	if stmt.CoverageStart.IsZero() || !stmt.CoverageEnd.After(stmt.CoverageStart) {
		return statement.PaymentStatement{}, fmt.Errorf("%w: coverage window [%s, %s) is empty", ErrPaymentStatementInvalid, stmt.CoverageStart, stmt.CoverageEnd)
	}
	if limit := time.Now().UTC().Add(PaymentCoverageMaxClockSkew); stmt.CoverageEnd.After(limit) {
		return statement.PaymentStatement{}, fmt.Errorf("%w: coverage end %s is later than the fetch time plus %s", ErrPaymentStatementInvalid, stmt.CoverageEnd, PaymentCoverageMaxClockSkew)
	}
	for i := range stmt.Lines {
		l := &stmt.Lines[i]
		l.OccurredAt = l.OccurredAt.UTC().Truncate(time.Microsecond)
		if err := validatePaymentLine(providerID, *l); err != nil {
			return statement.PaymentStatement{}, fmt.Errorf("%w: line %d: %w", ErrPaymentStatementInvalid, i, err)
		}
	}
	return stmt, nil
}

func validatePaymentLine(providerID string, l statement.PaymentStatementLine) error {
	if l.ProviderID != providerID {
		// INV-IO-14 / S95-C1: a source speaks for its own provider only.
		return fmt.Errorf("provider_id is not the source's provider")
	}
	if err := providerref.Validate("provider_reference", l.ProviderReference); err != nil {
		return err
	}
	if err := providerref.ValidateOptional("original_provider_reference", l.OriginalProviderReference); err != nil {
		return err
	}
	if err := providerref.ValidateOptional("settlement_reference", l.SettlementReference); err != nil {
		return err
	}
	// Security C2 (rv-prh-i5-security.md): the providerref rule (valid
	// UTF-8, no C0/DEL/C1 control character - so no NUL that would fail the
	// text insert and force a P1 every run, and no newline/escape into the
	// mismatch detail strings) plus the 64-byte merchant bound.
	if err := providerref.ValidateOptional("merchant_reference", l.MerchantReference); err != nil {
		return err
	}
	if len(l.MerchantReference) > 64 {
		return fmt.Errorf("merchant_reference must be at most 64 bytes")
	}
	if !paymentAssetCodeRE.MatchString(l.AssetCode) {
		return fmt.Errorf("asset_code must match %s", paymentAssetCodeRE)
	}
	switch l.Kind {
	case statement.PaymentLineDeposit, statement.PaymentLineDepositReversal, statement.PaymentLinePayout:
	default:
		return fmt.Errorf("unknown kind")
	}
	switch l.Status {
	case statement.PaymentStatusPending, statement.PaymentStatusSucceeded, statement.PaymentStatusDeclined, statement.PaymentStatusReversed:
	default:
		return fmt.Errorf("unknown status")
	}
	if l.Amount < 0 {
		return fmt.Errorf("negative amount")
	}
	if l.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	return nil
}

// paymentStatementDigest is SHA-256 over a length-prefixed canonical
// encoding of the coverage window and every line in order.
func paymentStatementDigest(stmt statement.PaymentStatement) []byte {
	h := sha256.New()
	var buf [8]byte
	putInt := func(v int64) {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}
	putStr := func(s string) {
		putInt(int64(len(s)))
		h.Write([]byte(s))
	}
	putStr("igaming/payment-statement/v1")
	putInt(stmt.CoverageStart.UnixMicro())
	putInt(stmt.CoverageEnd.UnixMicro())
	putInt(int64(len(stmt.Lines)))
	for _, l := range stmt.Lines {
		putStr(l.ProviderID)
		putStr(l.Kind)
		putStr(l.ProviderReference)
		putStr(l.MerchantReference)
		putStr(l.OriginalProviderReference)
		putStr(l.SettlementReference)
		putStr(l.Status)
		putInt(l.Amount)
		putStr(l.AssetCode)
		putInt(l.OccurredAt.UnixMicro())
	}
	return h.Sum(nil)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// IngestPaymentStatement is phase 2: it stores a statement FetchPaymentStatement
// already validated, inside tx (a short db.Pool.WithTenant transaction). It
// returns the import id; reused is true when the identical content was
// already stored (nothing new is written).
func IngestPaymentStatement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, source statement.PaymentStatementSource, stmt statement.PaymentStatement, fetchedAt time.Time) (importID uuid.UUID, reused bool, err error) {
	if err := validateSourceIdentity(source); err != nil {
		return uuid.Nil, false, err
	}
	if len(stmt.Lines) > statement.MaxPaymentStatementLines {
		return uuid.Nil, false, ErrPaymentStatementTooLarge
	}
	providerID, label := source.ProviderID(), source.Label()
	digest := paymentStatementDigest(stmt)
	newID := uuid.New()
	err = tx.QueryRow(ctx,
		`INSERT INTO payment_statement_imports
		    (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (tenant_id, provider_id, source_label, coverage_start, coverage_end, content_digest) DO NOTHING
		 RETURNING id`,
		newID, tenantID, providerID, label, isSyntheticSource(source), stmt.CoverageStart, stmt.CoverageEnd,
		len(stmt.Lines), digest, fetchedAt.UTC(),
	).Scan(&importID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx,
			`SELECT id FROM payment_statement_imports
			  WHERE tenant_id = $1 AND provider_id = $2 AND source_label = $3
			    AND coverage_start = $4 AND coverage_end = $5 AND content_digest = $6`,
			tenantID, providerID, label, stmt.CoverageStart, stmt.CoverageEnd, digest,
		).Scan(&importID); err != nil {
			return uuid.Nil, false, fmt.Errorf("reconciliation: find existing payment statement import: %w", err)
		}
		return importID, true, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("reconciliation: insert payment statement import: %w", err)
	}
	// Batched multi-row INSERTs (COPY is not available under row-level
	// security). The statement-level count trigger runs once per batch.
	const batch = 5000
	for start := 0; start < len(stmt.Lines); start += batch {
		end := min(start+batch, len(stmt.Lines))
		n := end - start
		ids, lineNos, amounts := make([]uuid.UUID, n), make([]int32, n), make([]int64, n)
		providers, kinds, refs, statuses, assets := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		merchants, originals, settlements := make([]*string, n), make([]*string, n), make([]*string, n)
		occurred := make([]time.Time, n)
		for i := 0; i < n; i++ {
			l := stmt.Lines[start+i]
			ids[i], lineNos[i] = uuid.New(), int32(start+i)
			providers[i], kinds[i], refs[i], statuses[i], assets[i] = l.ProviderID, l.Kind, l.ProviderReference, l.Status, l.AssetCode
			merchants[i], originals[i], settlements[i] = nullIfEmpty(l.MerchantReference), nullIfEmpty(l.OriginalProviderReference), nullIfEmpty(l.SettlementReference)
			amounts[i], occurred[i] = l.Amount, l.OccurredAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_statement_lines
			    (id, tenant_id, import_id, line_no, provider_id, kind, provider_reference,
			     merchant_reference, original_provider_reference, settlement_reference,
			     status, amount, asset_code, occurred_at)
			SELECT u.id, $1, $2, u.line_no, u.provider_id, u.kind, u.provider_reference,
			       u.merchant_reference, u.original_provider_reference, u.settlement_reference,
			       u.status, u.amount, u.asset_code, u.occurred_at
			  FROM unnest($3::uuid[], $4::int[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[],
			              $11::text[], $12::bigint[], $13::text[], $14::timestamptz[])
			       AS u(id, line_no, provider_id, kind, provider_reference, merchant_reference, original_provider_reference,
			            settlement_reference, status, amount, asset_code, occurred_at)`,
			tenantID, importID, ids, lineNos, providers, kinds, refs, merchants, originals, settlements,
			statuses, amounts, assets, occurred,
		); err != nil {
			return uuid.Nil, false, fmt.Errorf("reconciliation: store payment statement lines: %w", err)
		}
	}
	return importID, false, nil
}

// PaymentStatementInfo describes one run's stored statement, for the run's
// audit metadata. It is never a mismatch.
type PaymentStatementInfo struct {
	ImportID                   uuid.UUID
	ProviderID                 string
	SourceLabel                string
	IsMock                     bool
	CoverageStart, CoverageEnd time.Time
	Lines                      int
	ImportReused               bool
	// LegacyUnattempted counts deposit / withdrawal_completed postings in
	// the window that the LF95-C13 ledger join did NOT flag because they
	// come from the legacy, pre-cutover path: the posting is linked from a
	// deposit intent or withdrawal request that has no payment_attempts row
	// at all. Information, never a mismatch; see checkLedgerJoin.
	LegacyUnattempted int
}

// AuditMetadata renders i for the run's audit record (ADR 0095 §12.1
// step 4).
func (i PaymentStatementInfo) AuditMetadata() map[string]any {
	return map[string]any{
		"import_id":        i.ImportID.String(),
		"provider_id":      i.ProviderID,
		"statement_source": i.SourceLabel,
		"is_mock":          i.IsMock,
		"coverage_start":   i.CoverageStart.UTC().Format(time.RFC3339Nano),
		"coverage_end":     i.CoverageEnd.UTC().Format(time.RFC3339Nano),
		"statement_lines":  i.Lines,
		"import_reused":    i.ImportReused,
		// Disclosed interim exclusion (ADR 0095 §12.7): expected to stop
		// growing once live deposits and withdrawals create attempts.
		"legacy_unattempted": i.LegacyUnattempted,
	}
}

// requireSnapshotIsolationFor fails closed unless tx reads one snapshot
// for its whole life.
func requireSnapshotIsolationFor(ctx context.Context, tx pgx.Tx, stream Stream) error {
	var level string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&level); err != nil {
		return fmt.Errorf("reconciliation: read transaction isolation: %w", err)
	}
	if level != "repeatable read" && level != "serializable" {
		return fmt.Errorf("reconciliation: %s requires a REPEATABLE READ transaction (db.Pool.WithTenantSnapshot), got %q", stream, level)
	}
	return nil
}

// RunPaymentStatement is phase 3: it matches the stored import importID
// for tenantID inside tx (db.Pool.WithTenantSnapshot - REPEATABLE READ is
// required and enforced), recording exactly one Run and its mismatches
// atomically. The run's period is the import's coverage window.
func RunPaymentStatement(ctx context.Context, tx pgx.Tx, tenantID, importID uuid.UUID, opts PaymentStatementOptions) (Run, []Mismatch, PaymentStatementInfo, error) {
	if err := requireSnapshotIsolationFor(ctx, tx, StreamPaymentStatement); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, err
	}
	return runPaymentStatementUnchecked(ctx, tx, tenantID, importID, opts)
}

// payMatchAfterAttemptsHook, when non-nil, runs between the match's
// attempt read and its ledger read. TEST-ONLY (the snapshot-consistency
// test commits a concurrent posting there); always nil in production.
var payMatchAfterAttemptsHook func()

// runPaymentStatementUnchecked is RunPaymentStatement without the isolation
// check. Only RunPaymentStatement and the snapshot test's READ COMMITTED
// kill control call it.
func runPaymentStatementUnchecked(ctx context.Context, tx pgx.Tx, tenantID, importID uuid.UUID, opts PaymentStatementOptions) (Run, []Mismatch, PaymentStatementInfo, error) {
	info := PaymentStatementInfo{ImportID: importID}
	var lineCount int
	if err := tx.QueryRow(ctx,
		`SELECT provider_id, source_label, is_mock, coverage_start, coverage_end, line_count
		   FROM payment_statement_imports WHERE id = $1 AND tenant_id = $2`,
		importID, tenantID,
	).Scan(&info.ProviderID, &info.SourceLabel, &info.IsMock, &info.CoverageStart, &info.CoverageEnd, &lineCount); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: load payment statement import %s: %w", importID, err)
	}

	m := &payMatcher{
		tenantID: tenantID, provider: info.ProviderID,
		label: "[" + info.SourceLabel + "] ",
		cs:    info.CoverageStart, ce: info.CoverageEnd,
		agedBefore: info.CoverageEnd.Add(-opts.horizon()),
		r:          &sbRecorder{tenantID: tenantID},
	}
	lines, err := m.loadLines(ctx, tx, importID)
	if err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: load payment statement lines: %w", err)
	}
	if len(lines) != lineCount {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: payment statement import %s declares %d lines, %d stored", importID, lineCount, len(lines))
	}
	info.Lines = len(lines)
	if err := m.loadPlatform(ctx, tx); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: load payment platform records: %w", err)
	}
	m.matchLines(lines)
	m.checkUnmatchedAttempts()
	m.checkLedgerJoin()
	info.LegacyUnattempted = m.legacyUnattempted
	if err := m.checkPlatformDuplicates(ctx, tx); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: payment duplicate check: %w", err)
	}
	if err := m.checkDeferredReceipts(ctx, tx); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: payment receipt check: %w", err)
	}

	run := Run{
		ID: uuid.New(), TenantID: tenantID, Stream: StreamPaymentStatement,
		PeriodStart: info.CoverageStart, PeriodEnd: info.CoverageEnd, RunAt: time.Now().UTC(),
		Status: StatusClean,
	}
	mismatches := m.r.mismatches
	if len(mismatches) > 0 {
		run.Status = StatusMismatchesFound
	}
	if err := persistRun(ctx, tx, run, mismatches); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, err
	}
	return run, mismatches, info, nil
}

// ---------------------------------------------------------------------
// Matching.

type payLine struct {
	lineNo                                    int
	kind, ref, merchant, original, settlement string
	status, asset                             string
	amount                                    *big.Int
}

type payAttempt struct {
	id                  uuid.UUID
	operation           string // "deposit" | "payout"
	depositIntent       *uuid.UUID
	providerRef         string // "" when NULL
	merchantRef         string
	state               string
	amount              *big.Int
	asset               string
	sentAt              time.Time
	ledgerTx            *uuid.UUID // deposit: payment_attempts.ledger_transaction_id
	releaseTx           *uuid.UUID // payout: withdrawal_requests.release_ledger_transaction_id
	settlementRef       string     // payout: the release tx's provider_tx_id when it is this provider's withdrawal_completed
	releaseIsCompletion bool
}

type payLedgerTx struct {
	id       uuid.UUID
	txType   string
	ref      string
	postedAt time.Time
	amount   *big.Int // the psp_clearing leg(s)
	asset    string
	// legacyUnattempted: the posting is linked from a deposit intent
	// (deposit_intents.ledger_transaction_id) or a withdrawal request
	// (release_ledger_transaction_id) that has NO payment_attempts row -
	// the legacy, pre-cutover path. A posting linked from nothing is not
	// legacy (it is the orphan class and stays a P1).
	legacyUnattempted bool
}

type payMatcher struct {
	tenantID   uuid.UUID
	provider   string
	label      string
	cs, ce     time.Time
	agedBefore time.Time
	r          *sbRecorder

	attempts     []*payAttempt
	byRef        map[string]*payAttempt // op + "\x00" + provider_reference
	bySettlement map[string]*payAttempt // payout settlement reference
	byMerchant   map[string]*payAttempt
	matchedBy    map[uuid.UUID]string // attempt id -> first line key that matched it

	ledger      []*payLedgerTx
	ledgerByRef map[string]*payLedgerTx // type + "\x00" + provider_tx_id
	ledgerByID  map[uuid.UUID]*payLedgerTx

	legacyUnattempted int
}

func (m *payMatcher) key(parts ...string) string {
	return "provider=" + m.provider + " " + strings.Join(parts, " ")
}

func (m *payMatcher) loadLines(ctx context.Context, tx pgx.Tx, importID uuid.UUID) ([]payLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT line_no, provider_id, kind, provider_reference, COALESCE(merchant_reference, ''),
		       COALESCE(original_provider_reference, ''), COALESCE(settlement_reference, ''),
		       status, amount::text, asset_code
		  FROM payment_statement_lines
		 WHERE tenant_id = $1 AND import_id = $2
		 ORDER BY provider_reference, kind, line_no`, m.tenantID, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []payLine
	for rows.Next() {
		var l payLine
		var provider, amount string
		if err := rows.Scan(&l.lineNo, &provider, &l.kind, &l.ref, &l.merchant, &l.original, &l.settlement, &l.status, &amount, &l.asset); err != nil {
			return nil, err
		}
		if provider != m.provider {
			// Refused at fetch; a backstop against a hand-written row.
			return nil, fmt.Errorf("line %d carries provider %q in an import of provider %q", l.lineNo, provider, m.provider)
		}
		if l.amount, err = parseBig(amount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// loadPlatform reads, for this provider only (INV-IO-14), every attempt
// and every deposit / withdrawal_completed / deposit_reversal / tombstone
// ledger transaction.
func (m *payMatcher) loadPlatform(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.operation, a.deposit_intent_id, COALESCE(a.provider_reference, ''), a.merchant_reference,
		       a.state, a.amount::text, a.asset_code, COALESCE(a.first_submitted_at, a.created_at),
		       a.ledger_transaction_id, wr.release_ledger_transaction_id,
		       COALESCE(rl.provider_tx_id, ''),
		       COALESCE(rl.transaction_type = 'withdrawal_completed' AND rl.provider_id = a.provider_id, false)
		  FROM payment_attempts a
		  LEFT JOIN withdrawal_requests wr ON wr.id = a.withdrawal_request_id
		  LEFT JOIN ledger_transactions rl ON rl.id = wr.release_ledger_transaction_id
		 WHERE a.tenant_id = $1 AND a.provider_id = $2
		 ORDER BY a.id`, m.tenantID, m.provider)
	if err != nil {
		return err
	}
	m.byRef, m.bySettlement, m.byMerchant = map[string]*payAttempt{}, map[string]*payAttempt{}, map[string]*payAttempt{}
	m.matchedBy = map[uuid.UUID]string{}
	for rows.Next() {
		a := &payAttempt{}
		var amount string
		if err := rows.Scan(&a.id, &a.operation, &a.depositIntent, &a.providerRef, &a.merchantRef, &a.state, &amount, &a.asset,
			&a.sentAt, &a.ledgerTx, &a.releaseTx, &a.settlementRef, &a.releaseIsCompletion); err != nil {
			rows.Close()
			return err
		}
		if a.amount, err = parseBig(amount); err != nil {
			rows.Close()
			return err
		}
		if !a.releaseIsCompletion {
			a.settlementRef = ""
		}
		m.attempts = append(m.attempts, a)
		if a.providerRef != "" {
			m.byRef[a.operation+"\x00"+a.providerRef] = a
		}
		if a.settlementRef != "" {
			m.bySettlement[a.settlementRef] = a
		}
		m.byMerchant[a.merchantRef] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if payMatchAfterAttemptsHook != nil {
		payMatchAfterAttemptsHook()
	}
	rows, err = tx.Query(ctx, `
		SELECT t.id, t.transaction_type, t.provider_tx_id, t.created_at,
		       COALESCE((SELECT SUM(e.amount) FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		                  WHERE e.ledger_transaction_id = t.id AND la.account_type = 'psp_clearing'), 0)::text,
		       (SELECT CASE WHEN count(DISTINCT la.asset_code) = 1 THEN min(la.asset_code)
		                    WHEN count(DISTINCT la.asset_code) = 0 THEN '<none>' ELSE '<multiple>' END
		          FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		         WHERE e.ledger_transaction_id = t.id),
		       CASE t.transaction_type
		         WHEN 'deposit' THEN EXISTS (
		           SELECT 1 FROM deposit_intents di
		            WHERE di.tenant_id = t.tenant_id AND di.ledger_transaction_id = t.id
		              AND NOT EXISTS (SELECT 1 FROM payment_attempts pa WHERE pa.tenant_id = di.tenant_id AND pa.deposit_intent_id = di.id))
		         WHEN 'withdrawal_completed' THEN EXISTS (
		           SELECT 1 FROM withdrawal_requests wr
		            WHERE wr.tenant_id = t.tenant_id AND wr.release_ledger_transaction_id = t.id
		              AND NOT EXISTS (SELECT 1 FROM payment_attempts pa WHERE pa.tenant_id = wr.tenant_id AND pa.withdrawal_request_id = wr.id))
		         ELSE false END
		  FROM ledger_transactions t
		 WHERE t.tenant_id = $1 AND t.provider_id = $2 AND t.provider_tx_id IS NOT NULL
		   AND t.transaction_type IN ('deposit', 'withdrawal_completed', 'deposit_reversal', 'tombstone')
		 ORDER BY t.transaction_type, t.provider_tx_id, t.id`, m.tenantID, m.provider)
	if err != nil {
		return err
	}
	defer rows.Close()
	m.ledgerByRef, m.ledgerByID = map[string]*payLedgerTx{}, map[uuid.UUID]*payLedgerTx{}
	for rows.Next() {
		t := &payLedgerTx{}
		var amount string
		if err := rows.Scan(&t.id, &t.txType, &t.ref, &t.postedAt, &amount, &t.asset, &t.legacyUnattempted); err != nil {
			return err
		}
		if t.amount, err = parseBig(amount); err != nil {
			return err
		}
		m.ledger = append(m.ledger, t)
		m.ledgerByRef[t.txType+"\x00"+t.ref] = t
		m.ledgerByID[t.id] = t
	}
	return rows.Err()
}

func (m *payMatcher) inCoverage(t time.Time) bool {
	return !t.Before(m.cs) && t.Before(m.ce)
}

func (m *payMatcher) aged(a *payAttempt) bool {
	return a.sentAt.Before(m.agedBefore)
}

func inFlight(state string) bool {
	return state == "submitting" || state == "pending" || state == "ambiguous"
}

func (a *payAttempt) render() string {
	return fmt.Sprintf("attempt=%s op=%s state=%s amount=%s asset=%s provider_reference=%s",
		a.id, a.operation, a.state, a.amount, a.asset, orNone(a.providerRef))
}

func (l payLine) render() string {
	s := fmt.Sprintf("line=%d kind=%s status=%s amount=%s asset=%s", l.lineNo, l.kind, l.status, l.amount, l.asset)
	if l.merchant != "" {
		s += " merchant_reference=" + l.merchant
	}
	if l.original != "" {
		s += " original=" + l.original
	}
	if l.settlement != "" {
		s += " settlement_reference=" + l.settlement
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// matchLines walks the lines in sorted (provider_reference, kind, line_no)
// order: duplicate keys are reported once and only their first line is
// matched further.
func (m *payMatcher) matchLines(lines []payLine) {
	reportedDup := map[string]bool{}
	for i, l := range lines {
		lk := m.key("provider_reference="+l.ref, "kind="+l.kind)
		if i > 0 && lines[i-1].ref == l.ref && lines[i-1].kind == l.kind {
			if !reportedDup[lk] {
				reportedDup[lk] = true
				m.r.add(MismatchKindPayDuplicate, lk+" check=duplicate_line", "one statement line per provider reference and kind",
					m.label+"duplicate statement line: "+l.render())
			}
			continue
		}
		if l.kind == statement.PaymentLineDepositReversal {
			m.matchReversal(lk, l)
			continue
		}
		m.matchPayment(lk, l)
	}
}

func (m *payMatcher) matchReversal(lk string, l payLine) {
	rev, posted := m.ledgerByRef[string("deposit_reversal")+"\x00"+l.ref]
	if !posted {
		if l.original != "" {
			if _, ok := m.ledgerByRef["tombstone\x00"+l.original]; ok {
				return // rollback of an unseen original: the tombstone is the platform's record
			}
		}
		if l.status == statement.PaymentStatusSucceeded || l.status == statement.PaymentStatusReversed {
			// A pending or declined reversal (e.g. a chargeback the
			// merchant won) expects no posting (code review F3).
			m.r.add(MismatchKindPayMissingPlatformRecord, lk+" check=reversal", "ledger: a deposit_reversal under this reference or a tombstone under its original",
				m.label+l.render())
		}
		return
	}
	if rev.asset != l.asset {
		m.r.add(MismatchKindPayAssetMismatch, lk+" check=asset", "ledger: asset="+rev.asset, m.label+l.render())
	} else if rev.amount.Cmp(l.amount) != 0 {
		m.r.add(MismatchKindPayAmountMismatch, lk+" check=amount", "ledger: amount="+rev.amount.String(), m.label+l.render())
	}
	if l.status == statement.PaymentStatusPending || l.status == statement.PaymentStatusDeclined {
		m.r.add(MismatchKindPayStatusMismatch, lk+" check=status", "ledger: reversal posted (transaction "+rev.id.String()+")", m.label+l.render())
	}
}

func (m *payMatcher) matchPayment(lk string, l payLine) {
	op := "deposit"
	if l.kind == statement.PaymentLinePayout {
		op = "payout"
	}
	a := m.byRef[op+"\x00"+l.ref]
	if a == nil && op == "payout" {
		// LF95-C13: a payout line may carry the Step B settlement reference
		// as its own reference, or name it separately.
		a = m.bySettlement[l.ref]
		if a == nil && l.settlement != "" {
			a = m.bySettlement[l.settlement]
		}
	}
	byMerchant := false
	if a == nil && l.merchant != "" {
		if c := m.byMerchant[l.merchant]; c != nil && c.operation == op {
			a, byMerchant = c, true
		}
	}
	if a == nil {
		m.r.add(MismatchKindPayMissingPlatformRecord, lk+" check=resolve", "platform: an attempt of this provider with this reference", m.label+l.render())
		return
	}
	if first, seen := m.matchedBy[a.id]; seen {
		m.r.add(MismatchKindPayDuplicate, lk+" attempt="+a.id.String()+" check=duplicate_match",
			"one statement line per attempt (first: "+first+")", m.label+l.render())
		return
	}
	m.matchedBy[a.id] = lk
	ak := lk + " attempt=" + a.id.String()

	if byMerchant && a.providerRef != "" && a.providerRef != l.ref {
		m.r.add(MismatchKindPayReferenceMismatch, ak+" check=reference", "platform: "+a.render(), m.label+l.render())
	}
	if op == "payout" && l.settlement != "" && a.settlementRef != "" && l.settlement != a.settlementRef {
		// Code review F4: the provider's stated settlement reference
		// contradicts the ledger's withdrawal_completed provider_tx_id.
		m.r.add(MismatchKindPayReferenceMismatch, ak+" check=settlement_reference",
			"ledger: withdrawal_completed provider_tx_id="+a.settlementRef, m.label+l.render())
	}
	if a.asset != l.asset {
		m.r.add(MismatchKindPayAssetMismatch, ak+" check=asset", "platform: "+a.render(), m.label+l.render())
	} else if a.amount.Cmp(l.amount) != 0 {
		m.r.add(MismatchKindPayAmountMismatch, ak+" check=amount", "platform: "+a.render(), m.label+l.render())
	}

	providerSucceeded := l.status == statement.PaymentStatusSucceeded || l.status == statement.PaymentStatusReversed
	switch {
	case a.state == "disputed" || (a.state == "rejected" && !providerSucceeded):
		// disputed: already a payments P1 (see the file comment).
	case providerSucceeded && a.state != "succeeded":
		m.r.add(MismatchKindPayStatusMismatch, ak+" check=status", "platform: "+a.render(), m.label+l.render())
	case l.status == statement.PaymentStatusDeclined && a.state == "succeeded":
		m.r.add(MismatchKindPayStatusMismatch, ak+" check=status", "platform: "+a.render(), m.label+l.render())
	case !providerSucceeded && inFlight(a.state) && m.aged(a):
		m.r.add(MismatchKindPayUnresolved, ak+" check=unresolved",
			"resolution within "+m.ce.Sub(m.agedBefore).String()+" of submission", "platform: "+a.render()+"; "+m.label+l.render())
	}
}

// checkUnmatchedAttempts: attempts in the coverage window that no line
// matched.
func (m *payMatcher) checkUnmatchedAttempts() {
	for _, a := range m.attempts {
		if _, ok := m.matchedBy[a.id]; ok {
			continue
		}
		k := m.key("attempt="+a.id.String(), "provider_reference="+orNone(a.providerRef))
		switch {
		// The coverage window protects only the "missing provider record"
		// rule. Ageing is not coverage-gated (code review F2): with a
		// window no longer than the horizon an in-flight attempt would
		// otherwise never be flagged. aged() implies sentAt < coverage_end.
		case a.state == "succeeded" && m.inCoverage(a.sentAt):
			m.r.add(MismatchKindPayMissingProviderRecord, k+" check=unmatched", "statement: a line for this attempt", m.label+"no statement line; platform: "+a.render())
		case inFlight(a.state) && m.aged(a):
			m.r.add(MismatchKindPayUnresolved, k+" check=unresolved",
				"resolution within "+m.ce.Sub(m.agedBefore).String()+" of submission", m.label+"no statement line; platform: "+a.render())
		}
	}
}

// checkLedgerJoin is LF95-C13: the ledger's own (provider_id,
// provider_tx_id) against the succeeded attempts, both directions.
func (m *payMatcher) checkLedgerJoin() {
	// (a) every succeeded attempt has exactly its one posting.
	for _, a := range m.attempts {
		if a.state != "succeeded" || !m.inCoverage(a.sentAt) {
			continue
		}
		k := m.key("attempt="+a.id.String(), "provider_reference="+orNone(a.providerRef), "check=ledger_join")
		switch a.operation {
		case "deposit":
			t := m.ledgerByRef["deposit\x00"+a.providerRef]
			if t == nil || a.ledgerTx == nil || *a.ledgerTx != t.id {
				linked := "<none>"
				if a.ledgerTx != nil {
					linked = a.ledgerTx.String()
				}
				m.r.add(MismatchKindPayStatusMismatch, k, "ledger: one deposit posting under (provider_id, provider_reference) linked from the attempt",
					"platform: "+a.render()+" ledger_transaction_id="+linked)
			}
		case "payout":
			if a.releaseTx == nil || !a.releaseIsCompletion {
				m.r.add(MismatchKindPayStatusMismatch, k, "ledger: one withdrawal_completed posting of this provider linked from the withdrawal",
					"platform: "+a.render()+" settlement_reference=<none>")
			}
		}
	}
	// (b) every deposit / withdrawal_completed posting in the window maps
	// to exactly one succeeded attempt.
	succeededFor := map[uuid.UUID]int{}
	for _, a := range m.attempts {
		if a.state != "succeeded" {
			continue
		}
		if a.operation == "deposit" && a.ledgerTx != nil {
			succeededFor[*a.ledgerTx]++
		}
		if a.operation == "payout" && a.releaseTx != nil && a.releaseIsCompletion {
			succeededFor[*a.releaseTx]++
		}
	}
	for _, t := range m.ledger {
		if (t.txType != "deposit" && t.txType != "withdrawal_completed") || !m.inCoverage(t.postedAt) {
			continue
		}
		n := succeededFor[t.id]
		if n == 0 && t.legacyUnattempted {
			// Interim exclusion (ledger-finance ruling on code review F1,
			// ADR 0095 §12.7): the live deposit and withdrawal-completion
			// paths are not yet cut over to payment_attempts, so their
			// postings have no attempt BY CONSTRUCTION. Flagging them would
			// raise a P1 per posting per run. They are counted instead
			// (legacy_unattempted, audited on every run). The rule is
			// self-retiring: a posting whose intent/request has ANY attempt
			// is fully checked, and once the cutover makes every new intent
			// and request carry an attempt the count stops growing.
			m.legacyUnattempted++
			continue
		}
		if n != 1 {
			m.r.add(MismatchKindPayMissingPlatformRecord,
				m.key("provider_tx_id="+t.ref, "type="+t.txType, "check=ledger_join"),
				"platform: exactly one succeeded attempt for this posting",
				fmt.Sprintf("ledger: transaction=%s amount=%s asset=%s; succeeded attempts=%d", t.id, t.amount, t.asset, n))
		}
	}
}

// checkPlatformDuplicates: more than one succeeded attempt for one deposit
// intent, where at least one of them is this provider's and in the window.
func (m *payMatcher) checkPlatformDuplicates(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT a.deposit_intent_id, count(*), string_agg(a.id::text || '@' || COALESCE(a.provider_id, '<none>'), ',' ORDER BY a.id)
		  FROM payment_attempts a
		 WHERE a.tenant_id = $1 AND a.operation = 'deposit' AND a.state = 'succeeded'
		 GROUP BY a.deposit_intent_id
		HAVING count(*) > 1
		   AND bool_or(a.provider_id = $2 AND COALESCE(a.first_submitted_at, a.created_at) >= $3
		                                  AND COALESCE(a.first_submitted_at, a.created_at) < $4)
		 ORDER BY a.deposit_intent_id`, m.tenantID, m.provider, m.cs, m.ce)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var intent uuid.UUID
		var n int
		var list string
		if err := rows.Scan(&intent, &n, &list); err != nil {
			return err
		}
		m.r.add(MismatchKindPayDuplicate, m.key("deposit_intent="+intent.String(), "check=duplicate_success"),
			"platform: at most one succeeded attempt per deposit intent", fmt.Sprintf("platform: %d succeeded attempts: %s", n, list))
	}
	return rows.Err()
}

// checkDeferredReceipts: a verified callback receipt of this provider still
// unresolved past the horizon (a possible lost success; LF95-C5,
// S95-C2(ii)).
func (m *payMatcher) checkDeferredReceipts(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT id, event_type, provider_reference, outcome, received_at
		  FROM payment_provider_events
		 WHERE tenant_id = $1 AND provider_id = $2 AND resolved_at IS NULL
		   AND disposition_at_receipt = 'deferred_unresolved' AND received_at < $3
		 ORDER BY received_at, id`, m.tenantID, m.provider, m.agedBefore)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var eventType, ref, outcome string
		var receivedAt time.Time
		if err := rows.Scan(&id, &eventType, &ref, &outcome, &receivedAt); err != nil {
			return err
		}
		m.r.add(MismatchKindPayUnresolved, m.key("receipt="+id.String(), "provider_reference="+ref, "check=deferred_receipt"),
			"receipt resolved within "+m.ce.Sub(m.agedBefore).String(),
			fmt.Sprintf("platform: unresolved %s receipt outcome=%s received_at=%s", eventType, outcome, receivedAt.UTC().Format(time.RFC3339)))
	}
	return rows.Err()
}
