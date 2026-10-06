package payments

// PRH-2 D (PAY-POLL-AMOUNT-1 + FH7-06; ADR 0095 §36): the evidence checks the
// sweeper applies to a status poll that reports SUCCESS for a live deposit
// attempt, before anything is posted. The order is the one in ADR 0095 §36.1
// (not literally the callback path's §34.8 order): amount/asset, echoed reference,
// binding conflict, tombstone; the INV-DEP-1 choke point and the posting follow in
// applyStatusEvidence.
//
// Two rules are absolute here:
//   - the tombstone lookup, the binding check and the posting key use the
//     attempt's BOUND provider reference, never the poll's echo;
//   - nothing in this file posts, and no outcome of it writes a ledger
//     transaction: every contradiction is a T10 through parkDepositAttempt
//     (state change, one audit with adapter_outcome, intent recompute).

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// attemptAwaitingEvidence reports whether a deposit attempt is in one of the
// three live states a poll result can still move (T4/T6/T7/T8/T9/T10/T11).
func attemptAwaitingEvidence(st AttemptState) bool {
	return st == AttemptSubmitting || st == AttemptPending || st == AttemptAmbiguous
}

// pollAmountEvidence classifies the amount/asset a poll reported for an
// attempt, using the shared C helper (CompareProviderAmount) and ONE
// poll-specific tightening: the helper classes any echo with a zero amount or
// an empty asset as Missing, which on the sync path routes to "the poll decides".
// On the poll path nothing decides after it, so partial evidence that already
// CONTRADICTS the record (a non-zero amount that differs, or a non-empty asset
// that differs) is a Mismatch, never Missing (code review of C, F5). Partial
// evidence that contradicts nothing (amount equal, asset omitted) stays Missing.
func pollAmountEvidence(attempt PaymentAttempt, res StatusResult) AmountEvidence {
	ev := CompareProviderAmount(attempt.Amount, attempt.AssetCode, res.Amount, res.AssetCode)
	if ev == AmountEvidenceMissing &&
		((res.Amount != 0 && res.Amount != attempt.Amount) || (res.AssetCode != "" && res.AssetCode != attempt.AssetCode)) {
		return AmountEvidenceMismatch
	}
	return ev
}

// checkPollSuccessEvidence applies the pre-posting checks to a poll success.
// handled=true means the poll was fully dealt with (parked, audited,
// rescheduled or tombstoned) and the caller must not post; handled=false means
// every check passed and the caller proceeds to INV-DEP-1 and the posting, keyed
// on boundRef. Called under the intent lock with the attempt freshly re-read.
//
// What "Missing" means on the poll path (ADR 0095 §36.2): the provider says
// "succeeded" but gave no usable amount or asset. That is NOT confirmation, so
// nothing is posted - ever - and it is NOT a dispute either (a callback with its
// own amount evidence can still resolve the attempt, and a terminal dispute
// would make that callback recorded-only, stranding a possible capture behind a
// manual M1). The attempt therefore stays live and is rescheduled at the poll
// backoff, and every such poll writes one audit record so a provider that never
// echoes an amount is visible rather than a silent loop.
func (s *Sweeper) checkPollSuccessEvidence(
	ctx context.Context, tx pgx.Tx, intent DepositIntent, attempt PaymentAttempt, res StatusResult, boundRef string,
) (handled bool, err error) {
	providerID := *attempt.ProviderID
	live := attemptAwaitingEvidence(attempt.State)

	// contradict is the single exit for a contradicting poll. A live attempt is
	// parked (T10). A declined one (the T13 second-capture shape) has no live->
	// disputed transition; §4.4 makes declined x contradicting success a P1
	// anomaly with no state change, so it is audited only.
	contradict := func(reason string, extra map[string]any) (bool, error) {
		if live {
			_, perr := parkDepositAttempt(ctx, tx, intent, attempt, providerID, EvidenceQueryStatus, reason, OutcomeSucceeded, "", extra)
			return true, perr
		}
		return true, auditPollTerminalContradiction(ctx, tx, attempt, reason, extra)
	}

	// 1. Amount and asset.
	switch pollAmountEvidence(attempt, res) {
	case AmountEvidenceMismatch:
		return contradict(TerminalReasonPollAmountMismatch, withAssetEcho(map[string]any{
			"provider_reference": boundRef, "provider_amount": res.Amount,
		}, "provider_asset_code", res.AssetCode))
	case AmountEvidenceMissing:
		if !live {
			// D1-M1: a declined attempt is not rescheduled, so without a record this
			// evidence (a possible T13 second capture) would vanish. One audit row per
			// poll that reaches here (the sweeper does not poll a declined attempt
			// again, so in practice once); no posting, no state change.
			return true, auditPollTerminalContradiction(ctx, tx, attempt, "poll_amount_unconfirmed", withAssetEcho(map[string]any{
				"provider_reference": boundRef, "provider_amount": res.Amount,
			}, "provider_asset_code", res.AssetCode))
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payment.attempt_poll_amount_unconfirmed",
			TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
			Metadata: withAssetEcho(map[string]any{
				"provider_id": providerID, "deposit_intent_id": intent.ID.String(), "provider_reference": boundRef,
				"amount": attempt.Amount, "asset_code": attempt.AssetCode,
				"provider_amount": res.Amount, "adapter_outcome": string(OutcomeSucceeded),
			}, "provider_asset_code", res.AssetCode),
		}); err != nil {
			return true, fmt.Errorf("payments: audit unconfirmed poll success: %w", err)
		}
		return true, RescheduleNonTerminal(ctx, tx, attempt.ID, s.backoff(attempt.PollCount))
	}

	// 2. An echoed reference, when present, must be the bound one. An empty echo
	// is allowed (the poll was asked about the bound reference). The echo is
	// never used for anything else; it is audited only when it is itself a valid
	// reference, otherwise only its length and hash prefix.
	if res.ProviderReference != "" && res.ProviderReference != boundRef {
		extra := echoAuditMeta(res.ProviderReference)
		extra["provider_reference"] = boundRef
		// PRH-2 K3 (POLL-REF-CLEAR-1): a live attempt is about to be parked, so Y
		// is persisted first as typed evidence - only a Y that passes the payments
		// validator, and never for the declined (audit-only) shape, which has no
		// park for the deferred database check to bind to.
		if live && providerref.ValidatePaymentReference("poll.provider_reference", res.ProviderReference) == nil {
			if err := insertPollReferenceEvidence(ctx, tx, attempt, providerID, res.ProviderReference); err != nil {
				return true, err
			}
			extra["evidence_recorded"] = true
		}
		return contradict(TerminalReasonPollReferenceMismatch, extra)
	}

	// 3. The bound reference must not be held by another attempt, intent or a
	// non-tombstone ledger transaction (LF-6, and F-C4 for the ledger).
	conflict, boundOp, err := foreignReferenceBinding(ctx, tx, attempt.TenantID, providerID, boundRef, attempt.ID, intent.ID)
	if err != nil {
		return true, err
	}
	if conflict {
		return contradict(TerminalReasonProviderReferenceConflict, map[string]any{
			"provider_reference": boundRef, "bound_to_operation": boundOp,
		})
	}

	// 4. Reversal tombstone on the BOUND reference (T10, or T13t from declined).
	tombstoned, err := tombstoneExists(ctx, tx, attempt.TenantID, providerID, boundRef)
	if err != nil {
		return true, err
	}
	if tombstoned {
		if !live {
			return true, ApplyTombstonePrecedesSuccess(ctx, tx, attempt.ID, EvidenceQueryStatus)
		}
		_, perr := parkDepositAttempt(ctx, tx, intent, attempt, providerID, EvidenceQueryStatus,
			TerminalReasonTombstonePrecedesSuccess, OutcomeSucceeded, "", map[string]any{"provider_reference": boundRef})
		return true, perr
	}
	return false, nil
}

// auditRefusedPollEcho records one audit row for a poll echo the payments
// validator refused (ADR 0101 5.4): only the closed reason, length and hash
// prefix - never the value. The caller then reschedules (or declines with a nil
// reference); it never returns an error for a hostile echo.
func auditRefusedPollEcho(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, intent DepositIntent, site, echo string) error {
	meta := echoAuditMeta(echo)
	meta["site"] = site
	meta["deposit_intent_id"] = intent.ID.String()
	meta["provider_id"] = providerIDOrEmpty(attempt)
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.poll_echo_reference_refused",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied, Metadata: meta,
	}); err != nil {
		return fmt.Errorf("payments: audit refused poll echo (%s): %w", site, err)
	}
	return nil
}

// insertPollReferenceEvidence persists the poll's returned reference Y as typed
// evidence (PAY-RECON-POLL-REF-CLEAR-1, ADR 0101 8.6(a)): a PLAIN INSERT (a
// duplicate raises: a park happens exactly once), in the park's own
// transaction and BEFORE parkDepositAttempt, so the park's alert raise stays the
// last alert-table statement and a failed park rolls the row back. Written only
// for a Y that passed ValidatePaymentReference.
func insertPollReferenceEvidence(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, providerID, y string) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
		 VALUES ($1, $2, $3, 'poll_returned_reference', $4)`,
		attempt.TenantID, attempt.ID, providerID, y); err != nil {
		return fmt.Errorf("payments: record poll returned reference evidence: %w", err)
	}
	return nil
}

// auditPollTerminalContradiction records a poll success that contradicts an
// attempt which is not live (declined): no state change and no posting, a P1
// audit only (§4.4 declined x contradicting success).
func auditPollTerminalContradiction(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, reason string, extra map[string]any) error {
	meta := map[string]any{
		"reason": reason, "attempt_state": string(attempt.State), "amount": attempt.Amount, "asset_code": attempt.AssetCode,
		"adapter_outcome": string(OutcomeSucceeded),
	}
	for k, v := range extra {
		meta[k] = v
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.poll_evidence_contradicts_terminal_attempt",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied, Metadata: meta,
	}); err != nil {
		return fmt.Errorf("payments: audit poll contradiction on terminal attempt (%s): %w", reason, err)
	}
	// ADR 0102 I-wire / PAY-POLL-DECLINED-ALERT-RECON-1 (a): durable P1 for the
	// audit action (reason from a closed set). Savepoint-guarded, last statement.
	return raisePollContradictionAlert(ctx, tx, attempt, reason)
}

// echoAuditMeta returns the audit fields for a provider-echoed reference that is
// NOT the bound one: the value itself only when it passes providerref.Validate,
// otherwise only the closed reason, length and hash prefix (never the value).
func echoAuditMeta(echo string) map[string]any {
	meta := map[string]any{}
	if verr := providerref.ValidatePaymentReference("poll.provider_reference", echo); verr != nil {
		if perr, ok := providerref.AsError(verr); ok {
			meta["echo_ref_reason"], meta["echo_ref_len"], meta["echo_ref_sha256_prefix"] = string(perr.Reason), perr.Length, perr.HashPrefix
		}
	} else {
		meta["echoed_provider_reference"] = echo
	}
	return meta
}

// auditPollPendingEchoDiffers (B5, LF L1): a poll Pending for an attempt that already has a
// bound reference never rebinds it, but a DIFFERING non-empty echo is recorded (one audit
// row; the echo itself only when it is a valid reference, else length and hash prefix) so it
// is not silently ignored. An empty or identical echo records nothing.
func auditPollPendingEchoDiffers(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, intent DepositIntent, site, echo string) error {
	if echo == "" || attempt.ProviderReference == nil || *attempt.ProviderReference == "" || echo == *attempt.ProviderReference {
		return nil
	}
	meta := echoAuditMeta(echo)
	meta["site"] = site
	meta["provider_reference"] = *attempt.ProviderReference
	meta["deposit_intent_id"] = intent.ID.String()
	meta["provider_id"] = providerIDOrEmpty(attempt)
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.poll_pending_reference_echo_differs",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied, Metadata: meta,
	}); err != nil {
		return fmt.Errorf("payments: audit differing poll pending echo (%s): %w", site, err)
	}
	return nil
}
