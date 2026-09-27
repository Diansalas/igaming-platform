// PRH-I1 step (c): cascade-via-committed-decline (ADR 0095 §4.6),
// deposit path only. A decline is never followed inline by another
// provider call inside the same transaction or webhook handler - the
// cascade row (T1, attempt n+1) is inserted in the SAME tx that commits
// the decline (T8), and driving that new row is a SEPARATE T2 claim,
// exactly like any other 'created' attempt.
package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
