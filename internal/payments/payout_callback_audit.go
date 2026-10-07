package payments

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// PAY-PAYOUT-CALLBACK-AUDIT-1 (ADR 0095 section 42.7).
//
// The payout receipt-callback cells that move a payout attempt to disputed
// (T14 success after a decline, T15 success for a never-sent attempt, a
// mismatched amount/asset success, a tombstoned reference) used to leave only
// the receipt row and the B12 alert: no audit row. Every other payout park
// writes one (payments.payout_provider_reference_mismatch,
// payments.payout_amount_asset_mismatch, ...). This file adds the same record
// for these four cells, in the SAME transaction as the dispute CAS, and changes
// nothing else: no state-machine change, no release, no settlement, no posting.
const auditActionPayoutCallbackDispute = "payments.payout_callback_dispute"

// auditPayoutCallbackDispute records the park of a payout attempt by a receipt
// callback. It is a no-op for a deposit attempt (deposits are untouched by this
// item) and when the dispute CAS itself failed (disputeErr is returned
// untouched: a conflicting CAS wrote no dispute, so there is nothing to
// record). Nothing provider-controlled is stored raw: terminal_reason is one of
// the closed receipt-cell reasons, the echoed reference goes through
// echoAuditMeta (the value only when it is a valid reference, else length and
// hash prefix) and the echoed asset code through withAssetEcho (B5). Amounts are
// integer minor units, as in the sibling auditTerminalAmountAssetMismatch row.
//
// It must run BEFORE the B12 raise (ADR 0102 7.7: the raise is the last
// statement of the cell); the cells compose it inside payoutAlertAfterDispute.
func auditPayoutCallbackDispute(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, ev ReceiptEvidence, terminalReason string, disputeErr error) error {
	if disputeErr != nil || attempt.Operation != AttemptOperationPayout {
		return disputeErr
	}
	meta := echoAuditMeta(ev.ProviderReference)
	meta["terminal_reason"] = terminalReason
	meta["attempt_state_before"] = string(attempt.State)
	meta["attempt_state_after"] = string(AttemptDisputed)
	meta["evidence"] = string(EvidenceCallback)
	meta["provider_id"] = providerIDOrEmpty(attempt)
	meta["stored_amount"] = attempt.Amount
	meta["stored_asset_code"] = attempt.AssetCode
	meta["echoed_amount"] = ev.Amount
	withAssetEcho(meta, "echoed_asset_code", ev.AssetCode)
	if attempt.WithdrawalRequestID != nil {
		meta["withdrawal_request_id"] = attempt.WithdrawalRequestID.String()
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: auditActionPayoutCallbackDispute,
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: meta,
	}); err != nil {
		return fmt.Errorf("payments: audit payout callback dispute: %w", err)
	}
	return nil
}
