// PRH-I1 step (c): cascade-via-committed-decline (ADR 0095 §4.6),
// deposit path only. A decline is never followed inline by another
// provider call inside the same transaction or webhook handler - the
// cascade row (T1, attempt n+1) is inserted in the SAME tx that commits
// the decline (T8), and driving that new row is a SEPARATE T2 claim,
// exactly like any other 'created' attempt.
package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// cascadeEligible implements §4.6's eligibility predicate. sweeperDriven
// is true when the caller is the sweeper (case (b): only non-interactive
// attempts cascade unattended); false means the caller is the same
// synchronous request that just received the decline (case (a), which
// may cascade an interactive attempt too, since the same request can
// still show the player a new redirect).
//
// NOT implemented in this step, and explicitly not silently assumed:
// the kill-switch predicate (migration 0102 does not exist yet - a
// later PRH-I1 step re-checks it here too, per §4.6 "the kill switch is
// not engaged for the next candidate (re-checked at T2)").
func cascadeEligible(attempt PaymentAttempt, intentStatus DepositIntentStatus, evidenceCascadable bool, maxDepth int, sweeperDriven bool) bool {
	if !evidenceCascadable {
		return false
	}
	if attempt.AttemptNo >= maxDepth {
		return false
	}
	if intentStatus == DepositIntentSucceeded {
		return false
	}
	if sweeperDriven && attempt.Interactive {
		return false
	}
	return true
}

// insertCascadeAttempt performs T1 for the next attempt (n+1),
// provider_id NULL, excluded_provider_ids extended with every provider
// tried so far including the one that just declined.
func insertCascadeAttempt(ctx context.Context, tx pgx.Tx, prev PaymentAttempt) (PaymentAttempt, error) {
	excluded := append([]string{}, prev.ExcludedProviderIDs...)
	if prev.ProviderID != nil {
		excluded = append(excluded, *prev.ProviderID)
	}
	next, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
		ID: uuid.New(), TenantID: prev.TenantID, Operation: prev.Operation,
		DepositIntentID: prev.DepositIntentID, WithdrawalRequestID: prev.WithdrawalRequestID,
		AttemptNo: prev.AttemptNo + 1, ExcludedProviderIDs: excluded,
		PaymentMethod: prev.PaymentMethod, AssetCode: prev.AssetCode, Amount: prev.Amount,
		Interactive: prev.Interactive,
	})
	if err != nil {
		return PaymentAttempt{}, fmt.Errorf("payments: insert cascade attempt (T1): %w", err)
	}
	return next, nil
}

// insertCascadeAttemptIfEligible is insertCascadeAttempt's H3-safe wrapper
// (RV-PRH-I1 ledger-finance review, ADR 0095 §10.3): "callbacks, QueryStatus
// polls... are never stopped" by an engaged kill switch. insertCascadeAttempt
// -> InsertCreatedAttempt refuses (ErrKillSwitchEngaged) when a wildcard
// (provider_scope='*') switch is engaged for this operation - that refusal
// must mean "this decline is not cascade-eligible right now", never a
// caller-visible error: the decline itself (T8, already applied by the
// caller before this is invoked) and the intent's own projection still
// commit normally. Called from every site that used to call
// insertCascadeAttempt directly (the callback/receipt path, phase C's
// synchronous cascade loop, and the sweeper) - one shared decision instead
// of three copies that could drift.
//
// A non-kill-switch error is still returned - only ErrKillSwitchEngaged is
// downgraded to "no child, logged".
func insertCascadeAttemptIfEligible(ctx context.Context, tx pgx.Tx, prev PaymentAttempt) (*PaymentAttempt, error) {
	next, err := insertCascadeAttempt(ctx, tx, prev)
	if err == nil {
		return &next, nil
	}
	if !errors.Is(err, ErrKillSwitchEngaged) {
		return nil, err
	}
	// Labelled, never silent: an engaged switch deferred a cascade the
	// decline would otherwise have taken - the intent finalizes declined
	// (via the normal §5.1 projection, since no live/created attempt
	// remains) rather than rolling back or 500ing.
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: prev.TenantID, ActorType: audit.ActorSystem, Action: "payment.cascade_skipped_kill_switch",
		TargetType: "payment_attempt", TargetID: prev.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{"attempt_no": prev.AttemptNo, "operation": string(prev.Operation)},
	}); err != nil {
		return nil, fmt.Errorf("payments: audit cascade skipped by kill switch: %w", err)
	}
	return nil, nil
}

// rejectCreatedSiblings implements ADR 0095 §4.3 T13(c)/LF95-C6(c): the
// tx that commits a deposit's T13 success (a DECLINED attempt succeeding
// after a sibling cascade child was already created) also moves any
// sibling 'created' attempt of the SAME intent to 'rejected'
// (terminal_reason "intent_succeeded") in the SAME transaction - so a
// cascade child left over from before the late success can never be
// claimed (T2) and charge the player a second time at another PSP
// (RV-PRH-I1 ledger-finance H4). Called AFTER the success has actually
// posted, so intentSucceeded is already true; the T2 CAS predicate
// (ClaimCreatedForSubmission) is the second, independent guard for the
// race where a claim is already in flight concurrently.
func rejectCreatedSiblings(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt) error {
	if attempt.DepositIntentID == nil {
		return nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND state = 'created' AND id <> $2`,
		*attempt.DepositIntentID, attempt.ID,
	)
	if err != nil {
		return fmt.Errorf("payments: query created siblings for T13 rejection: %w", err)
	}
	var siblingIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("payments: scan created sibling id: %w", err)
		}
		siblingIDs = append(siblingIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("payments: query created siblings rows: %w", err)
	}
	rows.Close()

	for _, id := range siblingIDs {
		if err := RejectCreated(ctx, tx, id, EvidenceCallback, "intent_succeeded"); err != nil {
			// A concurrent T2 claim may have already moved this sibling out
			// of 'created' between the SELECT and this UPDATE - the T2
			// predicate's own succeeded-sibling guard is the backstop for
			// that race (H4), so a CAS conflict here is expected and safe,
			// never a reason to fail the T13 success that is already
			// committed in this same transaction.
			if errors.Is(err, ErrAttemptStateConflict) {
				continue
			}
			return fmt.Errorf("payments: reject created sibling %s (T13/intent_succeeded): %w", id, err)
		}
	}
	return nil
}
