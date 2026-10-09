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
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/tenant"
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
// specific HTTP statuses (see internal/httpserver/withdrawal_handlers.go's
// error-mapping switch on each handler for the exact, current mapping -
// e.g. ErrStateConflict/ErrDuplicateApproval/ErrInsufficientFunds -> 409,
// ErrSelfApproval/ErrStepUpRequired -> 403, ErrNotFound -> 404).
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
	// ApprovalPolicy.RequireStepUp (policy.go) is true for a decision at
	// or above threshold and no step-up/MFA challenge exists yet (ADR
	// 0017 preserved: MFA itself is not implemented here). Approve's own
	// doc comment below has the full Stage 3C directive item 6
	// rationale and the exact point a real MFA integration should hook
	// in.
	ErrStepUpRequired = errors.New("withdrawal: this decision requires a step-up/MFA challenge, which is not yet implemented (ADR 0017)")

	// ErrApproverNotLinked is returned by Approve/Reject when the
	// caller-supplied ApproverEligibility reports that approverPrincipalID
	// is not linked to a Person - including when the identity cannot be
	// resolved at all (no matching staff account). "Linked" means a
	// confirmed staff_users.person_id association (established by an
	// admin action), not KYC/identity verification of the Person itself -
	// see migration 0034's own doc comment. Stage 3D business policy: an
	// unlinked staff identity is never eligible to record a human
	// withdrawal decision, full stop - not merely "less trusted." See
	// ApproverEligibility's doc comment.
	ErrApproverNotLinked = errors.New("withdrawal: approver has no confirmed Person linkage and is not eligible to record withdrawal decisions")

	// ErrApproverInactive is returned by Approve/Reject when the
	// caller-supplied ApproverEligibility reports the approver's staff
	// account is not active (e.g. suspended).
	ErrApproverInactive = errors.New("withdrawal: approver staff account is not active")

	// ErrIdempotencyKeyReused is returned when a retried RequestWithdrawal
	// call's idempotency_key matches an existing request whose wallet,
	// asset, or amount differs from the one now requested - the
	// workflow-layer equivalent of ledger.ErrIdempotencyKeyReused.
	ErrIdempotencyKeyReused = errors.New("withdrawal: idempotency key reused with a different wallet/asset/amount/payout instrument")

	// ErrPayoutInstrumentRequired: a new withdrawal request named no payout
	// instrument (B13-B, ADR 0111 2.4: binding happens at request creation).
	// No row and no hold are written and the idempotency key is not consumed.
	ErrPayoutInstrumentRequired = errors.New("withdrawal: a payout instrument is required")
	// ErrPayoutInstrumentNotUsable: the named instrument failed the ADR 0111
	// 2.2 gate rule (wrong player/brand/tenant, unverified, expired, blocked,
	// asset not listed, integrity failure). Wraps the *payoutinstrument.GateRefusal
	// (closed reason, never a value). No row, no hold, key not consumed.
	ErrPayoutInstrumentNotUsable = errors.New("withdrawal: payout instrument is not usable")
	// ErrDestinationGateUnavailable: the caller supplied no destination gate. A
	// new withdrawal can never be created without one (fail closed).
	ErrDestinationGateUnavailable = errors.New("withdrawal: destination gate is not configured")
)

// DestinationGate is the payout-instrument gate RequestWithdrawal applies
// (satisfied by *payoutinstrument.Service). Declared here so the withdrawal
// package depends on the gate's behaviour, not on how it is built.
type DestinationGate interface {
	EvaluateGate(ctx context.Context, tx pgx.Tx, p payoutinstrument.GateParams) (payoutinstrument.GateResult, error)
}

// KYCDeniedError is returned by RequestWithdrawal when ADR 0096's
// structural withdrawal rule (§3.2 point 1) denies the request - no
// withdrawal_requests row is inserted and no ledger effect is posted.
//
// LF-I3-3 (ledger-finance's preferred fix, 2026-09-27, superseding the
// original roll-back-then-fresh-transaction design): the decision row and
// the audit record are ALREADY WRITTEN, in the SAME transaction, by the
// time RequestWithdrawal returns this error - see the call site just
// above. The CALLER MUST NOT let this error propagate as the return value
// of a db.Pool.WithTenant/WithPlayerScope closure, because that would
// roll back the transaction and discard those very rows. The sanctioned
// pattern (internal/httpserver/withdrawal_handlers.go's own
// newRequestWithdrawalHandler): detect *KYCDeniedError via errors.As
// INSIDE the closure, capture it in an outer variable, and return nil so
// the transaction commits exactly "decision + audit, nothing else"; after
// the WithTenant call returns (with a nil error), check the captured
// variable and map it to the player-facing response.
type KYCDeniedError struct {
	Params   kyc.EnforcementParams
	Decision kyc.EnforcementDecision
}

func (e *KYCDeniedError) Error() string {
	return fmt.Sprintf("withdrawal: kyc enforcement denied (%s)", e.Decision.Code)
}

// ErrKYCUnavailable signals that the KYC enforcement gate could not be
// evaluated/acted on as a real compliance decision - fail-closed, no
// domain effect, and RETRYABLE by the caller (never a terminal outcome).
// Two call sites return it, both post-KYC-ENF-OUTAGE-1:
//   - RequestWithdrawal, when kyc.EvaluateEnforcement itself returns a
//     non-nil Go error. Since KYC-ENF-OUTAGE-1 (2026-09-28), that only
//     happens for a STRUCTURALLY INVALID call (a missing tenant/brand/
//     player/person id) - a caller bug, not a runtime DB outage: every
//     genuine DB-read failure inside EvaluateEnforcement is now contained
//     by a savepoint and surfaces as an ordinary
//     EnforcementDecision{Outcome: OutcomeUnavailable} with a NIL error
//     (see enforcement.go's own doc comment), which RequestWithdrawal
//     handles via its normal *KYCDeniedError/decision-row path, not this
//     branch. The withdrawal HTTP handler maps THIS branch to a
//     non-retryable 500 (a caller bug is never worth retrying as-is), and
//     the OutcomeUnavailable branch above it to a retryable 503 -
//     see withdrawal_handlers.go:138-164.
//   - DenyForCompliance (N5, code review rv-prh-i3-code-review.md), when a
//     caller passes a decision whose Outcome is OutcomeUnavailable. An
//     `unavailable` outcome means the evaluator could not determine
//     whether the player is actually KYC-cleared - it is NEVER evidence
//     of a compliance failure, so DenyForCompliance must never convert it
//     into a terminal `rejected` withdrawal (that would strand a
//     compliant player's funds behind a mere outage). Its callers are NOT
//     "future": payments.ClaimForDispatch (ADR 0095 T1p) and the sweeper's
//     T2/T12 re-checks already exist, and since PRH-2 F-pay
//     (PAY-KYC-UNAVAIL-1) they handle an `unavailable` outcome BEFORE
//     reaching DenyForCompliance - ClaimForDispatch commits the decision
//     and the denied-submit audit and returns a retryable
//     payments.ErrPayoutKYCUnavailable (503), and the sweeper reschedules
//     the attempt. This refusal is the defence-in-depth backstop should a
//     caller ever pass an `unavailable` decision anyway.
var ErrKYCUnavailable = errors.New("withdrawal: kyc enforcement evaluation unavailable")

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
	// PayoutInstrumentID / PayoutInstrumentFingerprint are the B13 binding (migration
	// 0123): both set for every request created since B13-B, both NULL only for a legacy
	// row. The fingerprint is internal and is never serialised by any API type.
	PayoutInstrumentID          *uuid.UUID
	PayoutInstrumentFingerprint *string
}

// Bound reports whether the request carries a payout instrument binding.
func (wr WithdrawalRequest) Bound() bool { return wr.PayoutInstrumentID != nil }

const requestColumns = `id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state,
	idempotency_key, provider_id, provider_reference, hold_ledger_transaction_id, release_ledger_transaction_id,
	requested_at, updated_at, payout_instrument_id, payout_instrument_fingerprint`

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
		&wr.RequestedAt, &wr.UpdatedAt, &wr.PayoutInstrumentID, &wr.PayoutInstrumentFingerprint,
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

// LockForPayoutEvidence takes the row lock on requestID (SELECT ... FOR
// UPDATE) with NO state precondition. R3 (RV-PRH-I1 ledger re-review):
// ADR 0082 A7 fixes ONE lock order for the withdrawal/attempt pair -
// withdrawal FIRST, then the attempt CAS, then any ledger-affecting
// posting - matching the T2/T12 claim statements
// (withdrawal.LockSubmittedForResolution) and the receipt path. Phase C
// (internal/payments) must take this lock as its very first statement,
// even when the row has already left `submitted` (a legitimate race
// against an earlier, faster piece of evidence): the state check still
// happens downstream, in the attempt CAS and in Complete/Fail/
// AttachProviderReference's own preconditions, exactly as before this
// lock was added - this function only fixes WHEN the lock is taken, not
// what is allowed once it is held. Exported for internal/payments only;
// every other caller with a state expectation should use
// LockApprovedForSubmission or LockSubmittedForResolution instead, which
// both call this same underlying lock.
func LockForPayoutEvidence(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (WithdrawalRequest, error) {
	return lockRequestForUpdate(ctx, tx, requestID)
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
	// PersonID is required (ADR 0096 §3.2 point 1 / §5), resolved
	// server-side exactly like every other identity field here - never
	// accepted from a client. The primary KYC read key is still this
	// wallet's own PlayerAccountID (security re-verification N1);
	// PersonID is used only as an additional, deny-only cross-account
	// overlay so a rejection recorded against a DIFFERENT PlayerAccount
	// of the same Person cannot be sidestepped by withdrawing from this
	// one - see kyc.EnforcementParams.PersonID's own doc comment for the
	// full reasoning.
	PersonID  uuid.UUID
	WalletID  uuid.UUID
	AssetCode string
	// Amount is strictly positive minor units.
	Amount int64
	// IdempotencyKey is the client/session-supplied value deduplicating a
	// double-submit, namespaced (tenant_id, player_account_id,
	// idempotency_key) at the database level (migration 0026) - never
	// inserted into a global or ledger-level uniqueness namespace
	// directly (withdrawal-state-machine.md §4).
	IdempotencyKey string
	// PayoutInstrumentID is the player's chosen payout instrument (B13-B, ADR 0111
	// 2.4). REQUIRED for a new request; the instrument is the ONLY source of the
	// destination and of the payout rail. Resolved server-side by the caller from
	// the authenticated player's own instruments; the gate re-checks ownership.
	PayoutInstrumentID uuid.UUID
	// Destinations applies the gate rule. REQUIRED (nil refuses: fail closed).
	Destinations DestinationGate
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
// balance is locked and compared against Amount INSIDE this same
// transaction, immediately before posting the debit - two concurrent
// requests against a wallet with only enough balance for one of them
// will not both observe "sufficient".
//
// That lock is now taken by internal/ledger, over ALL of this posting's
// wallet_balance_projection rows at once (player_cash AND
// player_withdrawal_hold) in canonical ascending ledger_account_id order
// - ledger.LockProjectionsForPosting, ADR 0082 R1/R4. The package-local
// lockCashBalanceForUpdate helper that used to lock player_cash alone is
// DELETED, not wrapped: it locked a SUBSET of the accounts this posting
// touches, which is finding LOCK-1c (RequestWithdrawal took
// (player_cash, player_withdrawal_hold) while Reject/Fail/Cancel took the
// same pair in the opposite order, and the withdrawal_requests row lock
// does NOT serialize them because a second request is a DIFFERENT request
// row on the same wallet).
func RequestWithdrawal(ctx context.Context, tx pgx.Tx, params RequestParams) (WithdrawalRequest, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.WalletID == uuid.Nil {
		return WithdrawalRequest{}, fmt.Errorf("%w: tenant/brand/player/wallet ids are required", ErrInvalidInput)
	}
	if params.PersonID == uuid.Nil {
		return WithdrawalRequest{}, fmt.Errorf("%w: person id is required", ErrInvalidInput)
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

	// ADR 0096 §8 item 3 / ledger-finance N2: a REPLAY of an existing
	// idempotency key is checked FIRST, read-only, before the KYC gate
	// ever runs - a retried call after a successful hold must not
	// re-evaluate KYC (a status change between the original request and
	// its replay must never turn an already-placed hold into a denial).
	if existing, lookupErr := getByTenantPlayerIdempotencyKey(ctx, tx, params.TenantID, params.PlayerAccountID, params.IdempotencyKey); lookupErr == nil {
		if !sameRequest(existing, params) {
			return WithdrawalRequest{}, fmt.Errorf("%w: existing request %s", ErrIdempotencyKeyReused, existing.ID)
		}
		return existing, nil
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: look up existing request for idempotency key: %w", lookupErr)
	}

	// H-SEC-11: a NEW withdrawal request (and its hold) for a tenant or brand
	// that is not 'active' is refused in THIS tx, AFTER the read-only replay
	// lookup above (a replay of an already-placed request returns the original
	// even on a non-active tenant: ADR 0096 s8 item 3 / LF N2) and BEFORE the
	// KYC gate, the insert and the hold. Nothing is written, so the
	// idempotency key is not consumed (it lives only on the request row).
	if err := tenant.RequireActiveForPaymentInitiation(ctx, tx, params.TenantID, params.BrandID); err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: request refused: %w", err)
	}

	// B13-B (ADR 0111 2.4, owner decisions 2 and 4): BINDING happens here. After the
	// H-SEC-11 gate and BEFORE the KYC evaluation and any write, the named instrument
	// is locked FOR SHARE and the gate rule applies (same tenant/brand/player/person,
	// verified, in-force latest verification, unexpired, no revoke / later suspend,
	// seals and fingerprint intact, asset listed). A refusal returns an error with NO
	// row, NO hold and NO decision row: the caller rolls back and the idempotency key
	// is not consumed. No adapter is involved yet, so the tiering predicate is not
	// applied here (it runs at T1p, phase B and T2/T12).
	if params.PayoutInstrumentID == uuid.Nil {
		return WithdrawalRequest{}, ErrPayoutInstrumentRequired
	}
	if params.Destinations == nil {
		return WithdrawalRequest{}, ErrDestinationGateUnavailable
	}
	gate, gerr := params.Destinations.EvaluateGate(ctx, tx, payoutinstrument.GateParams{
		TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID, PersonID: params.PersonID,
		InstrumentID: params.PayoutInstrumentID, AssetCode: params.AssetCode, Lock: true,
	})
	if gerr != nil {
		if _, ok := payoutinstrument.IsGateRefusal(gerr); ok {
			return WithdrawalRequest{}, fmt.Errorf("%w: %w", ErrPayoutInstrumentNotUsable, gerr)
		}
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: evaluate payout instrument gate: %w", gerr)
	}

	// ADR 0096 §5 exact placement: immediately after the idempotency-
	// replay lookup, before the first statement that could need undoing
	// (IdempotentInsert). correlationID is generated up front so the
	// decision row (written on deny, in THIS SAME transaction - LF-I3-3,
	// §17.1: the earlier "caller-opened fresh transaction" design was
	// replaced before this comment was last accurate, see KYCDeniedError's
	// own doc) and, on allow, the request itself (written below) share one
	// correlation id.
	correlationID := uuid.New()
	kycParams := kyc.EnforcementParams{
		TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
		PersonID: params.PersonID, Operation: kyc.EnforcementWithdrawalHold,
		AssetCode: params.AssetCode, Amount: params.Amount, CorrelationID: correlationID,
	}
	decision, err := kyc.EvaluateEnforcement(ctx, tx, kycParams)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("%w: %w", ErrKYCUnavailable, err)
	}
	if !decision.Allowed {
		// LF-I3-3 (ledger-finance's preferred fix, superseding the earlier
		// roll-back-then-fresh-transaction pattern): the decision + audit
		// rows are written HERE, in THIS transaction, before returning the
		// typed KYCDeniedError. The caller MUST NOT propagate this error
		// as its own return from a db.Pool.WithTenant closure (that would
		// roll back and discard these very rows) - it must detect
		// *KYCDeniedError via errors.As, capture it, and return nil so the
		// transaction commits exactly what was written here: the decision
		// row, the audit record, and NOTHING else (no withdrawal_requests
		// row, no ledger posting) - see internal/httpserver/
		// withdrawal_handlers.go's own handling for the sanctioned
		// pattern.
		if err := kyc.RecordDecision(ctx, tx, kycParams, decision); err != nil {
			return WithdrawalRequest{}, err
		}
		return WithdrawalRequest{}, &KYCDeniedError{Params: kycParams, Decision: decision}
	}

	requestID := uuid.New()
	conflict, _, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
		_, err := spTx.Exec(ctx,
			`INSERT INTO withdrawal_requests
				(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key,
				 payout_instrument_id, payout_instrument_fingerprint)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			requestID, params.TenantID, params.BrandID, params.PlayerAccountID, params.WalletID, params.AssetCode,
			params.Amount, params.IdempotencyKey, gate.Instrument.ID, gate.Instrument.Fingerprint,
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
		if !sameRequest(existing, params) {
			return WithdrawalRequest{}, fmt.Errorf("%w: existing request %s", ErrIdempotencyKeyReused, existing.ID)
		}
		return existing, nil
	}

	// ADR 0082 §3.2/§4.6: load-bearing here, not cosmetic. This function
	// resolved (cash, hold) while Reject/Complete/Fail/Cancel resolve
	// (hold, cash), so two of them creating both accounts for the first
	// time could block on each other's uncommitted ledger_accounts index
	// entries in opposite orders. GetOrCreateAccounts creates in canonical
	// (wallet, account_type, asset) order regardless of argument order.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, params.TenantID,
		ledger.AccountSpec{WalletID: &params.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: params.AssetCode},
		ledger.AccountSpec{WalletID: &params.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: params.AssetCode},
	)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: resolve hold ledger accounts: %w", err)
	}
	cashAccountID, holdAccountID := accounts[0], accounts[1]

	// The COMPLETE, final posting, built before the sufficiency check so
	// the pre-lock below covers every account it touches. The very same
	// value is handed to ledger.Post; it is never rebuilt (ADR 0082 R3).
	holdInput := ledger.TransactionInput{
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
	}

	locked, err := ledger.LockProjectionsForPosting(ctx, tx, holdInput)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: lock hold projections: %w", err)
	}
	cashBalance, err := locked.Balance(cashAccountID)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: read locked player_cash balance: %w", err)
	}
	// player_cash is credit-normal (ledger-accounting-model.md §5).
	if cashBalance.Signed() < params.Amount {
		return WithdrawalRequest{}, ErrInsufficientFunds
	}

	postResult, err := ledger.Post(ctx, tx, holdInput)
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
			// B13-B: the instrument id only. Never the fingerprint, mask or detail.
			"payout_instrument_id": gate.Instrument.ID.String(),
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

	// ADR 0096 §3.6: on allow, the kyc_enforcement_decisions row commits
	// WITH the domain effect - same transaction as the hold just posted.
	if err := kyc.RecordDecision(ctx, tx, kycParams, decision); err != nil {
		return WithdrawalRequest{}, err
	}

	return GetByID(ctx, tx, requestID)
}

// sameRequest reports whether an idempotency replay names the same wallet,
// asset, amount AND payout instrument as the stored request (B13-B, ADR 0111
// 2.4: a replay with a different instrument is ErrIdempotencyKeyReused). A legacy
// request (NULL binding) replays only with no instrument id.
func sameRequest(existing WithdrawalRequest, p RequestParams) bool {
	if existing.WalletID != p.WalletID || existing.AssetCode != p.AssetCode || existing.Amount != p.Amount {
		return false
	}
	if existing.PayoutInstrumentID == nil {
		return p.PayoutInstrumentID == uuid.Nil
	}
	return *existing.PayoutInstrumentID == p.PayoutInstrumentID
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

// ApproverEligibility reports whether approverPrincipalID is currently
// eligible to record a HUMAN withdrawal decision: linked to a verified
// Person (linked) and an active staff account (active) - Stage 3D's
// business policy (docs/decisions/0024 §1: "any staff identity that can
// approve, reject, or submit withdrawals MUST be linked to a Person
// identity"). Like BeneficiaryCheck, this package has, and should have,
// no direct dependency on staff_users (that lives in internal/identity),
// so the caller supplies this closure - but unlike BeneficiaryCheck, a
// nil ApproverEligibility on a non-automated call is itself a fail-closed
// ERROR here (ErrInvalidInput), never a silent skip: this check exists
// specifically because Stage 3C's optional, unenforced Person linkage
// was found insufficient, so the package that owns the invariant must
// not let a caller accidentally omit it. The database-level backstop
// (migration 0034's withdrawal_approvals_enforce_governance trigger)
// independently re-verifies the SAME facts directly from staff_users
// before the row can ever be inserted, regardless of what this closure
// reports - so a caller that gets this wrong (or a future direct-SQL
// caller that bypasses this package entirely) still cannot commit a
// prohibited decision.
type ApproverEligibility func(approverPrincipalID uuid.UUID) (linked, active bool, err error)

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
// approverEligibility implements Stage 3D's mandatory-Person-linkage
// policy - see ApproverEligibility's doc. Required (non-nil) whenever
// isAutomated is false; a nil value there is itself a fail-closed
// ErrInvalidInput, not a skip.
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
	isAutomated bool, beneficiaryCheck BeneficiaryCheck, approverEligibility ApproverEligibility,
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

	if !isAutomated {
		if approverEligibility == nil {
			return false, fmt.Errorf("%w: approver eligibility check is required for a human decision", ErrInvalidInput)
		}
		linked, active, err := approverEligibility(approverPrincipalID)
		if err != nil {
			return false, fmt.Errorf("withdrawal: approver eligibility check: %w", err)
		}
		if !linked {
			return false, ErrApproverNotLinked
		}
		if !active {
			return false, ErrApproverInactive
		}
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
		// Stage 3C specialist review (code-reviewer P1): counting distinct
		// approver_principal_id alone lets ONE human, holding two staff
		// logins linked to the same person, supply both of the required
		// approvals for someone ELSE's withdrawal - the self-approval
		// guard (migration 0029/0033) never fires here since neither
		// login is the withdrawing player's own beneficiary. Migration
		// 0029 already added staff_users.person_id for exactly this class
		// of identity - counting COALESCE(su.person_id,
		// wa.approver_principal_id) collapses two logins resolving to the
		// same person into one. The COALESCE's unlinked-fallback branch
		// (falling back to approver_principal_id) is now unreachable for
		// any row this query counts: Stage 3D's governance trigger
		// (migration 0034) refuses to INSERT a non-automated approval
		// from an unlinked staff account in the first place, so every
		// human approval counted here already has a non-NULL
		// su.person_id. Kept as written (not simplified to a bare
		// su.person_id) as defense-in-depth - correct even if that
		// trigger is ever bypassed - and to avoid every automated
		// decision's join failing to match staff_users, which the
		// is_automated_approval = false filter above excludes anyway.
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(DISTINCT COALESCE(su.person_id, wa.approver_principal_id))
			 FROM withdrawal_approvals wa
			 LEFT JOIN staff_users su ON su.id = wa.approver_principal_id
			 WHERE wa.withdrawal_request_id = $1 AND wa.decision = 'approve' AND wa.is_automated_approval = false`,
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
//
// approverEligibility is Stage 3D's mandatory-Person-linkage check (see
// ApproverEligibility's doc) - a rejection is always a human decision
// (there is no automated-rejection path), so this is unconditionally
// required, never nil.
func Reject(ctx context.Context, tx pgx.Tx, requestID, approverPrincipalID uuid.UUID, reasonCode string, approverEligibility ApproverEligibility) error {
	if approverPrincipalID == uuid.Nil {
		return fmt.Errorf("%w: approver principal id is required", ErrInvalidInput)
	}
	if reasonCode == "" {
		return fmt.Errorf("%w: reason code is required to reject a withdrawal", ErrInvalidInput)
	}
	if approverEligibility == nil {
		return fmt.Errorf("%w: approver eligibility check is required", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StatePendingReview {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StatePendingReview)
	}

	linked, active, err := approverEligibility(approverPrincipalID)
	if err != nil {
		return fmt.Errorf("withdrawal: approver eligibility check: %w", err)
	}
	if !linked {
		return ErrApproverNotLinked
	}
	if !active {
		return ErrApproverInactive
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

	// ADR 0082 §4.6: no LOCKING change is needed here - finding LOCK-1c
	// is closed by ledger.Post's own internal L3 pre-lock, and this
	// function's lockRequestForUpdate call is class L1, which already
	// precedes L3. The account resolution DOES change, and is
	// load-bearing: this function resolves (hold, cash) while
	// RequestWithdrawal resolves (cash, hold), so first-time creation of
	// both accounts could otherwise block on each other's uncommitted
	// ledger_accounts index entries in opposite orders.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode},
	)
	if err != nil {
		return fmt.Errorf("withdrawal: resolve hold/cash ledger accounts: %w", err)
	}
	holdAccountID, cashAccountID := accounts[0], accounts[1]

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
// before InitiateDepositAttempt ever calls provider.Deposit - [deleted
// by E2] this used to be InitiateDeposit's own idempotency insert, now
// deleted) prevents the same class of race on the deposit side.
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

// DenyForCompliance implements ADR 0096 §8 item 3 / §12.2 C2 (ledger-
// finance): the KYC payout-dispatch gate's DENY path. It is legal ONLY
// from `approved` (pre-dispatch) - the caller MUST call this inside the
// SAME transaction LockApprovedForSubmission already holds the L1 row
// lock in, BEFORE ever contacting a PaymentProvider, and MUST NOT call
// MarkSubmitted/RouteProvider/Withdraw if this returns nil (the request
// is now `rejected`, terminal for this attempt).
//
// Unlike RequestWithdrawal's deny (ADR 0096 §3.6 "on deny: zero domain
// effect"), THIS enforcement point's denial is a CARVE-OUT (ledger-
// finance re-verification N1, 2026-09-27): a committed deny here
// necessarily INCLUDES the withdrawal_rejected reversal posting and the
// approved->rejected state transition - there is no "decision + audit
// only, nothing else" version of releasing an already-placed hold, since
// leaving the hold in place would strand the player's funds with no
// release path. The whole thing (reversal, transition, decision, audit)
// commits together in ONE transaction, exactly like Reject/Fail already
// do - this is not a rollback-then-retry-in-a-fresh-transaction case.
//
// Posting mirrors Reject/Fail exactly: accounts resolved (hold, cash);
// entries debit player_withdrawal_hold, credit player_cash for
// wr.Amount; TransactionType = withdrawal_rejected;
// ReversesTransactionID = wr.HoldLedgerTransactionID; CorrelationID =
// requestID. Idempotency key requestID+":kyc_denied", distinct from
// ":rejected"/":failed". Exactly-once release via the L1 lock the caller
// already holds PLUS the conditional
// `UPDATE ... WHERE state = 'approved'` below (ledger-finance
// re-verification N3: release_ledger_transaction_id is set in that SAME
// conditional UPDATE, exactly like every other release path -
// RowsAffected()==0 => ErrStateConflict, which rolls back the posting).
//
// No reason_code on the ledger transaction (migration 0021's CHECK
// allows one only for manual_adjustment) - the compliance reason lives on
// the audit entry only. No withdrawal_approvals row (this is not a
// four-eyes decision).
//
// ADR 0095 coordination (§5/§12.2 C5): once ADR 0095 lands, this function
// must be called from the same claim transaction that performs
// approved -> the first dispatch state (T1p), and from the SAME claim
// transaction a sweeper uses to re-claim a "prepared, not yet sent"
// payout for its first send (ADR 0095's M3 re-claim path) - never once a
// request has possibly reached the provider. This function itself has no
// dependency on ADR 0095 and requires only that its caller hold the L1
// lock LockApprovedForSubmission already takes.
func DenyForCompliance(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, decision kyc.EnforcementDecision, kycParams kyc.EnforcementParams) (WithdrawalRequest, error) {
	// B6 (code review rv-prh-i3-code-review.md): this function trusts
	// nothing about the decision/params a caller hands it beyond what it
	// verifies itself - a caller passing an ALLOWED decision, or a
	// decision for a different operation, must never be able to reverse
	// a legitimate hold and record a self-contradictory audit trail
	// (an "allowed" decision under the action `withdrawal.rejected_kyc`).
	if decision.Allowed {
		return WithdrawalRequest{}, fmt.Errorf("%w: DenyForCompliance requires a DENIED decision (Allowed=false), got Allowed=true", ErrInvalidInput)
	}
	if kycParams.Operation != kyc.EnforcementWithdrawalPayout {
		return WithdrawalRequest{}, fmt.Errorf("%w: DenyForCompliance requires kycParams.Operation == EnforcementWithdrawalPayout, got %q", ErrInvalidInput, kycParams.Operation)
	}
	// N5 (code review rv-prh-i3-code-review.md, KYC-ENF-TESTPINS-1): an
	// `unavailable` outcome means the evaluator could not determine the
	// player's actual KYC state - it is a transient inability to decide,
	// never a compliance denial. Converting it into a terminal `rejected`
	// withdrawal here would strand a possibly-fully-compliant player's
	// funds behind nothing more than a KYC-store outage, with no path
	// back to `approved`. Refuse it outright and return ErrKYCUnavailable
	// (retryable - see its own doc comment) BEFORE touching the ledger,
	// the request row, or the audit log, so the request stays exactly
	// `approved` and a later, healthy re-evaluation can still allow or
	// (only then) genuinely deny the payout.
	if decision.Outcome == kyc.OutcomeUnavailable {
		return WithdrawalRequest{}, fmt.Errorf("%w: DenyForCompliance refuses an unavailable decision (request %s); retry the KYC evaluation, do not deny", ErrKYCUnavailable, requestID)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return WithdrawalRequest{}, err
	}
	if wr.State != StateApproved {
		return WithdrawalRequest{}, fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateApproved)
	}

	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode},
	)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: resolve hold/cash ledger accounts: %w", err)
	}
	holdAccountID, cashAccountID := accounts[0], accounts[1]

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:              wr.TenantID,
		TransactionType:       ledger.TxWithdrawalRejected,
		IdempotencyKey:        requestID.String() + ":kyc_denied",
		CorrelationID:         requestID,
		ReversesTransactionID: wr.HoldLedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: holdAccountID, Direction: ledger.Debit, Amount: wr.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: wr.Amount},
		},
	})
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: post kyc denial reversal: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, release_ledger_transaction_id = $2, updated_at = now()
		 WHERE id = $3 AND state = $4`,
		StateRejected, postResult.TransactionID, requestID, StateApproved,
	)
	if err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: transition to rejected (kyc): %w", err)
	}
	if tag.RowsAffected() == 0 {
		return WithdrawalRequest{}, ErrStateConflict
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.rejected_kyc",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"outcome":                    string(decision.Outcome),
			"policy_version":             decision.PolicyVersion,
			"hold_ledger_transaction":    wr.HoldLedgerTransactionID,
			"release_ledger_transaction": postResult.TransactionID.String(),
		},
	}); err != nil {
		return WithdrawalRequest{}, fmt.Errorf("withdrawal: audit: %w", err)
	}

	if err := kyc.RecordDecision(ctx, tx, kycParams, decision); err != nil {
		return WithdrawalRequest{}, err
	}

	return GetByID(ctx, tx, requestID)
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

// ValidStates lists every value withdrawal_requests.state's CHECK
// constraint (migration 0026) allows, in the order withdrawal-state-
// machine.md §1 documents them - the full operational range a staff
// history view (ListForTenant) needs to filter across, not just the
// pending-review subset the four-eyes queue serves.
var ValidStates = []State{
	StateRequested, StatePendingReview, StateApproved, StateRejected,
	StateSubmitted, StateCompleted, StateFailed, StateCancelled, StateReversed,
}

// IsValidState reports whether s is one of ValidStates. Callers accepting
// a client-supplied status filter (e.g. an HTTP query parameter) should
// validate against this before ever reaching a query, rather than letting
// an arbitrary string reach the database.
func IsValidState(s State) bool {
	for _, v := range ValidStates {
		if v == s {
			return true
		}
	}
	return false
}

// ListForTenant returns a paginated slice of every WithdrawalRequest in
// the caller's tenant scope, most recently requested first, optionally
// filtered to a single State, plus the total matching row count (ignoring
// limit/offset) for pagination. This is the Stage 5 Back Office
// operational-history view: tenant-wide across the FULL state range, as
// distinct from ListSubmittedForTenant's narrower stranded-hold queue and
// the pending-review-only queue newListPendingWithdrawalsHandler serves -
// and, unlike that queue, a pure read with no MoveToPendingReview side
// effect. Intended to run under db.Pool.WithTenant, same as every other
// staff/system withdrawal query - this function applies no authorization
// of its own beyond whatever RLS scope the caller's transaction already
// carries.
func ListForTenant(ctx context.Context, tx pgx.Tx, status *State, limit, offset int) ([]WithdrawalRequest, int, error) {
	var total int
	var countErr error
	if status != nil {
		countErr = tx.QueryRow(ctx, `SELECT COUNT(*) FROM withdrawal_requests WHERE state = $1`, *status).Scan(&total)
	} else {
		countErr = tx.QueryRow(ctx, `SELECT COUNT(*) FROM withdrawal_requests`).Scan(&total)
	}
	if countErr != nil {
		return nil, 0, fmt.Errorf("withdrawal: count for tenant: %w", countErr)
	}

	var rows pgx.Rows
	var err error
	if status != nil {
		rows, err = tx.Query(ctx,
			`SELECT `+requestColumns+` FROM withdrawal_requests WHERE state = $1 ORDER BY requested_at DESC LIMIT $2 OFFSET $3`,
			*status, limit, offset,
		)
	} else {
		rows, err = tx.Query(ctx,
			`SELECT `+requestColumns+` FROM withdrawal_requests ORDER BY requested_at DESC LIMIT $1 OFFSET $2`,
			limit, offset,
		)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("withdrawal: list for tenant: %w", err)
	}
	defer rows.Close()

	var out []WithdrawalRequest
	for rows.Next() {
		wr, err := scanRequest(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, wr)
	}
	return out, total, rows.Err()
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

// MarkSubmittedPending performs the ADR 0095 T1p companion transition on
// the withdrawal_requests side: approved -> submitted, recording the
// provider the payout was routed to, but WITHOUT a provider reference yet
// (P95-C2, withdrawal-state-machine.md §2/§3: `submitted` may lack a
// provider reference at the moment this commits, because ADR 0095's whole
// point is that this transition commits BEFORE the provider is ever
// called - the old MarkSubmitted, which requires a non-empty reference,
// modeled the pre-ADR-0095 world where the provider call already happened
// in the same transaction). AttachProviderReference (below) records the
// reference once phase B/C actually receives one, or never, if the
// provider never returns one before a definite outcome.
func MarkSubmittedPending(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, providerID string) error {
	if providerID == "" {
		return fmt.Errorf("%w: provider id is required", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StateApproved {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateApproved)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET state = $1, provider_id = $2, updated_at = now()
		 WHERE id = $3 AND state = $4`,
		StateSubmitted, providerID, requestID, StateApproved,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: mark submitted (pending reference): %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}

	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   wr.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     "withdrawal.dispatch_claimed",
		TargetType: "withdrawal_request",
		TargetID:   requestID.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id": providerID,
		},
	})
}

// AttachProviderReference records the PSP/custodian's own reference for an
// already-`submitted` payout, once phase B/C actually receives one
// (MarkSubmittedPending leaves it NULL). A no-op, never an error and never
// an overwrite, if a reference is already recorded - provider_reference is
// set-once, mirroring payment_attempts.provider_reference's identical
// immutability rule, since a caller might call this more than once for the
// same request (e.g. a QueryStatus-driven resolution after a Pending
// acceptance already attached one).
func AttachProviderReference(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, providerReference string) error {
	if providerReference == "" {
		return fmt.Errorf("%w: provider reference is required", ErrInvalidInput)
	}

	wr, err := lockRequestForUpdate(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if wr.State != StateSubmitted {
		return fmt.Errorf("%w: request %s is in state %q, expected %q", ErrStateConflict, requestID, wr.State, StateSubmitted)
	}
	if wr.ProviderReference != nil {
		return nil
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET provider_reference = $1, updated_at = now()
		 WHERE id = $2 AND state = $3 AND provider_reference IS NULL`,
		providerReference, requestID, StateSubmitted,
	)
	if err != nil {
		return fmt.Errorf("withdrawal: attach provider reference: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStateConflict
	}
	return nil
}

// MarkSubmitted transitions a request from `approved` to `submitted` once
// the orchestrator has sent the payout instruction to the PSP/custodian
// adapter (payment-orchestration.md). No ledger effect yet - submission
// is not confirmation (withdrawal-state-machine.md §1).
//
// SUPERSEDED for the ADR-0095 payout dispatch path (internal/payments'
// ClaimForDispatch/ApplyPayoutResult, PRH-I1): that path uses
// MarkSubmittedPending (before any provider call) plus AttachProviderReference
// (once one is available) instead of this single combined call, because
// this function's precondition - a non-empty providerReference already in
// hand - can only be satisfied by calling the provider BEFORE this
// transition commits, which is exactly the double-payout hazard ADR 0095
// exists to remove (security review prh-ref-provider-reference-bound.md
// §10 C1). Retained for this package's existing tests and any caller that
// already has a confirmed, non-empty provider reference in hand at
// transition time: it is still a perfectly correct atomic transition,
// just not the one the payout dispatch call site (internal/payments'
// ClaimForDispatch/ApplyPayoutResult) uses.
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
//
// Invariant (Stage 10 F-7 remediation): one confirmation reference
// completes exactly one request. Complete does not assert that
// providerTxID equals the request's own provider_reference, because Flow 3
// allows the two to differ; a confirmation reference already used by a
// DIFFERENT request is rejected by ledger.Post as
// ErrIdempotencyPayloadMismatch (its correlation_id and entries are that
// other request's), posting nothing - never silently linked to the other
// request's ledger transaction.
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

	// ADR 0082 §4.6: account resolution only - see Reject's own comment
	// for why no locking change is needed on this path.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: wr.AssetCode},
	)
	if err != nil {
		return fmt.Errorf("withdrawal: resolve hold/psp_clearing ledger accounts: %w", err)
	}
	holdAccountID, clearingAccountID := accounts[0], accounts[1]

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

	// ADR 0082 §4.6: no LOCKING change is needed here - finding LOCK-1c
	// is closed by ledger.Post's own internal L3 pre-lock, and this
	// function's lockRequestForUpdate call is class L1, which already
	// precedes L3. The account resolution DOES change, and is
	// load-bearing: this function resolves (hold, cash) while
	// RequestWithdrawal resolves (cash, hold), so first-time creation of
	// both accounts could otherwise block on each other's uncommitted
	// ledger_accounts index entries in opposite orders.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode},
	)
	if err != nil {
		return fmt.Errorf("withdrawal: resolve hold/cash ledger accounts: %w", err)
	}
	holdAccountID, cashAccountID := accounts[0], accounts[1]

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

	// ADR 0082 §4.6: no LOCKING change is needed here - finding LOCK-1c
	// is closed by ledger.Post's own internal L3 pre-lock, and this
	// function's lockRequestForUpdate call is class L1, which already
	// precedes L3. The account resolution DOES change, and is
	// load-bearing: this function resolves (hold, cash) while
	// RequestWithdrawal resolves (cash, hold), so first-time creation of
	// both accounts could otherwise block on each other's uncommitted
	// ledger_accounts index entries in opposite orders.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode},
	)
	if err != nil {
		return fmt.Errorf("withdrawal: resolve hold/cash ledger accounts: %w", err)
	}
	holdAccountID, cashAccountID := accounts[0], accounts[1]

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
