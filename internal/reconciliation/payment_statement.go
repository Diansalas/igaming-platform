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
//	                            reference; or (check=merchant, D2-1)
//	                            resolved by provider/settlement reference
//	                            while the line's merchant reference names
//	                            another attempt, the other operation, or
//	                            no attempt of this provider.
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
//     stream does not second-guess that terminal decision. EXCEPT the
//     "provider captured, platform disputed, nothing posted" reasons,
//     which are pay_captured_unposted (ADR 0095 §28.9, extended by §35,
//     PAY-RECON-PARKED-CAPTURE-1): see disputeReasonClasses.
//   - an unbound park (provider_reference_conflict,
//     invalid_provider_reference:*) holds no provider reference, so it is
//     flagged whenever ANY persisted import of the tenant and provider has a
//     succeeded line resolving to it by merchant reference - on every run,
//     unwindowed (PRH-2 K3, ADR 0101 9.2 S1/S2: IMPLEMENTED against MOCK;
//     the N1 attribution residual is disclosed in ADR 0095 §35.4).
//   - an unbound PAYOUT park gets the same standing coverage from succeeded
//     PAYOUT lines only, and clears only on the payout signal (a
//     withdrawal_completed keyed by the line's reference and attributable
//     to the attempt): never on a deposit_reversal line or a tombstone
//     (PAY-PAYOUT-UNBOUND-STANDING-1, ADR 0095 §35.2 PO-1 / §35.6).
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
	// MismatchKindPayCapturedUnposted is ADR 0095 §28.9 (INV-DEP-1 /
	// PAY-DOUBLE-CREDIT-1, HD-LEDGER-UNALLOC-1 interim (A)): "provider
	// captured, platform disputed, not posted" - a disputed attempt with
	// terminal_reason='multiple_success_for_intent' whose statement line
	// reports succeeded, with no reversal line and no ledger tombstone.
	// Reported on every run until it clears (a reversal/tombstone appears,
	// or, later, an allocation posting under LEDGER-SUSPENSE-B-1). An M1
	// resolution only acknowledges it and never clears or suppresses it
	// (ADR 0101 §4, LF-3, F13). Never auto-resolved, never a
	// T17/re-drive trigger.
	//
	// ADR 0095 §35 (PRH-2 D2, PAY-RECON-PARKED-CAPTURE-1) widens the
	// reason set to every T10 where the provider says it captured and the
	// platform disputed without posting: disputeReasonClasses (bound:
	// in-run and standing; unbound: in-run only).
	MismatchKindPayCapturedUnposted MismatchKind = "pay_captured_unposted"
)

// disputeReasonClass is how pay_captured_unposted treats a disputed
// attempt's terminal_reason (ADR 0095 §28.9 as extended by §35):
//
//   - reasonBound: the reason's write site normally leaves the attempt
//     HOLDING the provider reference the provider reported as captured,
//     with nothing posted. Reported in-run (a succeeded line names the
//     attempt) and standing (no line this run, unwindowed); clears only on
//     capturedUnposted's signals (deposit: a reversal line or a tombstone
//     on the reference; payout: only the attempt's own positively attributed
//     withdrawal_completed, PAY-PAYOUT-BOUND-CLEAR-1). At run time it applies only if the
//     attempt really holds a reference (captureClass, D2F-1); otherwise the
//     attempt is treated as unbound.
//   - reasonUnbound: a park that never binds the adapter's reference
//     (§34.8). A real capture behind it is visible only through a succeeded
//     line resolving to the attempt (by construction by merchant
//     reference), so it is reported in-run only (§35.2); standing coverage
//     is PAY-RECON-PARKED-CAPTURE-STANDING-1.
//   - reasonBoundIfReferenced: reasonBound when the attempt holds a
//     reference, otherwise reasonUnbound. Never excluded.
//   - reasonExcluded: no captured-and-unposted exposure by construction.
//   - reasonUnclassified: not in this table. No finding at run time (the
//     pre-D2 behaviour for an unknown reason), and refused by the
//     classification pin test (payment_reason_classification_test.go),
//     which iterates payments.DepositDisputeTerminalReasons() - so a new
//     payments reason cannot ship without an explicit decision here.
type disputeReasonClass int

const (
	reasonUnclassified disputeReasonClass = iota
	reasonExcluded
	reasonBound
	reasonUnbound
	reasonBoundIfReferenced
)

// invalidProviderReferencePrefix: the invalid-reference park writes
// "invalid_provider_reference:<reason>" (closed providerref reason).
const invalidProviderReferencePrefix = "invalid_provider_reference:"

// disputeReasonClasses has one explicit row per deposit dispute reason
// (ledger-finance rulings on QA C-F2 (a) and the D2 review, P1). String
// literals, never imported from internal/payments: reconciliation reads
// payments' rows, not its code; the pin test ties the two together.
var disputeReasonClasses = map[string]disputeReasonClass{
	"multiple_success_for_intent":         reasonBound,             // T13d, §28.4
	"sync_amount_mismatch":                reasonBound,             // PRH-2 C, §34.2 (the park binds the reference)
	"poll_amount_mismatch":                reasonBound,             // PRH-2 D, §36 (polled by the bound reference)
	"poll_reference_mismatch":             reasonBound,             // PRH-2 D, §36 (the echo is never bound)
	"callback_amount_asset_mismatch":      reasonBound,             // T10: verified callback for the bound reference
	"provider_reference_conflict":         reasonBoundIfReferenced, // §34.3 phase C park: no reference (unbound); §36 poll F-C4 park: holds X (bound) - LF D2 final PM-1
	"invalid_provider_reference":          reasonUnbound,           // bare form (PAY-POLL-ECHO-HARDENING-1)
	"success_for_never_sent_attempt":      reasonBoundIfReferenced, // T15
	"reversal_tombstone_precedes_success": reasonExcluded,          // net zero at the PSP (§28.9)
}

// classifyDisputeReason returns the class of a terminal reason, including
// the invalid_provider_reference:<reason> prefix family.
func classifyDisputeReason(r string) disputeReasonClass {
	if c, ok := disputeReasonClasses[r]; ok {
		return c
	}
	if strings.HasPrefix(r, invalidProviderReferencePrefix) {
		return reasonUnbound
	}
	return reasonUnclassified
}

// captureClass resolves a disputed attempt's class against its own row
// (reasonBoundIfReferenced becomes bound or unbound).
//
// Runtime rule (D2 code final review D2F-1): a reason classed bound is bound
// only when the attempt actually HOLDS a reference. A park whose write site
// did not bind one (callback_amount_asset_mismatch on an attempt the callback
// resolved by merchant reference; multiple_success_for_intent likewise, a gap
// that predates D2) has nothing to key a standing finding on, and checking it
// on the empty reference could never clear. Such an attempt is treated as
// unbound: reported in-run by merchant reference, cleared on the line's
// reference. So every bound reason behaves as bound-if-referenced; the table
// still records what the reason IS.
func (a *payAttempt) captureClass() disputeReasonClass {
	c := classifyDisputeReason(a.terminalReason)
	if c == reasonBound || c == reasonBoundIfReferenced {
		if a.providerRef != "" {
			return reasonBound
		}
		return reasonUnbound
	}
	return c
}

// boundCapture: a disputed attempt holding the captured reference (in-run
// and standing pay_captured_unposted).
func (a *payAttempt) boundCapture() bool {
	return a.state == "disputed" && a.captureClass() == reasonBound
}

// unboundPark: a disputed attempt with no captured reference of its own
// (in-run pay_captured_unposted only, cleared on the line's reference).
func (a *payAttempt) unboundPark() bool {
	return a.state == "disputed" && a.captureClass() == reasonUnbound
}

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
	// ObservationOnlyStatus is set by the scheduler's non-active-tenant
	// observation sweep (PRH-2 R3, H-W1) to the tenant's status ("suspended",
	// "closed"). It changes NOTHING about fetch, ingest or matching: it is
	// recorded in the run's audit metadata only, so staff can see the run was
	// made for a tenant that was not active.
	ObservationOnlyStatus string
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
	if err := providerref.ValidatePaymentReference("provider_reference", l.ProviderReference); err != nil {
		return err
	}
	if err := providerref.ValidatePaymentReferenceOptional("original_provider_reference", l.OriginalProviderReference); err != nil {
		return err
	}
	if err := providerref.ValidatePaymentReferenceOptional("settlement_reference", l.SettlementReference); err != nil {
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
	if err := m.loadK3Evidence(ctx, tx, info.IsMock); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: load persisted payment evidence: %w", err)
	}
	m.matchLines(lines)
	m.checkUnmatchedAttempts()
	m.checkStandingUnbound()
	m.checkLedgerJoin()
	info.LegacyUnattempted = m.legacyUnattempted
	if err := m.checkPlatformDuplicates(ctx, tx); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: payment duplicate check: %w", err)
	}
	if err := m.checkDeferredReceipts(ctx, tx); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: payment receipt check: %w", err)
	}
	if err := m.checkM2Standing(ctx, tx); err != nil {
		return Run{}, nil, PaymentStatementInfo{}, fmt.Errorf("reconciliation: M2 standing check: %w", err)
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
	terminalReason      string // "" when NULL - ADR 0095 §28.9's pay_captured_unposted condition
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

	// reversalOriginals: ADR 0095 §28.9's "no deposit_reversal line naming
	// its reference" condition for pay_captured_unposted - the set of
	// every deposit_reversal statement line's OriginalProviderReference in
	// THIS run, pre-scanned by matchLines before any line is matched.
	reversalOriginals map[string]bool

	legacyUnattempted int

	// PRH-2 K3 (ADR 0101 9.2/9.3): the persisted-evidence substrate. Set by
	// loadK3Evidence before any line is matched.
	k3 *k3Evidence
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
		       COALESCE(rl.transaction_type = 'withdrawal_completed' AND rl.provider_id = a.provider_id, false),
		       COALESCE(a.terminal_reason, '')
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
			&a.sentAt, &a.ledgerTx, &a.releaseTx, &a.settlementRef, &a.releaseIsCompletion, &a.terminalReason); err != nil {
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
	// ADR 0095 §28.9 pay_captured_unposted: "no deposit_reversal line
	// naming its reference" is a statement-wide fact, independent of
	// which order the lines happen to appear in - pre-scanned once here
	// so matchPayment (called below, possibly BEFORE a later reversal
	// line for the same original in this same statement) can already see
	// it.
	m.reversalOriginals = map[string]bool{}
	// B4 review C3 (security + ledger-finance): only a COMPLETED reversal line
	// (reversalCompleted) clears. A pending reversal may still fail and a declined
	// one means the capture stands; either clearing would let a manual credit and
	// the later refund both reach the player.
	// PRH-2 K3 (D-4, section 26 RC-3): this run's own reversal lines clear only
	// when its import may clear (a MOCK import clears only when no non-MOCK
	// import exists for the tenant and provider); every ELIGIBLE persisted
	// import's reversal lines clear too (S2/S3/S4).
	if m.k3.clearEligible(m.k3.importIsMock) {
		for _, l := range lines {
			if l.kind == statement.PaymentLineDepositReversal && l.original != "" && reversalCompleted(l.status) {
				m.reversalOriginals[l.original] = true
			}
		}
	}
	for o := range m.k3.reversalOriginal {
		m.reversalOriginals[o] = true
	}
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

// reversalCompleted reports whether a deposit_reversal statement line's status
// says the money went back to the payer: succeeded or reversed - the same two
// statuses matchReversal treats as "a posting is expected". pending and declined
// never clear a captured-unposted exposure (B4 review C3). Migration 0119's
// payment_ref_cleared uses the identical status set (pinned by the MA020 parity
// test).
func reversalCompleted(status string) bool {
	return status == statement.PaymentStatusSucceeded || status == statement.PaymentStatusReversed
}

// matchReversal's statement-side l.status values (succeeded/pending/
// declined/reversed) come from the PSP's OWN statement-line status field -
// a field on the STATEMENT import, produced independently of, and on its
// own schedule relative to, the wire callback Outcome ADR 0095's callback
// path receives for the very same underlying event. The two are never the
// same read: a statement line's status is this reconciliation run's only
// signal for that reference, while the callback path may have already
// applied (or deferred) its own evidence for it in an earlier run. This
// matcher's own `PaymentStatusPending`/`PaymentStatusDeclined`/
// `PaymentStatusSucceeded`/`PaymentStatusReversed` cases below are a
// closed set over that statement-side field only - RawOutcome/H1's
// pending/ambiguous distinction (a CALLBACK-side, wire-outcome concept)
// has no counterpart here and must never be reintroduced as one.
//
// RV-PRH-I1 ledger-finance H1 RULING (rule 6, doc-only - no code change
// here, confirmed complete): a callback whose wire outcome was pending/
// ambiguous never posts or tombstones on the platform side
// (payments.applyReversalReceiptEvidence rule 2 - see that function's own
// doc comment for the full ruling). Consequently, IF the PSP's own
// statement feed ever reports a line for that same reference before ITS
// OWN final state (a timing gap this matcher must tolerate, not assume
// away), that line already falls correctly into the existing
// `PaymentStatusPending`/`PaymentStatusDeclined` "no posting yet expected"
// branches below - through `rev, posted := m.ledgerByRef[...]`'s own
// !posted case, since a genuinely non-final callback outcome never wrote
// a ledger row for this matcher to find either. This matcher therefore
// does NOT need, and must NEVER gain, its own separate pending/ambiguous
// carve-out mirroring H1 rule 2: the existing !posted branches already
// cover the case, by construction, without needing to know anything about
// the callback path's own wire-outcome vocabulary at all.
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
	if !byMerchant && l.merchant != "" {
		m.checkMerchantAttribution(lk, ak, op, a, l)
	}
	if op == "payout" && l.settlement != "" && a.settlementRef != "" && l.settlement != a.settlementRef &&
		!strings.HasPrefix(a.settlementRef, reservedOperatorPrefix) {
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

	// ADR 0101 9.1 (b): a matching succeeded line for an M2-declared-paid payout
	// is a confirmation (counted), never a mismatch.
	m.countM2Confirmation(context.Background(), a, l)

	providerSucceeded := l.status == statement.PaymentStatusSucceeded || l.status == statement.PaymentStatusReversed
	switch {
	// Code review R1 (rv-fh3-code-review.md, 95a1c34): ONLY a `succeeded`
	// statement-line status counts as "still captured" for this check -
	// deliberately `l.status == statement.PaymentStatusSucceeded`, never
	// the wider `providerSucceeded` (which also includes `reversed`). A
	// `reversed` line is the PSP's OWN statement confirming this capture
	// was refunded - exactly the signal that must CLEAR the flag, not
	// keep it standing - so it falls through to the plain "disputed:
	// already a payments P1" case below instead, precisely like every
	// other disputed reason.
	case a.boundCapture() && (l.status == statement.PaymentStatusSucceeded ||
		// PAY-RECON-PARKED-CAPTURE-STANDING-1 M-S2 (ledger-finance ruling M-S2 in
		// docs/plans/prh2-hardening-round/analysis/psp-prerequisites-analysis.md 2.4,
		// re-confirmed by the LF implementation review; fail-closed tightening, no
		// new money path): a `pending` or `declined`
		// line naming a bound park is NOT a clearing signal - only a reversal
		// line or a tombstone clears a DEPOSIT park (ADR 0095 §35.4, S2), and
		// only the attempt's own positively attributed withdrawal_completed
		// clears a PAYOUT park (capturedUnposted, PAY-PAYOUT-BOUND-CLEAR-1). A PSP that keeps
		// listing X as pending/declined in every window must not suppress the
		// standing finding. `reversed` stays the R1 in-run clear (above).
		l.status == statement.PaymentStatusPending || l.status == statement.PaymentStatusDeclined) &&
		m.capturedUnposted(a) && m.markCaptured(a, a.providerRef):
		// ADR 0095 §28.9, extended by §35: the disputed reason codes that
		// are NOT already a plain payments P1 for reconciliation's
		// purposes - a real PSP capture the platform never posted and has
		// not (yet) been refunded is a standing, reported exposure, never
		// silently folded into "disputed: already a payments P1" like
		// every other disputed reason below.
		m.r.add(MismatchKindPayCapturedUnposted, ak+" check=captured_unposted",
			capturedUnpostedHintFor(a, boundCapturedUnpostedResolutionHint), "platform: "+a.render()+" terminal_reason="+a.terminalReason+m.yHolderNote(a)+"; "+m.label+l.render())
	case a.unboundPark() && l.status == statement.PaymentStatusSucceeded && !m.clearedRefFor(a, l.ref) && m.markCaptured(a, l.ref):
		// ADR 0095 §35.2 (LF ruling on QA C-F2 (a)): a park that never
		// bound a reference (a binding conflict, or an invalid reference)
		// and a succeeded deposit line resolving to it - by construction
		// by merchant reference, since such an attempt holds no provider
		// reference. Deliberately NOT gated on byMerchant: should such an
		// attempt ever hold a reference, a succeeded line naming it is the
		// same exposure and stays loud. The PSP says it captured for this
		// attempt and the platform posted nothing; the clearing signals
		// are read on the LINE's reference, per operation (clearedRefFor: a
		// payout park never clears on a deposit-shaped signal,
		// PAY-PAYOUT-UNBOUND-STANDING-1). Standing: checkStandingUnbound.
		m.r.add(MismatchKindPayCapturedUnposted, ak+" check=captured_unposted",
			capturedUnpostedHintFor(a, capturedUnpostedResolutionHint), "platform: "+a.render()+" terminal_reason="+a.terminalReason+"; "+m.label+l.render())
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

// checkMerchantAttribution is the D2-1 merchant cross-check (ledger-finance
// ruling docs/plans/prh2-hardening-round/reviews/d2-1-ledger-finance-
// ruling.md (a); ADR 0095 §35.2). It runs for a line resolved to attempt a
// by provider reference or settlement reference (never by merchant
// reference) that carries a non-empty merchant reference. The platform
// issues merchant references, so the PSP naming any attempt other than a -
// a different attempt, an attempt of the other operation, or no attempt of
// this provider at all - says whose capture this line is and contradicts
// the attribution to a: pay_reference_mismatch check=merchant against a.
// Attempt IDENTITY is compared (the byMerchant resolution), never strings.
//
// When the named attempt b is an unbound park (unboundPark: it
// cannot hold the line's reference) and the line reports succeeded and is
// not cleared on the line's reference, the PSP is reporting b's capture
// that the platform never posted: pay_captured_unposted against b too.
// b is NOT recorded in matchedBy - this line stays matched to a, and a
// separate line for b must be matched normally, with no false
// pay_duplicate. a's own checks are unaffected (the finding is additive).
func (m *payMatcher) checkMerchantAttribution(lk, ak, op string, a *payAttempt, l payLine) {
	b := m.byMerchant[l.merchant]
	switch {
	case b == nil:
		m.r.add(MismatchKindPayReferenceMismatch, ak+" check=merchant",
			"platform: merchant_reference "+a.merchantRef+" ("+a.render()+")",
			m.label+"merchant_reference names no platform attempt of this provider; "+l.render())
		return
	case b.operation != op:
		m.r.add(MismatchKindPayReferenceMismatch, ak+" check=merchant",
			"platform: merchant_reference "+a.merchantRef+" ("+a.render()+")",
			m.label+"merchant_reference names attempt="+b.id.String()+" of the other operation ("+b.operation+"); "+l.render())
		return
	case b == a:
		return
	}
	m.r.add(MismatchKindPayReferenceMismatch, ak+" check=merchant",
		"platform: merchant_reference "+a.merchantRef+" ("+a.render()+")",
		m.label+"merchant_reference names attempt="+b.id.String()+" ("+b.render()+" terminal_reason="+orNone(b.terminalReason)+"); "+l.render())
	if b.unboundPark() && l.status == statement.PaymentStatusSucceeded && !m.clearedRefFor(b, l.ref) && m.markCaptured(b, l.ref) {
		m.r.add(MismatchKindPayCapturedUnposted, lk+" attempt="+b.id.String()+" check=captured_unposted",
			capturedUnpostedHintFor(b, capturedUnpostedResolutionHint),
			"platform: "+b.render()+" terminal_reason="+b.terminalReason+"; line resolved by reference to attempt="+a.id.String()+"; "+m.label+l.render())
	}
}

// capturedUnposted is ADR 0095 §28.9's pay_captured_unposted clearing
// predicate, shared by matchPayment (a statement line for this run DOES
// name the attempt) and checkUnmatchedAttempts (ledger-finance review C2/
// F2, rv-fh3-ledger.md 076e42e: no line names it in THIS run, which is
// exactly the case that let the exposure silently drop out of
// reconciliation once the capture's own statement period passed) - the
// SAME conditions in both places, so a future edit to one can never
// silently diverge from the other. For a PAYOUT attempt the predicate is the
// payout clearing rule (PAY-PAYOUT-BOUND-CLEAR-1, below). For a deposit: no
// deposit_reversal line in THIS run
// named this reference (m.reversalOriginals, populated once per run by
// matchLines before either caller runs), and no tombstone ledger row
// exists for it. Both callers already gate on the terminal reason
// (boundCapture / unboundPark, ADR 0095 §35) themselves - not
// duplicated here, since matchPayment's own case additionally requires
// providerSucceeded (a statement-line-status concept this predicate has
// no business knowing about).
func (m *payMatcher) capturedUnposted(a *payAttempt) bool {
	// PAY-PAYOUT-BOUND-CLEAR-1 (ADR 0095 §35.6, ledger-finance ruling on the
	// STANDING-1 review): a bound-class PAYOUT park (e.g.
	// callback_amount_asset_mismatch on a payout holding X) clears per
	// OPERATION, through the same rule as an unbound payout park
	// (clearedRefFor -> payoutCompletedRef, keyed on the held reference X):
	// only the parked attempt's OWN withdrawal_completed, positively attributed
	// (its withdrawal_requests.release_ledger_transaction_id, keyed by X, no
	// other attempt holding X). Deposit-shaped signals - a deposit_reversal
	// line naming X (of ANY amount), a tombstone on X, and the deposit-only
	// typed Y evidence below - NEVER clear a payout finding: the PSP refund of
	// a deposit says nothing about whether a payout left the platform. Any
	// operation other than deposit goes through clearedRefFor (an unknown
	// operation never clears: fail closed). Deposits are unchanged.
	if a.operation != paymentStatementKindDeposit {
		return !m.clearedRefFor(a, a.providerRef)
	}
	// PRH-2 K3 (S4, POLL-REF-CLEAR-1, LF B3): a poll_reference_mismatch park with
	// typed Y evidence (the poll's returned reference, never audit JSON) also
	// clears on a reversal line or a tombstone on Y. Without a Y row only X
	// clears, as before.
	//
	// PAY-RECON-POLL-REF-CLEAR-1 (ledger-finance G-Y1, fail closed on
	// ambiguity): Y clears ONLY when it is attributable to this capture
	// (yAttributable). A Y that another attempt or another posting holds is a
	// DIFFERENT capture's reference; a reversal of that one must never clear a
	// park on X (LF F-1 / resolvesTo precedent: borrowed attribution may raise,
	// never clear). Then only X clears, as for a park without a Y row.
	//
	// PAY-RECON-POLL-REF-CLEAR-1 G-Y2 (ledger-finance ruling, REQUIRED): when
	// eligible succeeded deposit lines evidence BOTH X and Y (this run or any
	// persisted import), they are two evidenced captures, not one capture known
	// by two names: the finding clears only when BOTH are cleared. Otherwise the
	// "X or Y" rule stays.
	x := a.providerRef
	y, ok := m.k3.yRef[a.id]
	if !ok || y == "" || !m.yAttributable(a, y) {
		return !m.clearedRef(x)
	}
	if m.evidencedCapture(x) && m.evidencedCapture(y) {
		return !m.clearedRef(x) || !m.clearedRef(y)
	}
	return !m.clearedRef(x) && !m.clearedRef(y)
}

// evidencedCapture reports whether an ELIGIBLE (D-4/RC-3) persisted `succeeded`
// deposit line names ref. The run's own import is persisted before the match, so
// "this run or any persisted import" is one lookup.
func (m *payMatcher) evidencedCapture(ref string) bool {
	if ref == "" {
		return false
	}
	for _, l := range m.k3.byRef[ref] {
		if l.kind == "deposit" && l.status == paymentStatementStatusSucceeded && l.eligible {
			return true
		}
	}
	return false
}

// yAttributable reports whether the poll's returned reference y (typed Y
// evidence for attempt a) may serve as a CLEARING reference for a's park on X.
// It is true only if y is not already someone else's reference: no OTHER
// deposit or payout attempt of this (tenant, provider) holds it as its
// provider reference or payout settlement reference, and no ledger posting of
// type deposit, withdrawal_completed or deposit_reversal is keyed by it
// (a tombstone on y is allowed: it is exactly "a reversal naming y with no
// posted original"). All inputs are in the run snapshot (loadPlatform).
// MA020-SYNC-MISMATCH-1 must reuse this rule for its own Y clearing.
func (m *payMatcher) yAttributable(a *payAttempt, y string) bool {
	if y == "" {
		return false
	}
	// Security F-1: a Y that ANOTHER park also recorded as its returned
	// reference is ambiguous (one reversal must not clear two parks).
	for id, oy := range m.k3.yRef {
		if id != a.id && oy == y {
			return false
		}
	}
	for _, op := range []string{"deposit", "payout"} {
		if h := m.byRef[op+"\x00"+y]; h != nil && h != a {
			return false
		}
	}
	if h := m.bySettlement[y]; h != nil && h != a {
		return false
	}
	for _, typ := range []string{"deposit", "withdrawal_completed", "deposit_reversal"} {
		if m.ledgerByRef[typ+"\x00"+y] != nil {
			return false
		}
	}
	return true
}

// yHolderNote names an unattributable Y on a bound finding so the operator's
// manual-PSP-verification rule applies (POLL-REF-CLEAR-1): "" when a has no Y
// row or Y is attributable.
func (m *payMatcher) yHolderNote(a *payAttempt) string {
	y, ok := m.k3.yRef[a.id]
	if !ok || y == "" {
		return ""
	}
	if m.yAttributable(a, y) {
		if m.evidencedCapture(a.providerRef) && m.evidencedCapture(y) {
			return "; captures evidenced on BOTH " + a.providerRef + " and the poll_returned_reference=" + y + ": both must be reversed to clear"
		}
		return ""
	}
	return "; poll_returned_reference=" + y + " is held by another attempt, park or posting and is not used for clearing (verify with the PSP)"
}

// checkUnmatchedAttempts: attempts that no line in THIS run matched.
func (m *payMatcher) checkUnmatchedAttempts() {
	for _, a := range m.attempts {
		if _, ok := m.matchedBy[a.id]; ok {
			continue
		}
		k := m.key("attempt="+a.id.String(), "provider_reference="+orNone(a.providerRef))
		switch {
		// Ledger-finance review C2/F2 (rv-fh3-ledger.md, 076e42e,
		// HD-LEDGER-UNALLOC-1 (A)): a disputed multiple_success_for_intent
		// deposit attempt is real, standing, unallocated money off-ledger
		// until a PSP-side reversal or tombstone clears it - this report
		// is its ONLY record, so it must be raised on EVERY run, not only
		// the run whose statement happens to carry a line for it (a
		// capture's own settlement period passes, after which no line
		// ever names it again). Deliberately UNWINDOWED, like
		// m.attempts itself (loadPlatform's own query has no date
		// filter) - never gated on m.inCoverage/m.aged, which exist for
		// the OTHER two cases below, not this one.
		//
		// ADR 0095 §35: the same holds for every reason in
		// boundCapture (the attempt holds the captured
		// reference). An unbound park (unboundPark) is NOT
		// reported here: it holds no reference this rule could clear on.
		// A bound PAYOUT park clears only on its own positively attributed
		// withdrawal_completed keyed by X, never on a deposit-shaped signal
		// (capturedUnposted, PAY-PAYOUT-BOUND-CLEAR-1).
		case a.boundCapture() && m.capturedUnposted(a):
			m.r.add(MismatchKindPayCapturedUnposted, k+" check=captured_unposted",
				capturedUnpostedHintFor(a, boundCapturedUnpostedResolutionHint), m.label+"no statement line; platform: "+a.render()+" terminal_reason="+a.terminalReason+m.yHolderNote(a))
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
