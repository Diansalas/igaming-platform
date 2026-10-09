package payments

// PAY-PAYOUT-UNBOUND-RESOLVE-1, ADR 0111 section 4.8 (C-2 / LF H-2) and section 17.7 F-1: the post-resolution signal
// cells. Status: IMPLEMENTED against MOCK; the status semantics of a real provider are PROVIDER DEPENDENT (ADR 0111
// section 20).
//
// An executed M4 resolves the HOLD, never the attempt: the attempt stays `disputed` (A-15). The provider can still
// report afterwards, and what it reports can contradict the resolution:
//
//	executed m4_evidence_not_paid (the withdrawal is `failed`, the hold went back to player_cash)
//	    + a `succeeded` callback or poll          -> success_after_m4_not_paid     (a payout the platform released)
//	executed m4_evidence_paid (the withdrawal is `completed`)
//	    + a `declined` callback or poll           -> contradiction_after_m4_paid   (a payout the platform completed)
//
// Each cell writes ONE audit row (payments.payout_post_m4_contradiction, closed vocabulary) and then raises the B12
// P1 as its LAST statement (ADR 0102 7.7; existing Kind payment.webhook_integrity, discriminator
// "payout_attempt:<attempt_id>:reason:<reason>", attribute provider_id only). Nothing changes state, posts, releases
// or schedules: it is a signal, not a resolution. Replays are one open alert with growing occurrences.
//
// Audit once-per-receipt (PAY-PAYOUT-CALLBACK-AUDIT-2): a callback writes the row only for a NEW receipt (the
// caller's !alreadyApplied); the raise is unconditional (the alert dedupes). A poll or a late sync result has no
// receipt, so its row is written once per (attempt, reason, evidence kind), checked under the withdrawal row lock
// the caller holds (a poll repeated every few seconds must not grow the audit log without bound).
//
// The M4 fact is read, never trusted from the evidence: an EXECUTED payment_manual_resolutions row of the right
// M4 kind for this tenant, this attempt and this withdrawal (tenant-bound by the row's own tenant_id and by RLS), and
// the withdrawal in the state that executed M4 leaves it in. A pending, rejected, cancelled, expired or refused M4
// gives no signal; neither does an M2/M1 resolution, or evidence that agrees with the M4 (a decline after not-paid,
// a success after paid).
//
// Lock order L1 (ADR 0082 A7): the withdrawal (LockForPayoutEvidence, a harmless re-lock under every caller), then the
// attempt re-read. The alert tables are the terminal lock level.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const (
	// alertReasonPayoutSuccessAfterM4NotPaid: a success reported after an executed m4_evidence_not_paid.
	alertReasonPayoutSuccessAfterM4NotPaid = "success_after_m4_not_paid"
	// alertReasonPayoutContradictionAfterM4Paid: a decline (or reversal) reported after an executed m4_evidence_paid.
	alertReasonPayoutContradictionAfterM4Paid = "contradiction_after_m4_paid"

	auditActionPayoutPostM4Contradiction = "payments.payout_post_m4_contradiction"
)

// postM4Cell names what one cell looks for.
type postM4Cell struct {
	reason   string
	kind     ResolutionKind
	wrState  withdrawal.State
	observed Outcome
}

// postM4CellFor maps the evidence outcome to its cell; ok=false for any outcome that has none (pending, ambiguous).
func postM4CellFor(observed Outcome) (postM4Cell, bool) {
	switch observed {
	case OutcomeSucceeded:
		return postM4Cell{reason: alertReasonPayoutSuccessAfterM4NotPaid, kind: ResolutionM4EvidenceNotPaid, wrState: withdrawal.StateFailed, observed: observed}, true
	case OutcomeDeclined:
		return postM4Cell{reason: alertReasonPayoutContradictionAfterM4Paid, kind: ResolutionM4EvidencePaid, wrState: withdrawal.StateCompleted, observed: observed}, true
	default:
		return postM4Cell{}, false
	}
}

// payoutPostM4Cell is the single implementation of both cells, called from the callback cell (receipt.go), the poll
// cell and the late-evidence cell (payout.go). newReceipt is non-nil exactly for a callback that owns a receipt row
// (true: a new receipt, the audit row is written); nil means "no receipt": the row is written once per
// (attempt, reason, evidence kind). ref is the provider reference the evidence carried ("" when none).
//
// It is a no-op (nil) unless the attempt is a payout, currently `disputed`, and an executed M4 of the matching kind
// exists for it with the withdrawal in the matching state. The raise is the LAST statement.
func payoutPostM4Cell(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, observed Outcome, evidence EvidenceKind, newReceipt *bool, ref string) error {
	if attempt.Operation != AttemptOperationPayout || attempt.WithdrawalRequestID == nil {
		return nil
	}
	cell, ok := postM4CellFor(observed)
	if !ok {
		return nil
	}
	requestID := *attempt.WithdrawalRequestID
	// L1: withdrawal first, then the attempt re-read (the caller's copy may be stale).
	wr, err := withdrawal.LockForPayoutEvidence(ctx, tx, requestID)
	if err != nil {
		return err
	}
	fresh, err := GetAttemptByID(ctx, tx, attempt.ID)
	if err != nil {
		return fmt.Errorf("payments: post-M4 cell: re-read attempt: %w", err)
	}
	if fresh.State != AttemptDisputed || wr.State != cell.wrState {
		return nil
	}
	var resolutionID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM payment_manual_resolutions
		WHERE tenant_id = $1 AND attempt_id = $2 AND withdrawal_request_id = $3 AND kind = $4 AND state = 'executed'
		ORDER BY closed_at DESC NULLS LAST, id LIMIT 1`,
		fresh.TenantID, fresh.ID, requestID, string(cell.kind)).Scan(&resolutionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("payments: post-M4 cell: read the executed M4: %w", err)
	}

	write := false
	if newReceipt != nil {
		write = *newReceipt
	} else {
		var seen bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3
			AND created_at >= $6 AND metadata->>'reason' = $4 AND metadata->>'evidence' = $5)`,
			fresh.TenantID, auditActionPayoutPostM4Contradiction, fresh.ID.String(), cell.reason, string(evidence), fresh.CreatedAt).Scan(&seen); err != nil {
			return fmt.Errorf("payments: post-M4 cell: audit lookup: %w", err)
		}
		write = !seen
	}
	if write {
		meta := map[string]any{
			"reason":                cell.reason,
			"observed":              string(cell.observed),
			"m4_kind":               string(cell.kind),
			"m4_resolution_id":      resolutionID,
			"evidence":              string(evidence),
			"provider_id":           providerIDOrEmpty(fresh),
			"withdrawal_request_id": requestID.String(),
			"withdrawal_state":      string(wr.State),
			"attempt_state":         string(fresh.State),
		}
		if fresh.TerminalReason != nil {
			meta["attempt_terminal_reason"] = payoutAlertReasonFor(*fresh.TerminalReason)
		}
		if ref != "" {
			for k, v := range echoAuditMeta(ref) {
				meta[k] = v
			}
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: fresh.TenantID, ActorType: audit.ActorSystem, Action: auditActionPayoutPostM4Contradiction,
			TargetType: "payment_attempt", TargetID: fresh.ID.String(), Outcome: audit.OutcomeDenied, Metadata: meta,
		}); err != nil {
			return fmt.Errorf("payments: audit payout post-M4 contradiction: %w", err)
		}
	}
	// B12 / ADR 0102 7.7: the raise is the LAST statement of the cell.
	return raisePayoutDisputeAlert(ctx, tx, fresh, cell.reason)
}
