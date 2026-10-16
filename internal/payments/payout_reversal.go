package payments

// A5 / WITHDRAWAL-REVERSAL-1 (owner decision 2026-10-15; migration 0162): the governed payout reversal,
// orchestrated. One staff call records one 'reverse' decision (withdrawal.RecordPayoutReversalDecision:
// four-eyes by distinct Persons on ONE payload digest, the policy's step-up, never the beneficiary);
// the call that completes the quorum executes, in the SAME transaction:
//
//   - the request is `completed` (the platform saw the payout): E11, the compensating
//     withdrawal_reversed posting (withdrawal.ReverseCompletedPayout). The payout attempt stays
//     `succeeded`: the payout did happen; the return is a new fact on the ledger.
//   - the request is still `submitted` (the provider reports the return BEFORE the platform saw the
//     completion: "a reversal seen before the original"): the tombstone rule. A ledger tombstone is
//     written on (provider_id, original payout reference), so a late completion for that reference can
//     never post (the (tenant, provider_id, provider_tx_id) UNIQUE refuses it); the live payout attempt is
//     parked (T10, reason reversal_tombstone_precedes_success, net zero at the PSP); the hold is released
//     to the player (E10, withdrawal.Fail) because the value is back with the PSP. A P1 alert is raised as
//     the LAST statement (the same B12 payout-dispute alert every payout park raises).
//
// Every outcome writes audit rows in the same transaction (the decision, the execution, and the
// caller's own HTTP row with IP / user agent / request id through PayoutReversalInput.Audit). Retries are
// idempotent: a repeat call by an approver whose vote for the same payload is already on file, after
// the reversal executed, returns PayoutReversalReplay and records nothing.
//
// Lock order (ADR 0082 A7/A8): the request FOR UPDATE, then (submitted only) its payout attempts FOR
// UPDATE in ascending id, then the approver's staff row FOR SHARE (inside the eligibility closure), then
// L2 the completion FOR UPDATE (completed only), then ledger.Post's L3/L4 (Fail before the tombstone,
// which takes L4 only).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/stepup"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// PayoutReturnedBeforeCompletionReason is the withdrawal.Fail reason of the tombstone branch.
const PayoutReturnedBeforeCompletionReason = "payout_returned_before_completion"

// PayoutReversalOutcome is the closed outcome of one DecidePayoutReversal call.
type PayoutReversalOutcome string

const (
	// PayoutReversalRecorded: the decision is on file; more distinct Persons must agree.
	PayoutReversalRecorded PayoutReversalOutcome = "decision_recorded"
	// PayoutReversalExecuted: E11 posted (completed -> reversed).
	PayoutReversalExecuted PayoutReversalOutcome = "reversed"
	// PayoutReversalTombstoned: the return came before the completion: tombstone + park + hold released.
	PayoutReversalTombstoned PayoutReversalOutcome = "tombstoned_hold_released"
	// PayoutReversalReplay: an idempotent retry after execution; nothing recorded.
	PayoutReversalReplay PayoutReversalOutcome = "already_executed"
)

var (
	// ErrPayoutReversalAttemptInFlight: the payout has an attempt that may be dispatching right now
	// (created / submitting): the reversal is refused until it settles (retry later). Nothing recorded.
	ErrPayoutReversalAttemptInFlight = errors.New("payments: the payout has an attempt in flight; retry the reversal once it settles")
	// ErrPayoutReversalAttemptInconsistent: the submitted payout's attempts do not have the one shape the
	// tombstone branch can act on (more than one open or parked attempt, or one that names another
	// provider or reference). Nothing recorded; an integrity case for investigation.
	ErrPayoutReversalAttemptInconsistent = errors.New("payments: the payout attempts are inconsistent with the reversal")
)

// PayoutReversalInput is one staff decision. ApproverID comes from the verified staff token.
type PayoutReversalInput struct {
	ApproverID uuid.UUID
	Payload    withdrawal.PayoutReversal
	StepUp     stepup.Assertion
	// Checks builds the transaction-bound beneficiary and eligibility checks (the HTTP layer's
	// staffIsWithdrawalBeneficiary / approverEligibilityCheckLocked). Required.
	Checks func(ctx context.Context, tx pgx.Tx) (withdrawal.BeneficiaryCheck, withdrawal.ApproverEligibility)
	// Audit writes the caller's own audit row (actor, IP, user agent, request id) in the same
	// transaction, before the alert. Optional; an error rolls everything back.
	Audit func(ctx context.Context, tx pgx.Tx, res PayoutReversalResult) error
}

// PayoutReversalResult is DecidePayoutReversal's outcome.
type PayoutReversalResult struct {
	Outcome           PayoutReversalOutcome
	Counted, Required int
	// LedgerTransactionID is the withdrawal_reversed posting (Executed / Replay of an executed reversal)
	// or the tombstone (Tombstoned); nil otherwise.
	LedgerTransactionID *uuid.UUID
	// ParkedAttemptID is the payout attempt the tombstone branch parked (nil when none was open).
	ParkedAttemptID *uuid.UUID
}

// DecidePayoutReversal records one governed reversal decision and executes the reversal when it
// completes the quorum. See the file comment.
func DecidePayoutReversal(ctx context.Context, pool *db.Pool, tenantID, requestID uuid.UUID, in PayoutReversalInput) (PayoutReversalResult, error) {
	if in.Checks == nil {
		return PayoutReversalResult{}, fmt.Errorf("%w: the eligibility and beneficiary checks are required", withdrawal.ErrInvalidInput)
	}
	if in.ApproverID == uuid.Nil {
		return PayoutReversalResult{}, fmt.Errorf("%w: the deciding staff principal is required", withdrawal.ErrInvalidInput)
	}
	var res PayoutReversalResult
	// A5 fix round SR5-1: the decision runs in the deciding principal's scope (app.principal_id), the only
	// tenant session in which staff_capability_grants are visible (0112 tenant_scope_select), so the Go
	// payment_force_resolve:approve check and the WD027 trigger can see the deciders' grants. In a session
	// without a principal the grants are invisible and both checks fail closed.
	pending, err := alerting.InTx(ctx, alerting.NewPrincipalRunner(pool, tenantID, in.ApproverID), func(actx context.Context, tx pgx.Tx) error {
		res = PayoutReversalResult{}
		wr, err := withdrawal.LockForPayoutEvidence(actx, tx, requestID)
		if err != nil {
			return err
		}
		var open *PaymentAttempt
		if wr.State == withdrawal.StateSubmitted {
			if open, err = lockPayoutAttemptsForReversal(actx, tx, wr, in.Payload); err != nil {
				return err
			}
		}
		beneficiary, eligibility := in.Checks(actx, tx)
		dec, err := withdrawal.RecordPayoutReversalDecision(actx, tx, requestID, in.ApproverID, in.Payload, beneficiary, eligibility, in.StepUp)
		if err != nil {
			return err
		}
		res.Counted, res.Required = dec.Counted, dec.Required

		var raise *PaymentAttempt
		switch {
		case dec.Replay:
			res.Outcome = PayoutReversalReplay
			if dec.Request.State == withdrawal.StateReversed {
				if res.LedgerTransactionID, err = withdrawal.ReversalLink(actx, tx, requestID); err != nil {
					return err
				}
			}
		case !dec.Ready:
			res.Outcome = PayoutReversalRecorded
		case dec.Request.State == withdrawal.StateCompleted:
			txID, err := withdrawal.ReverseCompletedPayout(actx, tx, requestID, in.Payload)
			if err != nil {
				return err
			}
			res.Outcome, res.LedgerTransactionID = PayoutReversalExecuted, &txID
		case dec.Request.State == withdrawal.StateSubmitted:
			txID, parked, err := reversePayoutBeforeCompletion(actx, tx, dec.Request, open, in.Payload)
			if err != nil {
				return err
			}
			res.Outcome, res.LedgerTransactionID = PayoutReversalTombstoned, &txID
			if open != nil {
				res.ParkedAttemptID = &open.ID
				if parked {
					raise = open
				}
			}
		default:
			return fmt.Errorf("%w: request %s is in state %q", withdrawal.ErrStateConflict, requestID, dec.Request.State)
		}
		if in.Audit != nil {
			if err := in.Audit(actx, tx, res); err != nil {
				return err
			}
		}
		if raise != nil {
			// B12: the alert is the LAST statement of the park.
			return raisePayoutDisputeAlert(actx, tx, *raise, TerminalReasonTombstonePrecedesSuccess)
		}
		return nil
	})
	if err != nil {
		return PayoutReversalResult{}, err
	}
	pending.Flush(ctx)
	return res, nil
}

// lockPayoutAttemptsForReversal locks every payout attempt of the submitted request (ascending id, after
// the request: ADR 0082 A7) and returns the ONE open-or-parked attempt the tombstone branch acts on, or
// nil when the request has no such attempt (nothing can then complete it either).
func lockPayoutAttemptsForReversal(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest, p withdrawal.PayoutReversal) (*PaymentAttempt, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+paymentAttemptColumns+` FROM payment_attempts
		  WHERE tenant_id = $1 AND withdrawal_request_id = $2 AND operation = 'payout'
		  ORDER BY id FOR UPDATE`, wr.TenantID, wr.ID)
	if err != nil {
		return nil, fmt.Errorf("payments: lock payout attempts for reversal: %w", err)
	}
	var attempts []PaymentAttempt
	for rows.Next() {
		a, err := scanPaymentAttempt(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("payments: scan payout attempt: %w", err)
		}
		attempts = append(attempts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payments: lock payout attempts for reversal: %w", err)
	}
	var open *PaymentAttempt
	for i := range attempts {
		a := attempts[i]
		switch a.State {
		case AttemptCreated, AttemptSubmitting:
			return nil, ErrPayoutReversalAttemptInFlight
		case AttemptPending, AttemptAmbiguous, AttemptDisputed:
			if open != nil {
				return nil, ErrPayoutReversalAttemptInconsistent
			}
			open = &attempts[i]
		case AttemptSucceeded:
			// A succeeded attempt on a still-submitted request is not a shape any writer produces
			// (ApplySuccess and Complete commit together): refuse rather than tombstone a paid payout.
			return nil, ErrPayoutReversalAttemptInconsistent
		}
	}
	if open != nil {
		if (open.ProviderID != nil && *open.ProviderID != p.ProviderID) ||
			(open.ProviderReference != nil && *open.ProviderReference != p.OriginalProviderReference) {
			return nil, ErrPayoutReversalAttemptInconsistent
		}
	}
	// The original reference must be one the platform itself holds for this payout (on the request or
	// on its attempt): the tombstone is written on it, so a staff-typed reference alone is never enough.
	known := (wr.ProviderReference != nil && *wr.ProviderReference == p.OriginalProviderReference) ||
		(open != nil && open.ProviderReference != nil && *open.ProviderReference == p.OriginalProviderReference)
	if !known {
		return nil, withdrawal.ErrReversalPayloadMismatch
	}
	return open, nil
}

// reversePayoutBeforeCompletion is the tombstone branch (see the file comment). It returns the tombstone
// transaction id and whether it parked the open attempt now.
func reversePayoutBeforeCompletion(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest, open *PaymentAttempt, p withdrawal.PayoutReversal) (uuid.UUID, bool, error) {
	parked := false
	if open != nil && (open.State == AttemptPending || open.State == AttemptAmbiguous) {
		if err := ApplyDisputeFromNonTerminal(ctx, tx, open.ID, EvidencePlatform, TerminalReasonTombstonePrecedesSuccess); err != nil {
			return uuid.Nil, false, err
		}
		parked = true
	}
	// E10: the value is back with the PSP, so the hold goes back to the player (L3/L4).
	if err := withdrawal.Fail(ctx, tx, wr.ID, PayoutReturnedBeforeCompletionReason); err != nil {
		return uuid.Nil, false, err
	}
	// The tombstone (L4 only, after Fail's L3): a late completion of the original reference can never post.
	providerID, originalRef := p.ProviderID, p.OriginalProviderReference
	posted, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: wr.TenantID, TransactionType: ledger.TxTombstone,
		IdempotencyKey: fmt.Sprintf("tombstone:%s:%s", providerID, originalRef),
		ProviderID:     &providerID, ProviderTxID: &originalRef,
		CorrelationID: wr.ID,
	})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("payments: post payout reversal tombstone: %w", err)
	}
	meta := map[string]any{
		"state_before": string(withdrawal.StateSubmitted), "state_after": string(withdrawal.StateFailed),
		"provider_id": providerID, "original_provider_reference": originalRef, "return_reference": p.ReturnReference,
		"amount": wr.Amount, "asset_code": wr.AssetCode, "tombstone_ledger_transaction_id": posted.TransactionID.String(),
		"payload_digest": p.Digest(wr.ID), "evidence_ref_hash": p.EvidenceRefHash, "attempt_parked": parked,
	}
	if open != nil {
		meta["attempt_id"] = open.ID.String()
		meta["attempt_state_before"] = string(open.State)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: wr.TenantID, ActorType: audit.ActorSystem,
		Action: "payments.payout_reversal_tombstoned", TargetType: "withdrawal_request", TargetID: wr.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: meta,
	}); err != nil {
		return uuid.Nil, false, fmt.Errorf("payments: audit payout reversal tombstone: %w", err)
	}
	return posted.TransactionID, parked, nil
}

// alertReasonPayoutSuccessAfterReversalTombstone is the closed alert reason of LF-A5-1 (raise only).
const alertReasonPayoutSuccessAfterReversalTombstone = "success_after_payout_reversal_tombstone"

// payoutReversalTombstoneCell is LF-A5-1: success evidence (callback or poll) for a payout attempt whose
// withdrawal was failed by the A5 tombstone branch (a tombstone correlated to the request, on the attempt's
// provider). The staff attestation "the PSP returned it" is then contradicted: the hold is already back with the
// player, so this is a double-payout candidate. It writes its own audit row (once per new receipt) and raises a
// DISTINCT P1 (its own discriminator, so the earlier park alert does not swallow it) as the LAST statement. No
// state change, no posting. handled = false when the attempt is not such a payout (the caller continues).
func payoutReversalTombstoneCell(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, evidence EvidenceKind, newReceipt bool, reference string) (bool, error) {
	if attempt.Operation != AttemptOperationPayout || attempt.WithdrawalRequestID == nil || attempt.ProviderID == nil {
		return false, nil
	}
	var tombstone *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM ledger_transactions
		  WHERE tenant_id = $1 AND transaction_type = 'tombstone' AND correlation_id = $2 AND provider_id = $3
		  ORDER BY id LIMIT 1`,
		attempt.TenantID, *attempt.WithdrawalRequestID, *attempt.ProviderID).Scan(&tombstone); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("payments: read payout reversal tombstone: %w", err)
	}
	if tombstone == nil {
		return false, nil
	}
	if newReceipt {
		meta := map[string]any{
			"withdrawal_request_id": attempt.WithdrawalRequestID.String(), "tombstone_ledger_transaction_id": tombstone.String(),
			"evidence": string(evidence), "provider_id": *attempt.ProviderID, "attempt_state": string(attempt.State),
		}
		if reference != "" {
			meta["reported_reference"] = reference
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_success_after_reversal_tombstone",
			TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied, Metadata: meta,
		}); err != nil {
			return false, fmt.Errorf("payments: audit success after reversal tombstone: %w", err)
		}
	}
	return true, raisePayoutDisputeAlert(ctx, tx, attempt, alertReasonPayoutSuccessAfterReversalTombstone)
}
