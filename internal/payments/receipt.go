// PRH-I1 step (d): the receipt resolution path (ADR 0095 §6, §9.3,
// INV-IO-10, INV-IO-14). These are NEW, ADDITIVE functions - not wired
// into the live ReceiveVerifiedCallback/ReceiveCallback path
// (orchestrator.go), which keeps running exactly as it does today. The
// cutover (replacing that path's evidence handling with this one) is a
// SEPARATE, later commit, exactly as scoped.
//
// Every function here takes a tx the caller already has open inside
// db.Pool.WithTenant, and NONE of them makes a provider call (§6.5:
// "what a callback may never do" - no cascade I/O, no QueryStatus).
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// ReceiptDisposition mirrors payment_provider_events.disposition_at_receipt.
type ReceiptDisposition string

const (
	DispositionApplied            ReceiptDisposition = "applied"
	DispositionDuplicateEffect    ReceiptDisposition = "duplicate_effect"
	DispositionDeferredUnresolved ReceiptDisposition = "deferred_unresolved"
	DispositionAnomaly            ReceiptDisposition = "anomaly"
	DispositionUnsupportedEvent   ReceiptDisposition = "unsupported_event"
)

// ReceiptResolution mirrors payment_provider_events.resolution.
type ReceiptResolution string

const (
	ResolutionApplied                   ReceiptResolution = "applied"
	ResolutionAnomalyCrossProvider      ReceiptResolution = "anomaly_cross_provider"
	ResolutionAnomalyReferenceConflict  ReceiptResolution = "anomaly_reference_conflict"
	ResolutionAnomalyPredatesSubmission ReceiptResolution = "anomaly_predates_submission"
	ResolutionAnomalyOther              ReceiptResolution = "anomaly_other"
)

// maxDeclineReasonBytes mirrors the payment_attempts/payment_provider_events
// decline_reason CHECK (octet_length(decline_reason) <= 64) exactly.
const maxDeclineReasonBytes = 64

// boundedDeclineReasonSentinel is substituted whenever the vendor's own
// decline-reason text does not fit the CHECK. RV-PRH-I1 code review F2: an
// unbounded vendor decline_reason (a real-world example ran 83 bytes) must
// never reach either CHECK'd column as raw text - that violates the
// constraint, which fails the whole write with a Go error, which the
// webhook handler turns into a 500, which the PSP redelivers forever,
// identically, every time (an unrecoverable poison-message loop). Never
// silently truncated mid-UTF-8 either (a truncated multi-byte sequence is
// itself invalid UTF-8, which Postgres would also reject) - out-of-range
// text is replaced with this fixed, ASCII, well-under-64-byte sentinel
// instead of any prefix of the original.
const boundedDeclineReasonSentinel = "vendor_reason_too_long"

// boundedDeclineReason normalizes a PSP adapter's free-text decline reason
// to fit maxDeclineReasonBytes before it is ever passed to ApplyDecline,
// finalizeDeclined or any other CHECK'd decline_reason column. Called at
// every site that accepts vendor-controlled decline text (the receipt
// path, phase C's synchronous drive, and the sweeper's QueryStatus path) -
// never left to the database CHECK to discover.
func boundedDeclineReason(reason string) string {
	if len(reason) <= maxDeclineReasonBytes {
		return reason
	}
	return boundedDeclineReasonSentinel
}

// ReceiptEvidence is this step's canonical, adapter-agnostic evidence
// shape - deliberately separate from the existing CallbackEvent type
// (which the live, un-cut-over webhook path still owns), so this file
// never has to widen that type or its many existing call sites just to
// carry the ADR 0095 §9.3 additions (MerchantReference,
// SettlementReference, DeclineStage) a real cutover will eventually need.
type ReceiptEvidence struct {
	EventType                 string // "deposit" | "deposit_reversal" | "payout" | "payout_returned"
	ProviderReference         string
	OriginalProviderReference string
	MerchantReference         string
	SettlementReference       string
	Outcome                   Outcome
	Amount                    int64
	AssetCode                 string
	DeclineReason             string
	Cascadable                bool
	DeclineStage              DeclineStage
}

// ErrDeferredReceiptCapExceeded is returned when the unapplied-receipt
// cap for (tenant, provider) is already at or past the configured limit
// (S95-C2(i)) - the caller must map this to a retryable 503 and an
// alert, storing nothing (§6.1 step 5).
var ErrDeferredReceiptCapExceeded = errors.New("payments: deferred receipt cap exceeded for this (tenant, provider)")

// DeferredReceiptCap is §6.1 step 5's RECOMMENDATION default.
const DeferredReceiptCap = 10_000

// computeEventFingerprint is §9.3's SHA-256 over the canonical, length-
// prefixed tuple (event_type, provider_reference, original_provider_reference,
// merchant_reference, outcome, amount, asset_code, settlement_reference) -
// never raw bytes, so a vendor re-signing or reordering a redelivery
// never changes the fingerprint.
func computeEventFingerprint(ev ReceiptEvidence) []byte {
	h := sha256.New()
	writeField := func(s string) {
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	writeField(ev.EventType)
	writeField(ev.ProviderReference)
	writeField(ev.OriginalProviderReference)
	writeField(ev.MerchantReference)
	writeField(string(ev.Outcome))
	var amountBuf [8]byte
	binary.BigEndian.PutUint64(amountBuf[:], uint64(ev.Amount))
	h.Write(amountBuf[:])
	writeField(ev.AssetCode)
	writeField(ev.SettlementReference)
	return h.Sum(nil)
}

// CountUnappliedReceipts is §6.1 step 5's cap probe: unresolved receipts
// for (tenantID, providerID), capped at limit+1 so the caller only ever
// needs to know "at or past the cap", never the exact count above it.
func CountUnappliedReceipts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, limit int) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM (
			SELECT 1 FROM payment_provider_events
			WHERE tenant_id = $1 AND provider_id = $2 AND resolved_at IS NULL
			LIMIT $3
		) s`,
		tenantID, providerID, limit+1,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("payments: count unapplied receipts: %w", err)
	}
	return n, nil
}

// insertReceiptDeduped is §6.1 step 6 (R0): INSERT ... ON CONFLICT
// (tenant_id, provider_id, event_fingerprint) DO NOTHING RETURNING id.
// On conflict (duplicate delivery), it looks up and returns the EXISTING
// receipt's id, never inserting a second row for the same event.
func insertReceiptDeduped(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, ev ReceiptEvidence, disposition ReceiptDisposition) (uuid.UUID, bool, error) {
	fingerprint := computeEventFingerprint(ev)
	var declineStage, declineReason *string
	var cascadable *bool
	// decline_stage/cascadable are §4.4 deposit-ATTEMPT-decline concepts
	// (the CHECK constraint only accepts 'at_submission'/'after_acceptance',
	// never an empty string) - a deposit_reversal event reuses the wire
	// Outcome field for historical reasons (some callers still send
	// "declined" as a chargeback-reason carrier) but is never itself a
	// §4.4 decline cell, so it must never populate these columns.
	if ev.Outcome == OutcomeDeclined && ev.EventType == string(CallbackEventDeposit) {
		ds := string(ev.DeclineStage)
		declineStage = &ds
		if ev.DeclineReason != "" {
			declineReason = &ev.DeclineReason
		}
		c := ev.Cascadable
		cascadable = &c
	}
	var amount *int64
	var assetCode *string
	if ev.Outcome == OutcomeSucceeded {
		amount = &ev.Amount
		assetCode = &ev.AssetCode
	}
	var originalRef, merchantRef, settlementRef *string
	if ev.OriginalProviderReference != "" {
		originalRef = &ev.OriginalProviderReference
	}
	if ev.MerchantReference != "" {
		merchantRef = &ev.MerchantReference
	}
	if ev.SettlementReference != "" {
		settlementRef = &ev.SettlementReference
	}

	id := uuid.New()
	var returnedID uuid.UUID
	err := tx.QueryRow(ctx,
		`INSERT INTO payment_provider_events (
			id, tenant_id, provider_id, event_type, provider_reference, original_provider_reference,
			merchant_reference, settlement_reference, outcome, amount, asset_code,
			cascadable, decline_stage, decline_reason, event_fingerprint, disposition_at_receipt
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (tenant_id, provider_id, event_fingerprint) DO NOTHING
		RETURNING id`,
		id, tenantID, providerID, ev.EventType, ev.ProviderReference, originalRef,
		merchantRef, settlementRef, string(ev.Outcome), amount, assetCode,
		cascadable, declineStage, declineReason, fingerprint, string(disposition),
	).Scan(&returnedID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Conflict: a duplicate delivery. Look up the existing row.
		var existing uuid.UUID
		if lookupErr := tx.QueryRow(ctx,
			`SELECT id FROM payment_provider_events WHERE tenant_id = $1 AND provider_id = $2 AND event_fingerprint = $3`,
			tenantID, providerID, fingerprint,
		).Scan(&existing); lookupErr != nil {
			return uuid.Nil, false, fmt.Errorf("payments: look up duplicate receipt: %w", lookupErr)
		}
		return existing, true, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("payments: insert receipt: %w", err)
	}
	return returnedID, false, nil
}

// resolvedAttempt is ResolveAttemptForEvidence's outcome.
type resolvedAttempt struct {
	Attempt       PaymentAttempt
	Found         bool
	Anomaly       bool
	AnomalyReason ReceiptResolution
}

// ResolveAttemptForEvidence implements §6.1 step 4 and INV-IO-14: the
// attempt is resolved ONLY within verifiedProviderID (the route-resolved,
// credential-verified provider - NEVER a payload value), by provider
// reference, by merchant reference (also bound to verifiedProviderID),
// or both. A cross-provider or cross-attempt conflict is an anomaly with
// NO state change - never a rollback, never a 5xx-redelivery loop
// (LF95-C3, S95-C1).
func ResolveAttemptForEvidence(ctx context.Context, tx pgx.Tx, verifiedProviderID string, providerReference, merchantReference string) (resolvedAttempt, error) {
	var byRef, byMerchant *PaymentAttempt

	if providerReference != "" {
		a, err := GetAttemptByProviderReference(ctx, tx, verifiedProviderID, providerReference)
		if err != nil && !errors.Is(err, ErrAttemptNotFound) {
			return resolvedAttempt{}, err
		}
		if err == nil {
			byRef = &a
		}
	}
	if merchantReference != "" {
		a, err := GetAttemptByMerchantReference(ctx, tx, merchantReference)
		if err != nil && !errors.Is(err, ErrAttemptNotFound) {
			return resolvedAttempt{}, err
		}
		if err == nil {
			// INV-IO-14: a merchant-reference match must also be bound
			// to the verified provider. A match for a DIFFERENT
			// provider is never resolved from here at all (S95-C1) -
			// the merchant reference space is not provider-partitioned
			// by construction, so this check is load-bearing.
			if a.ProviderID != nil && *a.ProviderID == verifiedProviderID {
				byMerchant = &a
			} else {
				// A merchant-reference collision naming a different (or
				// unrouted) provider's attempt is itself an anomaly -
				// never silently ignored, never resolved as if absent.
				return resolvedAttempt{Anomaly: true, AnomalyReason: ResolutionAnomalyCrossProvider}, nil
			}
		}
	}

	switch {
	case byRef != nil && byMerchant != nil:
		if byRef.ID != byMerchant.ID {
			return resolvedAttempt{Anomaly: true, AnomalyReason: ResolutionAnomalyReferenceConflict}, nil
		}
		return resolvedAttempt{Attempt: *byRef, Found: true}, nil
	case byRef != nil:
		return resolvedAttempt{Attempt: *byRef, Found: true}, nil
	case byMerchant != nil:
		return resolvedAttempt{Attempt: *byMerchant, Found: true}, nil
	default:
		return resolvedAttempt{Found: false}, nil
	}
}

// ApplyReceiptEvidence is the top-level receipt-path orchestration:
// §6.1 steps 4 through 8, plus §4.4's preconditions. tx must already be
// a tenant-scoped transaction (db.Pool.WithTenant) with no prior write in
// it other than a possible re-check (§6.1 step 1, unchanged, not this
// function's concern). It performs, in order:
//
//  1. Resolve the attempt (INV-IO-14).
//  2. If unresolved: the cap probe, then a deferred receipt insert.
//  3. If resolved and unambiguous: lock the parent then the attempt
//     (ADR 0095 §14), re-read under the lock, insert the receipt, and
//     apply evidence via the SAME §4.4 matrix functions the sweeper and
//     phase C use (attempt.go), including a cascade insert on an
//     eligible decline and the T13t tombstone check on a deposit success.
//  4. If an anomaly was found in step 1 (cross-provider, reference
//     conflict, or a provider-reference already bound to a DIFFERENT
//     attempt than the one this call is about): insert the receipt as
//     `anomaly`, commit, change nothing (LF95-C3).
//
// evidence.ProviderReference and evidence.MerchantReference are BOTH
// resolved only within verifiedProviderID (INV-IO-14) - this function
// never reads a provider id from evidence itself.
func ApplyReceiptEvidence(ctx context.Context, tx pgx.Tx, o *Orchestrator, tenantID uuid.UUID, verifiedProviderID string, ev ReceiptEvidence) (ReceiptDisposition, error) {
	// §6.1 step 3 / §9.4: the platform-wide reference bound, checked at
	// this verified boundary before any read or write below.
	if err := validateReceiptReferences(ev); err != nil {
		return "", fmt.Errorf("%w: %w", ErrProviderReferenceInvalid, err)
	}

	// PRH-payments-callback-cutover / ADR 0095 §5.4, LF95-C6(b): a
	// deposit_reversal event is a fact about the ORIGINAL deposit attempt's
	// ledger posting, never a transition of that attempt's own state
	// machine (§4.4 has no reversal column). It is therefore resolved and
	// applied by its own dedicated function, keyed on
	// OriginalProviderReference - NEVER on ev.ProviderReference (the
	// reversal's own reference) or ev.MerchantReference, which the deposit
	// matrix below uses instead.
	if ev.EventType == string(CallbackEventDepositReversal) {
		return applyReversalReceiptEvidence(ctx, tx, tenantID, verifiedProviderID, ev)
	}

	resolved, err := ResolveAttemptForEvidence(ctx, tx, verifiedProviderID, ev.ProviderReference, ev.MerchantReference)
	if err != nil {
		return "", err
	}

	if resolved.Anomaly {
		receiptID, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, verifiedProviderID, ev, DispositionAnomaly)
		if err != nil {
			return "", err
		}
		// RV-PRH-I1 code review F1: every stored receipt must eventually be
		// resolved, or it counts toward the §6.1 step 5 unapplied-receipt
		// cap forever. A precondition anomaly (cross-provider/reference
		// conflict) has no single attempt to attach to, so attempt_id stays
		// NULL (the schema allows it) and resolution names the reason.
		if !duplicate {
			if err := ResolveReceipt(ctx, tx, receiptID, nil, string(resolved.AnomalyReason)); err != nil {
				return "", err
			}
		}
		return DispositionAnomaly, nil
	}

	if !resolved.Found {
		// Unresolved: the cap probe, then a deferred receipt (§6.1 steps
		// 5-6). Never a 200 without storing, never a 404 (S95-C2(i)).
		n, err := CountUnappliedReceipts(ctx, tx, tenantID, verifiedProviderID, DeferredReceiptCap)
		if err != nil {
			return "", err
		}
		if n > DeferredReceiptCap {
			return "", ErrDeferredReceiptCapExceeded
		}
		if _, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, verifiedProviderID, ev, DispositionDeferredUnresolved); err != nil {
			return "", err
		} else if duplicate {
			return DispositionDuplicateEffect, nil
		}
		return DispositionDeferredUnresolved, nil
	}

	attempt := resolved.Attempt

	// §4.4 precondition 2: a provider reference already bound to a
	// DIFFERENT attempt than the one just resolved is an anomaly, never
	// a unique-violation-then-5xx-redelivery-loop (LF95-C3).
	if ev.ProviderReference != "" {
		other, err := GetAttemptByProviderReference(ctx, tx, verifiedProviderID, ev.ProviderReference)
		if err == nil && other.ID != attempt.ID {
			receiptID, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, verifiedProviderID, ev, DispositionAnomaly)
			if err != nil {
				return "", err
			}
			if !duplicate {
				if err := ResolveReceipt(ctx, tx, receiptID, nil, string(ResolutionAnomalyReferenceConflict)); err != nil {
					return "", err
				}
			}
			return DispositionAnomaly, nil
		}
	}

	// ADR 0082 A7 / §14 (RV-PRH-I1 ledger-finance M3): R0 sits between L0
	// and L1 - the receipt insert is this transaction's FIRST write, before
	// the parent/attempt lock below, never after it. attempt is still the
	// UNLOCKED read from ResolveAttemptForEvidence above; a concurrent
	// transition between this insert and the lock below cannot change
	// WHICH attempt this receipt is fingerprinted/deduplicated against
	// (that is fixed by (provider_id, event_fingerprint), computed only
	// from ev), so this ordering is safe even though the attempt's exact
	// state could move before the lock is actually taken.
	receiptID, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, verifiedProviderID, ev, DispositionApplied)
	if err != nil {
		return "", err
	}

	// Lock parent then attempt (ADR 0095 §14), re-read under the lock.
	if attempt.DepositIntentID != nil {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *attempt.DepositIntentID); err != nil {
			return "", fmt.Errorf("payments: lock deposit intent for receipt: %w", err)
		}
	} else if attempt.WithdrawalRequestID != nil {
		if _, err := tx.Exec(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, *attempt.WithdrawalRequestID); err != nil {
			return "", fmt.Errorf("payments: lock withdrawal request for receipt: %w", err)
		}
	}
	attempt, err = GetAttemptByID(ctx, tx, attempt.ID)
	if err != nil {
		return "", err
	}

	changed, resolution, err := applyResolvedReceiptEvidence(ctx, tx, o, attempt, ev)
	if err != nil {
		return "", err
	}

	// ADR 0095 §5.1 "Ledger link" / LF95-C7: deposit_intents.status is a
	// projection of its attempts, recomputed in the SAME tx as every
	// attempt transition - not just the terminal succeeded/declined cases
	// applyDepositSuccessAndPost/finalizeDeclined already write directly.
	// Without this, a callback that only moves the ATTEMPT to pending/
	// ambiguous/disputed (T4, T6/T9/T11, T10, T13t) would leave the
	// intent's own status stale - e.g. still 'pending' after its one live
	// attempt actually went ambiguous.
	if changed && attempt.Operation == AttemptOperationDeposit && attempt.DepositIntentID != nil {
		if err := recomputeDepositIntentProjection(ctx, tx, *attempt.DepositIntentID); err != nil {
			return "", err
		}
	}

	if !duplicate {
		if err := ResolveReceipt(ctx, tx, receiptID, &attempt.ID, string(resolution)); err != nil {
			return "", err
		}
	}

	// RV-PRH-I1 ledger-finance H2 / S-Q2 (ADR 0095 §6.4): once THIS
	// attempt has learned its provider_reference (a T4/T9 transition just
	// ran inside applyResolvedReceiptEvidence above, or it already had one),
	// apply every OTHER deferred, unresolved receipt waiting on that same
	// (provider_id, provider_reference) - the ordinary "webhook beats phase
	// C" race this callback itself might have just resolved for a sibling
	// delivery. Runs under the SAME parent+attempt locks already held.
	if changed {
		if _, err := ApplyDeferredReceiptsForAttempt(ctx, tx, o, attempt); err != nil {
			return "", err
		}
	}

	if duplicate || !changed {
		return DispositionDuplicateEffect, nil
	}
	return DispositionApplied, nil
}

// applyResolvedReceiptEvidence maps evidence onto the §4.4 matrix for an
// attempt already resolved, locked and re-read. Returns changed=false for
// a genuine no-op cell (e.g. a duplicate success on an already-succeeded
// attempt) - the caller uses this to choose applied vs duplicate_effect -
// and resolution, the value this receipt's payment_provider_events.
// resolution column is one-shot-set to.
//
// RV-PRH-I1 ledger-finance M1: every §4.4 cell for EVERY reachable attempt
// state is handled explicitly below - none of them falls through to a CAS
// transition whose predicate cannot match the current state (which used
// to CAS-conflict and 500-loop for e.g. a mismatched success on an
// already-succeeded/declined attempt, or ANY evidence on a disputed/
// created/rejected attempt).
func applyResolvedReceiptEvidence(ctx context.Context, tx pgx.Tx, o *Orchestrator, attempt PaymentAttempt, ev ReceiptEvidence) (bool, ReceiptResolution, error) {
	switch ev.Outcome {
	case OutcomePending:
		switch attempt.State {
		case AttemptSubmitting, AttemptAmbiguous:
			if ev.ProviderReference == "" {
				return false, ResolutionAnomalyOther, nil
			}
			return true, ResolutionApplied, MarkAccepted(ctx, tx, attempt.ID, EvidenceCallback, ev.ProviderReference, time.Now().Add(30*time.Second))
		default:
			// created/rejected/pending/declined/succeeded/disputed: a
			// bare "still pending" reschedule signal is never itself a
			// state change or an anomaly worth disputing.
			return false, ResolutionAnomalyOther, nil
		}

	case OutcomeSucceeded:
		if ev.ProviderReference == "" {
			// §4.4 precondition 3: success with no reference is treated
			// as ambiguous evidence, never applied.
			return false, ResolutionAnomalyOther, nil
		}
		mismatched := attempt.Amount != ev.Amount || attempt.AssetCode != ev.AssetCode

		switch attempt.State {
		case AttemptCreated, AttemptRejected:
			// T15: success evidence for an attempt that was never sent to
			// (or was rejected before reaching) a provider.
			return true, ResolutionAnomalyOther, ApplyDisputeFromNeverSent(ctx, tx, attempt.ID, EvidenceCallback, "success_for_never_sent_attempt")
		case AttemptDisputed:
			return false, ResolutionAnomalyOther, nil // already terminal-disputed: no-op
		case AttemptSucceeded:
			return false, ResolutionApplied, nil // duplicate; the ledger itself is idempotent
		case AttemptDeclined:
			if mismatched {
				return false, ResolutionAnomalyOther, nil
			}
			if attempt.Operation != AttemptOperationDeposit {
				return true, ResolutionAnomalyOther, ApplyDisputeFromDeclinedPayout(ctx, tx, attempt.ID, EvidenceCallback, "success_after_payout_declined")
			}
			tombstoned, err := tombstoneExists(ctx, tx, attempt.TenantID, *attempt.ProviderID, ev.ProviderReference)
			if err != nil {
				return false, "", err
			}
			if tombstoned {
				return true, ResolutionAnomalyOther, ApplyTombstonePrecedesSuccess(ctx, tx, attempt.ID, EvidenceCallback)
			}
			if err := applyDepositSuccessAndPost(ctx, tx, o, attempt, ev); err != nil {
				return false, "", err
			}
			// RV-PRH-I1 ledger-finance H4/T13(c): this success came after
			// this SAME attempt had already declined - i.e. a genuine
			// T13 second capture - so any leftover 'created' cascade
			// sibling of the same intent must be rejected NOW, in this
			// same transaction, before it can ever reach T2 and place a
			// second real PSP charge.
			if err := rejectCreatedSiblings(ctx, tx, attempt); err != nil {
				return false, "", err
			}
			return true, ResolutionApplied, nil
		case AttemptSubmitting, AttemptPending, AttemptAmbiguous:
			if mismatched {
				return true, ResolutionAnomalyOther, ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, EvidenceCallback, "callback_amount_asset_mismatch")
			}
			tombstoned, err := tombstoneExists(ctx, tx, attempt.TenantID, *attempt.ProviderID, ev.ProviderReference)
			if err != nil {
				return false, "", err
			}
			if tombstoned {
				// Only a deposit's declined->disputed pair exists for a
				// tombstone (T13t); for a non-declined attempt a
				// tombstone collision is a plain anomaly dispute.
				return true, ResolutionAnomalyOther, ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, EvidenceCallback, "reversal_tombstone_precedes_success")
			}
			if attempt.Operation == AttemptOperationDeposit {
				if err := applyDepositSuccessAndPost(ctx, tx, o, attempt, ev); err != nil {
					return false, "", err
				}
				return true, ResolutionApplied, nil
			}
			// Payout success (T7 via withdrawal.Complete) is wired in a
			// later PRH-I1 step (payout dispatch); until then a payout
			// success receipt is recorded as a disputed anomaly rather
			// than silently dropped or, worse, guessed at.
			return true, ResolutionAnomalyOther, ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, EvidenceCallback, "payout_success_not_yet_wired")
		default:
			return false, ResolutionAnomalyOther, nil
		}

	case OutcomeDeclined:
		switch attempt.State {
		case AttemptSubmitting, AttemptPending, AttemptAmbiguous:
			// handled below, outside the switch
		default:
			// created/rejected/declined/succeeded/disputed: §4.4's
			// declined/succeeded rows (and every other terminal state)
			// are a no-op for decline evidence, never a CAS attempt.
			return false, ResolutionAnomalyOther, nil
		}
		var refPtr *string
		if ev.ProviderReference != "" {
			refPtr = &ev.ProviderReference
		}
		reason := boundedDeclineReason(ev.DeclineReason)
		var updated DepositIntent
		var err error
		if attempt.Operation == AttemptOperationDeposit {
			updated, err = o.finalizeDeclined(ctx, tx, DepositIntent{ID: *attempt.DepositIntentID, TenantID: attempt.TenantID}, attempt.ProviderID, refPtr, reason)
			if err != nil {
				return false, "", err
			}
		}
		cascadable := ev.Cascadable
		if err := ApplyDecline(ctx, tx, attempt.ID, DeclineEvidence{
			Evidence: EvidenceCallback, Reason: reason, Stage: DeclineAfterAcceptance,
			Cascadable: &cascadable, ProviderRef: refPtr,
		}); err != nil {
			return false, "", err
		}
		if attempt.Operation == AttemptOperationDeposit {
			// RV-PRH-I1 code review F3: cascade eligibility is decided
			// from updated.Status - finalizeDeclined's OWN return value,
			// which is a no-op (and so still reports 'succeeded') when a
			// sibling already succeeded - never from a fresh read taken
			// after finalizeDeclined ran, which would otherwise always
			// observe whatever finalizeDeclined itself just wrote.
			if cascadeEligible(attempt, updated.Status, cascadable, o.maxCascadeDepth(), true) {
				// RV-PRH-I1 ledger-finance H3 (ADR 0095 §10.3): an
				// engaged kill switch must never turn this callback into
				// a 500 - insertCascadeAttemptIfEligible downgrades
				// ErrKillSwitchEngaged to "no child, audited", the
				// decline itself (already applied above) still commits.
				if _, err := insertCascadeAttemptIfEligible(ctx, tx, attempt); err != nil {
					return false, "", err
				}
			}
		}
		return true, ResolutionApplied, nil

	default: // OutcomeAmbiguous
		switch attempt.State {
		case AttemptPending:
			return true, ResolutionApplied, MarkAmbiguousFromPending(ctx, tx, attempt.ID, EvidenceCallback, time.Now().Add(30*time.Second))
		case AttemptSubmitting:
			return true, ResolutionApplied, MarkAmbiguousFromSubmitting(ctx, tx, attempt.ID, EvidenceCallback, time.Now().Add(30*time.Second))
		default:
			// ambiguous->ambiguous, and every terminal state: no-op
			// (reschedule), never a CAS attempt against an unmatched
			// predicate.
			return false, ResolutionAnomalyOther, nil
		}
	}
}

// applyDepositSuccessAndPost posts Flow 1 (if not already posted) and
// applies T7/T13, reusing postDepositSuccess exactly as phase C and the
// sweeper do, so there is exactly one place that ever posts a deposit.
//
// PRH-I5 finding fix (ADR 0095 §4.3 T13, LF95-C6(a)): the attempt is
// linked to postDepositSuccess's own returned transaction id
// (postedTxID), NEVER to updated.LedgerTransactionID. For a T13 second
// capture (this attempt was 'declined', a SIBLING already 'succeeded'),
// intent.LedgerTransactionID still names the sibling's FIRST posting -
// linking THIS attempt to that id would collide with
// payment_attempts_tenant_ledger_tx's per-attempt uniqueness, which is
// exactly the unique violation / infinite-redelivery bug PRH-I5 found.
func applyDepositSuccessAndPost(ctx context.Context, tx pgx.Tx, o *Orchestrator, attempt PaymentAttempt, ev ReceiptEvidence) error {
	intent, err := GetDepositIntentByID(ctx, tx, *attempt.DepositIntentID)
	if err != nil {
		return err
	}
	_, postedTxID, err := o.postDepositSuccess(ctx, tx, intent, *attempt.ProviderID, ev.ProviderReference, ev.Amount, ev.AssetCode)
	if err != nil {
		return err
	}
	return ApplySuccess(ctx, tx, attempt.ID, SuccessEvidence{
		Evidence: EvidenceCallback, ProviderReference: ev.ProviderReference, LedgerTransactionID: &postedTxID,
	})
}

// applyReversalReceiptEvidence implements ADR 0095 §5.4 and LF95-C6(b)/(d):
// a deposit_reversal event's ORIGINAL deposit attempt is resolved by
// (verifiedProviderID, ev.OriginalProviderReference) - never by the
// reversal's own ev.ProviderReference or by ev.MerchantReference - and the
// ledger link used is THAT ATTEMPT's own ledger_transaction_id, never the
// parent deposit_intent's. Reversing the intent's (possibly first-capture)
// ledger_transaction_id instead of the resolved attempt's own would let a
// reversal of a SECOND capture (T13) incorrectly reverse the FIRST.
//
// No matching attempt, or a matching attempt that has never posted
// (LedgerTransactionID is nil - e.g. still submitting, or declined with no
// T13 success yet), takes the tombstone branch: a ledger TxTombstone is
// written on (provider_id, original_provider_reference) so a
// late-arriving original deposit success for that reference is rejected
// by the ledger's own uniqueness rather than posted after the fact
// (financial-transaction-flows.md Flow 2, CLAUDE.md's rollback rule).
//
// Typed reversal rejections (ErrDepositAlreadyReversed via
// *DepositAlreadyReversedError, ErrCallbackPayloadMismatch,
// ErrDepositReversalIntegrity, ErrCallbackProviderMismatch) are returned as
// plain Go errors and are UNCHANGED by this cutover (§6.2 "typed reversal
// rejections... unchanged: 409, with the existing separate-tx evidence
// record"): the caller's transaction (including any receipt insert this
// function attempted) rolls back, and the HTTP layer records the denial in
// a SEPARATE, freshly-opened transaction via RecordDepositReversalRejection
// exactly as it did before the cutover.
func applyReversalReceiptEvidence(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, verifiedProviderID string, ev ReceiptEvidence) (ReceiptDisposition, error) {
	// RV-PRH-I1 ledger-finance H1: wireOutcome is the UNMODIFIED evidence
	// as it arrived. Only a reversal whose wire outcome is actually
	// 'succeeded' may ever debit player_cash or write a tombstone -
	// anything else (declined/pending/ambiguous, or any other non-success
	// value some PSP sends) is recorded with no ledger effect whatsoever,
	// never silently normalized to a claimed success. See
	// applyNonSucceededReversalEvidence's own doc comment for the storage
	// compromise this requires.
	wireOutcome := ev.Outcome
	if wireOutcome != OutcomeSucceeded {
		return applyNonSucceededReversalEvidence(ctx, tx, tenantID, verifiedProviderID, ev, wireOutcome)
	}
	// Normalize the stored/fingerprinted outcome to 'succeeded' for every
	// deposit_reversal receipt this function actually persists (both the
	// tombstone and posting branches below) - already true here since
	// wireOutcome == OutcomeSucceeded, kept explicit for readability and
	// so the fingerprint computed below is stable across redeliveries.
	ev.Outcome = OutcomeSucceeded
	original, err := GetAttemptByProviderReference(ctx, tx, verifiedProviderID, ev.OriginalProviderReference)
	if err != nil && !errors.Is(err, ErrAttemptNotFound) {
		return "", err
	}
	unresolved := errors.Is(err, ErrAttemptNotFound)

	// RV-PRH-I1 ledger-finance M4 / code-review F4: the not-a-deposit
	// integrity check runs BEFORE the tombstone branch below. A reversal
	// naming a PAYOUT's provider_reference resolves to a payout attempt,
	// which never has a LedgerTransactionID linking a captured DEPOSIT (it
	// links a withdrawal completion instead, or nothing yet) - if the
	// tombstone branch ran first, "original.LedgerTransactionID == nil"
	// would ALWAYS be true for a payout attempt, silently tombstoning a
	// withdrawal reference and making this integrity check permanently
	// unreachable for payouts (and later making a genuine
	// withdrawal.Complete for that reference fail forever, looking up an
	// idempotency key that can never exist).
	if !unresolved && (original.Operation != AttemptOperationDeposit || original.DepositIntentID == nil) {
		return "", fmt.Errorf("%w: attempt %s resolved by a deposit_reversal event is not a deposit attempt with a parent intent",
			ErrDepositReversalIntegrity, original.ID)
	}

	if unresolved || original.LedgerTransactionID == nil {
		// RV-PRH-I1 ledger-finance L5: the parent lock, when there is a
		// resolved (but never-posted) original attempt, is cheap and
		// closes the "spurious 500 before this became safe" gap noted by
		// the review - taken here, before the tombstone write, mirroring
		// the posting branch's own lock below.
		if !unresolved && original.DepositIntentID != nil {
			if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *original.DepositIntentID); err != nil {
				return "", fmt.Errorf("payments: lock deposit intent for reversal tombstone: %w", err)
			}
		}
		// RV-PRH-I1 ledger-finance M3 (ADR 0082 A7): the receipt insert is
		// this transaction's own first write for this branch - moved
		// ahead of the tombstone/ledger write and the audit record below,
		// rather than after them.
		receiptID, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, verifiedProviderID, ev, DispositionApplied)
		if err != nil {
			return "", err
		}
		if duplicate {
			return DispositionDuplicateEffect, nil
		}
		txID, err := postDepositReversalTombstone(ctx, tx, tenantID, verifiedProviderID, ev.OriginalProviderReference)
		if err != nil {
			return "", err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "deposit.reversal_tombstoned",
			TargetType: "ledger_transaction", TargetID: txID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": verifiedProviderID, "original_provider_reference": ev.OriginalProviderReference,
				"reversal_provider_reference": ev.ProviderReference,
			},
		}); err != nil {
			return "", fmt.Errorf("payments: audit reversal tombstone: %w", err)
		}
		// RV-PRH-I1 ledger-finance M2: resolve this receipt in the SAME
		// transaction, or it counts toward the §6.1 step 5 unapplied cap
		// forever. attempt_id stays NULL (no attempt exists yet, or the
		// resolved one never posted) - the schema allows it.
		if err := ResolveReceipt(ctx, tx, receiptID, nil, string(ResolutionApplied)); err != nil {
			return "", err
		}
		return DispositionApplied, nil
	}

	// RV-PRH-I1 ledger-finance M3 (ADR 0082 A7): R0, this transaction's
	// first write for the posting branch, sits before the parent lock -
	// mirroring the deposit branch's own R0 placement in
	// ApplyReceiptEvidence above. A typed rejection further down (a
	// mismatch, an integrity failure, or DepositAlreadyReversedError)
	// still rolls back this entire transaction including this insert,
	// exactly as this function's own doc comment already describes -
	// the HTTP layer's separate-tx RecordDepositReversalRejection is
	// unaffected either way.
	receiptID, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, verifiedProviderID, ev, DispositionApplied)
	if err != nil {
		return "", err
	}

	// ADR 0095 §14 lock order: parent (deposit_intents) FOR UPDATE, then
	// the attempt, then re-read under the lock - mirrors the deposit
	// branch's own ordering in ApplyReceiptEvidence above, and Stage 10.1
	// PAY-REV-1's S2 step (docs/plans/stage-10.1-planning-gate-proposal.md
	// §E): two distinct-reference reversal callbacks for the SAME original
	// deposit must serialize on this lock, or both could observe
	// "not yet reversed" and both post, over-debiting player_cash.
	if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *original.DepositIntentID); err != nil {
		return "", fmt.Errorf("payments: lock deposit intent for reversal: %w", err)
	}
	original, err = GetAttemptByID(ctx, tx, original.ID)
	if err != nil {
		return "", err
	}

	// A missing ledger_transactions row, or one whose type is not
	// 'deposit', is an INTEGRITY failure - reaching here at all means the
	// attempt already POINTS AT a specific ledger_transactions id via
	// LedgerTransactionID; if that row does not exist, or is not a
	// deposit, the data is corrupted, not merely a late/duplicate
	// reversal, and posting anything against it would be worse than
	// failing closed.
	var lockedType ledger.TransactionType
	err = tx.QueryRow(ctx,
		`SELECT transaction_type FROM ledger_transactions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		*original.LedgerTransactionID, tenantID,
	).Scan(&lockedType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: attempt %s names ledger transaction %s, which does not exist for tenant %s",
			ErrDepositReversalIntegrity, original.ID, *original.LedgerTransactionID, tenantID)
	}
	if err != nil {
		return "", fmt.Errorf("payments: lock original deposit transaction: %w", err)
	}
	if lockedType != ledger.TxDeposit {
		return "", fmt.Errorf("%w: ledger transaction %s has type %q, expected %q",
			ErrDepositReversalIntegrity, *original.LedgerTransactionID, lockedType, ledger.TxDeposit)
	}

	// A reversal callback's amount/asset are payload-controlled facts
	// about a debit the platform is about to post (CLAUDE.md's
	// authorization rule applies here exactly as it does to a tenant_id).
	// Whole-deposit reversal only, never a partial refund: the only
	// legitimate value is the resolved ATTEMPT's own amount/asset (not the
	// parent intent's, in case of a T13 second capture).
	if ev.Amount > 0 && ev.Amount != original.Amount {
		return "", fmt.Errorf("%w: reversal amount %d does not match original deposit amount %d",
			ErrCallbackProviderMismatch, ev.Amount, original.Amount)
	}
	if ev.AssetCode != "" && ev.AssetCode != original.AssetCode {
		return "", fmt.Errorf("%w: reversal asset %q does not match original deposit asset %q",
			ErrCallbackProviderMismatch, ev.AssetCode, original.AssetCode)
	}
	amount := original.Amount

	// Reject a second reversal of the same original deposit under a NEW
	// provider_reference of its own; a REDELIVERY of the same reversal
	// (same provider_reference) falls through to ledger.Post, whose
	// idempotency check applies it idempotently. Run AFTER the lock above
	// is held (Stage 10.1 PAY-REV-1 S4): READ COMMITTED gives this
	// statement a fresh snapshot, so a concurrent distinct-reference
	// reversal that committed while this call queued on the lock is
	// visible here. IS DISTINCT FROM still counts a reversal row with a
	// NULL provider reference; LIMIT 1 is sufficient because
	// INV-PAY-REV-1 (migration 0092) guarantees at most one exists.
	var existingReversalID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id FROM ledger_transactions WHERE reverses_transaction_id = $1
		           AND (provider_id IS DISTINCT FROM $2 OR provider_tx_id IS DISTINCT FROM $3)
		 LIMIT 1`,
		original.LedgerTransactionID, verifiedProviderID, ev.ProviderReference,
	).Scan(&existingReversalID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("payments: check existing reversal: %w", err)
	}
	if err == nil {
		existingID := existingReversalID
		return "", &DepositAlreadyReversedError{
			DepositIntentID:               *original.DepositIntentID,
			OriginalLedgerTransactionID:   *original.LedgerTransactionID,
			RejectedReversalReference:     ev.ProviderReference,
			ExistingReversalTransactionID: &existingID,
		}
	}

	intent, err := GetDepositIntentByID(ctx, tx, *original.DepositIntentID)
	if err != nil {
		return "", err
	}
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &intent.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: original.AssetCode},
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: original.AssetCode},
	)
	if err != nil {
		return "", fmt.Errorf("payments: resolve deposit reversal ledger accounts: %w", err)
	}
	cashAccountID, clearingAccountID := accounts[0], accounts[1]

	reversalRef := ev.ProviderReference
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxDepositReversal, IdempotencyKey: verifiedProviderID + ":" + reversalRef,
		ProviderID: &verifiedProviderID, ProviderTxID: &reversalRef, CorrelationID: intent.ID,
		ReversesTransactionID: original.LedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: cashAccountID, Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: clearingAccountID, Direction: ledger.Credit, Amount: amount},
		},
	})
	if errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) {
		// This reversal reference is already posted with a different
		// payload (typically: it reversed a DIFFERENT deposit). Nothing is
		// posted; the caller gets an integrity failure.
		return "", fmt.Errorf("%w: post deposit reversal: %w", ErrCallbackPayloadMismatch, err)
	}
	if errors.Is(err, ledger.ErrReversalAlreadyExists) {
		// The S4 re-check above did not catch this (a writer bypassed the
		// lock, or a vanishingly unlikely timing gap), but the ledger's own
		// partial unique index still refused the INSERT. Mapped to the
		// SAME typed sentinel S4 uses, so there is exactly one denial code
		// path with exactly one shape.
		var existingID *uuid.UUID
		var id uuid.UUID
		if lookupErr := tx.QueryRow(ctx,
			`SELECT id FROM ledger_transactions
			  WHERE reverses_transaction_id = $1 AND transaction_type = 'deposit_reversal'
			  LIMIT 1`,
			original.LedgerTransactionID,
		).Scan(&id); lookupErr == nil {
			existingID = &id
		}
		return "", &DepositAlreadyReversedError{
			DepositIntentID:               *original.DepositIntentID,
			OriginalLedgerTransactionID:   *original.LedgerTransactionID,
			RejectedReversalReference:     reversalRef,
			ExistingReversalTransactionID: existingID,
		}
	}
	if err != nil {
		return "", fmt.Errorf("payments: post deposit reversal: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: "deposit.reversed",
		TargetType: "deposit_intent", TargetID: original.DepositIntentID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id": verifiedProviderID, "reversal_provider_reference": reversalRef,
			"original_ledger_transaction_id": original.LedgerTransactionID.String(),
			"reversal_ledger_transaction_id": postResult.TransactionID.String(), "amount": amount,
		},
	}); err != nil {
		return "", fmt.Errorf("payments: audit deposit reversal: %w", err)
	}

	// RV-PRH-I1 ledger-finance M2: resolve the receipt inserted as R0
	// above, in this SAME transaction - never left permanently unresolved
	// (it would otherwise count toward the §6.1 step 5 unapplied-receipt
	// cap forever).
	if !duplicate {
		if err := ResolveReceipt(ctx, tx, receiptID, &original.ID, string(ResolutionApplied)); err != nil {
			return "", err
		}
	}
	if duplicate {
		return DispositionDuplicateEffect, nil
	}
	return DispositionApplied, nil
}

// applyNonSucceededReversalEvidence implements RV-PRH-I1 ledger-finance H1:
// a deposit_reversal callback whose WIRE outcome is not 'succeeded' (a
// declined/pending/ambiguous value, including PSPs that reuse this field
// as a chargeback/refund-reason carrier) must never debit player_cash and
// must never write a reversal tombstone. It is recorded as an anomaly
// receipt with NO ledger effect and no attempt/intent state change, so a
// genuinely successful, later reversal (or a still-pending original
// deposit's genuine success) is never blocked by this one.
//
// Storage note (disclosed limitation, not full ADR fidelity): 'pending'
// and 'ambiguous' wire outcomes are stored verbatim - no CHECK constraint
// restricts them. A 'declined' wire outcome cannot be stored verbatim
// today: payment_provider_events_check1 requires cascadable+decline_stage
// whenever outcome='declined', and those are deposit-ATTEMPT-decline
// columns a reversal event has no value for; populating them here would
// misrepresent this as a deposit-attempt decline, which it is not.
// Storing a false outcome='succeeded' (silently claiming money-safety this
// event never had) would be strictly worse, so a 'declined' wire outcome
// is instead stored as 'ambiguous' - a disclosed stopgap pending a schema
// change (e.g. a reversal-specific outcome shape, or relaxing the CHECK
// for event_type='deposit_reversal') that must not be created as a new
// migration without confirming the next free migration number first. The
// TRUE wire value is preserved in the audit record either way, so nothing
// is silently lost even though the stored `outcome` column itself is not
// byte-for-byte faithful for this one case.
func applyNonSucceededReversalEvidence(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, verifiedProviderID string, ev ReceiptEvidence, wireOutcome Outcome) (ReceiptDisposition, error) {
	storedOutcome := wireOutcome
	if wireOutcome == OutcomeDeclined {
		storedOutcome = OutcomeAmbiguous
	}
	ev.Outcome = storedOutcome

	receiptID, duplicate, err := insertReceiptDeduped(ctx, tx, tenantID, verifiedProviderID, ev, DispositionAnomaly)
	if err != nil {
		return "", err
	}
	if duplicate {
		return DispositionDuplicateEffect, nil
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: "deposit.reversal_non_succeeded_evidence",
		TargetType: "payment_provider_event", TargetID: receiptID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{
			"provider_id": verifiedProviderID, "original_provider_reference": ev.OriginalProviderReference,
			"reversal_provider_reference": ev.ProviderReference, "wire_outcome": string(wireOutcome),
		},
	}); err != nil {
		return "", fmt.Errorf("payments: audit non-succeeded reversal evidence: %w", err)
	}
	if err := ResolveReceipt(ctx, tx, receiptID, nil, string(ResolutionAnomalyOther)); err != nil {
		return "", err
	}
	return DispositionAnomaly, nil
}

// recomputeDepositIntentProjection implements ADR 0095 §5.1's intent
// projection rule (LF95-C7), evaluated in this order over every
// payment_attempts row for intentID: any 'succeeded' -> 'succeeded'
// (naturally sticky - a succeeded attempt is never un-succeeded, so this
// value never regresses); any 'disputed' (and no 'succeeded') ->
// 'ambiguous', never 'declined', because funds may already have been
// captured; a live attempt that is 'ambiguous' -> 'ambiguous'; any other
// live attempt ('created','submitting','pending') -> 'pending'; none
// live -> 'declined'.
func recomputeDepositIntentProjection(ctx context.Context, tx pgx.Tx, intentID uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT state FROM payment_attempts WHERE deposit_intent_id = $1`, intentID)
	if err != nil {
		return fmt.Errorf("payments: recompute intent projection: query attempts: %w", err)
	}
	var anySucceeded, anyDisputed, anyAmbiguous, anyOtherLive bool
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			rows.Close()
			return fmt.Errorf("payments: recompute intent projection: scan state: %w", err)
		}
		switch AttemptState(state) {
		case AttemptSucceeded:
			anySucceeded = true
		case AttemptDisputed:
			anyDisputed = true
		case AttemptAmbiguous:
			anyAmbiguous = true
		case AttemptCreated, AttemptSubmitting, AttemptPending:
			anyOtherLive = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("payments: recompute intent projection: rows: %w", err)
	}
	rows.Close()

	var status DepositIntentStatus
	switch {
	case anySucceeded:
		status = DepositIntentSucceeded
	case anyDisputed, anyAmbiguous:
		status = DepositIntentAmbiguous
	case anyOtherLive:
		status = DepositIntentPending
	default:
		status = DepositIntentDeclined
	}
	if _, err := tx.Exec(ctx, `UPDATE deposit_intents SET status = $1 WHERE id = $2`, status, intentID); err != nil {
		return fmt.Errorf("payments: recompute intent projection: update: %w", err)
	}
	return nil
}

// tombstoneExists reports whether a reversal tombstone already occupies
// (providerID, providerReference) - postDepositReversalTombstone's own
// key shape (orchestrator.go), read-only here.
func tombstoneExists(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, providerReference string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone' AND provider_id = $2 AND provider_tx_id = $3)`,
		tenantID, providerID, providerReference,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("payments: check reversal tombstone: %w", err)
	}
	return exists, nil
}

// ApplyDeferredReceiptsForAttempt is §6.4's phase-C/sweeper backstop:
// once an attempt learns its provider_reference (T4/T9), every deferred,
// unresolved receipt for (attempt.ProviderID, attempt.ProviderReference)
// is applied in ascending receipt id, but ONLY if received_at >=
// attempt.FirstSubmittedAt (S95-C3) - an earlier receipt cannot describe
// this submission and is resolved as anomaly_predates_submission instead,
// never applied. The caller must already hold the parent and attempt
// locks (this function takes no lock itself).
func ApplyDeferredReceiptsForAttempt(ctx context.Context, tx pgx.Tx, o *Orchestrator, attempt PaymentAttempt) (int, error) {
	if attempt.ProviderID == nil || attempt.ProviderReference == nil {
		return 0, nil
	}
	// RV-PRH-I1 ledger-finance M2 / code-review F1: this backstop applies
	// evidence through the DEPOSIT §4.4 matrix below - a stored
	// deposit_reversal receipt sharing this attempt's (provider_id,
	// provider_reference) must never be picked up and replayed here as
	// deposit evidence (a reversal's Outcome is normalized to 'succeeded'
	// for storage, see applyReversalReceiptEvidence, which would otherwise
	// look exactly like a deposit success to the code below).
	rows, err := tx.Query(ctx,
		`SELECT id, event_type, provider_reference, original_provider_reference, merchant_reference,
		        settlement_reference, outcome, amount, asset_code, decline_reason, decline_stage, cascadable, received_at
		 FROM payment_provider_events
		 WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3 AND resolved_at IS NULL
		   AND event_type = 'deposit'
		 ORDER BY id`,
		attempt.TenantID, *attempt.ProviderID, *attempt.ProviderReference,
	)
	if err != nil {
		return 0, fmt.Errorf("payments: query deferred receipts: %w", err)
	}
	type deferredRow struct {
		id                                           uuid.UUID
		eventType, providerReference                 string
		originalProviderReference, merchantReference *string
		settlementReference                          *string
		outcome                                      string
		amount                                       *int64
		assetCode, declineReason, declineStage       *string
		cascadable                                   *bool
		receivedAt                                   time.Time
	}
	var deferred []deferredRow
	for rows.Next() {
		var d deferredRow
		if err := rows.Scan(&d.id, &d.eventType, &d.providerReference, &d.originalProviderReference, &d.merchantReference,
			&d.settlementReference, &d.outcome, &d.amount, &d.assetCode, &d.declineReason, &d.declineStage, &d.cascadable, &d.receivedAt); err != nil {
			rows.Close()
			return 0, err
		}
		deferred = append(deferred, d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	applied := 0
	for _, d := range deferred {
		if attempt.FirstSubmittedAt != nil && d.receivedAt.Before(*attempt.FirstSubmittedAt) {
			if err := ResolveReceipt(ctx, tx, d.id, &attempt.ID, string(ResolutionAnomalyPredatesSubmission)); err != nil {
				return applied, err
			}
			continue
		}
		ev := ReceiptEvidence{EventType: d.eventType, ProviderReference: d.providerReference, Outcome: Outcome(d.outcome)}
		if d.originalProviderReference != nil {
			ev.OriginalProviderReference = *d.originalProviderReference
		}
		if d.merchantReference != nil {
			ev.MerchantReference = *d.merchantReference
		}
		if d.settlementReference != nil {
			ev.SettlementReference = *d.settlementReference
		}
		if d.amount != nil {
			ev.Amount = *d.amount
		}
		if d.assetCode != nil {
			ev.AssetCode = *d.assetCode
		}
		if d.declineReason != nil {
			ev.DeclineReason = *d.declineReason
		}
		if d.declineStage != nil {
			ev.DeclineStage = DeclineStage(*d.declineStage)
		}
		if d.cascadable != nil {
			ev.Cascadable = *d.cascadable
		}

		current, err := GetAttemptByID(ctx, tx, attempt.ID)
		if err != nil {
			return applied, err
		}
		changed, resolution, err := applyResolvedReceiptEvidence(ctx, tx, o, current, ev)
		if err != nil {
			return applied, err
		}
		if changed && current.Operation == AttemptOperationDeposit && current.DepositIntentID != nil {
			if err := recomputeDepositIntentProjection(ctx, tx, *current.DepositIntentID); err != nil {
				return applied, err
			}
		}
		if err := ResolveReceipt(ctx, tx, d.id, &attempt.ID, string(resolution)); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// validateReceiptReferences is PROVIDER-REF-BOUND-1 (§9.4): every
// provider-supplied reference on the evidence is validated at THIS
// verified boundary, before any write, using the same platform-wide
// bound migration 0099/internal/providerref define everywhere else. A
// violation is deterministic and non-retryable - the caller must map it
// to a 4xx-class rejection, never a retry signal.
func validateReceiptReferences(ev ReceiptEvidence) error {
	return providerref.ValidateAll(
		providerref.Field{Name: "provider_reference", Value: ev.ProviderReference, Required: true},
		providerref.Field{Name: "original_provider_reference", Value: ev.OriginalProviderReference, Required: false},
		providerref.Field{Name: "merchant_reference", Value: ev.MerchantReference, Required: false},
		providerref.Field{Name: "settlement_reference", Value: ev.SettlementReference, Required: false},
	)
}
