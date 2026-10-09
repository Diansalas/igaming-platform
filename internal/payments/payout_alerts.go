package payments

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// PAY-PAYOUT-DISPUTE-ALERT-1 (Class-B B12, RAISE ONLY; ADR 0095 section 42).
//
// Every payout attempt that moves to disputed (T10 park, T14 success after a
// payout decline, the B10 provider_reference_conflict park, the T15
// success_for_never_sent_attempt park) and every payout T16 escalation raises
// ONE durable P1 on the existing platform-owned Kind payment.webhook_integrity
// (ADR 0102 section 17.1: no new Kind, no migration) with the discriminator
// "payout_attempt:<attempt_id>:reason:<closed reason>". A repeated condition is
// one open alert with growing occurrences.
//
// This file raises and nothing else. It never resolves, releases, settles or
// posts anything, never decides a recipient or channel (routing stays disabled;
// ALERT-DELIVERY-1 remains a production blocker) and never changes an audit
// row: the audit rows each site writes are unchanged and come first.
//
// Placement rule (ADR 0102 7.7, as for raiseDepositEscalationAlert): the raise
// is the LAST statement of its site (after the CAS and after the site's audit
// row), inside the tx opened by an alerting.InTx owner (ApplyPayoutResult,
// applyPayoutStatusEvidence, the payout escalation transaction, the webhook
// handler) whose Pending is flushed after the commit. RaiseGuarded swallows only
// the alert statement's own deterministic failure (P0001, class 22/23, 42501),
// so such a failure never rolls the dispute back; a transient failure
// propagates and rolls the whole transaction back, exactly as for deposits.
//
// The reason is ALWAYS taken from the closed set below, never a raw error and
// never provider text; the attributes carry only provider_id (a registered id).

// Closed payout escalation reasons (T16, payout_sweep.go escalateAmbiguousPayout).
const (
	alertReasonPayoutNonIdempotentManifest = "non_idempotent_manifest"
	alertReasonPayoutMaxResubmits          = "max_resubmits_exhausted"
	alertReasonPayoutResubmitCASRefused    = "resubmit_cas_refused"
)

// payoutEscalationReasons is the closed set of T16 payout escalation reasons.
var payoutEscalationReasons = map[string]struct{}{
	alertReasonPayoutNonIdempotentManifest: {},
	alertReasonPayoutMaxResubmits:          {},
	alertReasonPayoutResubmitCASRefused:    {},
	// B13-B (ADR 0111 2.4, 2.6): T2/T12 destination gate refusals.
	alertReasonPayoutDestinationNotUsable:     {},
	TerminalReasonDestinationIntegrityFailure: {},
}

// R-5 (ADR 0095 section 42.5): a signal raised WITHOUT any state change. A success whose
// amount/asset is mismatched, arriving by callback on an already-declined payout (the hold is
// already released: a double-payout candidate), leaves the attempt declined and writes only the
// terminal-mismatch audit; it now also raises this reason. It is not a terminal reason (the
// attempt is not parked, so it is not in PayoutDisputeReasons(), which mirrors the terminal-reason
// CHECK) and not an escalation, hence its own closed set.
const alertReasonPayoutMismatchedSuccessOnDeclined = "mismatched_success_on_declined_payout"

// R-6 (ADR 0095 section 42.8): the sibling signal for a mismatched success on an already-SUCCEEDED
// payout. Same pattern as R-5: raise only, no state change, own closed set, not a terminal reason.
const alertReasonPayoutMismatchedSuccessOnSucceeded = "mismatched_success_on_succeeded_payout"

// PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1 (ADR 0095 section 42.8, ledger-finance M-1): a success with
// matching amount/asset but a DIFFERENT provider reference than the one stored on an already-
// SUCCEEDED payout (resolved to the attempt via the merchant reference). Same pattern as R-5/R-6:
// raise only, no state change, no rebind of the stored reference, own closed reason.
const alertReasonPayoutForeignRefSuccessOnSucceeded = "foreign_reference_success_on_succeeded_payout"

// payoutSignalReasons is the closed set of payout alert reasons raised with no state change.
var payoutSignalReasons = map[string]struct{}{
	alertReasonPayoutMismatchedSuccessOnDeclined:  {},
	alertReasonPayoutMismatchedSuccessOnSucceeded: {},
	alertReasonPayoutForeignRefSuccessOnSucceeded: {},
	// B13-B: a differing destination echo on an already succeeded/declined payout.
	alertReasonPayoutDestinationMismatchOnTerminal: {},
}

// payoutAlertReasonFor maps a payout terminal or escalation reason to its
// closed alert-reason value. The invalid_provider_reference:<detail> family
// collapses to its prefix; a member of PayoutDisputeReasons() (which includes
// provider_reference_conflict, the B10 park) or of the escalation set is used
// as is; anything else becomes "unclassified" rather than leaking a raw string
// into a discriminator.
func payoutAlertReasonFor(reason string) string {
	if strings.HasPrefix(reason, invalidProviderReferencePrefix) {
		return TerminalReasonInvalidProviderReference
	}
	if _, ok := PayoutDisputeReasons()[reason]; ok {
		return reason
	}
	if _, ok := payoutEscalationReasons[reason]; ok {
		return reason
	}
	if _, ok := payoutSignalReasons[reason]; ok {
		return reason
	}
	slog.Default().Error("payments_alert_reason_unclassified")
	return alertReasonUnclassified
}

// payoutDisputeAlert builds the payment.webhook_integrity P1 for a payout
// attempt. Server-side ids only: the discriminator carries the attempt id and
// the closed reason; the one attribute is provider_id.
func payoutDisputeAlert(attempt PaymentAttempt, reasonValue string) alerting.Alert {
	attrs := map[string]alerting.AttrValue{}
	if pid := providerIDOrEmpty(attempt); pid != "" {
		attrs["provider_id"] = pid
	}
	return alerting.Alert{
		Kind:            alerting.KindPaymentWebhookIntegrity,
		SubjectTenantID: attempt.TenantID,
		Discriminator:   "payout_attempt:" + attempt.ID.String() + ":reason:" + reasonValue,
		Attributes:      attrs,
	}
}

// raisePayoutDisputeAlert raises the durable P1 for a payout dispute or T16
// escalation. A deposit attempt is a no-op (deposits have their own helpers in
// alerts.go). It must be the LAST statement a transaction runs on the alert
// tables and must run inside an alerting.InTx closure's tx.
func raisePayoutDisputeAlert(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, reason string) error {
	if attempt.Operation != AttemptOperationPayout {
		return nil
	}
	return alerting.RaiseGuarded(ctx, tx, payoutDisputeAlert(attempt, payoutAlertReasonFor(reason)))
}

// payoutAlertAfterDispute raises the payout P1 only when the dispute transition
// itself succeeded; a dispute error is returned untouched. It composes with
// alertAfterDispute (the deposit twin, which is a no-op for a payout) at the
// shared receipt cells.
func payoutAlertAfterDispute(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, reason string, disputeErr error) error {
	if disputeErr != nil {
		return disputeErr
	}
	return raisePayoutDisputeAlert(ctx, tx, attempt, reason)
}
