//go:build integration

package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// initiateDepositWithAttempt is the TEST-ONLY bridge (PRH-payments-
// callback-cutover) for domain tests written against the pre-ADR-0095
// legacy Orchestrator.InitiateDeposit path: it drives InitiateDeposit's
// existing synchronous flow AND a matching payment_attempts row through
// the SAME exported T1+T2/T4/T7/T8/T6/T9/T11 transitions
// InitiateDepositAttempt's own real path would have produced, so a
// callback for the resulting reference resolves under ADR 0095's
// INV-IO-14 attempt-based resolution (receipt.go ApplyReceiptEvidence)
// exactly as it would in production. InitiateDeposit itself is legacy and
// unreachable from any live HTTP path (the real one is
// InitiateDepositAttempt); this bridge exists ONLY so these tests keep
// asserting the domain behaviour (ledger, idempotency, locks, audit,
// concurrency) they were written for, unchanged, against the receipt
// path's new resolution requirement - it never changes what InitiateDeposit
// itself does.
//
// A retried call (same idempotency key) returns the SAME intent
// InitiateDeposit already returned before - this function is idempotent
// with respect to that: it only backfills an attempt if the intent does
// not already have one.
func initiateDepositWithAttempt(ctx context.Context, tx pgx.Tx, orch *Orchestrator, params InitiateDepositParams) (DepositIntent, error) {
	intent, err := orch.InitiateDeposit(ctx, tx, params)
	if err != nil {
		return intent, err
	}
	var existing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1`, intent.ID).Scan(&existing); err != nil {
		return intent, fmt.Errorf("payments test bridge: count existing attempts: %w", err)
	}
	if existing > 0 {
		return intent, nil
	}
	return intent, backfillAttemptForIntent(ctx, tx, intent)
}

// backfillAttemptForIntent drives one payment_attempts row to match
// intent's CURRENT state (as InitiateDeposit's synchronous flow, possibly
// including an in-process cascade, already decided it) - see
// initiateDepositWithAttempt's doc comment.
func backfillAttemptForIntent(ctx context.Context, tx pgx.Tx, intent DepositIntent) error {
	if intent.ProviderID == nil {
		// A phase-A pre-call refusal (RG/KYC deny, no routable provider):
		// no provider was ever tried, so no attempt row exists for this
		// case even on the real InitiateDepositAttempt path (ADR 0095 §4.3
		// T3's "no attempt" outcome) - nothing to backfill.
		return nil
	}
	attempt, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
		ID: uuid.New(), TenantID: intent.TenantID, Operation: AttemptOperationDeposit,
		DepositIntentID: &intent.ID, ProviderID: *intent.ProviderID, PaymentMethod: intent.PaymentMethod,
		AssetCode: intent.AssetCode, Amount: intent.Amount, Interactive: false,
		ClaimToken: uuid.New(), LeaseOwner: "test-backfill", LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		return fmt.Errorf("payments test bridge: backfill attempt: %w", err)
	}
	nextAction := time.Now().Add(time.Minute)

	switch intent.Status {
	case DepositIntentSucceeded:
		var ref string
		if intent.ProviderReference != nil {
			ref = *intent.ProviderReference
		}
		return ApplySuccess(ctx, tx, attempt.ID, SuccessEvidence{
			Evidence: EvidenceSync, ProviderReference: ref, LedgerTransactionID: intent.LedgerTransactionID,
		})
	case DepositIntentDeclined:
		cascadable := false
		return ApplyDecline(ctx, tx, attempt.ID, DeclineEvidence{
			Evidence: EvidenceSync, Reason: "test_backfill_decline", Stage: DeclineAfterAcceptance,
			Cascadable: &cascadable, ProviderRef: intent.ProviderReference,
		})
	case DepositIntentAmbiguous:
		if intent.ProviderReference != nil {
			// The provider accepted, THEN a status check came back
			// ambiguous (T9 then T11) - the reference is already known.
			if err := MarkAccepted(ctx, tx, attempt.ID, EvidenceSync, *intent.ProviderReference, nextAction); err != nil {
				return err
			}
			return MarkAmbiguousFromPending(ctx, tx, attempt.ID, EvidenceSync, nextAction)
		}
		// The call itself timed out/errored with no reference at all (T6).
		return MarkAmbiguousFromSubmitting(ctx, tx, attempt.ID, EvidenceSync, nextAction)
	case DepositIntentPending, DepositIntentFailed:
		if intent.ProviderReference == nil {
			return nil // still awaiting acceptance - stays 'submitting'
		}
		return MarkAccepted(ctx, tx, attempt.ID, EvidenceSync, *intent.ProviderReference, nextAction)
	default:
		return fmt.Errorf("payments test bridge: unhandled deposit intent status %q for attempt backfill", intent.Status)
	}
}
