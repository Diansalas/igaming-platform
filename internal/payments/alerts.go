package payments

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// ADR 0102 I-wire: durable P1 alerts for the deposit evidence transactions
// (T10 / T13d / T15 and the poll contradiction audit). Every raise here goes
// through alerting.RaiseGuarded, which runs the alert INSERT under its own
// SAVEPOINT and swallows only the alert statement's own deterministic error
// (classes 22/23, 42501, P0001): a DETERMINISTIC alert-statement failure never
// rolls back the dispute, receipt or posting it reports (LF-7; security
// addendum 1 (a)). Transient, lock and cancellation classes (25P02, 40001,
// 40P01, 55P03, 57014, 08, 53, context) are deliberately NOT swallowed: they
// propagate exactly as any other statement of the transaction would, and the
// evidence application is retried by redelivery or the sweeper.
// A swallowed raise is retried detached AFTER the commit by the Pending that
// the transaction's owner opened with alerting.InTx and flushes after commit
// (condition (b)). The transaction owners that wire this are: the webhook
// handler (httpserver/deposit_handlers.go and the simulation twin),
// driveCreatedAttempt's phase C (drive.go) and the sweeper's status
// evidence transaction (sweeper.go). B-1 is pinned by
// internal/alerting/static_wiring_test.go.
//
// No new alert Kind is created (no migration, ADR 0102 section 17): the T10
// park reasons reuse the existing p1 platform-owned Kind
// payment.webhook_integrity with a discriminator
// "attempt:<attempt_id>:reason:<closed reason>". The reason is ALWAYS taken
// from the closed set below - never a raw error string, never provider text.

// Closed alert-reason vocabulary for the discriminator.
const (
	alertReasonUnclassified          = "unclassified"
	alertReasonPollContradictsTermin = "poll_evidence_contradicts_terminal_attempt"
)

// pollContradictionSubReasons is the closed set of sub-reasons an
// auditPollTerminalContradiction call can carry.
var pollContradictionSubReasons = map[string]struct{}{
	TerminalReasonPollAmountMismatch:        {},
	TerminalReasonPollReferenceMismatch:     {},
	TerminalReasonProviderReferenceConflict: {},
	"poll_amount_unconfirmed":               {},
}

// alertReasonFor maps a terminal reason to its closed alert-reason value:
// the "invalid_provider_reference:<detail>" family collapses to its prefix,
// any member of DepositDisputeTerminalReasons() is used as-is, and anything
// else (a future reason added without updating the list) becomes
// "unclassified" rather than leaking a raw string into a discriminator.
func alertReasonFor(reason string) string {
	if strings.HasPrefix(reason, invalidProviderReferencePrefix) {
		return TerminalReasonInvalidProviderReference
	}
	if IsDepositDisputeTerminalReason(reason) {
		return reason
	}
	slog.Default().Error("payments_alert_reason_unclassified")
	return alertReasonUnclassified
}

func providerIDOrEmpty(attempt PaymentAttempt) string {
	if attempt.ProviderID != nil {
		return *attempt.ProviderID
	}
	return ""
}

// depositParkAlert builds the payment.webhook_integrity P1 for a deposit
// attempt moved to disputed (or audited) for a closed reason.
func depositParkAlert(attempt PaymentAttempt, providerID, reasonValue string) alerting.Alert {
	attrs := map[string]alerting.AttrValue{}
	if providerID != "" {
		attrs["provider_id"] = providerID
	}
	return alerting.Alert{
		Kind:            alerting.KindPaymentWebhookIntegrity,
		SubjectTenantID: attempt.TenantID,
		Discriminator:   "attempt:" + attempt.ID.String() + ":reason:" + reasonValue,
		Attributes:      attrs,
	}
}

// raiseDepositParkAlert raises the P1 for a deposit T10/T13d/T15 dispute. It
// must be the LAST statement a transaction runs on the alert tables (ADR
// 0102 7.7) and must run inside an alerting.InTx closure's tx.
func raiseDepositParkAlert(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, providerID, terminalReason string) error {
	if attempt.Operation != AttemptOperationDeposit {
		return nil // payout disputes are F-pay's (kycgate/payout) surface
	}
	return alerting.RaiseGuarded(ctx, tx, depositParkAlert(attempt, providerID, alertReasonFor(terminalReason)))
}

// raisePollContradictionAlert raises the P1 for the
// payments.poll_evidence_contradicts_terminal_attempt audit action.
func raisePollContradictionAlert(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, subReason string) error {
	if _, ok := pollContradictionSubReasons[subReason]; !ok {
		subReason = alertReasonUnclassified
	}
	return alerting.RaiseGuarded(ctx, tx, depositParkAlert(attempt, providerIDOrEmpty(attempt),
		alertReasonPollContradictsTermin+":"+subReason))
}

func intentDiscriminator(intentID uuid.UUID) string { return "intent:" + intentID.String() }

// raiseMultipleSuccessAlert raises payment.multiple_success_for_intent (ADR
// 0102 8 row 1) and, when the INV-DEP-1 backstop fired, the additional
// payment.deposit_intent_index_backstop_fired (row 2). Both are keyed by the
// intent, so a repeating condition is one open alert with growing
// occurrences.
func raiseMultipleSuccessAlert(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, providerID string, evidence EvidenceKind, backstopFired bool) error {
	if attempt.DepositIntentID == nil {
		return nil
	}
	disc := intentDiscriminator(*attempt.DepositIntentID)
	attrs := map[string]alerting.AttrValue{"attempt_id": attempt.ID.String(), "evidence_kind": string(evidence)}
	if providerID != "" {
		attrs["provider_id"] = providerID
	}
	if err := alerting.RaiseGuarded(ctx, tx, alerting.Alert{
		Kind: alerting.KindPaymentMultipleSuccessForIntent, SubjectTenantID: attempt.TenantID,
		Discriminator: disc, Attributes: attrs,
	}); err != nil {
		return err
	}
	if !backstopFired {
		return nil
	}
	return alerting.RaiseGuarded(ctx, tx, alerting.Alert{
		Kind: alerting.KindPaymentDepositIntentIndexBackstop, SubjectTenantID: attempt.TenantID,
		Discriminator: disc, Attributes: map[string]alerting.AttrValue{"attempt_id": attempt.ID.String()},
	})
}

// alertAfterDispute raises the deposit park P1 only when the dispute
// transition itself succeeded; a dispute error is returned untouched.
func alertAfterDispute(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, reason string, disputeErr error) error {
	if disputeErr != nil {
		return disputeErr
	}
	return raiseDepositParkAlert(ctx, tx, attempt, providerIDOrEmpty(attempt), reason)
}
