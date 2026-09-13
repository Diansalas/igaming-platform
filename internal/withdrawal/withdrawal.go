// Package withdrawal implements the withdrawal state machine and its
// four-eyes approval workflow (docs/architecture/withdrawal-state-
// machine.md). It owns WithdrawalRequest/WithdrawalApproval and every
// state transition between them; it never posts a ledger entry outside
// the exact moments financial-transaction-flows.md Flows 3-4 specify,
// and it never mutates a balance directly - all money movement goes
// through internal/ledger's Post().
//
// This package does not authenticate anyone. Every function that takes
// an approverPrincipalID/actor id documents, at the call site, that the
// CALLER (an HTTP handler acting on an authenticated session, in a
// future task) is responsible for deriving that id from verified
// session/RBAC state - never from a request body or any other
// client-controlled input. This package's job is to enforce the
// workflow and four-eyes invariants given a trustworthy id, not to
// establish that the id is trustworthy.
package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// State is one of the nine values in withdrawal_requests' CHECK
// constraint (migration 0026), mirroring withdrawal-state-machine.md §1.
type State string

const (
	StateRequested     State = "requested"
	StatePendingReview State = "pending_review"
	StateApproved      State = "approved"
	StateRejected      State = "rejected"
	StateSubmitted     State = "submitted"
	StateCompleted     State = "completed"
	StateFailed        State = "failed"
	StateCancelled     State = "cancelled"
	StateReversed      State = "reversed"
)

// Sentinel errors. Callers (HTTP handlers) are expected to map these to
// specific HTTP statuses (e.g. ErrStateConflict/ErrDuplicateApproval/
// ErrSelfApproval -> 409, ErrInsufficientFunds -> 422, ErrNotFound -> 404).
var (
	// ErrInvalidInput is returned for a structurally invalid call (missing
	// required id, non-positive amount, empty reason code, etc.) caught
	// before ever reaching the database.
	ErrInvalidInput = errors.New("withdrawal: invalid input")

	// ErrNotFound is returned when a WithdrawalRequest does not exist.
	ErrNotFound = errors.New("withdrawal: request not found")

	// ErrInsufficientFunds is returned when the wallet's player_cash
	// balance, locked and read inside the same transaction as the
	// prospective debit (invariant #15), does not cover the requested
	// amount.
	ErrInsufficientFunds = errors.New("withdrawal: insufficient player_cash balance")

	// ErrStateConflict is returned whenever a transition's precondition
	// on the request's current state is not met - either because the
	// caller passed a request already in some other state, or because a
	// concurrent transition won the race (the UPDATE ... WHERE state =
	// $expected clause affected zero rows). This package does not
	// distinguish the two cases in its error type: both mean "the
	// transition this call wanted did not happen, and the caller's view
	// of the request's state was stale" (withdrawal-state-machine.md §4).
	ErrStateConflict = errors.New("withdrawal: request is not in the expected state")

	// ErrDuplicateApproval is returned when approverPrincipalID has
	// already recorded ANY decision (approve or reject) for this
	// request - UNIQUE (withdrawal_request_id, approver_principal_id)
	// (withdrawal-state-machine.md §5 bypass #7 - approvals are
	// append-only, a reconsideration is a new request, not an edited
	// decision).
	ErrDuplicateApproval = errors.New("withdrawal: this approver has already recorded a decision for this request")

	// ErrSelfApproval is returned when the caller-supplied
	// beneficiaryCheck reports that approverPrincipalID resolves to the
	// same Person as the withdrawing player_account_id (bypass #2).
	ErrSelfApproval = errors.New("withdrawal: an approver cannot approve their own withdrawal")

	// ErrStepUpRequired is returned by Approve when the resolved
	// ApprovalPolicy (policy.go) requires a step-up/MFA challenge for
	// this decision and none exists yet - see ErrStepUpRequired's use in
	// policy.go for the full Stage 3C directive item 6 rationale (ADR
	// 0017 preserved: MFA itself is not implemented here).
	ErrStepUpRequired = errors.New("withdrawal: this decision requires a step-up/MFA challenge, which is not yet implemented (ADR 0017)")

	// ErrIdempotencyKeyReused is returned when a retried RequestWithdrawal
	// call's idempotency_key matches an existing request whose wallet,
	// asset, or amount differs from the one now requested - the
	// workflow-layer equivalent of ledger.ErrIdempotencyKeyReused.
	ErrIdempotencyKeyReused = errors.New("withdrawal: idempotency key reused with a different wallet/asset/amount")
)

// WithdrawalRequest mirrors the withdrawal_requests row
// (withdrawal-state-machine.md §2). Amount is int64 minor units, matching
// internal/ledger's representation - never floating-point.
type WithdrawalRequest struct {
	ID                         uuid.UUID
	TenantID                   uuid.UUID
	BrandID                    uuid.UUID
	PlayerAccountID            uuid.UUID
	WalletID                   uuid.UUID
	AssetCode                  string
	Amount                     int64
	State                      State
	IdempotencyKey             string
	ProviderID                 *string
	ProviderReference          *string
	HoldLedgerTransactionID    *uuid.UUID
	ReleaseLedgerTransactionID *uuid.UUID
	RequestedAt                time.Time
	UpdatedAt                  time.Time
}

const requestColumns = `id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state,
	idempotency_key, provider_id, provider_reference, hold_ledger_transaction_id, release_ledger_transaction_id,
	requested_at, updated_at`

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query,
// via its own Next()-then-Scan() loop) so scanRequest can be shared by
// both single-row lookups and ListForPlayer.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequest(row rowScanner) (WithdrawalRequest, error) {
	var wr WithdrawalRequest
	err := row.Scan(
		&wr.ID, &wr.TenantID, &wr.BrandID, &wr.PlayerAccountID, &wr.WalletID, &wr.AssetCode, &wr.Amount, &wr.State,
		&wr.IdempotencyKey, &wr.ProviderID, &wr.ProviderReference, &wr.HoldLedgerTransactionID, &wr.ReleaseLedgerTransactionID,
		&wr.RequestedAt, &wr.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WithdrawalRequest{}, ErrNotFound
	}
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: scan request: %w", err)
	}
	return wr, nil
}

// GetByID looks up a WithdrawalRequest by id within the caller's current
// RLS scope (tenant staff scope, via db.Pool.WithTenant, for back-office/
// system callers; player scope, via db.Pool.WithPlayerScope, only ever
// returns the row if it belongs to that player - see ListForPlayer for
// the intended player-self-service read).
func GetByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (WithdrawalRequest, error) {
	row := tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM withdrawal_requests WHERE id = $1`, id)
	return scanRequest(row)
}

// ListForPlayer returns every WithdrawalRequest belonging to
// playerAccountID, most recent first. Intended to be called inside
// db.Pool.WithPlayerScope so RLS's player_self_scope policy (SELECT-only,
// migration 0026) is what actually restricts visibility to the caller's
// own rows - this function itself applies no authorization, it is a
// plain query that relies on the transaction's RLS scope being correct.
func ListForPlayer(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID) ([]WithdrawalRequest, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+requestColumns+` FROM withdrawal_requests WHERE player_account_id = $1 ORDER BY requested_at DESC`,
		playerAccountID,
	)
	if err != nil {
		return nil, fmt.Errorf("withdrawal: list for player: %w", err)
	}
	defer rows.Close()

	var out []WithdrawalRequest
	for rows.Next() {
		wr, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wr)
	}
	return out, rows.Err()
}

func getByTenantPlayerIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, key string) (WithdrawalRequest, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM withdrawal_requests WHERE tenant_id = $1 AND player_account_id = $2 AND idempotency_key = $3`,
		tenantID, playerAccountID, key,
	)
	return scanRequest(row)
}

// lockRequestForUpdate reads a WithdrawalRequest AND takes a row lock on
// it (SELECT ... FOR UPDATE) for the duration of the caller's
// transaction. Every transition function below that performs more than
// one statement (an approval insert plus a conditional state UPDATE, or
// a ledger Post plus a state UPDATE) takes this lock FIRST, so two
// concurrent transitions against the SAME request serialize on Postgres's
// own row lock rather than racing between an early read and a later
// conditional write. The `UPDATE ... WHERE state = $expected` clause each
// transition still uses is therefore normally guaranteed to match by the
// time it runs - it is kept anyway as the belt-and-braces optimistic-
// concurrency check withdrawal-state-machine.md §4 asks for, and as the
// only defense left for the single-statement transitions that don't need
// a separate lock.
func lockRequestForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (WithdrawalRequest, error) {
	row := tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, id)
	return scanRequest(row)
}

// lockCashBalanceForUpdate takes a row lock on the wallet's player_cash
// wallet_balance_projection row (if it exists yet) and returns its raw
// debit/credit totals. internal/ledger deliberately exposes no locking
// balance read of its own (GetProjectedBalance is a plain, unlocked read,
// correct for reporting but not for a check-then-post decision) - per
// this task's own instruction, that lock is implemented here, directly
// against wallet_balance_projection, rather than by changing
// internal/ledger. Two concurrent RequestWithdrawal calls against the
// same wallet therefore serialize on this single row: the second call's
// SELECT ... FOR UPDATE blocks until the first's transaction commits (at
// which point the AFTER INSERT trigger on ledger_entries has already
// updated this exact row) or rolls back (at which point nothing changed),
// so the second call always sees a balance that already reflects the
// first's hold - never a stale "sufficient" read (invariant #15).
//
// A wallet whose player_cash account has never been posted to has no
// projection row at all; that is reported as a zero balance (not an
// error), which is always insufficient for any positive withdrawal
// amount (withdrawal_requests_amount_check requires amount > 0).
func lockCashBalanceForUpdate(ctx context.Context, tx pgx.Tx, ledgerAccountID uuid.UUID) (debitTotal, creditTotal int64, err error) {
	err = tx.QueryRow(ctx,
		`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
		ledgerAccountID,
	).Scan(&debitTotal, &creditTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("withdrawal: lock cash balance: %w", err)
	}
	return debitTotal, creditTotal, nil
}

// RequestParams is everything RequestWithdrawal needs to create a
// WithdrawalRequest and post its hold.
//
// Every field here MUST be resolved server-side by the caller from
// authenticated context, per withdrawal-state-machine.md §7: "tenant_id,
// brand_id, player_account_id, and wallet_id on a new WithdrawalRequest
// are all resolved server-side from the authenticated player session; the
// request body supplies only asset_code... and amount. A client-supplied
// wallet or account identifier is never trusted, even when it looks like
// the caller's own." This package does not and cannot re-derive or check
// that for the caller - it trusts these fields exactly as given.
type RequestParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
	AssetCode       string
	// Amount is strictly positive minor units.
	Amount int64
	// IdempotencyKey is the client/session-supplied value deduplicating a
	// double-submit, namespaced (tenant_id, player_account_id,
	// idempotency_key) at the database level (migration 0026) - never
	// inserted into a global or ledger-level uniqueness namespace
	// directly (withdrawal-state-machine.md §4).
	IdempotencyKey string
}

// RequestWithdrawal creates a WithdrawalRequest row AND posts Flow 3 Step
// A (debit player_cash, credit player_withdrawal_hold) in the SAME
// database transaction (tx), so there is never a row in `requested` state
// without a hold_ledger_transaction_id (withdrawal-state-machine.md §4).
//
// Idempotency: a retried call with the same (TenantID, PlayerAccountID,
// IdempotencyKey) returns the original request rather than creating a
// second one or posting a second hold. A retried call whose wallet,
// asset, or amount differs from the original's returns
// ErrIdempotencyKeyReused.
//
// Concurrency/sufficiency (invariant #15): the wallet's player_cash
// balance is locked (SELECT ... FOR UPDATE on its
// wallet_balance_projection row, see lockCashBalanceForUpdate) and
// compared against Amount INSIDE this same transaction, immediately
// before posting the debit - two concurrent requests against a wallet
// with only enough balance for one of them will not both observe
// "sufficient".
func RequestWithdrawal(ctx context.Context, tx pgx.Tx, params RequestParams) (WithdrawalRequest, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.WalletID == uuid.Nil {
		return WithdrawalRequest{}, fmt.Errorf("%w: tenant/brand/player/wallet ids are required", ErrInvalidInput)
	}
	if params.AssetCode == "" {
		return WithdrawalRequest{}, fmt.Errorf("%w: asset code is required", ErrInvalidInput)
	}
	if params.Amount <= 0 {
		return WithdrawalRequest{}, fmt.Errorf("%w: amount must be positive, got %d", ErrInvalidInput, params.Amount)
	}
	if params.IdempotencyKey == "" {
		return WithdrawalRequest{}, fmt.Errorf("%w: idempotency key is required", ErrInvalidInput)
	}

	requestID := uuid.New()
	conflict, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
		_, err := spTx.Exec(ctx,
			`INSERT INTO withdrawal_requests
				(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			requestID, params.TenantID, params.BrandID, params.PlayerAccountID, params.WalletID, params.AssetCode,
			params.Amount, params.IdempotencyKey,
		)
		return err
	})
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: insert request: %w", err)
	}

	if conflict {
		existing, lookupErr := getByTenantPlayerIdempotencyKey(ctx, tx, params.TenantID, params.PlayerAccountID, params.IdempotencyKey)
		if lookupErr != nil {
			return WithdrawalRequest{}, fmt.Errorf("withdrawal: look up existing request for idempotency key: %w", lookupErr)
		}
		if existing.WalletID != params.WalletID || existing.AssetCode != params.AssetCode || existing.Amount != params.Amount {
			return WithdrawalRequest{}, fmt.Errorf("%w: existing request %s", ErrIdempotencyKeyReused, existing.ID)
		}
		return existing, nil
	}

	cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, params.TenantID, &params.WalletID, ledger.AccountPlayerCash, params.AssetCode)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: get or create cash account: %w", err)
	}
	holdAccountID, err := ledger.GetOrCreateAccount(ctx, tx, params.TenantID, &params.WalletID, ledger.AccountPlayerWithdrawalHold, params.AssetCode)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: get or create withdrawal hold account: %w", err)
	}

	debitTotal, creditTotal, err := lockCashBalanceForUpdate(ctx, tx, cashAccountID)
	if err != nil {
		return WithdrawalRequest{}, err
	}
	available := creditTotal - debitTotal // player_cash is credit-normal (ledger-accounting-model.md §5)
	if available < params.Amount {
		return WithdrawalRequest{}, ErrInsufficientFunds
	}

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:        params.TenantID,
		TransactionType: ledger.TxWithdrawalRequested,
		// Step A's own idempotency key is the WithdrawalRequest's own id,
		// per financial-transaction-flows.md Flow 3 ("one request = one
		// hold") - deliberately distinct from the client-supplied
		// workflow-layer IdempotencyKey field, which never reaches the
		// ledger's uniqueness namespace directly (withdrawal-state-
		// machine.md §4).
		IdempotencyKey: requestID.String(),
		CorrelationID:  requestID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: cashAccountID, Direction: ledger.Debit, Amount: params.Amount},
			{LedgerAccountID: holdAccountID, Direction: ledger.Credit, Amount: params.Amount},
		},
	})
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: post hold: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET hold_ledger_transaction_id = $1, updated_at = now() WHERE id = $2`,
		postResult.TransactionID, requestID,
	); err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: record hold transaction id: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID:   params.TenantID,
		ActorType:  audit.ActorPlayer,
		ActorID:    params.PlayerAccountID,
		Action:     "withdrawal.requested",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"amount":     params.Amount,
			"asset_code": params.AssetCode,
			"wallet_id":  params.WalletID.String(),
		},
	}); err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: audit: %w", err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID:   params.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.hold_placed",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"amount":                  params.Amount,
			"asset_code":              params.AssetCode,
			"hold_ledger_transaction": postResult.TransactionID.String(),
		},
	}); err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: audit: %w", err)
	}

	return GetByID(ctx, tx, requestID)
}

// MoveToPendingReview transitions a request from `requested` to
// `pending_review` once automated KYC/velocity/risk checks are queued
// (owned by identity-compliance, not this package). No ledger effect.
func MoveToPendingReview(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) error {
	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StateRequested {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateRequested)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, updated_at = now() WHERE id = $2 AND state = $3`,
		StatePendingReview, requestID, StateRequested,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: move to pending review: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}

	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.pending_review",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
	})
}

// BeneficiaryCheck reports whether approverPrincipalID resolves to the
// same Person as the withdrawal's own player_account_id (bypass #2 in
// withdrawal-state-machine.md §5 - "a staff member who is also a player
// self-approves their own payout"). This package has, and should have, no
// dependency on identity/Person resolution (that graph lives in
// internal/identity), so the caller (an HTTP handler with access to both)
// supplies this function. Passing nil skips the check entirely, which is
// only safe for a caller that cannot possibly be the beneficiary (e.g.
// the automated risk-engine service identity).
type BeneficiaryCheck func(approverPrincipalID uuid.UUID) (isBeneficiary bool, err error)

// Approve records a four-eyes 'approve' decision for a withdrawal request
// and, once the four-eyes requirement for the request's amount is
// satisfied, transitions the request to `approved`. approved reports
// whether THIS call caused that transition (false means the decision was
// recorded but a second distinct human approver is still required).
//
// approverPrincipalID MUST be derived by the caller from the approving
// staff member's own authenticated session - NEVER from a request body -
// and the caller MUST have already verified, server-side, that this
// principal holds the withdrawal-approval RBAC permission in the tenant
// that owns the request (bypass #1). This package enforces the counting/
// threshold/duplicate/append-only invariants given a trustworthy id; it
// does not authenticate the approver.
//
// The ApprovalPolicy in force is resolved internally, from
// ResolveApprovalPolicy (policy.go), using the locked request's own
// tenant/brand/asset - never passed in by the caller, so a caller can
// never (accidentally or otherwise) apply one asset's policy to another
// asset's withdrawal. Its ThresholdAmount is recorded verbatim on the
// WithdrawalApproval row (threshold_amount_at_decision) specifically so
// a later policy mutation is detectable after the fact (bypass #3); this
// package cannot prevent that bypass on its own (that requires
// policy-config-edit and withdrawal-approval permissions to be held by
// disjoint roles - an `OPEN DECISION` per withdrawal-state-machine.md §5
// and docs/architecture/withdrawal-policy-configuration.md, not resolved
// here).
//
// isAutomated must be true ONLY when approverPrincipalID identifies a
// risk-engine/service identity (ADR 0014) auto-approving a below-
// threshold request. Such a decision can never count toward the
// RequiredApprovals human approvals an above-threshold request requires
// (bypass #6) - this is enforced by filtering on
// is_automated_approval = false when counting distinct approvers below,
// not merely by convention.
//
// beneficiaryCheck implements bypass #2 - see BeneficiaryCheck's doc.
//
// If the resolved policy requires a step-up/MFA challenge for a request
// at or above threshold (ApprovalPolicy.RequireStepUp - directive item
// 6), this returns ErrStepUpRequired before recording anything: MFA is
// not implemented (ADR 0017 preserved), so a policy that asks for it
// must fail closed, never silently approve as if the challenge had
// happened.
//
// The policy resolution, distinct-approvers count, and the threshold
// comparison all run inside the same transaction (tx) as the approval
// insert and the conditional state UPDATE (bypass #5 - no TOCTOU
// window). A row lock on the WithdrawalRequest itself
// (lockRequestForUpdate) additionally serializes concurrent Approve
// calls against the SAME request, beyond what a single-transaction check
// alone would guarantee for genuinely simultaneous calls.
func Approve(
	ctx context.Context, tx pgx.Tx, requestID, approverPrincipalID uuid.UUID,
	isAutomated bool, beneficiaryCheck BeneficiaryCheck,
) (approved bool, err error) {
	if approverPrincipalID == uuid.Nil {
		return false, fmt.Errorf("%w: approver principal id is required", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return false, err
	}
	if wr.State != StatePendingReview {
		return false, fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StatePendingReview)
	}

	policy, err := ResolveApprovalPolicy(ctx, tx, wr.TenantID, wr.BrandID, nil, wr.AssetCode, time.Now())
	if err != nil {
		return false, err
	}
	if policy.RequiredApprovals < 1 {
		return false, fmt.Errorf("%w: resolved policy requires at least 1 approval, got %d", ErrInvalidInput, policy.RequiredApprovals)
	}

	if beneficiaryCheck != nil {
		isBeneficiary, err := beneficiaryCheck(approverPrincipalID)
		if err != nil {
			return false, fmt.Errorf("withdrawal: beneficiary check: %w", err)
		}
		if isBeneficiary {
			return false, ErrSelfApproval
		}
	}

	requiresMultipleApprovals := wr.Amount >= policy.ThresholdAmount
	if requiresMultipleApprovals && policy.RequireStepUp {
		return false, ErrStepUpRequired
	}

	approvalID := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawal_approvals
			(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
			 threshold_amount_at_decision, request_amount_at_decision)
		 VALUES ($1, $2, $3, $4, $5, 'approve', $6, $7)`,
		approvalID, wr.TenantID, requestID, approverPrincipalID, isAutomated, policy.ThresholdAmount, wr.Amount,
	); err != nil {
		if db.IsUniqueViolation(err) {
			return false, ErrDuplicateApproval
		}
		return false, fmt.Errorf("withdrawal: insert approval: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  approverActorType(isAutomated),
		ActorID:    approverPrincipalID,
		Action:     "withdrawal.approval_recorded",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"decision":              "approve",
			"is_automated_approval": isAutomated,
			"threshold_amount":      policy.ThresholdAmount,
			"required_approvals":    policy.RequiredApprovals,
			"request_amount":        wr.Amount,
		},
	}); err != nil {
		return false, fmt.Errorf("withdrawal: audit: %w", err)
	}

	ready := !requiresMultipleApprovals
	if requiresMultipleApprovals {
		var distinctHumanApprovers int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(DISTINCT approver_principal_id) FROM withdrawal_approvals
			 WHERE withdrawal_request_id = $1 AND decision = 'approve' AND is_automated_approval = false`,
			requestID,
		).Scan(&distinctHumanApprovers); err != nil {
			return false, fmt.Errorf("withdrawal: count approvals: %w", err)
		}
		ready = distinctHumanApprovers >= policy.RequiredApprovals
	}

	if !ready {
		return false, nil
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, updated_at = now() WHERE id = $2 AND state = $3`,
		StateApproved, requestID, StatePendingReview,
	)
	if err != nil {
		return false, fmt.Errorf("withdrawal: transition to approved: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, ErrStateConflict
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.approved",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"threshold_amount": policy.ThresholdAmount,
			"request_amount":   wr.Amount,
		},
	}); err != nil {
		return false, fmt.Errorf("withdrawal: audit: %w", err)
	}

	return true, nil
}

func approverActorType(isAutomated bool) audit.ActorType {
	if isAutomated {
		return audit.ActorService
	}
	return audit.ActorStaff
}

// Reject records a 'reject' decision, transitions the request straight to
// `rejected` (a single reviewer's rejection is final - the state diagram
// has no rejected-pending-second-opinion state), and posts Flow 4's
// pre-submission-rejection reversal (debit player_withdrawal_hold, credit
// player_cash), setting release_ledger_transaction_id. All in tx.
//
// approverPrincipalID has the same caller-responsibility contract as
// Approve's (server-derived from the authenticated session, RBAC-checked
// by the caller). The ApprovalPolicy in force is resolved internally
// (ResolveApprovalPolicy, policy.go, using the locked request's own
// tenant/brand/asset - the same reasoning as Approve's) and its
// ThresholdAmount is recorded on the WithdrawalApproval row for the same
// audit-completeness reason Approve's doc explains -
// withdrawal-state-machine.md §7 requires "the threshold and amount in
// force at the time" on every approval decision, including rejections,
// even though a rejection is not itself threshold-gated.
func Reject(ctx context.Context, tx pgx.Tx, requestID, approverPrincipalID uuid.UUID, reasonCode string) error {
	if approverPrincipalID == uuid.Nil {
		return fmt.Errorf("%w: approver principal id is required", ErrInvalidInput)
	}
	if reasonCode == "" {
		return fmt.Errorf("%w: reason code is required to reject a withdrawal", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StatePendingReview {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StatePendingReview)
	}

	policy, err := ResolveApprovalPolicy(ctx, tx, wr.TenantID, wr.BrandID, nil, wr.AssetCode, time.Now())
	if err != nil {
		return err
	}

	approvalID := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawal_approvals
			(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code,
			 threshold_amount_at_decision, request_amount_at_decision)
		 VALUES ($1, $2, $3, $4, false, 'reject', $5, $6, $7)`,
		approvalID, wr.TenantID, requestID, approverPrincipalID, reasonCode, policy.ThresholdAmount, wr.Amount,
	); err != nil {
		if db.IsUniqueViolation(err) {
			return ErrDuplicateApproval
		}
		return fmt.Errorf("withdrawal: insert rejection: %w", err)
	}

	holdAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, &wr.WalletID, ledger.AccountPlayerWithdrawalHold, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get hold account: %w", err)
	}
	cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, &wr.WalletID, ledger.AccountPlayerCash, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get cash account: %w", err)
	}

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:              wr.TenantID,
		TransactionType:       ledger.TxWithdrawalRejected,
		IdempotencyKey:        requestID.String() + ":rejected",
		CorrelationID:         requestID,
		ReversesTransactionID: wr.HoldLedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: holdAccountID, Direction: ledger.Debit, Amount: wr.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: wr.Amount},
		},
	})
	if err != nil {
		return fmt.Errorf("withdrawal: post rejection reversal: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, release_ledger_transaction_id = $2, updated_at = now()
		 WHERE id = $3 AND state = $4`,
		StateRejected, postResult.TransactionID, requestID, StatePendingReview,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: transition to rejected: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}

	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorStaff,
		ActorID:    approverPrincipalID,
		Action:     "withdrawal.rejected",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"reason_code":      reasonCode,
			"threshold_amount": policy.ThresholdAmount,
			"request_amount":   wr.Amount,
		},
	})
}

// LockApprovedForSubmission takes the row lock on requestID and verifies
// it is in state `approved`, returning ErrStateConflict otherwise. It
// must be called - and its resulting lock held open, in the SAME
// transaction - BEFORE the caller ever contacts a PaymentProvider to
// submit the payout, per the Stage 3B security/architecture review's
// finding: without a lock taken first, two concurrent submit attempts
// (a staff double-click, or a client retry racing the original request)
// can both observe `approved` via a plain, non-locking read and both
// call the provider - a real double payout, with only one of the two
// MarkSubmitted calls afterward winning the database's own state
// transition and the other's evidence of ever having called the
// provider lost to its rolled-back transaction.
//
// With this lock taken first instead, the second concurrent caller's
// SELECT ... FOR UPDATE blocks until the first caller's transaction
// commits (state now `submitted`) or rolls back (state still `approved`,
// but the same lock ordering applies to the next contender); either way,
// by the time the second caller re-reads the row under its own lock, at
// most one of the two attempts observes `approved`, so the provider is
// called at most once per request - exactly mirroring how the deposit
// orchestrator's own idempotency insert (IdempotentInsert, executed
// before InitiateDeposit ever calls provider.Deposit) prevents the same
// class of race on the deposit side.
func LockApprovedForSubmission(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (WithdrawalRequest, error) {
	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return WithdrawalRequest{}, err
	}
	if wr.State != StateApproved {
		return WithdrawalRequest{}, fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateApproved)
	}
	return wr, nil
}

// LockSubmittedForResolution takes the row lock on requestID and
// verifies it is in state `submitted`, returning ErrStateConflict
// otherwise. Stage 3C hardening: a `submitted` withdrawal has no
// automated path forward without this - the mock adapter's default
// behavior (and, for a real PSP, an entirely ordinary async settlement
// window) is to return OutcomePending from Withdraw, and Stage 3B never
// implemented a withdrawal callback path, so without an explicit
// resolution mechanism a submitted request could remain financially
// stranded (money held, no ledger release) indefinitely with no
// observable recovery path. The caller (internal/httpserver's resolve
// handler, or a scheduled sweep) is expected to follow this with a
// PaymentProvider.QueryStatus call and then Complete or Fail based on
// the result - NEVER a second Withdraw call (that would be resubmission,
// which risks a duplicate payout) and NEVER routing to a different
// provider (the request is already bound to the provider/reference
// MarkSubmitted recorded - see withdrawal-state-machine.md's own
// "never automatically resubmit on timeout/ambiguity" rule, carried over
// from the deposit orchestrator's identical rule).
func LockSubmittedForResolution(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (WithdrawalRequest, error) {
	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return WithdrawalRequest{}, err
	}
	if wr.State != StateSubmitted {
		return WithdrawalRequest{}, fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateSubmitted)
	}
	return wr, nil
}

// ListSubmittedForTenant returns every WithdrawalRequest currently in
// `submitted` state, tenant-wide - the staff review-queue equivalent for
// stranded-hold recovery (see newListSubmittedWithdrawalsHandler) and
// the input to a scheduled resolution sweep. Intended to run under
// db.Pool.WithTenant, same as every other staff/system withdrawal query.
func ListSubmittedForTenant(ctx context.Context, tx pgx.Tx) ([]WithdrawalRequest, error) {
	rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM withdrawal_requests WHERE state = $1 ORDER BY requested_at ASC`, StateSubmitted)
	if err != nil {
		return nil, fmt.Errorf("withdrawal: list submitted: %w", err)
	}
	defer rows.Close()

	var out []WithdrawalRequest
	for rows.Next() {
		wr, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wr)
	}
	return out, rows.Err()
}

// MarkSubmitted transitions a request from `approved` to `submitted` once
// the orchestrator has sent the payout instruction to the PSP/custodian
// adapter (payment-orchestration.md). No ledger effect yet - submission
// is not confirmation (withdrawal-state-machine.md §1).
func MarkSubmitted(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, providerID, providerReference string) error {
	if providerID == "" || providerReference == "" {
		return fmt.Errorf("%w: provider id and provider reference are required", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StateApproved {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateApproved)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, provider_id = $2, provider_reference = $3, updated_at = now()
		 WHERE id = $4 AND state = $5`,
		StateSubmitted, providerID, providerReference, requestID, StateApproved,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: mark submitted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}

	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.sent",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id":        providerID,
			"provider_reference": providerReference,
		},
	})
}

// Complete posts Flow 3 Step B (debit player_withdrawal_hold, credit
// psp_clearing) and transitions `submitted` -> `completed`, setting
// release_ledger_transaction_id. providerID/providerTxID are the PSP/
// custodian's own send-confirmation reference, which is Step B's ledger
// idempotency key (financial-transaction-flows.md Flow 3) - distinct from
// MarkSubmitted's providerReference (the outbound instruction reference).
func Complete(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, providerID, providerTxID string) error {
	if providerID == "" || providerTxID == "" {
		return fmt.Errorf("%w: provider id and provider tx id are required", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StateSubmitted {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateSubmitted)
	}

	holdAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, &wr.WalletID, ledger.AccountPlayerWithdrawalHold, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get hold account: %w", err)
	}
	clearingAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, nil, ledger.AccountPSPClearing, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get psp clearing account: %w", err)
	}

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:        wr.TenantID,
		TransactionType: ledger.TxWithdrawalCompleted,
		// Namespaced by providerID: the PaymentProvider contract never
		// guarantees provider_tx_id uniqueness ACROSS providers (only
		// within one adapter's own reference space), and the DB-enforced
		// (tenant_id, provider_id, provider_tx_id) unique index already
		// scopes per-provider - this keeps the idempotency_key check
		// agreeing with it instead of colliding across two providers'
		// otherwise-independent reference spaces.
		IdempotencyKey: providerID + ":" + providerTxID,
		ProviderID:     &providerID,
		ProviderTxID:   &providerTxID,
		CorrelationID:  requestID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: holdAccountID, Direction: ledger.Debit, Amount: wr.Amount},
			{LedgerAccountID: clearingAccountID, Direction: ledger.Credit, Amount: wr.Amount},
		},
	})
	if err != nil {
		return fmt.Errorf("withdrawal: post completion: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, release_ledger_transaction_id = $2, updated_at = now()
		 WHERE id = $3 AND state = $4`,
		StateCompleted, postResult.TransactionID, requestID, StateSubmitted,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: transition to completed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}

	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.completed",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id":    providerID,
			"provider_tx_id": providerTxID,
			"amount":         wr.Amount,
		},
	})
}

// Fail posts Flow 4's post-submission-failure variant and transitions
// `submitted` -> `failed`, setting release_ledger_transaction_id.
//
// The task that produced this package describes the post-submission
// failure case as needing to reverse Step B first (if it posted) and then
// Step A, since reverses_transaction_id is a single FK
// (ledger-accounting-model.md §1.2). This function always reverses Step A
// ONLY: per the state diagram (withdrawal-state-machine.md §1), `failed`
// is reachable only from `submitted`, and `completed` (which is where
// Step B posts) is a separate, terminal transition out of `submitted` -
// the two are mutually exclusive. A request that already completed can
// only reach `reversed` (a distinct, rare, not-yet-implemented
// transition - see this package's doc/report), never `failed`. There is
// therefore no reachable state in which Fail must reverse a Step B that
// was never posted in the first place; documented here rather than adding
// dead code for an unreachable precondition.
func Fail(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, reasonCode string) error {
	if reasonCode == "" {
		return fmt.Errorf("%w: reason code is required to fail a withdrawal", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StateSubmitted {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateSubmitted)
	}

	holdAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, &wr.WalletID, ledger.AccountPlayerWithdrawalHold, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get hold account: %w", err)
	}
	cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, &wr.WalletID, ledger.AccountPlayerCash, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get cash account: %w", err)
	}

	// NOTE: reason_code is deliberately NOT set on the ledger transaction
	// itself - ledger_transactions' CHECK constraint (migration 0021)
	// requires reason_code IS NOT NULL if-and-only-if transaction_type =
	// 'manual_adjustment', so setting it here for a withdrawal_failed
	// transaction would violate that constraint. reasonCode is recorded
	// on the audit entry instead, which is where CLAUDE.md's "every
	// mutating administrative/financial action writes an audit record...
	// reason code" requirement is satisfied for this transition.
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:              wr.TenantID,
		TransactionType:       ledger.TxWithdrawalFailed,
		IdempotencyKey:        requestID.String() + ":failed",
		CorrelationID:         requestID,
		ReversesTransactionID: wr.HoldLedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: holdAccountID, Direction: ledger.Debit, Amount: wr.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: wr.Amount},
		},
	})
	if err != nil {
		return fmt.Errorf("withdrawal: post failure reversal: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, release_ledger_transaction_id = $2, updated_at = now()
		 WHERE id = $3 AND state = $4`,
		StateFailed, postResult.TransactionID, requestID, StateSubmitted,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: transition to failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}

	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.failed",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"reason_code": reasonCode,
			"amount":      wr.Amount,
		},
	})
}

// Cancel transitions a request from `requested` to `cancelled` - only
// valid before pending_review has started (withdrawal-state-machine.md
// §1's explicit "once pending_review has started, the request is not
// player-cancellable" architectural decision, avoiding a race between a
// player cancelling and a reviewer approving in the same window). It
// releases the hold exactly like Reject does (Flow 4's pre-submission
// variant), because the hold posts at `requested`, before cancellation is
// even reachable - a cancelled request still needs its hold released.
//
// This function's ledger posting uses ledger.TxWithdrawalRejected, not a
// distinct "cancelled" transaction type: internal/ledger's TransactionType
// enum (which this package must not modify) has no separate
// withdrawal-cancellation constant, and withdrawal-state-machine.md §1
// itself describes cancellation's ledger effect "as for a pre-submission
// rejection (Flow 4)" - the ledger records the FINANCIAL fact (hold
// released back to cash before submission), while WithdrawalRequest.state
// (`cancelled` vs `rejected`) is what actually distinguishes the two
// workflow outcomes for reporting/audit purposes.
func Cancel(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) error {
	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StateRequested {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateRequested)
	}

	holdAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, &wr.WalletID, ledger.AccountPlayerWithdrawalHold, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get hold account: %w", err)
	}
	cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, wr.TenantID, &wr.WalletID, ledger.AccountPlayerCash, wr.AssetCode)
	if err != nil {
		return fmt.Errorf("withdrawal: get cash account: %w", err)
	}

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:              wr.TenantID,
		TransactionType:       ledger.TxWithdrawalRejected,
		IdempotencyKey:        requestID.String() + ":cancelled",
		CorrelationID:         requestID,
		ReversesTransactionID: wr.HoldLedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: holdAccountID, Direction: ledger.Debit, Amount: wr.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: wr.Amount},
		},
	})
	if err != nil {
		return fmt.Errorf("withdrawal: post cancellation reversal: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, release_ledger_transaction_id = $2, updated_at = now()
		 WHERE id = $3 AND state = $4`,
		StateCancelled, postResult.TransactionID, requestID, StateRequested,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: transition to cancelled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}

	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorPlayer,
		ActorID:    wr.PlayerAccountID,
		Action:     "withdrawal.cancelled",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"amount": wr.Amount,
		},
	})
}
