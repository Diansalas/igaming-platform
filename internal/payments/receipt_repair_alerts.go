package payments

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// PAY-RECEIPT-ANOMALY-APPLIED-1 Option A (ADR 0095 section 45): the durable operational signal for a
// REFUSED receipt attribution repair (owner decision 29: ambiguous or insufficient attribution
// evidence leaves the receipt anomalous and raises a signal).
//
// Same pattern as R-5/R-6 (payout_alerts.go): ONE P1 on the existing platform-owned Kind
// payment.webhook_integrity (ADR 0102 section 17.1: no new Kind), a discriminator built from
// server-side ids and a reason from a CLOSED set, raise only, the LAST statement of its site, inside
// the tx opened by an alerting.InTx owner (RepairReceiptAttribution). There is usually no single
// attempt to name (that is why the repair refused), so the subject is the receipt:
// "receipt:<receipt_id>:reason:<closed reason>"; the one attribute is provider_id (a registered id).
// A repeated refusal is one open alert with growing occurrences.
const (
	alertReasonReceiptRepairNoCandidate = "receipt_repair_no_candidate"
	alertReasonReceiptRepairAmbiguous   = "receipt_repair_ambiguous_candidates"
	alertReasonReceiptRepairContradict  = "receipt_repair_contradictory_evidence"
	alertReasonReceiptRepairInsufficent = "receipt_repair_insufficient_evidence"
)

// receiptRepairSignalReasons is the closed set of refusal reasons that raise. A refusal because the
// receipt is simply not eligible (already attributed, unresolved, or not an anomaly closure) is a
// caller mistake, not an evidence problem, and raises nothing.
var receiptRepairSignalReasons = map[string]struct{}{
	alertReasonReceiptRepairNoCandidate: {},
	alertReasonReceiptRepairAmbiguous:   {},
	alertReasonReceiptRepairContradict:  {},
	alertReasonReceiptRepairInsufficent: {},
}

func receiptRepairAlert(tenantID, receiptID uuid.UUID, providerID, reason string) alerting.Alert {
	attrs := map[string]alerting.AttrValue{}
	if providerID != "" {
		attrs["provider_id"] = providerID
	}
	return alerting.Alert{
		Kind:            alerting.KindPaymentWebhookIntegrity,
		SubjectTenantID: tenantID,
		Discriminator:   "receipt:" + receiptID.String() + ":reason:" + reason,
		Attributes:      attrs,
	}
}

// raiseReceiptRepairRefusedAlert raises the P1 for a refused repair. The reason is validated against
// the closed set (anything else becomes "unclassified", never echoed). It must be the LAST statement a
// transaction runs on the alert tables and must run inside an alerting.InTx closure's tx.
func raiseReceiptRepairRefusedAlert(ctx context.Context, tx pgx.Tx, tenantID, receiptID uuid.UUID, providerID, reason string) error {
	if _, ok := receiptRepairSignalReasons[reason]; !ok {
		slog.Default().Error("payments_alert_reason_unclassified")
		reason = alertReasonUnclassified
	}
	return alerting.RaiseGuarded(ctx, tx, receiptRepairAlert(tenantID, receiptID, providerID, reason))
}
