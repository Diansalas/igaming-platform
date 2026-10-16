package withdrawal

// A5 / WITHDRAWAL-REVERSAL-1 (owner decision 2026-10-15; ADR 0113 s9.7; migration 0162): the governed
// payout reversal - a payout the provider later RETURNS or REVERSES. Two primitives live here; the
// orchestration (which one runs, the payout-attempt park and the tombstone of the reversal-before-
// completion case) is internal/payments' DecidePayoutReversal, which is the only production caller.
//
//   - RecordPayoutReversalDecision records ONE human 'reverse' decision in withdrawal_approvals (the
//     existing withdrawal four-eyes store), bound to the payload by its digest (reason_code
//     'payout_reversal:<sha256 hex>'), and reports whether enough DISTINCT Persons agreed on that same
//     payload: max(2, the withdrawal policy's RequiredApprovals). Four-eyes is unconditional (ADR 0100 s3.4
//     interim (b): never a single-person money correction); the policy's require_step_up applies at ANY
//     amount (stricter than Approve). Linked, active staff only; never the beneficiary. The database
//     re-checks the decision shape (0034 governance trigger, extended by 0162) and refuses the E11 edge
//     without two distinct-Person decisions on one payload (WD026).
//   - ReverseCompletedPayout is E11 (completed -> reversed): a COMPENSATING posting, never an edit of
//     the completion. withdrawal_reversed Dr psp_clearing / Cr player_cash, the request amount in the
//     request asset (same asset back to the same wallet), key '<id>:reversed', reverses the completion,
//     provider_tx_id = the provider's RETURN reference. Idempotency is database-enforced three ways: the
//     (tenant, idempotency_key) UNIQUE, the (tenant, provider_id, provider_tx_id) UNIQUE and 0162's one
//     reversal per completion; the state machine (WD020/WD021) makes a second E11 impossible.
//
// Lock order (ADR 0082 + A8): L1 the request FOR UPDATE (then, by the caller, the payout attempt), the
// approver's staff row FOR SHARE (the eligibility closure), L2 the completion FOR UPDATE, then
// ledger.Post's L3/L4. Nothing here takes a lock after L3.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/stepup"
)

// DecisionReverse is the withdrawal_approvals.decision value of a payout reversal vote (migration 0162).
const DecisionReverse = "reverse"

// ReversalReasonPrefix prefixes the payload digest stored as a 'reverse' decision's reason_code.
const ReversalReasonPrefix = "payout_reversal:"

// MinReversalApprovers is the four-eyes floor for a payout reversal (the database floor, WD026).
const MinReversalApprovers = 2

var (
	// ErrReversalPayloadMismatch: the reversal payload does not describe this payout (provider, original
	// reference, amount or asset differ from the request / its completion). Nothing is recorded. A partial
	// or different-asset return is NOT supported (PAYOUT-AMOUNT-DISPUTE-1 residual): it is refused here.
	ErrReversalPayloadMismatch = errors.New("withdrawal: the payout reversal does not match the payout (provider, reference, amount or asset)")
	// ErrReversalAlreadyExists: a different reversal of this completion is already on the ledger.
	ErrReversalAlreadyExists = errors.New("withdrawal: this payout completion has already been reversed")
)

var evidenceRefHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PayoutReversal is the payload every approver of one reversal must agree on. All fields are staff
// input checked against the platform's own records; none is trusted on its own.
type PayoutReversal struct {
	// ProviderID must equal the request's provider_id.
	ProviderID string
	// OriginalProviderReference is the payout's reference at the provider: the completion's
	// provider_tx_id (completed request) or the request's provider_reference (submitted request).
	OriginalProviderReference string
	// ReturnReference is the provider's own reference for the return / reversal (the posting's
	// provider_tx_id). It must differ from OriginalProviderReference.
	ReturnReference string
	// Amount / AssetCode must equal the request's (whole-payout return only).
	Amount    int64
	AssetCode string
	// EvidenceRefHash is the hex SHA-256 of the external evidence reference (no PII; ADR 0100 s5.1).
	EvidenceRefHash string
}

// Validate checks the payload's own shape (not its match with the request).
func (p PayoutReversal) Validate() error {
	if err := providerref.ValidateAll(providerref.Field{Name: "provider_id", Value: p.ProviderID, Required: true}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := providerref.ValidatePaymentReferences(
		providerref.Field{Name: "original_provider_reference", Value: p.OriginalProviderReference, Required: true},
		providerref.Field{Name: "return_reference", Value: p.ReturnReference, Required: true},
	); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if p.ReturnReference == p.OriginalProviderReference {
		return fmt.Errorf("%w: the return reference must differ from the original payout reference", ErrInvalidInput)
	}
	if p.Amount <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidInput)
	}
	if p.AssetCode == "" || len(p.AssetCode) > 32 {
		return fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if !evidenceRefHashRE.MatchString(p.EvidenceRefHash) {
		return fmt.Errorf("%w: evidence_ref_hash must be 64 lowercase hex characters", ErrInvalidInput)
	}
	return nil
}

// Digest is the canonical SHA-256 (hex) of the payload for requestID: length-prefixed fields, so no
// two different payloads share an encoding.
func (p PayoutReversal) Digest(requestID uuid.UUID) string {
	var b strings.Builder
	for _, f := range []string{"payout_reversal.v1", requestID.String(), p.ProviderID, p.OriginalProviderReference,
		p.ReturnReference, strconv.FormatInt(p.Amount, 10), p.AssetCode, p.EvidenceRefHash} {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ReasonCode is the withdrawal_approvals.reason_code of a 'reverse' decision on this payload.
func (p PayoutReversal) ReasonCode(requestID uuid.UUID) string {
	return ReversalReasonPrefix + p.Digest(requestID)
}

// ReversalDecision is RecordPayoutReversalDecision's outcome.
type ReversalDecision struct {
	// Request is the request as locked at the start of the call.
	Request WithdrawalRequest
	// Replay: this approver already voted for this exact payload and the request is already terminal
	// (the reversal executed): an idempotent retry. Nothing was recorded.
	Replay bool
	// Ready: Counted >= Required distinct Persons agreed on this payload; the caller executes now, in this
	// transaction.
	Ready bool
	// Counted / Required: distinct Persons that agreed on this payload, and how many are needed.
	Counted, Required int
}

// RecordPayoutReversalDecision records approverPrincipalID's 'reverse' decision for requestID on payload
// p. approverPrincipalID MUST come from the authenticated staff session (never a body); the caller has
// already checked the RBAC permission and tenant scope. beneficiaryCheck and approverEligibility are
// REQUIRED (a reversal is always a human decision). stepUp is the HTTP step-up gate's assertion.
//
// The request must be `completed` (the normal reversal) or `submitted` (a return reported before the
// platform saw the completion: the caller's tombstone branch). The payload must match it. Errors:
// ErrInvalidInput, ErrNotFound, ErrStateConflict, ErrReversalPayloadMismatch, ErrApproverNotLinked,
// ErrApproverInactive, ErrSelfApproval, ErrStepUpRequired, ErrDuplicateApproval (this approver already
// voted on this request, for any payload).
func RecordPayoutReversalDecision(
	ctx context.Context, tx pgx.Tx, requestID, approverPrincipalID uuid.UUID, p PayoutReversal,
	beneficiaryCheck BeneficiaryCheck, approverEligibility ApproverEligibility, stepUp stepup.Assertion,
) (ReversalDecision, error) {
	if approverPrincipalID == uuid.Nil {
		return ReversalDecision{}, fmt.Errorf("%w: approver principal id is required", ErrInvalidInput)
	}
	if beneficiaryCheck == nil || approverEligibility == nil {
		return ReversalDecision{}, fmt.Errorf("%w: a payout reversal needs the beneficiary and eligibility checks", ErrInvalidInput)
	}
	if err := p.Validate(); err != nil {
		return ReversalDecision{}, err
	}
	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return ReversalDecision{}, err
	}
	reason := p.ReasonCode(requestID)

	switch wr.State {
	case StateCompleted, StateSubmitted:
	case StateReversed, StateFailed:
		// An idempotent retry of the call that executed the reversal (or its tombstone branch): this
		// approver's vote for this exact payload is on file, that payload reached the four-eyes floor, and
		// the ledger carries its execution (the reversal with this return reference, or the tombstone on
		// this original reference). Anything else is a conflict.
		replay, err := isExecutedReversalReplay(ctx, tx, wr, approverPrincipalID, p, reason)
		if err != nil {
			return ReversalDecision{}, err
		}
		if replay {
			return ReversalDecision{Request: wr, Replay: true}, nil
		}
		return ReversalDecision{}, fmt.Errorf("%w: request %s is in state %q", ErrStateConflict, requestID, wr.State)
	default:
		return ReversalDecision{}, fmt.Errorf("%w: request %s is in state %q, a payout reversal needs completed or submitted", ErrStateConflict, requestID, wr.State)
	}
	if err := matchReversalPayload(ctx, tx, wr, p); err != nil {
		return ReversalDecision{}, err
	}

	linked, active, err := approverEligibility(approverPrincipalID)
	if err != nil {
		return ReversalDecision{}, fmt.Errorf("withdrawal: approver eligibility check: %w", err)
	}
	if !linked {
		return ReversalDecision{}, ErrApproverNotLinked
	}
	if !active {
		return ReversalDecision{}, ErrApproverInactive
	}
	isBeneficiary, err := beneficiaryCheck(approverPrincipalID)
	if err != nil {
		return ReversalDecision{}, fmt.Errorf("withdrawal: beneficiary check: %w", err)
	}
	if isBeneficiary {
		return ReversalDecision{}, ErrSelfApproval
	}

	policy, err := ResolveApprovalPolicy(ctx, tx, wr.TenantID, wr.BrandID, nil, wr.AssetCode, time.Now())
	if err != nil {
		return ReversalDecision{}, err
	}
	required := policy.RequiredApprovals
	if required < MinReversalApprovers {
		required = MinReversalApprovers
	}
	if policy.RequireStepUp {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return ReversalDecision{}, fmt.Errorf("withdrawal: read database clock: %w", err)
		}
		if !stepUp.FreshFor(approverPrincipalID, now, stepup.PolicyMaxAge) {
			return ReversalDecision{}, ErrStepUpRequired
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawal_approvals
			(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code,
			 threshold_amount_at_decision, request_amount_at_decision)
		 VALUES ($1, $2, $3, $4, false, 'reverse', $5, $6, $7)`,
		uuid.New(), wr.TenantID, requestID, approverPrincipalID, reason, policy.ThresholdAmount, wr.Amount,
	); err != nil {
		if db.IsUniqueViolation(err) {
			return ReversalDecision{}, ErrDuplicateApproval
		}
		return ReversalDecision{}, fmt.Errorf("withdrawal: insert reversal decision: %w", err)
	}

	var counted int
	if err := tx.QueryRow(ctx,
		`SELECT count(DISTINCT su.person_id)
		   FROM withdrawal_approvals wa
		   JOIN staff_users su ON su.id = wa.approver_principal_id
		  WHERE wa.withdrawal_request_id = $1 AND wa.decision = 'reverse' AND wa.is_automated_approval = false
		    AND wa.reason_code = $2 AND su.person_id IS NOT NULL`,
		requestID, reason).Scan(&counted); err != nil {
		return ReversalDecision{}, fmt.Errorf("withdrawal: count reversal decisions: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: wr.TenantID, ActorType: audit.ActorStaff, ActorID: approverPrincipalID,
		Action: "withdrawal.payout_reversal_decision_recorded", TargetType: "withdrawal_request", TargetID: requestID.String(),
		Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"decision": DecisionReverse, "payload_digest": p.Digest(requestID), "state": string(wr.State),
			"provider_id": p.ProviderID, "original_provider_reference": p.OriginalProviderReference,
			"return_reference": p.ReturnReference, "amount": p.Amount, "asset_code": p.AssetCode,
			"evidence_ref_hash": p.EvidenceRefHash, "required_approvals": required, "counted_approvals": counted,
			"require_step_up": policy.RequireStepUp,
		},
	}); err != nil {
		return ReversalDecision{}, fmt.Errorf("withdrawal: audit reversal decision: %w", err)
	}
	return ReversalDecision{Request: wr, Ready: counted >= required, Counted: counted, Required: required}, nil
}

// matchReversalPayload checks p against the request (and, for a completed request, its completion).
func matchReversalPayload(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, p PayoutReversal) error {
	if wr.ProviderID == nil || *wr.ProviderID != p.ProviderID || wr.Amount != p.Amount || wr.AssetCode != p.AssetCode {
		return ErrReversalPayloadMismatch
	}
	if wr.State == StateSubmitted {
		// The reference may be bound only on the payout attempt (a parked mismatch binds the attempt); the
		// caller (internal/payments) proves the reference against the attempt. A reference on the request
		// must agree.
		if wr.ProviderReference != nil && *wr.ProviderReference != p.OriginalProviderReference {
			return ErrReversalPayloadMismatch
		}
		return nil
	}
	if wr.ReleaseLedgerTransactionID == nil {
		return fmt.Errorf("withdrawal: completed request %s has no completion link", wr.ID)
	}
	var typ string
	var providerID, providerTxID *string
	if err := tx.QueryRow(ctx,
		`SELECT transaction_type, provider_id, provider_tx_id FROM ledger_transactions WHERE id = $1 AND tenant_id = $2`,
		*wr.ReleaseLedgerTransactionID, wr.TenantID).Scan(&typ, &providerID, &providerTxID); err != nil {
		return fmt.Errorf("withdrawal: read completion: %w", err)
	}
	if typ != string(ledger.TxWithdrawalCompleted) || providerID == nil || *providerID != p.ProviderID ||
		providerTxID == nil || *providerTxID != p.OriginalProviderReference {
		return ErrReversalPayloadMismatch
	}
	return nil
}

// isExecutedReversalReplay reports whether this call repeats the decision that executed the reversal.
func isExecutedReversalReplay(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, approverID uuid.UUID, p PayoutReversal, reason string) (bool, error) {
	var replay bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM withdrawal_approvals
		                WHERE withdrawal_request_id = $1 AND approver_principal_id = $2 AND decision = 'reverse' AND reason_code = $3)
		   AND (SELECT count(DISTINCT su.person_id) FROM withdrawal_approvals wa JOIN staff_users su ON su.id = wa.approver_principal_id
		         WHERE wa.withdrawal_request_id = $1 AND wa.decision = 'reverse' AND NOT wa.is_automated_approval
		           AND wa.reason_code = $3 AND su.person_id IS NOT NULL) >= $4
		   AND CASE $5::text
		         WHEN 'reversed' THEN EXISTS (
		             SELECT 1 FROM withdrawal_requests w JOIN ledger_transactions t ON t.id = w.reversal_ledger_transaction_id
		              WHERE w.id = $1 AND t.provider_id = $6 AND t.provider_tx_id = $7)
		         ELSE EXISTS (
		             SELECT 1 FROM ledger_transactions t
		              WHERE t.tenant_id = $9 AND t.transaction_type = 'tombstone' AND t.correlation_id = $1
		                AND t.provider_id = $6 AND t.provider_tx_id = $8)
		       END`,
		wr.ID, approverID, reason, MinReversalApprovers, string(wr.State), p.ProviderID, p.ReturnReference, p.OriginalProviderReference, wr.TenantID,
	).Scan(&replay)
	if err != nil {
		return false, fmt.Errorf("withdrawal: read reversal replay: %w", err)
	}
	return replay, nil
}

// ReverseCompletedPayout performs E11 for a `completed` request whose reversal decision is Ready, in the
// SAME transaction as that decision (the caller holds the request lock already; re-locking is a no-op).
// It returns the withdrawal_reversed transaction id.
func ReverseCompletedPayout(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, p PayoutReversal) (uuid.UUID, error) {
	if err := p.Validate(); err != nil {
		return uuid.Nil, err
	}
	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return uuid.Nil, err
	}
	if wr.State != StateCompleted {
		return uuid.Nil, fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateCompleted)
	}
	if wr.ReleaseLedgerTransactionID == nil {
		return uuid.Nil, fmt.Errorf("withdrawal: completed request %s has no completion link", requestID)
	}
	completionID := *wr.ReleaseLedgerTransactionID
	// L2 (ADR 0082): the completion row FOR UPDATE serialises any concurrent reversal of the same
	// completion before the L3 pre-lock (0162's unique index is the backstop).
	var typ string
	var providerID, providerTxID *string
	if err := tx.QueryRow(ctx,
		`SELECT transaction_type, provider_id, provider_tx_id FROM ledger_transactions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		completionID, wr.TenantID).Scan(&typ, &providerID, &providerTxID); err != nil {
		return uuid.Nil, fmt.Errorf("withdrawal: lock completion: %w", err)
	}
	if typ != string(ledger.TxWithdrawalCompleted) || wr.ProviderID == nil || providerID == nil || *providerID != *wr.ProviderID ||
		*wr.ProviderID != p.ProviderID || providerTxID == nil || *providerTxID != p.OriginalProviderReference ||
		wr.Amount != p.Amount || wr.AssetCode != p.AssetCode {
		return uuid.Nil, ErrReversalPayloadMismatch
	}

	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode},
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("withdrawal: resolve psp_clearing/cash ledger accounts: %w", err)
	}
	clearingAccountID, cashAccountID := accounts[0], accounts[1]
	returnRef := p.ReturnReference
	posted, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:              wr.TenantID,
		TransactionType:       ledger.TxWithdrawalReversed,
		IdempotencyKey:        requestID.String() + ":reversed",
		ProviderID:            wr.ProviderID,
		ProviderTxID:          &returnRef,
		CorrelationID:         requestID,
		ReversesTransactionID: &completionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: clearingAccountID, Direction: ledger.Debit, Amount: wr.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: wr.Amount},
		},
	})
	if errors.Is(err, ledger.ErrWithdrawalReversalAlreadyExists) || errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrReversalAlreadyExists, err)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("withdrawal: post payout reversal: %w", err)
	}
	if posted.AlreadyPosted {
		// Unreachable while the request is `completed` (the posting and E11 commit together); never link a
		// transaction this call did not post.
		return uuid.Nil, fmt.Errorf("%w: the reversal key is already on the ledger", ErrReversalAlreadyExists)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, reversal_ledger_transaction_id = $2, updated_at = now()
		 WHERE id = $3 AND state = $4`,
		StateReversed, posted.TransactionID, requestID, StateCompleted)
	if err != nil {
		return uuid.Nil, fmt.Errorf("withdrawal: transition to reversed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return uuid.Nil, ErrStateConflict
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: wr.TenantID, ActorType: audit.ActorSystem,
		Action: "withdrawal.reversed", TargetType: "withdrawal_request", TargetID: requestID.String(),
		Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"state_before": string(StateCompleted), "state_after": string(StateReversed),
			"provider_id": p.ProviderID, "original_provider_reference": p.OriginalProviderReference,
			"return_reference": p.ReturnReference, "amount": wr.Amount, "asset_code": wr.AssetCode,
			"completion_ledger_transaction_id": completionID.String(),
			"reversal_ledger_transaction_id":   posted.TransactionID.String(),
			"payload_digest":                   p.Digest(requestID), "evidence_ref_hash": p.EvidenceRefHash,
		},
	}); err != nil {
		return uuid.Nil, fmt.Errorf("withdrawal: audit reversal: %w", err)
	}
	return posted.TransactionID, nil
}

// ReversalLink returns the request's reversal link (migration 0162), read on its own so the shared
// request column list keeps working against databases pinned below 0162 (migration tests).
func ReversalLink(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (*uuid.UUID, error) {
	var id *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT reversal_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, requestID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("withdrawal: read reversal link: %w", err)
	}
	return id, nil
}
