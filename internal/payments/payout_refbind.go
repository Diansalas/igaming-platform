package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// PAY-PAYOUT-REFBIND-1 (LF-6 / F-C4 for payouts).
//
// A provider reference that passed the providerref validator is still only
// WELL-FORMED, not ours: a real PSP can echo a reference that is already bound
// to a different attempt, a different withdrawal, or to a ledger transaction
// (the "reverse collision": a payout settlement whose ledger key
// provider_id:provider_tx_id equals an existing deposit key). Without a
// pre-check the collision surfaces only at the statement that binds it
// (MarkAccepted, ApplyDecline, AttachProviderReference, ledger.Post) as a unique
// violation or ErrIdempotencyPayloadMismatch that rolls the whole phase-C
// transaction back and recurs forever, with the withdrawal hold stuck.
//
// payoutForeignReferenceBinding is the payout-shaped twin of the deposit-side
// foreignReferenceBinding; payoutGuardReferenceBinding is the shared call that
// every payout bind site makes BEFORE it binds, parking (T10) on a conflict.

// payoutReferenceConflictAuditAction is the audit action of a conflict park.
const payoutReferenceConflictAuditAction = "payments.payout_parked_reference_conflict"

// payoutForeignReferenceBinding reports whether (tenantID, providerID,
// reference) is already bound to something OTHER than this payout, and what:
//   - another payment_attempts row (deposit or payout) -> its operation;
//   - another withdrawal_requests.provider_reference   -> "withdrawal_request";
//   - ANY ledger transaction holding the settlement key (tenant, provider,
//     provider_tx_id), tombstones included -> "ledger_<transaction_type>"
//     (e.g. "ledger_tombstone"). This
//     request's own withdrawal_completed row (written atomically with
//     ApplySuccess) can only be seen on a replay against an already-terminal
//     attempt, where the park's CAS fails and the late-evidence routing takes
//     over - it is never a new bind.
//
// Same tenant only: the predicates name the tenant, and the unique indexes they
// mirror are per tenant under FORCE RLS. A tombstone is deliberately NOT
// excluded here (unlike the deposit-side foreignReferenceBinding, whose
// tombstone has its own T10/T13t handling): a payout has no such handling, and
// a reversal tombstone occupies provider_tx_id, so withdrawal.Complete's
// ledger.Post would hit the unique index on every redelivery and roll back with
// the hold stuck. It is a conflict for payouts. deposit_intents is not
// consulted: a deposit intent's reference is mirrored on its attempt row
// (checked above) and on its ledger key (checked here), so a legacy deposit key
// is still caught, and a bare intent row alone cannot make a payout bind loop.
// The caller must hold the withdrawal lock.
func payoutForeignReferenceBinding(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, reference string, attemptID, requestID uuid.UUID) (bool, string, error) {
	var op string
	err := tx.QueryRow(ctx,
		`SELECT operation FROM payment_attempts
		 WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3 AND id <> $4
		 LIMIT 1`,
		tenantID, providerID, reference, attemptID,
	).Scan(&op)
	if err == nil {
		return true, op, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("payments: check payout reference binding (attempts): %w", err)
	}

	var one int
	err = tx.QueryRow(ctx,
		`SELECT 1 FROM withdrawal_requests
		 WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3 AND id <> $4
		 LIMIT 1`,
		tenantID, providerID, reference, requestID,
	).Scan(&one)
	if err == nil {
		return true, "withdrawal_request", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("payments: check payout reference binding (withdrawals): %w", err)
	}

	var txType string
	err = tx.QueryRow(ctx,
		`SELECT transaction_type FROM ledger_transactions
		 WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3
		 LIMIT 1`,
		tenantID, providerID, reference,
	).Scan(&txType)
	if err == nil {
		return true, "ledger_" + txType, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("payments: check payout reference binding (ledger): %w", err)
	}
	return false, "", nil
}

// payoutGuardReferenceBinding is called before every payout statement that binds
// a provider reference (attempt, withdrawal, ledger settlement key). An empty
// reference binds nothing and passes. On a conflict it parks the attempt T10
// with provider_reference_conflict (hold kept: nothing is released or settled),
// writes one audit row in the same transaction, and returns parked=true; the
// caller then returns nil so the park commits and nothing retries in a loop. A
// redelivery of the SAME attempt's own reference is not a conflict.
func payoutGuardReferenceBinding(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, requestID uuid.UUID, reference string, evidence EvidenceKind, class ErrorClass) (bool, error) {
	if reference == "" || attempt.ProviderID == nil || *attempt.ProviderID == "" {
		return false, nil
	}
	// Two payouts racing to bind the SAME fresh reference both pass this read;
	// the unique indexes then make the loser's whole transaction fail and roll
	// back (hold untouched). For a synchronous Withdraw the loser's result is
	// LOST on that rollback: convergence is the sweeper finding the attempt still
	// `submitting` with no reference, moving it to ambiguous (T6, no-reference),
	// a resend, and the resend's result then hitting this guard, which sees the
	// winner's committed binding and parks. A callback or status redelivery for
	// the loser converges the same way on its next delivery. No advisory lock is
	// taken: they are confined to claimBatch
	// (TestSweeperAdvisoryLock_OnlyInsideClaimBatch).
	conflict, boundTo, err := payoutForeignReferenceBinding(ctx, tx, attempt.TenantID, *attempt.ProviderID, reference, attempt.ID, requestID)
	if err != nil {
		return false, err
	}
	if !conflict {
		return false, nil
	}
	if err := ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, evidence, TerminalReasonProviderReferenceConflict); err != nil {
		// A concurrent transition already moved the attempt: the same routing
		// every other phase-C CAS conflict takes. Never fall through to the bind.
		return true, payoutHandleContradiction(ctx, tx, attempt, evidence, class, err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: payoutReferenceConflictAuditAction,
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{
			"withdrawal_request_id": requestID.String(),
			"provider_id":           *attempt.ProviderID,
			"provider_reference":    reference,
			"bound_to":              boundTo,
			"reason":                TerminalReasonProviderReferenceConflict,
		},
	}); err != nil {
		return false, err
	}
	// B12 (PAY-PAYOUT-DISPUTE-ALERT-1): the park raises a durable P1 as the LAST statement,
	// after the CAS and the audit row. parked stays true on a raise error so no caller falls
	// through to a bind; a (transient) error propagates and rolls the whole park back.
	return true, raisePayoutDisputeAlert(ctx, tx, attempt, TerminalReasonProviderReferenceConflict)
}
