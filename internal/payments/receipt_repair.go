package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// PAY-RECEIPT-ANOMALY-APPLIED-1 Option A (owner decisions 24-30, ADR 0095 sections 44 and 45).
//
// RepairReceiptAttribution is a ONE-TIME, LOCKED, IDEMPOTENT, FULLY AUDITED attribution/audit repair
// of ONE anomaly-closed receipt. It is NOT a financial correction: it never moves money, alters a
// balance, releases a hold, settles or completes a payment, writes a ledger entry, changes an attempt
// or a withdrawal, or touches provider money state. Its only writes are
//
//	(a) payment_provider_events.attempt_id, NULL -> the derived attempt, once (the original
//	    resolution anomaly_* and resolved_at are NOT touched: the anomaly history is preserved);
//	(b) append-only audit_log rows: the replayed terminal-cell audit row the lost application would
//	    have written (flagged reconstructed), and payments.receipt_attribution_repaired; or, on a
//	    refusal, payments.receipt_attribution_repair_refused plus the durable signal.
//
// SYSTEM-INTERNAL. There is no HTTP route, no admin override and no operator entry point: a
// staff-triggered path would need four-eyes governance and a signed actor proof (ADR 0110), which the
// owner has not decided. The caller is a code path that supplies a bounded Caller label; the audit
// actor is always the system actor.
//
// The target is NEVER caller-supplied. RepairReceiptAttribution takes a receipt id and derives the
// target from the receipt's own immutable evidence (see deriveRepairTarget); the migration 0122 guard
// independently refuses any attribution that does not match that evidence, so even a direct UPDATE
// cannot reassign a receipt to a caller-chosen attempt.
//
// LOCK ORDER (ADR 0095 section 14, parent -> attempt -> receipt): the derived attempt's parent
// (deposit_intents / withdrawal_requests) FOR UPDATE, then the attempt row FOR UPDATE, then the
// receipt row FOR UPDATE. The target is derived from an unlocked read of the receipt's IMMUTABLE
// columns, and re-derived under the locks; a different outcome is ErrReceiptRepairConcurrentChange
// (nothing written, safe to retry). A refusal has no attempt to lock and takes only the receipt lock.
// Two concurrent repairs of one receipt serialize on the receipt row; the second sees attempt_id set
// and the repair audit row and returns RepairAlreadyRepaired. A concurrent callback or drain applying
// on the same attempt holds the same parent lock and so serializes with the repair; the repair never
// changes the attempt, so it cannot cause (or leak) a state conflict.

// RepairReasonAnomalyAppliedGap is the only reason code accepted (closed set of one).
const RepairReasonAnomalyAppliedGap = "anomaly_applied_attribution_gap"

const (
	auditActionReceiptAttributionRepaired      = "payments.receipt_attribution_repaired"
	auditActionReceiptAttributionRepairRefused = "payments.receipt_attribution_repair_refused"
	auditTargetPaymentProviderEvent            = "payment_provider_event"
)

// ReceiptRepairOutcome is the result class of one call.
type ReceiptRepairOutcome string

const (
	RepairRepaired        ReceiptRepairOutcome = "repaired"
	RepairAlreadyRepaired ReceiptRepairOutcome = "already_repaired"
	RepairRefused         ReceiptRepairOutcome = "refused"
)

// Refusal reasons. The first four also raise the durable signal; the last is a plain ineligibility.
const (
	RefusalNoCandidate           = alertReasonReceiptRepairNoCandidate
	RefusalAmbiguousCandidates   = alertReasonReceiptRepairAmbiguous
	RefusalContradictoryEvidence = alertReasonReceiptRepairContradict
	RefusalInsufficientEvidence  = alertReasonReceiptRepairInsufficent
	RefusalNotEligible           = "receipt_not_eligible"
)

var (
	// ErrReceiptRepairNotFound: no receipt with that id is visible in the tenant (RLS makes another
	// tenant's receipt indistinguishable from a missing one).
	ErrReceiptRepairNotFound = errors.New("payments: receipt attribution repair: receipt not found")
	// ErrReceiptRepairInvalidRequest: a malformed request (nil ids, unknown reason, bad caller label).
	ErrReceiptRepairInvalidRequest = errors.New("payments: receipt attribution repair: invalid request")
	// ErrReceiptRepairConcurrentChange: the derivation under the locks differs from the unlocked
	// one. Nothing was written; retry.
	ErrReceiptRepairConcurrentChange = errors.New("payments: receipt attribution repair: concurrent change, retry")
)

var repairCallerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,63}$`)

// ReceiptRepairRequest identifies exactly one receipt. There is deliberately NO target-attempt field.
type ReceiptRepairRequest struct {
	TenantID  uuid.UUID
	ReceiptID uuid.UUID
	Reason    string // must be RepairReasonAnomalyAppliedGap
	Caller    string // bounded label of the internal caller, [a-z0-9_.:-]{1,64}
}

// ReceiptRepairResult is the outcome of one call. AttemptID is set for repaired / already_repaired.
type ReceiptRepairResult struct {
	Outcome       ReceiptRepairOutcome
	ReceiptID     uuid.UUID
	AttemptID     uuid.UUID
	RefusalReason string
	Signalled     bool
	// ReplayedAudit lists the audit actions replayed from the receipt evidence; Unknowable lists what
	// could not be reconstructed (recorded in the repair audit row).
	ReplayedAudit []string
	Unknowable    []string
}

// RepairReceiptAttribution repairs exactly one anomaly-closed receipt, or refuses. See the file comment.
func RepairReceiptAttribution(ctx context.Context, pool *db.Pool, req ReceiptRepairRequest) (ReceiptRepairResult, error) {
	if req.TenantID == uuid.Nil || req.ReceiptID == uuid.Nil || req.Reason != RepairReasonAnomalyAppliedGap || !repairCallerRE.MatchString(req.Caller) {
		return ReceiptRepairResult{}, ErrReceiptRepairInvalidRequest
	}
	var res ReceiptRepairResult
	// ADR 0102 B-1: the transaction opens through alerting.InTx and flushes after the commit, so a
	// refusal's signal is durable (or retried) and a rolled-back transaction raises nothing.
	pending, err := alerting.InTx(ctx, alerting.NewTenantRunner(pool, req.TenantID), func(actx context.Context, tx pgx.Tx) error {
		var err error
		res, err = repairReceiptAttributionTx(actx, tx, req)
		return err
	})
	if err != nil {
		return ReceiptRepairResult{}, err
	}
	pending.Flush(ctx)
	return res, nil
}

// repairReceipt is the receipt's evidence (immutable) and closure state (mutable) as read.
type repairReceipt struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ProviderID  string
	EventType   string
	ProviderRef string
	MerchantRef *string
	Outcome     Outcome
	Amount      *int64
	AssetCode   *string
	AttemptID   *uuid.UUID
	Resolution  *string
	ResolvedAt  *time.Time
	ReceivedAt  time.Time
}

func (r repairReceipt) anomalyClosed() bool {
	if r.ResolvedAt == nil || r.Resolution == nil {
		return false
	}
	switch ReceiptResolution(*r.Resolution) {
	case ResolutionAnomalyCrossProvider, ResolutionAnomalyReferenceConflict, ResolutionAnomalyPredatesSubmission, ResolutionAnomalyOther:
		return true
	}
	return false
}

func readRepairReceipt(ctx context.Context, tx pgx.Tx, tenantID, receiptID uuid.UUID, lock bool) (repairReceipt, error) {
	q := `SELECT id, tenant_id, provider_id, event_type, provider_reference, merchant_reference, outcome, amount, asset_code,
	             attempt_id, resolution, resolved_at, received_at
	        FROM payment_provider_events WHERE id = $1 AND tenant_id = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	var r repairReceipt
	var outcome string
	err := tx.QueryRow(ctx, q, receiptID, tenantID).Scan(&r.ID, &r.TenantID, &r.ProviderID, &r.EventType, &r.ProviderRef, &r.MerchantRef,
		&outcome, &r.Amount, &r.AssetCode, &r.AttemptID, &r.Resolution, &r.ResolvedAt, &r.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return repairReceipt{}, ErrReceiptRepairNotFound
	}
	if err != nil {
		return repairReceipt{}, fmt.Errorf("payments: read receipt for attribution repair: %w", err)
	}
	r.Outcome = Outcome(outcome)
	return r, nil
}

// deriveRepairTarget derives the single target attempt from the receipt's own immutable evidence, or
// returns a closed refusal reason. It reads, never writes, and never consults a caller value.
//
//	byRef      the attempt of (receipt tenant, receipt provider, receipt provider_reference)
//	byMerchant the attempt of (receipt tenant, receipt merchant_reference), when the receipt has one
//
// Positive derivation requires ALL of: every attempt found is bound to the receipt's provider (INV-IO-14;
// a cross-provider or unrouted candidate is never used); the receipt's provider_reference resolves to
// the attempt, so the attempt has itself bound the reference (the circumstantial application trace);
// when the receipt has a merchant_reference, the SAME attempt carries it; the event type matches the
// attempt's operation (deposit/payout allow-list); and the evidence is consistent with the attempt
// having APPLIED it, and the receipt must postdate the attempt's first submission (a success cannot have been applied to an attempt still submitting/pending/ambiguous).
// Anything else is refused: zero candidates, two different candidates, contradictory evidence, or
// insufficient evidence. The Y-bound-refP reference-conflict race lands in ambiguous_candidates by design.
func deriveRepairTarget(ctx context.Context, tx pgx.Tx, r repairReceipt) (*PaymentAttempt, string, error) {
	if r.EventType != string(CallbackEventDeposit) && r.EventType != "payout" {
		return nil, RefusalInsufficientEvidence, nil
	}
	var byRef, byMerchant *PaymentAttempt
	row := tx.QueryRow(ctx, `SELECT `+paymentAttemptColumns+` FROM payment_attempts WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3`,
		r.TenantID, r.ProviderID, r.ProviderRef)
	if a, err := scanPaymentAttempt(row); err == nil {
		byRef = &a
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", fmt.Errorf("payments: derive repair target by provider reference: %w", err)
	}
	if r.MerchantRef != nil && *r.MerchantRef != "" {
		a, err := GetAttemptByMerchantReference(ctx, tx, r.TenantID, *r.MerchantRef)
		if err == nil {
			byMerchant = &a
		} else if !errors.Is(err, ErrAttemptNotFound) {
			return nil, "", err
		}
	}
	for _, c := range []*PaymentAttempt{byRef, byMerchant} {
		if c != nil && (c.ProviderID == nil || *c.ProviderID != r.ProviderID || c.TenantID != r.TenantID) {
			return nil, RefusalContradictoryEvidence, nil
		}
	}
	switch {
	case byRef == nil && byMerchant == nil:
		return nil, RefusalNoCandidate, nil
	case byRef != nil && byMerchant != nil && byRef.ID != byMerchant.ID:
		return nil, RefusalAmbiguousCandidates, nil
	case byRef == nil:
		// The merchant reference names an attempt that has not bound the receipt's provider reference:
		// no trace that the evidence was ever applied to it (or it bound a different reference).
		if byMerchant.ProviderReference == nil {
			return nil, RefusalInsufficientEvidence, nil
		}
		return nil, RefusalContradictoryEvidence, nil
	case r.MerchantRef != nil && *r.MerchantRef != "" && byMerchant == nil:
		// The receipt claims a merchant reference that names no attempt, while its provider reference
		// names one whose merchant reference differs.
		return nil, RefusalContradictoryEvidence, nil
	}
	a := byRef
	op := AttemptOperationDeposit
	if r.EventType == "payout" {
		op = AttemptOperationPayout
	}
	if a.Operation != op {
		return nil, RefusalContradictoryEvidence, nil
	}
	// LF C1: the receipt must postdate the attempt's first submission. provider_id and first_submitted_at are
	// set in the same T2 UPDATE, so a receipt that predates it (the S7 cross-provider closure of an unrouted
	// cascade child) cannot describe this submission (S95-C3); the drain would close it as predates_submission.
	if a.FirstSubmittedAt == nil || r.ReceivedAt.Before(*a.FirstSubmittedAt) {
		return nil, RefusalInsufficientEvidence, nil
	}
	if !repairApplicationConsistent(*a, r) {
		return nil, RefusalInsufficientEvidence, nil
	}
	return a, "", nil
}

// repairApplicationConsistent reports whether the attempt's CURRENT state is consistent with it having
// applied the receipt's evidence. A success applied to a non-terminal attempt either settles it or
// disputes it, so a success receipt on an attempt still submitting/pending/ambiguous/created/rejected
// cannot have been applied. A mismatched success applied to a succeeded/declined attempt is the
// no-state-change terminal cell; applied to anything else it disputes. Other outcomes constrain nothing
// beyond the attempt having bound the reference.
func repairApplicationConsistent(a PaymentAttempt, r repairReceipt) bool {
	if r.Outcome != OutcomeSucceeded {
		return true
	}
	switch a.State {
	case AttemptSucceeded, AttemptDisputed:
		return true
	case AttemptDeclined:
		return repairMismatched(a, r) // a matched success on a declined attempt posts or disputes
	}
	return false
}

func repairMismatched(a PaymentAttempt, r repairReceipt) bool {
	return r.Amount == nil || r.AssetCode == nil || a.Amount != *r.Amount || a.AssetCode != *r.AssetCode
}

func repairReceiptAttributionTx(ctx context.Context, tx pgx.Tx, req ReceiptRepairRequest) (ReceiptRepairResult, error) {
	res := ReceiptRepairResult{ReceiptID: req.ReceiptID}

	// 1. Unlocked read: only the IMMUTABLE evidence is relied on from here; closure state is re-read locked.
	snap, err := readRepairReceipt(ctx, tx, req.TenantID, req.ReceiptID, false)
	if err != nil {
		return res, err
	}
	snapEligible := snap.AttemptID == nil && snap.anomalyClosed()
	var candidate *PaymentAttempt
	var refusal string
	if snapEligible {
		if candidate, refusal, err = deriveRepairTarget(ctx, tx, snap); err != nil {
			return res, err
		}
	}

	// 2. Parent, then attempt (only when a candidate exists), then the receipt row.
	if candidate != nil {
		if err := lockAttemptParent(ctx, tx, *candidate); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM payment_attempts WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, candidate.ID, req.TenantID); err != nil {
			return res, fmt.Errorf("payments: lock attempt for attribution repair: %w", err)
		}
	}
	rcpt, err := readRepairReceipt(ctx, tx, req.TenantID, req.ReceiptID, true)
	if err != nil {
		return res, err
	}

	// 3. Eligibility under the receipt lock.
	if rcpt.AttemptID != nil {
		repaired, err := repairAuditExists(ctx, tx, req.TenantID, req.ReceiptID)
		if err != nil {
			return res, err
		}
		if repaired {
			res.Outcome, res.AttemptID = RepairAlreadyRepaired, *rcpt.AttemptID
			return res, nil
		}
		return refuseRepair(ctx, tx, req, rcpt, res, RefusalNotEligible, "receipt_already_attributed")
	}
	if rcpt.ResolvedAt == nil {
		return refuseRepair(ctx, tx, req, rcpt, res, RefusalNotEligible, "receipt_unresolved")
	}
	if !rcpt.anomalyClosed() {
		return refuseRepair(ctx, tx, req, rcpt, res, RefusalNotEligible, "receipt_not_anomaly_closed")
	}
	if !snapEligible {
		return res, ErrReceiptRepairConcurrentChange // became eligible between the reads
	}

	// 4. Re-derive under the locks; a different answer means something moved between the reads.
	target, refusal2, err := deriveRepairTarget(ctx, tx, rcpt)
	if err != nil {
		return res, err
	}
	if (target == nil) != (candidate == nil) || refusal2 != refusal || (target != nil && target.ID != candidate.ID) {
		return res, ErrReceiptRepairConcurrentChange
	}
	if target == nil {
		return refuseRepair(ctx, tx, req, rcpt, res, refusal2, "")
	}

	// 5. Apply. The UPDATE writes attempt_id only; the guard (migration 0122) independently checks
	// tenant, provider, references and operation of the target against the receipt's evidence.
	tag, err := tx.Exec(ctx,
		`UPDATE payment_provider_events SET attempt_id = $2
		  WHERE id = $1 AND tenant_id = $3 AND attempt_id IS NULL AND resolved_at IS NOT NULL AND left(resolution, 8) = 'anomaly_'`,
		rcpt.ID, target.ID, rcpt.TenantID)
	if err != nil {
		return res, fmt.Errorf("payments: attribute anomaly receipt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return res, fmt.Errorf("payments: attribute anomaly receipt: %d rows affected, want 1", tag.RowsAffected())
	}
	replayed, unknowable, err := replayLostCellAudit(ctx, tx, *target, rcpt)
	if err != nil {
		return res, err
	}
	basis := "provider_reference"
	if rcpt.MerchantRef != nil && *rcpt.MerchantRef != "" {
		basis = "provider_reference+merchant_reference"
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: rcpt.TenantID, ActorType: audit.ActorSystem, Action: auditActionReceiptAttributionRepaired,
		TargetType: auditTargetPaymentProviderEvent, TargetID: rcpt.ID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"reason": req.Reason, "caller": req.Caller,
			"attempt_id": target.ID.String(), "operation": string(target.Operation), "provider_id": rcpt.ProviderID,
			"derivation_basis": basis,
			"application_basis": "circumstantial: the attempt itself bound the receipt provider reference; " +
				"the application of the evidence is not directly recorded",
			"before":               map[string]any{"attempt_id": nil, "resolution": *rcpt.Resolution, "resolved_at": rcpt.ResolvedAt.UTC().Format(time.RFC3339Nano)},
			"after":                map[string]any{"attempt_id": target.ID.String(), "resolution": *rcpt.Resolution},
			"resolution_preserved": true,
			"reconstructed":        true,
			"attempt_state_now":    string(target.State),
			"replayed_audit":       replayed,
			"unknowable":           unknowable,
			"financial_effect":     "none",
		},
	}); err != nil {
		return res, fmt.Errorf("payments: audit receipt attribution repair: %w", err)
	}
	res.Outcome, res.AttemptID, res.ReplayedAudit, res.Unknowable = RepairRepaired, target.ID, replayed, unknowable
	return res, nil
}

// lockAttemptParent takes the parent row lock (deposit intent or withdrawal request), the first
// element of the parent -> attempt -> receipt order.
func lockAttemptParent(ctx context.Context, tx pgx.Tx, a PaymentAttempt) error {
	if a.DepositIntentID != nil {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *a.DepositIntentID); err != nil {
			return fmt.Errorf("payments: lock deposit intent for attribution repair: %w", err)
		}
	} else if a.WithdrawalRequestID != nil {
		if _, err := tx.Exec(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, *a.WithdrawalRequestID); err != nil {
			return fmt.Errorf("payments: lock withdrawal request for attribution repair: %w", err)
		}
	}
	return nil
}

func repairAuditExists(ctx context.Context, tx pgx.Tx, tenantID, receiptID uuid.UUID) (bool, error) {
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_type = $3 AND target_id = $4`,
		tenantID, auditActionReceiptAttributionRepaired, auditTargetPaymentProviderEvent, receiptID.String()).Scan(&n); err != nil {
		return false, fmt.Errorf("payments: read receipt repair audit: %w", err)
	}
	return n > 0, nil
}

// refuseRepair writes the refusal audit row and, for an evidence refusal, the durable signal as the
// LAST statement. The receipt is left exactly as it was.
func refuseRepair(ctx context.Context, tx pgx.Tx, req ReceiptRepairRequest, r repairReceipt, res ReceiptRepairResult, reason, detail string) (ReceiptRepairResult, error) {
	_, signal := receiptRepairSignalReasons[reason]
	meta := map[string]any{"reason": req.Reason, "caller": req.Caller, "refusal": reason, "signalled": signal, "provider_id": r.ProviderID}
	if detail != "" {
		meta["detail"] = detail
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: r.TenantID, ActorType: audit.ActorSystem, Action: auditActionReceiptAttributionRepairRefused,
		TargetType: auditTargetPaymentProviderEvent, TargetID: r.ID.String(), Outcome: audit.OutcomeDenied, Metadata: meta,
	}); err != nil {
		return res, fmt.Errorf("payments: audit receipt attribution repair refusal: %w", err)
	}
	res.Outcome, res.RefusalReason, res.Signalled = RepairRefused, reason, signal
	if signal {
		if err := raiseReceiptRepairRefusedAlert(ctx, tx, r.TenantID, r.ID, r.ProviderID, reason); err != nil {
			return res, err
		}
	}
	return res, nil
}

// replayLostCellAudit rebuilds the audit row the lost application would have written. The only
// application cells whose audit row is gated on "already applied" are the no-state-change terminal
// mismatch cells (a success with a different amount/asset on a succeeded or declined attempt); every
// other cell writes its own rows regardless. The M-1 foreign-reference cell cannot arise here: the
// derivation requires the attempt's reference to equal the receipt's.
//
// The attempt state AT THE TIME of the event is unknowable. It is inferred only where it is forced:
// a mismatched success applied to a non-terminal attempt would have disputed it, and succeeded and
// declined are final but for declined -> disputed, so a currently succeeded/declined attempt was in
// that state then. A currently disputed attempt is not replayed (the cell may have been the dispute
// itself, which wrote its own rows) and the gap is recorded. Nothing is raised: the cell's raise is
// unconditional and has already fired.
func replayLostCellAudit(ctx context.Context, tx pgx.Tx, a PaymentAttempt, r repairReceipt) (replayed, unknowable []string, err error) {
	replayed, unknowable = []string{}, []string{"attempt_state_at_event"}
	if r.Outcome != OutcomeSucceeded || !repairMismatched(a, r) {
		return replayed, unknowable, nil
	}
	if a.State != AttemptSucceeded && a.State != AttemptDeclined {
		return replayed, append(unknowable, "terminal_mismatch_audit_not_replayed_attempt_state_not_succeeded_or_declined"), nil
	}
	// LF C2: a terminal_reason (the attempt was parked, then possibly forced by M2 disputed -> succeeded/declined,
	// R-7 keeps the reason) or operator/legacy evidence means the state at the event may have been disputed, where
	// the terminal cell did not fire. Replaying would fabricate an audit fact: skip and record it.
	if a.TerminalReason != nil || a.LastEvidenceKind == EvidenceOperator || a.LastEvidenceKind == EvidenceLegacyDoNotUse {
		return replayed, append(unknowable, "terminal_mismatch_audit_not_replayed_attempt_parked_or_forced"), nil
	}
	ev := ReceiptEvidence{EventType: r.EventType, ProviderReference: r.ProviderRef, Outcome: r.Outcome}
	if r.Amount != nil {
		ev.Amount = *r.Amount
	}
	if r.AssetCode != nil {
		ev.AssetCode = *r.AssetCode
	}
	if err := auditTerminalAmountAssetMismatch(ctx, tx, a, ev, map[string]any{
		"reconstructed": true, "receipt_id": r.ID.String(), "repair_reason": RepairReasonAnomalyAppliedGap,
		// never a fabricated state: the state at the event is unknowable (a deposit may have been declined
		// before a T13 success); only the CURRENT state is a fact.
		"attempt_state": "unknown", "attempt_state_now": string(a.State),
	}); err != nil {
		return nil, nil, err
	}
	return []string{"payments.callback_amount_asset_mismatch_terminal"}, unknowable, nil
}
