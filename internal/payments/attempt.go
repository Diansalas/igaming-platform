// PRH-I1 step (a): the payment_attempts data-access layer (migration
// 0101, ADR 0095 §4 and §13.1). This file is deliberately scoped to
// types plus INSERT/CAS-transition functions ONLY - nothing here is
// called by InitiateDeposit, the webhook path or the withdrawal handlers
// yet (that wiring is PRH-I1 steps b-i). Every function is a thin,
// tenant-RLS-scoped wrapper around exactly one ADR 0095 §4.3 transition,
// so the CAS predicate in Go and the guard trigger in migration 0101
// enforce the SAME rule from two independent layers (defense in depth,
// never "trust the trigger alone").
//
// Every function takes a pgx.Tx the caller already opened inside
// db.Pool.WithTenant (or WithoutTenant plus an explicit RLS-satisfying
// tenant_id column value for INSERT). None of these functions may EVER
// be called while a provider-call gate holds no transaction (INV-IO-1);
// conversely, none of these functions makes any external call itself -
// they are pure SQL.
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AttemptOperation mirrors payment_attempts.operation.
type AttemptOperation string

const (
	AttemptOperationDeposit AttemptOperation = "deposit"
	AttemptOperationPayout  AttemptOperation = "payout"
)

// AttemptState is the closed 8-state set from ADR 0095 §4.1. The order
// here is documentation only - CAS predicates never rely on ordering.
type AttemptState string

const (
	AttemptCreated    AttemptState = "created"
	AttemptSubmitting AttemptState = "submitting"
	AttemptPending    AttemptState = "pending"
	AttemptAmbiguous  AttemptState = "ambiguous"
	AttemptSucceeded  AttemptState = "succeeded"
	AttemptDeclined   AttemptState = "declined"
	AttemptRejected   AttemptState = "rejected"
	AttemptDisputed   AttemptState = "disputed"
)

// EvidenceKind mirrors payment_attempts.last_evidence_kind (ADR 0095
// §4.2, as widened by the ledger-finance re-verification's N2/L1 fix to
// include "legacy"). EvidenceLegacy can only ever be written by the 0101
// migration's own backfill (enforced by the guard trigger, not by this
// enum) - application code must never construct a transition with it.
type EvidenceKind string

const (
	EvidenceSync           EvidenceKind = "sync"
	EvidenceCallback       EvidenceKind = "callback"
	EvidenceQueryStatus    EvidenceKind = "query_status"
	EvidenceSweeper        EvidenceKind = "sweeper"
	EvidenceOperator       EvidenceKind = "operator"
	EvidencePlatform       EvidenceKind = "platform"
	EvidenceLegacyDoNotUse EvidenceKind = "legacy"
)

// DeclineStage mirrors payment_attempts.decline_stage.
type DeclineStage string

const (
	DeclineAtSubmission    DeclineStage = "at_submission"
	DeclineAfterAcceptance DeclineStage = "after_acceptance"
)

// TerminalReasonTombstonePrecedesSuccess is the ONLY terminal_reason the
// guard trigger accepts on a deposit declined->disputed transition
// (ADR 0095 §4.3 T13t, LF95-C6(d)).
const TerminalReasonTombstonePrecedesSuccess = "reversal_tombstone_precedes_success"

// killSwitchNotEngagedSQL returns the ADR 0095 §10.3 kill-switch predicate
// as a boolean SQL expression, evaluated INSIDE the same claim statement
// that would otherwise submit to a provider - never as a separate
// check-then-act query. tenantExpr/providerExpr/operationExpr are SQL
// expressions (a column reference or a bound parameter placeholder), never
// caller-supplied data interpolated as a literal, so this composes safely
// into the surrounding CAS statement. Because the switch table's tenant
// SELECT policy shares payment_attempts' own tenant predicate
// (tenant_staff_scope), a claim running under any context that cannot see
// payment_attempts (a wrong tenant, an unset tenant, a player-scoped
// session, or a platform session) also sees zero switch rows and always
// evaluates this to true - it never fails OPEN (RV-0095 L1, §16.2 item 20).
func killSwitchNotEngagedSQL(tenantExpr, providerExpr, operationExpr string) string {
	return fmt.Sprintf(
		`NOT EXISTS (
			SELECT 1 FROM payment_kill_switches k
			WHERE k.tenant_id = %s
			  AND k.engaged
			  AND k.provider_scope IN ('*', %s)
			  AND k.operation_scope IN ('*', %s)
		)`,
		tenantExpr, providerExpr, operationExpr,
	)
}

// ErrAttemptStateConflict is returned by every CAS transition function
// below when its UPDATE affects zero rows - either the id does not
// exist (in this tenant's RLS scope) or the row is no longer in the
// state (and, where applicable, does not hold the claim_token) the
// caller expected. The caller must re-read the row rather than retry
// blindly: a concurrent transition already decided the outcome
// (INV-IO-4).
var ErrAttemptStateConflict = errors.New("payments: payment_attempts CAS transition conflict")

// ErrAttemptNotFound is returned by the by-id/by-reference lookups when
// no row is visible in the caller's RLS scope.
var ErrAttemptNotFound = errors.New("payments: no matching payment_attempts row")

// PaymentAttempt mirrors one payment_attempts row. Every field here has a
// 1:1 column in migration 0101.
type PaymentAttempt struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	Operation              AttemptOperation
	DepositIntentID        *uuid.UUID
	WithdrawalRequestID    *uuid.UUID
	AttemptNo              int
	ProviderID             *string
	ExcludedProviderIDs    []string
	PaymentMethod          string
	LegacyBackfill         bool
	AssetCode              string
	Amount                 int64
	Interactive            bool
	MerchantReference      string
	ExternalIdempotencyKey string
	ProviderReference      *string
	State                  AttemptState
	LastEvidenceKind       EvidenceKind
	EverPossiblySent       bool
	SubmitCount            int
	ClaimToken             *uuid.UUID
	LeaseOwner             *string
	LeaseUntil             *time.Time
	FirstSubmittedAt       *time.Time
	LastSentAt             *time.Time
	NextActionAt           *time.Time
	PollCount              int
	EscalatedAt            *time.Time
	DeclineReason          *string
	DeclineStage           *DeclineStage
	Cascadable             *bool
	TerminalReason         *string
	LedgerTransactionID    *uuid.UUID
	CreatedAt              time.Time
	AcceptedAt             *time.Time
	ResolvedAt             *time.Time
	UpdatedAt              time.Time
}

const paymentAttemptColumns = `
	id, tenant_id, operation, deposit_intent_id, withdrawal_request_id, attempt_no,
	provider_id, excluded_provider_ids, payment_method, legacy_backfill, asset_code, amount, interactive,
	merchant_reference, external_idempotency_key, provider_reference,
	state, last_evidence_kind, ever_possibly_sent,
	submit_count, claim_token, lease_owner, lease_until,
	first_submitted_at, last_sent_at, next_action_at, poll_count, escalated_at,
	decline_reason, decline_stage, cascadable, terminal_reason,
	ledger_transaction_id, created_at, accepted_at, resolved_at, updated_at`

func scanPaymentAttempt(row pgx.Row) (PaymentAttempt, error) {
	var a PaymentAttempt
	var op, state, evidence string
	var declineStage *string
	err := row.Scan(
		&a.ID, &a.TenantID, &op, &a.DepositIntentID, &a.WithdrawalRequestID, &a.AttemptNo,
		&a.ProviderID, &a.ExcludedProviderIDs, &a.PaymentMethod, &a.LegacyBackfill, &a.AssetCode, &a.Amount, &a.Interactive,
		&a.MerchantReference, &a.ExternalIdempotencyKey, &a.ProviderReference,
		&state, &evidence, &a.EverPossiblySent,
		&a.SubmitCount, &a.ClaimToken, &a.LeaseOwner, &a.LeaseUntil,
		&a.FirstSubmittedAt, &a.LastSentAt, &a.NextActionAt, &a.PollCount, &a.EscalatedAt,
		&a.DeclineReason, &declineStage, &a.Cascadable, &a.TerminalReason,
		&a.LedgerTransactionID, &a.CreatedAt, &a.AcceptedAt, &a.ResolvedAt, &a.UpdatedAt,
	)
	if err != nil {
		return PaymentAttempt{}, err
	}
	a.Operation = AttemptOperation(op)
	a.State = AttemptState(state)
	a.LastEvidenceKind = EvidenceKind(evidence)
	if declineStage != nil {
		ds := DeclineStage(*declineStage)
		a.DeclineStage = &ds
	}
	return a, nil
}

// GetAttemptByID returns the payment_attempts row for id, in the
// caller's RLS scope.
func GetAttemptByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (PaymentAttempt, error) {
	row := tx.QueryRow(ctx, `SELECT `+paymentAttemptColumns+` FROM payment_attempts WHERE id = $1`, id)
	a, err := scanPaymentAttempt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentAttempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return PaymentAttempt{}, fmt.Errorf("payments: get payment attempt by id: %w", err)
	}
	return a, nil
}

// GetAttemptByMerchantReference resolves an attempt by its own
// deterministic external identity (INV-IO-3) - the lookup a callback
// uses when it echoes the merchant reference (§6.1).
func GetAttemptByMerchantReference(ctx context.Context, tx pgx.Tx, merchantReference string) (PaymentAttempt, error) {
	row := tx.QueryRow(ctx, `SELECT `+paymentAttemptColumns+` FROM payment_attempts WHERE merchant_reference = $1`, merchantReference)
	a, err := scanPaymentAttempt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentAttempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return PaymentAttempt{}, fmt.Errorf("payments: get payment attempt by merchant reference: %w", err)
	}
	return a, nil
}

// GetAttemptByProviderReference resolves an attempt by (provider_id,
// provider_reference). Callers MUST treat providerID as the VERIFIED
// provider identity (route plus verified credential), never a
// payload-carried value (INV-IO-14) - this function does not and cannot
// enforce that; it is the caller's obligation, exactly like
// loadDepositIntentByProviderRef's existing doc comment.
func GetAttemptByProviderReference(ctx context.Context, tx pgx.Tx, providerID, providerReference string) (PaymentAttempt, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+paymentAttemptColumns+` FROM payment_attempts WHERE provider_id = $1 AND provider_reference = $2`,
		providerID, providerReference)
	a, err := scanPaymentAttempt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentAttempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return PaymentAttempt{}, fmt.Errorf("payments: get payment attempt by provider reference: %w", err)
	}
	return a, nil
}

// ListLiveAttemptForDepositIntent returns the single live (non-terminal)
// attempt for a deposit intent, if any (INV-IO-8 guarantees at most one).
func ListLiveAttemptForDepositIntent(ctx context.Context, tx pgx.Tx, depositIntentID uuid.UUID) (PaymentAttempt, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+paymentAttemptColumns+` FROM payment_attempts
		 WHERE deposit_intent_id = $1 AND state IN ('created','submitting','pending','ambiguous')`,
		depositIntentID)
	a, err := scanPaymentAttempt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentAttempt{}, false, nil
	}
	if err != nil {
		return PaymentAttempt{}, false, fmt.Errorf("payments: list live attempt for deposit intent: %w", err)
	}
	return a, true, nil
}

// GetLiveAttemptForWithdrawalRequest returns the single live (non-terminal)
// attempt for a payout, if any - the payout analog of
// ListLiveAttemptForDepositIntent, used by /resolve (B7/H4, RV-PRH-I1) to
// find the attempt row that must transition ALONGSIDE the withdrawal
// request, instead of that handler calling withdrawal.Complete/Fail
// directly with no matching attempt transition. `created` is included
// (unlike the deposit version's exact state list, reused verbatim) because
// a payout attempt returned to `created` by a NotSent result is still
// "live" - just not currently pollable via QueryStatus.
func GetLiveAttemptForWithdrawalRequest(ctx context.Context, tx pgx.Tx, withdrawalRequestID uuid.UUID) (PaymentAttempt, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+paymentAttemptColumns+` FROM payment_attempts
		 WHERE withdrawal_request_id = $1 AND state IN ('created','submitting','pending','ambiguous')
		 ORDER BY attempt_no DESC LIMIT 1`,
		withdrawalRequestID)
	a, err := scanPaymentAttempt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentAttempt{}, false, nil
	}
	if err != nil {
		return PaymentAttempt{}, false, fmt.Errorf("payments: get live attempt for withdrawal request: %w", err)
	}
	return a, true, nil
}

// MerchantReferenceFor and ExternalIdempotencyKeyFor are the two pure
// functions INV-IO-3 requires: deterministic functions of the attempt
// id, computed once and persisted, never regenerated per try.
func MerchantReferenceFor(attemptID uuid.UUID) string      { return attemptID.String() }
func ExternalIdempotencyKeyFor(attemptID uuid.UUID) string { return "pa:" + attemptID.String() }

// InsertCreatedAttempt performs T1: ∅ -> created. Used for a cascade row
// (attempt n+1, provider_id NULL) and by nothing else in this file - the
// player-request path's first attempt is always inserted directly into
// submitting (InsertSubmittingAttempt, T1+T2/T1p), per ADR 0095 §4.3.
type NewCreatedAttempt struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	Operation           AttemptOperation
	DepositIntentID     *uuid.UUID
	WithdrawalRequestID *uuid.UUID
	AttemptNo           int
	ExcludedProviderIDs []string
	PaymentMethod       string
	AssetCode           string
	Amount              int64
	Interactive         bool
}

func InsertCreatedAttempt(ctx context.Context, tx pgx.Tx, in NewCreatedAttempt) (PaymentAttempt, error) {
	// next_action_at is set to now() unconditionally (PRH-I1 step (c)):
	// the sweeper's claim query (sweeper.go) only ever looks at
	// next_action_at, for BOTH interactive and non-interactive 'created'
	// rows - interactive rows are found the same way and then expired
	// (T3) once older than the presence window, never resubmitted.
	// Without this, a cascade row (§4.6) would never be picked up at all.
	//
	// This T1 insert has not chosen a provider_id yet (it is NULL until a
	// later T2 claim, which carries its own full kill-switch check), so
	// this INSERT ... SELECT form can only refuse against a wildcard
	// provider_scope='*' switch - defence in depth so a cascade never even
	// creates a new row while the tenant/operation is wholesale contained,
	// on top of T2's own full (provider-specific) check at claim time.
	tag, err := tx.Exec(ctx,
		`INSERT INTO payment_attempts (
			id, tenant_id, operation, deposit_intent_id, withdrawal_request_id, attempt_no,
			excluded_provider_ids, payment_method, asset_code, amount, interactive,
			merchant_reference, external_idempotency_key, state, last_evidence_kind, next_action_at
		)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'created','platform', now()
		WHERE NOT EXISTS (
			SELECT 1 FROM payment_kill_switches k
			WHERE k.tenant_id = $2 AND k.engaged AND k.provider_scope = '*' AND k.operation_scope IN ('*', $3)
		)`,
		in.ID, in.TenantID, in.Operation, in.DepositIntentID, in.WithdrawalRequestID, in.AttemptNo,
		in.ExcludedProviderIDs, in.PaymentMethod, in.AssetCode, in.Amount, in.Interactive,
		MerchantReferenceFor(in.ID), ExternalIdempotencyKeyFor(in.ID),
	)
	if err != nil {
		return PaymentAttempt{}, fmt.Errorf("payments: insert created payment attempt (T1): %w", err)
	}
	if tag.RowsAffected() == 0 {
		return PaymentAttempt{}, fmt.Errorf("payments: insert created payment attempt (T1): %w", ErrKillSwitchEngaged)
	}
	return GetAttemptByID(ctx, tx, in.ID)
}

// NewSubmittingAttempt is InsertSubmittingAttempt's input: the combined
// T1+T2 (deposit, player path) or T1p (payout claim) insert - one
// committed row that is simultaneously "the intent to call" and "the
// claim to call it" (ADR 0095 §4.3, §5.1, §5.2).
type NewSubmittingAttempt struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	Operation           AttemptOperation
	DepositIntentID     *uuid.UUID
	WithdrawalRequestID *uuid.UUID
	ProviderID          string
	PaymentMethod       string
	AssetCode           string
	Amount              int64
	Interactive         bool
	ClaimToken          uuid.UUID
	LeaseOwner          string
	LeaseUntil          time.Time
}

// ErrKillSwitchEngaged is returned by InsertSubmittingAttempt when a
// payment_kill_switches row matching (tenant, provider, operation) is
// engaged (§10.3): the combined T1+T2 (deposit)/T1p (payout) INSERT ...
// SELECT form has no prior 'created' row to fall back into, so a refused
// insert is reported directly rather than through ErrAttemptStateConflict.
var ErrKillSwitchEngaged = errors.New("payments: refused, a matching payment kill switch is engaged")

func InsertSubmittingAttempt(ctx context.Context, tx pgx.Tx, in NewSubmittingAttempt) (PaymentAttempt, error) {
	// INSERT ... SELECT (not a plain INSERT ... VALUES): the ADR 0095
	// §10.3 kill-switch predicate must be evaluated INSIDE this statement,
	// atomically with the insert itself, for the T1+T2/T1p forms that have
	// no separate 'created' row for a later T2 claim to refuse - see
	// killSwitchNotEngagedSQL's own doc comment for why a wrong tenant
	// context inserts nothing here too.
	//
	// B2/H3 (RV-PRH-I1 code/ledger review): next_action_at is ALSO set to
	// LeaseUntil unconditionally (reusing the $15 placeholder), not left
	// NULL - claimBatch's own query only ever looks at next_action_at, so
	// a row left NULL here is invisible to the sweeper forever if nothing
	// else transitions it (a process crash during phase B, a phase-C
	// error, or an unregistered provider). This is additive/safe for the
	// deposit T1+T2 path too: phase B/C always runs synchronously right
	// after this insert there, and every phase-C transition (MarkAccepted/
	// ApplySuccess/ApplyDecline/MarkAmbiguousFromSubmitting/MarkNotSent)
	// already sets its own next_action_at (or NULL for a terminal state)
	// immediately afterward.
	tag, err := tx.Exec(ctx,
		`INSERT INTO payment_attempts (
			id, tenant_id, operation, deposit_intent_id, withdrawal_request_id, attempt_no,
			provider_id, payment_method, asset_code, amount, interactive,
			merchant_reference, external_idempotency_key, state, last_evidence_kind,
			claim_token, lease_owner, lease_until, submit_count, last_sent_at, first_submitted_at, next_action_at
		)
		SELECT $1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12,'submitting','platform',$13,$14,$15,1,now(),now(),$15
		WHERE `+killSwitchNotEngagedSQL("$2", "$6", "$3"),
		in.ID, in.TenantID, in.Operation, in.DepositIntentID, in.WithdrawalRequestID,
		in.ProviderID, in.PaymentMethod, in.AssetCode, in.Amount, in.Interactive,
		MerchantReferenceFor(in.ID), ExternalIdempotencyKeyFor(in.ID),
		in.ClaimToken, in.LeaseOwner, in.LeaseUntil,
	)
	if err != nil {
		return PaymentAttempt{}, fmt.Errorf("payments: insert submitting payment attempt (T1+T2/T1p): %w", err)
	}
	if tag.RowsAffected() == 0 {
		return PaymentAttempt{}, fmt.Errorf("payments: insert submitting payment attempt (T1+T2/T1p): %w", ErrKillSwitchEngaged)
	}
	return GetAttemptByID(ctx, tx, in.ID)
}

// casUpdate runs one CAS UPDATE and maps "zero rows affected" to
// ErrAttemptStateConflict, never to a silent no-op and never to a
// generic SQL error the caller might mistake for something retryable in
// a different sense.
func casUpdate(ctx context.Context, tx pgx.Tx, op string, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("payments: %s: %w", op, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("payments: %s: %w", op, ErrAttemptStateConflict)
	}
	return nil
}

// ClaimCreatedForSubmission performs T2: created -> submitting. The
// caller MUST have already locked the parent row (deposit_intents or
// withdrawal_requests) FOR UPDATE in this same transaction (ADR 0095
// §14 "parent before attempt, everywhere") and evaluated RG/KYC/kill
// switch BEFORE calling this - this function only performs the CAS
// itself, it does not evaluate any gate (that is orchestrator wiring,
// steps b/c). providerID may differ from a prior excluded set only
// insofar as the caller has already chosen a not-yet-excluded provider.
// ClaimCreatedForSubmission's T2 predicate refuses a claim once a sibling
// attempt of the SAME deposit intent has already succeeded (RV-PRH-I1
// ledger-finance H4/ADR 0095 §4.3 T2's own "NOT EXISTS(succeeded attempt
// for the same intent)" requirement, mirroring ResubmitAmbiguous's
// identical T12 guard below): without this, a cascade child left
// 'created' after a late T13 success on a DIFFERENT sibling could still be
// claimed and driven to a second, independent provider call for money
// that is already captured. A payout attempt has no deposit_intent_id, so
// the EXISTS subquery is vacuously false for it and never refuses a
// payout claim.
func ClaimCreatedForSubmission(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, providerID string, claimToken uuid.UUID, leaseOwner string, leaseUntil time.Time) error {
	return casUpdate(ctx, tx, "T2 claim created->submitting",
		`UPDATE payment_attempts
		 SET provider_id = $2, claim_token = $3, lease_owner = $4, lease_until = $5,
		     submit_count = submit_count + 1, last_sent_at = now(),
		     first_submitted_at = COALESCE(first_submitted_at, now()),
		     state = 'submitting', last_evidence_kind = 'platform', updated_at = now(),
		     next_action_at = $5 -- B2/H3: visible to the sweeper even if phase C never runs.
		 WHERE id = $1 AND state = 'created' AND (provider_id IS NULL OR provider_id = $2)
		   AND NOT EXISTS (
		     SELECT 1 FROM payment_attempts sib
		     WHERE payment_attempts.operation = 'deposit'
		       AND sib.deposit_intent_id = payment_attempts.deposit_intent_id
		       AND sib.state = 'succeeded' AND sib.id <> payment_attempts.id
		   )
		   AND `+killSwitchNotEngagedSQL("payment_attempts.tenant_id", "$2", "payment_attempts.operation"),
		attemptID, providerID, claimToken, leaseOwner, leaseUntil,
	)
}

// RejectCreated performs T3 (pre-call refusal) or M3 (staff abandonment,
// operation='payout' only, reason='kyc_denied' or similar - the CAS
// predicate `NOT ever_possibly_sent` is what the guard trigger and the
// CHECK both already guarantee for state='created', so no double-payout
// is structurally possible here). reason is a canonical code (S95-C10),
// never free text.
func RejectCreated(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, reason string) error {
	return casUpdate(ctx, tx, "T3/M3 created->rejected",
		`UPDATE payment_attempts
		 SET state = 'rejected', last_evidence_kind = $2, terminal_reason = $3, resolved_at = now(),
		     next_action_at = NULL, updated_at = now()
		 WHERE id = $1 AND state = 'created'`,
		attemptID, evidence, reason,
	)
}

// MarkNotSent performs T5: submitting -> created, only for the SAME
// claimant (claim_token match) and only when the guard's CHECK
// (state='created' implies NOT ever_possibly_sent) can still hold - i.e.
// the caller must only call this for ErrorClassNotSent, never for any
// result that might mean the provider received the call.
func MarkNotSent(ctx context.Context, tx pgx.Tx, attemptID, claimToken uuid.UUID, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "T5 submitting->created (NotSent)",
		`UPDATE payment_attempts
		 SET state = 'created', last_evidence_kind = 'platform', next_action_at = $3, updated_at = now()
		 WHERE id = $1 AND state = 'submitting' AND claim_token = $2 AND NOT ever_possibly_sent`,
		attemptID, claimToken, nextActionAt,
	)
}

// MarkAmbiguousFromSubmitting performs T6: submitting -> ambiguous
// (timeout after a possible send, unmapped 5xx, a NotProcessed code that
// may leave a trace, a lease expiry not resolved by QueryStatus, or a
// NotSent result on a T12 resend). Always sets ever_possibly_sent=true,
// per ADR 0095 §4.2's rule that the flag is only proof of "not
// classified NotSent yet", not proof of a send.
func MarkAmbiguousFromSubmitting(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "T6 submitting->ambiguous",
		`UPDATE payment_attempts
		 SET state = 'ambiguous', last_evidence_kind = $2, ever_possibly_sent = true,
		     next_action_at = $3, poll_count = poll_count + 1, updated_at = now()
		 WHERE id = $1 AND state = 'submitting'`,
		attemptID, evidence, nextActionAt,
	)
}

// MarkAccepted performs T4 (submitting->pending) or T9
// (ambiguous->pending): the provider acknowledged receipt and assigned a
// reference; the outcome is not yet final. providerReference is set only
// if the attempt does not already carry one (it is immutable once set;
// the guard trigger enforces this independently).
func MarkAccepted(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, providerReference string, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "T4/T9 ->pending",
		`UPDATE payment_attempts
		 SET state = 'pending', last_evidence_kind = $2,
		     provider_reference = COALESCE(provider_reference, $3), accepted_at = COALESCE(accepted_at, now()),
		     next_action_at = $4, updated_at = now()
		 WHERE id = $1 AND state IN ('submitting','ambiguous')`,
		attemptID, evidence, providerReference, nextActionAt,
	)
}

// MarkAmbiguousFromPending performs T11: pending -> ambiguous (an
// explicit "unknown"/not-found result for an attempt the provider had
// already accepted - the provider "forgot" it). Always a P1 anomaly at
// the caller's audit layer; this function only performs the CAS.
func MarkAmbiguousFromPending(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "T11 pending->ambiguous",
		`UPDATE payment_attempts
		 SET state = 'ambiguous', last_evidence_kind = $2, next_action_at = $3, poll_count = poll_count + 1, updated_at = now()
		 WHERE id = $1 AND state = 'pending'`,
		attemptID, evidence, nextActionAt,
	)
}

// ResubmitAmbiguous performs T12: ambiguous -> submitting, the idempotent
// resend of the SAME attempt with the SAME external key. The caller MUST
// have already checked the manifest's IdempotentSubmission flag (this
// function has no manifest to consult) - C1/B1 (RV-PRH-I1 ledger/code
// review): the payout call site (payout_sweep.go's resubmitPayoutAmbiguous)
// now does this before ever calling ResubmitAmbiguous. maxSubmits is this
// function's own defense-in-depth copy of the same bound, enforced in the
// CAS predicate itself (`submit_count < maxSubmits`) so a caller bug can
// never resend past the configured cap even if it forgets its own check -
// pass <= 0 to mean "no additional bound beyond the caller's own check"
// (only ever used by a test that wants to isolate the manifest check).
// For a deposit, the caller must also re-verify no sibling has succeeded
// (N3; the guard trigger repeats this check independently).
// legacy_backfill rows are refused by both the CAS predicate here and the
// guard trigger (defense in depth). next_action_at is set to leaseUntil
// (B2/H3), matching every other claim transition.
func ResubmitAmbiguous(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, claimToken uuid.UUID, leaseOwner string, leaseUntil time.Time, maxSubmits int) error {
	if maxSubmits <= 0 {
		maxSubmits = 1 << 30 // effectively unbounded - see doc comment.
	}
	return casUpdate(ctx, tx, "T12 ambiguous->submitting",
		`UPDATE payment_attempts
		 SET state = 'submitting', last_evidence_kind = 'platform',
		     claim_token = $2, lease_owner = $3, lease_until = $4,
		     submit_count = submit_count + 1, last_sent_at = now(), updated_at = now(),
		     next_action_at = $4
		 WHERE id = $1 AND state = 'ambiguous' AND NOT legacy_backfill AND submit_count < $5
		   AND NOT EXISTS (
		     SELECT 1 FROM payment_attempts sib
		     JOIN payment_attempts self ON self.id = $1
		     WHERE self.operation = 'deposit' AND sib.deposit_intent_id = self.deposit_intent_id
		       AND sib.state = 'succeeded' AND sib.id <> $1
		   )
		   AND `+killSwitchNotEngagedSQL("payment_attempts.tenant_id", "payment_attempts.provider_id", "payment_attempts.operation"),
		attemptID, claimToken, leaseOwner, leaseUntil, maxSubmits,
	)
}

// SuccessEvidence is ApplySuccess's input: the already-verified,
// amount/asset-matching evidence (INV-IO-6) that a caller (phase C, the
// callback path, or the sweeper) has already resolved to THIS attempt's
// own verified provider (INV-IO-14) before calling this function - this
// function performs only the CAS transition and does not itself verify
// evidence.
type SuccessEvidence struct {
	Evidence          EvidenceKind
	ProviderReference string
	// LedgerTransactionID must already be posted (deposit only, T7) or
	// nil (payout, T7 via withdrawal.Complete happens outside this
	// function - PRH-I1 step c wires that).
	LedgerTransactionID *uuid.UUID
}

// ApplySuccess performs T7 (submitting/pending/ambiguous -> succeeded)
// or T13 (declined -> succeeded, deposit only). It does NOT decide
// between T7 and T13t (tombstone) - a caller that has discovered a
// reversal tombstone must call ApplyTombstonePrecedesSuccess instead,
// never this function.
func ApplySuccess(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, ev SuccessEvidence) error {
	return casUpdate(ctx, tx, "T7/T13 ->succeeded",
		`UPDATE payment_attempts
		 SET state = 'succeeded', last_evidence_kind = $2,
		     provider_reference = COALESCE(provider_reference, $3),
		     ledger_transaction_id = COALESCE(ledger_transaction_id, $4),
		     resolved_at = now(), next_action_at = NULL, updated_at = now()
		 WHERE id = $1 AND (
		   state IN ('submitting','pending','ambiguous')
		   OR (state = 'declined' AND operation = 'deposit')
		 )`,
		attemptID, ev.Evidence, ev.ProviderReference, ev.LedgerTransactionID,
	)
}

// ApplyTombstonePrecedesSuccess performs T13t (ADR 0095 §4.3, LF95-C6(d)
// as fixed by revision 3): a deposit's late success evidence names a
// (provider_id, provider_reference) a reversal tombstone already
// occupies. Terminal, no posting, no error - the caller commits this in
// the SAME transaction whatever else it was about to do, so there is
// never a trigger-rejection/5xx-redelivery loop.
func ApplyTombstonePrecedesSuccess(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind) error {
	return casUpdate(ctx, tx, "T13t declined->disputed (tombstone)",
		`UPDATE payment_attempts
		 SET state = 'disputed', last_evidence_kind = $2, terminal_reason = $3, resolved_at = now(),
		     next_action_at = NULL, updated_at = now()
		 WHERE id = $1 AND state = 'declined' AND operation = 'deposit'`,
		attemptID, evidence, TerminalReasonTombstonePrecedesSuccess,
	)
}

// DeclineEvidence is ApplyDecline's input.
type DeclineEvidence struct {
	Evidence    EvidenceKind
	Reason      string
	Stage       DeclineStage
	Cascadable  *bool // deposit only; nil for a payout
	ProviderRef *string
}

// ApplyDecline performs T8 (submitting/pending/ambiguous -> declined).
// For a payout, evidence MUST be sync/callback/query_status (the guard
// trigger enforces this independently; INV-IO-7). For a deposit,
// evidence must never be operator (also independently enforced).
func ApplyDecline(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, ev DeclineEvidence) error {
	return casUpdate(ctx, tx, "T8 ->declined",
		`UPDATE payment_attempts
		 SET state = 'declined', last_evidence_kind = $2, decline_reason = $3, decline_stage = $4,
		     cascadable = $5, provider_reference = COALESCE(provider_reference, $6),
		     resolved_at = now(), next_action_at = NULL, updated_at = now()
		 WHERE id = $1 AND state IN ('submitting','pending','ambiguous')`,
		attemptID, ev.Evidence, ev.Reason, ev.Stage, ev.Cascadable, ev.ProviderRef,
	)
}

// ApplyDisputeFromDeclinedPayout performs T14: declined -> disputed,
// payout only (success evidence after the hold was already released -
// a double payout has already happened; P1, no automated remedy).
func ApplyDisputeFromDeclinedPayout(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, terminalReason string) error {
	return casUpdate(ctx, tx, "T14 declined->disputed (payout)",
		`UPDATE payment_attempts
		 SET state = 'disputed', last_evidence_kind = $2, terminal_reason = $3, resolved_at = now(),
		     next_action_at = NULL, updated_at = now()
		 WHERE id = $1 AND state = 'declined' AND operation = 'payout'`,
		attemptID, evidence, terminalReason,
	)
}

// ApplyDisputeFromNonTerminal performs T10: {submitting,pending,ambiguous}
// -> disputed (mismatched success, a provider-reference conflict, or any
// other contradiction ADR 0095 §4.4 routes here). Committed with its
// receipt whatever else happens (LF95-C3) - never rolled back to force a
// retry.
func ApplyDisputeFromNonTerminal(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, terminalReason string) error {
	return casUpdate(ctx, tx, "T10 ->disputed",
		`UPDATE payment_attempts
		 SET state = 'disputed', last_evidence_kind = $2, terminal_reason = $3, resolved_at = now(),
		     next_action_at = NULL, updated_at = now()
		 WHERE id = $1 AND state IN ('submitting','pending','ambiguous')`,
		attemptID, evidence, terminalReason,
	)
}

// ApplyDisputeFromNeverSent performs T15: {created,rejected} -> disputed.
// The caller MUST have already verified NEW's provider_id equals THIS
// attempt's own provider_id (INV-IO-14) - evidence from any other
// provider, or for a NULL provider_id, must never reach this function
// (route it to an anomaly receipt with no state change instead).
func ApplyDisputeFromNeverSent(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind, terminalReason string) error {
	return casUpdate(ctx, tx, "T15 {created,rejected}->disputed",
		`UPDATE payment_attempts
		 SET state = 'disputed', last_evidence_kind = $2, terminal_reason = $3, resolved_at = now(),
		     next_action_at = NULL, updated_at = now()
		 WHERE id = $1 AND state IN ('created','rejected')`,
		attemptID, evidence, terminalReason,
	)
}

// Escalate performs T16: no state change, escalated_at set (past the
// manifest SettlementWindow, or a payout created blocked by a gate).
//
// The state predicate below (found by TestA7_1b_SweeperClaimVsCallbackPhaseC_SameWithdrawal,
// A7-TESTS-1 item #1b) is required, not defense in depth: without it, a
// concurrent resolution (e.g. a real payout success callback landing
// between resubmitPayoutAmbiguous's own CAS refusal and this call) can
// still match `escalated_at IS NULL` on an attempt that has ALREADY moved
// to a terminal state, and this UPDATE's own next_action_at write then
// violates the payment_attempts_check9 CHECK (every terminal state
// requires next_action_at IS NULL) - a hard 500, not a benign no-op. The
// caller (escalateAmbiguousPayout) treats the resulting
// ErrAttemptStateConflict as exactly that: the attempt already resolved
// concurrently, nothing left to escalate.
func Escalate(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "T16 escalate",
		`UPDATE payment_attempts SET escalated_at = now(), next_action_at = $2, updated_at = now()
		 WHERE id = $1 AND escalated_at IS NULL
		   AND state NOT IN ('succeeded','declined','rejected','disputed')`,
		attemptID, nextActionAt,
	)
}

// RescheduleNonTerminal performs a same-state write (no ADR 0095 §4.3
// transition at all - the guard trigger allows OLD.state = NEW.state
// unconditionally): it bumps next_action_at (poll backoff) and
// poll_count for an attempt whose evidence this round was inconclusive
// (still pending, still ambiguous, or a transport error on QueryStatus
// itself). Refuses on a terminal attempt (next_action_at is already NULL
// there, per the table's own CHECK).
func RescheduleNonTerminal(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, nextActionAt time.Time) error {
	return casUpdate(ctx, tx, "reschedule (no-op state change)",
		`UPDATE payment_attempts SET next_action_at = $2, poll_count = poll_count + 1, updated_at = now()
		 WHERE id = $1 AND next_action_at IS NOT NULL`,
		attemptID, nextActionAt,
	)
}

// Touch performs T17: no state change, next_action_at moved to now() so
// the sweeper picks the attempt up on its next pass. Never calls a
// provider inline - it only asks the sweeper (or the caller's own next
// phase-C run) to look again.
func Touch(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID) error {
	return casUpdate(ctx, tx, "T17 re-verify request",
		`UPDATE payment_attempts SET next_action_at = now(), updated_at = now()
		 WHERE id = $1 AND next_action_at IS NOT NULL`,
		attemptID,
	)
}

// --- payment_provider_events (receipts, INV-IO-10) ----------------------

// ProviderEvent mirrors one payment_provider_events row.
type ProviderEvent struct {
	ID                        uuid.UUID
	TenantID                  uuid.UUID
	ProviderID                string
	EventType                 string
	ProviderReference         string
	OriginalProviderReference *string
	MerchantReference         *string
	SettlementReference       *string
	Outcome                   string
	Amount                    *int64
	AssetCode                 *string
	Cascadable                *bool
	DeclineStage              *string
	DeclineReason             *string
	EventFingerprint          []byte
	DispositionAtReceipt      string
	AttemptID                 *uuid.UUID
	Resolution                *string
	ResolvedAt                *time.Time
	ReceivedAt                time.Time
}

// InsertReceipt durably records a verified callback (or an
// unsupported/anomalous one) BEFORE any effect is applied, in the same
// transaction as that effect (or its deferral) - INV-IO-10. A UNIQUE
// violation on (tenant_id, provider_id, event_fingerprint) means this
// exact delivery was already receipted; the caller must treat that as
// "duplicate, already handled" (disposition duplicate_effect at the next
// layer up), never as an error to surface to the provider as a failure.
func InsertReceipt(ctx context.Context, tx pgx.Tx, ev ProviderEvent) (uuid.UUID, error) {
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO payment_provider_events (
			id, tenant_id, provider_id, event_type, provider_reference, original_provider_reference,
			merchant_reference, settlement_reference, outcome, amount, asset_code,
			cascadable, decline_stage, decline_reason, event_fingerprint, disposition_at_receipt
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		ev.ID, ev.TenantID, ev.ProviderID, ev.EventType, ev.ProviderReference, ev.OriginalProviderReference,
		ev.MerchantReference, ev.SettlementReference, ev.Outcome, ev.Amount, ev.AssetCode,
		ev.Cascadable, ev.DeclineStage, ev.DeclineReason, ev.EventFingerprint, ev.DispositionAtReceipt,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("payments: insert payment provider event receipt: %w", err)
	}
	return ev.ID, nil
}

// ResolveReceipt is the one-shot (attempt_id, resolution, resolved_at)
// update (§14 "Receipt one-shot updates"). The caller must already hold
// the resolved attempt's parent and attempt locks. attemptID is a POINTER:
// nil means "no single attempt to attach to" (a precondition anomaly, a
// reversal with no resolvable original, or a non-succeeded reversal's own
// anomaly - RV-PRH-I1), written as SQL NULL. payment_provider_events.
// attempt_id is `UUID NULL REFERENCES payment_attempts (id)` - passing
// uuid.Nil (the all-zero UUID) here instead of NULL would violate that
// foreign key, since no payment_attempts row ever has that id.
func ResolveReceipt(ctx context.Context, tx pgx.Tx, receiptID uuid.UUID, attemptID *uuid.UUID, resolution string) error {
	return casUpdate(ctx, tx, "resolve payment provider event receipt",
		`UPDATE payment_provider_events SET attempt_id = $2, resolution = $3, resolved_at = now()
		 WHERE id = $1 AND resolved_at IS NULL`,
		receiptID, attemptID, resolution,
	)
}
