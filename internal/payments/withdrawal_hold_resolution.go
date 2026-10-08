// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 revision 2 section 6; owner decisions ADR
// 0095 section 44, decisions 13-18): the governed four-eyes release of an `approved`
// withdrawal hold on a tenant/brand that is not active. Status: IMPLEMENTED against
// the MOCK stack; it dispatches nothing and calls no provider.
//
// What the decisions say, and what this file therefore does NOT do:
//   - no automatic release: nothing here runs without a human request and the
//     counted approvals of distinct, platform-acting Persons;
//   - the hold REMAINS until a controlled staff resolution executes; "resume after
//     reactivation" is the existing submit path and has no code here;
//   - one kind only, release_hold_to_player (approved -> rejected, the hold returns to
//     the player's own player_cash on the same wallet); there is no generic override;
//   - request, approval and execution are four-eyes regardless of amount; a single
//     staff member can never release; kill-switch semantics are untouched.
//
// The authority decisions live in migration 0124's triggers and SQL functions (the
// forced platform_acting actor, the in-force withdrawal_hold_resolution grant, the
// distinct-Person floor, S-12, S-2(iii), the pinned factual basis, the DB-side
// recount, the state machine, the executing-only ledger fences, the CT-R3 key guard
// and the approved-hold freeze). This package builds the statements, takes the ADR
// 0082 A8 locks in ADR 0111 6.5 order, calls withdrawal.ReleaseForGovernedResolution,
// classifies errors by SQLSTATE and writes the audit rows.
//
// It is the second (and last) non-adjustment caller of db.WithPlatformActingInTenant
// (the A-16 static test allows exactly manual_resolution.go and this file), from
// exactly one call site (runSession), whose principal is parsed from the verified
// token in the same function and whose target tenant can only come from
// NewResolutionTarget.
package payments

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// OperationKindHoldResolution is the ADR 0111 classification key (migration 0124).
const OperationKindHoldResolution = "withdrawal_hold_resolution"

// HoldResolutionKind mirrors withdrawal_hold_resolutions.kind. The set is one value.
type HoldResolutionKind string

// HoldReleaseToPlayer is the only kind (ADR 0111 A-16). Adding another kind needs a
// security review (M-9): a new capability pair or revocation and re-issue of every
// in-force grant.
const HoldReleaseToPlayer HoldResolutionKind = "release_hold_to_player"

// Refusal codes a resolution ends refused_at_execution with (closed set).
const (
	holdRefusedNotApproved   = "withdrawal_not_approved"
	holdRefusedAttemptExists = "attempt_exists"
	holdRefusedActiveAgain   = "tenant_brand_active_again"
	holdRefusedPolicy        = "policy_disabled"
)

// Sentinel errors for conditions this package detects itself.
var (
	ErrHoldResolutionNotFound   = errors.New("payments: withdrawal hold resolution not found")
	ErrHoldResolutionNotPending = errors.New("payments: withdrawal hold resolution is not pending")
	ErrHoldResolutionExpired    = errors.New("payments: withdrawal hold resolution expired")
	ErrHoldResolutionInvalid    = errors.New("payments: invalid withdrawal hold resolution input")
	// ErrHoldResolutionNotPermitted: a tenant-scoped caller (any tenant role) or an
	// unauthenticated one. Only a platform principal acting in the tenant may act.
	ErrHoldResolutionNotPermitted = errors.New("payments: only a platform principal acting in the tenant may use withdrawal hold resolutions")
	ErrHoldResolutionIntegrity    = errors.New("payments: withdrawal hold resolution integrity violation")
)

var (
	holdReasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	holdHex64RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// HoldResolution is one withdrawal_hold_resolutions row.
type HoldResolution struct {
	ID                         uuid.UUID
	TenantID                   uuid.UUID
	WithdrawalRequestID        uuid.UUID
	Kind                       HoldResolutionKind
	BrandID                    uuid.UUID
	PlayerAccountID            uuid.UUID
	WalletID                   uuid.UUID
	Amount                     int64
	AssetCode                  string
	PayoutInstrumentID         *uuid.UUID
	WithdrawalStateAtSubmit    string
	TenantStatusAtSubmission   string
	BrandStatusAtSubmission    string
	TenantStatusAtExecution    *string
	BrandStatusAtExecution     *string
	ReasonCode                 string
	EvidenceRefHash            string
	PayloadHash                string
	RequestedBy                uuid.UUID
	RequestedByScope           string
	RequestedByPersonID        uuid.UUID
	RequiredAtSubmission       int
	State                      ResolutionState
	ExpiresAt                  time.Time
	ExecutedTxID               *int64
	LedgerTransactionID        *uuid.UUID
	RefusalCode                *string
	CreatedAt                  time.Time
	ClosedAt                   *time.Time
	ContributingPolicyIDsAtSub []uuid.UUID
}

const holdResolutionColumns = `id, tenant_id, withdrawal_request_id, kind, brand_id, player_account_id, wallet_id, amount::text,
	asset_code, payout_instrument_id, withdrawal_state_at_submission, tenant_status_at_submission, brand_status_at_submission,
	tenant_status_at_execution, brand_status_at_execution, reason_code, evidence_ref_hash, payload_hash, requested_by,
	requested_by_scope, requested_by_person_id, required_at_submission, state, expires_at, executed_txid, ledger_transaction_id,
	refusal_code, created_at, closed_at, contributing_policy_ids`

func scanHoldResolution(row pgx.Row, r *HoldResolution) error {
	var amount string
	if err := row.Scan(&r.ID, &r.TenantID, &r.WithdrawalRequestID, &r.Kind, &r.BrandID, &r.PlayerAccountID, &r.WalletID, &amount,
		&r.AssetCode, &r.PayoutInstrumentID, &r.WithdrawalStateAtSubmit, &r.TenantStatusAtSubmission, &r.BrandStatusAtSubmission,
		&r.TenantStatusAtExecution, &r.BrandStatusAtExecution, &r.ReasonCode, &r.EvidenceRefHash, &r.PayloadHash, &r.RequestedBy,
		&r.RequestedByScope, &r.RequestedByPersonID, &r.RequiredAtSubmission, &r.State, &r.ExpiresAt, &r.ExecutedTxID,
		&r.LedgerTransactionID, &r.RefusalCode, &r.CreatedAt, &r.ClosedAt, &r.ContributingPolicyIDsAtSub); err != nil {
		return err
	}
	n, ok := new(big.Int).SetString(amount, 10)
	if !ok || !n.IsInt64() {
		return fmt.Errorf("%w: amount %q is not an int64", ErrHoldResolutionIntegrity, amount)
	}
	r.Amount = n.Int64()
	return nil
}

// HoldResolutionRequestInput is everything a caller supplies. Every actor, derived,
// pinned and hash column is forced by migration 0124: there is no field for any of
// them.
type HoldResolutionRequestInput struct {
	WithdrawalRequestID uuid.UUID
	Kind                HoldResolutionKind
	// ReasonCode matches ^[a-z][a-z0-9_]{0,63}$ (security L-6; the vocabulary is HD-CTF-7).
	ReasonCode string
	// EvidenceRefHash is the lowercase hex SHA-256 of an external evidence reference
	// (no PII ever reaches the platform). Required.
	EvidenceRefHash string
	// Note is bounded free text kept on the audit row only.
	Note string
}

func (in HoldResolutionRequestInput) validate() error {
	if in.WithdrawalRequestID == uuid.Nil {
		return fmt.Errorf("%w: withdrawal_request_id is required", ErrHoldResolutionInvalid)
	}
	if in.Kind != HoldReleaseToPlayer {
		return fmt.Errorf("%w: unknown kind", ErrHoldResolutionInvalid)
	}
	if !holdReasonRE.MatchString(in.ReasonCode) {
		return fmt.Errorf("%w: reason_code must match ^[a-z][a-z0-9_]{0,63}$", ErrHoldResolutionInvalid)
	}
	if !holdHex64RE.MatchString(in.EvidenceRefHash) {
		return fmt.Errorf("%w: evidence_ref_hash must be 64 lowercase hex characters", ErrHoldResolutionInvalid)
	}
	if len(in.Note) > 1000 {
		return fmt.Errorf("%w: note must be at most 1000 bytes", ErrHoldResolutionInvalid)
	}
	return nil
}

// HoldResolutionDecisionInput is what a decider supplies: the payload hash it
// reviewed (an approval pins exactly the payload it saw) and a reason code.
type HoldResolutionDecisionInput struct {
	Decision    ResolutionDecision
	PayloadHash string
	ReasonCode  string
}

// HoldResolutionOutcome is the result of a decision.
type HoldResolutionOutcome struct {
	Resolution HoldResolution
	ApprovalID uuid.UUID
	// Executed: this decision was the final approval and the hold was released in its
	// own transaction.
	Executed bool
	// Refused: the final approval found the preconditions no longer held and the
	// resolution ended refused_at_execution (committed, audited).
	Refused  bool
	Counted  int
	Required int
	// Expired: the resolution was found past expires_at; pending -> expired and its
	// audit row are committed, no decision is recorded, and Decide returns
	// ErrHoldResolutionExpired.
	Expired bool
}

// HoldResolutionCall identifies the actor of one governed call, derived from the
// verified token and the session actually opened - never from a body.
type HoldResolutionCall struct {
	ActorID  uuid.UUID
	TenantID uuid.UUID
	Meta     ResolutionMeta
	Proofs   *actorproof.Issuer
}

// attachProof signs and attaches the actor proof for ONE governed write. The scope
// is always platform_acting (the verifier refuses every other scope for this
// operation family).
func (c HoldResolutionCall) attachProof(ctx context.Context, tx pgx.Tx, operation, target, payloadHash string) error {
	iss := c.Proofs
	if iss == nil {
		iss = actorproof.Default()
	}
	return iss.Attach(ctx, tx, actorproof.Claims{
		Actor: c.ActorID, Scope: actorproof.ScopePlatformActing, Tenant: c.TenantID,
		Operation: operation, Target: target, PayloadHash: payloadHash,
	})
}

// WithdrawalHoldResolutionService runs governed hold-release calls against a pool.
type WithdrawalHoldResolutionService struct {
	pool   *db.Pool
	proofs *actorproof.Issuer
}

// NewWithdrawalHoldResolutionService returns a service over pool.
func NewWithdrawalHoldResolutionService(pool *db.Pool) *WithdrawalHoldResolutionService {
	return &WithdrawalHoldResolutionService{pool: pool}
}

// WithProofIssuer sets the issuer this service signs actor proofs with (default: the
// process-wide actorproof.Default()).
func (s *WithdrawalHoldResolutionService) WithProofIssuer(i *actorproof.Issuer) *WithdrawalHoldResolutionService {
	s.proofs = i
	return s
}

// runSession opens the ONE session shape migration 0124 admits: a platform
// principal acting in the target tenant (family A, ADR 0099 6.1; the database
// refuses it, CG020, unless the principal holds an in-force G-P2 grant for exactly
// that tenant). A tenant-scoped caller - any tenant role - is refused here, before
// any database work, and again by the database (no tenant policy, HR001).
//
// A-16 / K2-G4 call-site pin: principalID is `subject`, parsed from
// tenant.FromContext(ctx).Subject in THIS function; the target tenant is
// `target.tenantID`, set only by NewResolutionTarget.
func (s *WithdrawalHoldResolutionService) runSession(ctx context.Context, target ResolutionTarget, resolutionID uuid.UUID, meta ResolutionMeta, fn func(ctx context.Context, tx pgx.Tx, call HoldResolutionCall) error) error {
	tc, err := tenant.FromContext(ctx)
	if err != nil {
		return ErrHoldResolutionNotPermitted
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil || subject == uuid.Nil {
		return ErrHoldResolutionNotPermitted
	}
	if target.tenantID == uuid.Nil {
		return fmt.Errorf("%w: unvalidated target", ErrHoldResolutionInvalid)
	}
	if tc.TenantID != uuid.Nil {
		return ErrHoldResolutionNotPermitted
	}
	return s.pool.WithPlatformActingInTenant(ctx, subject, target.tenantID, resolutionID, OperationKindHoldResolution, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, tx, HoldResolutionCall{ActorID: subject, TenantID: target.tenantID, Meta: meta, Proofs: s.proofs})
	})
}

// --- request -----------------------------------------------------------------

// Request opens the caller's acting session and submits a hold resolution.
func (s *WithdrawalHoldResolutionService) Request(ctx context.Context, target ResolutionTarget, in HoldResolutionRequestInput, meta ResolutionMeta) (HoldResolution, error) {
	if err := in.validate(); err != nil {
		return HoldResolution{}, err
	}
	var out HoldResolution
	err := s.runSession(ctx, target, uuid.Nil, meta, func(ctx context.Context, tx pgx.Tx, call HoldResolutionCall) error {
		var err error
		out, err = s.requestInTx(ctx, tx, call, in)
		return err
	})
	return out, err
}

func (s *WithdrawalHoldResolutionService) requestInTx(ctx context.Context, tx pgx.Tx, call HoldResolutionCall, in HoldResolutionRequestInput) (HoldResolution, error) {
	// ADR 0111 6.4 lock order: the L1 withdrawal lock first, then (inside the guard) the
	// tenant status advisory lock SHARED and the brand row FOR SHARE.
	if _, err := withdrawal.LockForPayoutEvidence(ctx, tx, in.WithdrawalRequestID); err != nil {
		if errors.Is(err, withdrawal.ErrNotFound) {
			return HoldResolution{}, ErrHoldResolutionNotFound
		}
		return HoldResolution{}, err
	}
	// SIGNED-ACTOR-PROOF (migration 0124/0120): a FRESH proof per attempt; the id is
	// server-forced, so the target is 'new'; the digest binds the caller-supplied columns.
	if err := call.attachProof(ctx, tx, actorproof.OpHoldResolutionRequest, actorproof.TargetNew, actorproof.Digest(
		actorproof.S(call.TenantID.String()), actorproof.S(in.WithdrawalRequestID.String()), actorproof.S(string(in.Kind)),
		actorproof.S(in.EvidenceRefHash), actorproof.S(in.ReasonCode))); err != nil {
		return HoldResolution{}, err
	}
	var r HoldResolution
	// Every NOT NULL column migration 0124's guard forces is given a placeholder here
	// (K2/K3's pattern): the BEFORE INSERT trigger overwrites it.
	row := tx.QueryRow(ctx, `
		INSERT INTO withdrawal_hold_resolutions
			(tenant_id, withdrawal_request_id, kind, brand_id, player_account_id, wallet_id, amount, asset_code,
			 withdrawal_state_at_submission, tenant_status_at_submission, brand_status_at_submission, reason_code, evidence_ref_hash,
			 payload_hash, requested_by, requested_by_scope, requested_by_person_id, required_at_submission, contributing_policy_ids, expires_at)
		VALUES ($1, $2, $3, $4, $4, $4, 1, '-', '-', '-', '-', $5, $6, '-', $4, 'platform_acting', $4, 1, '{}', now())
		RETURNING `+holdResolutionColumns,
		call.TenantID, in.WithdrawalRequestID, string(in.Kind), uuid.Nil, in.ReasonCode, in.EvidenceRefHash)
	if err := scanHoldResolution(row, &r); err != nil {
		return HoldResolution{}, err
	}
	if err := recordHoldResolutionAudit(ctx, tx, call, "requested", r, nil, nil, map[string]any{
		"note": in.Note, "required_at_submission": r.RequiredAtSubmission,
	}); err != nil {
		return HoldResolution{}, err
	}
	return r, nil
}

// --- reads -------------------------------------------------------------------

// GetHoldResolutionInTx reads one resolution visible to tx's session.
func GetHoldResolutionInTx(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (HoldResolution, error) {
	var r HoldResolution
	err := scanHoldResolution(tx.QueryRow(ctx, `SELECT `+holdResolutionColumns+` FROM withdrawal_hold_resolutions WHERE id = $1 AND tenant_id = $2`, id, tenantID), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return HoldResolution{}, ErrHoldResolutionNotFound
	}
	return r, err
}

// Get opens the caller's acting session and reads one resolution.
func (s *WithdrawalHoldResolutionService) Get(ctx context.Context, target ResolutionTarget, id uuid.UUID, meta ResolutionMeta) (HoldResolution, error) {
	var out HoldResolution
	err := s.runSession(ctx, target, id, meta, func(ctx context.Context, tx pgx.Tx, call HoldResolutionCall) error {
		var err error
		out, err = GetHoldResolutionInTx(ctx, tx, call.TenantID, id)
		return err
	})
	return out, err
}

// List opens the caller's acting session and lists a tenant's resolutions, newest
// first (bounded).
func (s *WithdrawalHoldResolutionService) List(ctx context.Context, target ResolutionTarget, limit int, meta ResolutionMeta) ([]HoldResolution, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []HoldResolution
	err := s.runSession(ctx, target, uuid.Nil, meta, func(ctx context.Context, tx pgx.Tx, call HoldResolutionCall) error {
		rows, err := tx.Query(ctx, `SELECT `+holdResolutionColumns+` FROM withdrawal_hold_resolutions WHERE tenant_id = $1 ORDER BY created_at DESC, id LIMIT $2`,
			call.TenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r HoldResolution
			if err := scanHoldResolution(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// --- cancel ------------------------------------------------------------------

// Cancel opens the caller's acting session and cancels a pending resolution
// (requester only, trigger-enforced; a fresh proof per attempt).
func (s *WithdrawalHoldResolutionService) Cancel(ctx context.Context, target ResolutionTarget, id uuid.UUID, meta ResolutionMeta) (HoldResolution, error) {
	var out HoldResolution
	err := s.runSession(ctx, target, id, meta, func(ctx context.Context, tx pgx.Tx, call HoldResolutionCall) error {
		before, err := lockHoldResolution(ctx, tx, call.TenantID, id)
		if err != nil {
			return err
		}
		if before.State != ResolutionPending {
			return ErrHoldResolutionNotPending
		}
		if err := call.attachProof(ctx, tx, actorproof.OpHoldResolutionCancel, id.String(), before.PayloadHash); err != nil {
			return err
		}
		out, err = setHoldResolutionState(ctx, tx, id, ResolutionCancelled, nil)
		if err != nil {
			return err
		}
		st := before.State
		return recordHoldResolutionAudit(ctx, tx, call, "cancelled", out, &st, nil, nil)
	})
	return out, err
}

func lockHoldResolution(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (HoldResolution, error) {
	var r HoldResolution
	err := scanHoldResolution(tx.QueryRow(ctx, `SELECT `+holdResolutionColumns+` FROM withdrawal_hold_resolutions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return HoldResolution{}, ErrHoldResolutionNotFound
	}
	return r, err
}

// setHoldResolutionState moves the resolution's state; the migration 0124 guard
// decides whether the transition is legal (and forces executed_txid itself).
func setHoldResolutionState(ctx context.Context, tx pgx.Tx, id uuid.UUID, to ResolutionState, refusalCode *string) (HoldResolution, error) {
	var r HoldResolution
	err := scanHoldResolution(tx.QueryRow(ctx, `UPDATE withdrawal_hold_resolutions SET state = $2, refusal_code = $3 WHERE id = $1 RETURNING `+holdResolutionColumns,
		id, string(to), refusalCode), &r)
	return r, err
}

// --- decide + execute ----------------------------------------------------------

// holdExecStatus is migration 0124's withdrawal_hold_resolution_execution_status():
// the ONE counting implementation, shared with the -> executing guard.
type holdExecStatus struct {
	Required       int
	Counted        int
	CountedIDs     []uuid.UUID
	RequesterValid bool
	Contributing   []uuid.UUID
	TenantStatus   *string
	BrandStatus    *string
	Enabled        bool
}

func readHoldExecStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID) (holdExecStatus, error) {
	var st holdExecStatus
	err := tx.QueryRow(ctx, `SELECT required, counted, counted_approval_ids, requester_valid, contributing_policy_ids, tenant_status, brand_status, enabled
		FROM withdrawal_hold_resolution_execution_status($1)`, id).
		Scan(&st.Required, &st.Counted, &st.CountedIDs, &st.RequesterValid, &st.Contributing, &st.TenantStatus, &st.BrandStatus, &st.Enabled)
	return st, err
}

// testHookHoldResolutionAfterLocks, when set by an in-package test, runs after the
// L1 withdrawal lock, the tenant/brand scope locks and the resolution lock are held
// (before the approval insert) - so concurrency tests can race a reactivation, a
// submit or a second execute against a real execution deterministically.
var testHookHoldResolutionAfterLocks func(ctx context.Context, id uuid.UUID)

// Decide opens the caller's acting session and decides.
func (s *WithdrawalHoldResolutionService) Decide(ctx context.Context, target ResolutionTarget, id uuid.UUID, in HoldResolutionDecisionInput, meta ResolutionMeta) (HoldResolutionOutcome, error) {
	var out HoldResolutionOutcome
	err := s.runSession(ctx, target, id, meta, func(ctx context.Context, tx pgx.Tx, call HoldResolutionCall) error {
		var err error
		out, err = s.decideInTx(ctx, tx, call, id, in)
		return err
	})
	if err == nil && out.Expired {
		return out, ErrHoldResolutionExpired
	}
	return out, err
}

// decideInTx records a decision and, when it is the final approval that brings the
// counted approvals to the required number, releases the hold in THIS transaction
// (no approved-but-unexecuted window). Lock order is ADR 0111 6.5:
//
//	1 L1 withdrawal FOR UPDATE
//	2 tenant status advisory lock SHARED + brand FOR SHARE
//	3 resolution FOR UPDATE
//	4 approval insert
//	5 staff, then grants, FOR SHARE (ascending id)
//	6 recount and re-check
//	7 executing
//	8 posting (L3/L4, inside ledger.Post)
//	9 executed, audit, commit
func (s *WithdrawalHoldResolutionService) decideInTx(ctx context.Context, tx pgx.Tx, call HoldResolutionCall, id uuid.UUID, in HoldResolutionDecisionInput) (HoldResolutionOutcome, error) {
	if in.Decision != ResolutionApprove && in.Decision != ResolutionReject {
		return HoldResolutionOutcome{}, fmt.Errorf("%w: decision must be approve or reject", ErrHoldResolutionInvalid)
	}
	if !holdHex64RE.MatchString(in.PayloadHash) {
		return HoldResolutionOutcome{}, fmt.Errorf("%w: payload_hash must be 64 lowercase hex characters", ErrHoldResolutionInvalid)
	}
	if !holdReasonRE.MatchString(in.ReasonCode) {
		return HoldResolutionOutcome{}, fmt.Errorf("%w: reason_code must match ^[a-z][a-z0-9_]{0,63}$", ErrHoldResolutionInvalid)
	}

	peek, err := GetHoldResolutionInTx(ctx, tx, call.TenantID, id)
	if err != nil {
		return HoldResolutionOutcome{}, err
	}
	// Step 1.
	wr, err := withdrawal.LockForPayoutEvidence(ctx, tx, peek.WithdrawalRequestID)
	if err != nil {
		return HoldResolutionOutcome{}, fmt.Errorf("payments: lock withdrawal for hold resolution: %w", err)
	}
	// Step 2: the H-SEC lock discipline; the statuses read here are the ones every later
	// check of this transaction uses.
	var tenantStatus, brandStatus *string
	if err := tx.QueryRow(ctx, `SELECT tenant_status, brand_status FROM withdrawal_hold_resolution_lock_scope($1, $2)`,
		peek.TenantID, peek.BrandID).Scan(&tenantStatus, &brandStatus); err != nil {
		return HoldResolutionOutcome{}, fmt.Errorf("payments: lock tenant/brand status: %w", err)
	}
	// Step 3.
	res, err := lockHoldResolution(ctx, tx, call.TenantID, id)
	if err != nil {
		return HoldResolutionOutcome{}, err
	}
	if res.State != ResolutionPending {
		return HoldResolutionOutcome{Resolution: res}, ErrHoldResolutionNotPending
	}
	if testHookHoldResolutionAfterLocks != nil {
		testHookHoldResolutionAfterLocks(ctx, id)
	}
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT now() >= $1::timestamptz`, res.ExpiresAt).Scan(&expired); err != nil {
		return HoldResolutionOutcome{}, err
	}
	if expired {
		after, err := setHoldResolutionState(ctx, tx, id, ResolutionExpired, nil)
		if err != nil {
			return HoldResolutionOutcome{}, err
		}
		before := res.State
		return HoldResolutionOutcome{Resolution: after, Expired: true}, recordHoldResolutionAudit(ctx, tx, call, "expired", after, &before, nil, nil)
	}

	// Step 4: the approval insert (migration 0124 triggers: platform_acting only, payload
	// hash, in-force grant, distinct Person, beneficiary, S-2(iii), pending). A FRESH
	// proof per attempt.
	op := actorproof.OpHoldResolutionApprove
	if in.Decision == ResolutionReject {
		op = actorproof.OpHoldResolutionReject
	}
	if err := call.attachProof(ctx, tx, op, id.String(), in.PayloadHash); err != nil {
		return HoldResolutionOutcome{}, err
	}
	var approvalID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO withdrawal_hold_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, $5, 'platform_acting', $5, 0, $6)
		RETURNING id`,
		call.TenantID, id, string(in.Decision), in.PayloadHash, uuid.Nil, in.ReasonCode).Scan(&approvalID); err != nil {
		return HoldResolutionOutcome{}, err
	}
	out := HoldResolutionOutcome{ApprovalID: approvalID}

	if in.Decision == ResolutionReject {
		after, err := GetHoldResolutionInTx(ctx, tx, call.TenantID, id)
		if err != nil {
			return HoldResolutionOutcome{}, err
		}
		out.Resolution = after
		before := res.State
		return out, recordHoldResolutionAudit(ctx, tx, call, "rejected", after, &before, nil, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
		})
	}

	// Step 5-6: evaluate required and the candidate count; then lock staff and grants and
	// recount under the locks.
	st, err := readHoldExecStatus(ctx, tx, id)
	if err != nil {
		return HoldResolutionOutcome{}, err
	}
	out.Counted, out.Required = st.Counted, st.Required
	if !st.RequesterValid || st.Counted < st.Required {
		out.Resolution = res
		return out, recordHoldResolutionAudit(ctx, tx, call, "approved", res, nil, nil, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
			"counted": st.Counted, "required": st.Required, "requester_valid": st.RequesterValid,
			"preconditions_hold": holdPreconditionRefusal(ctx, tx, wr, tenantStatus, brandStatus) == "",
		})
	}
	if err := lockHoldResolutionStaffAndGrants(ctx, tx, res); err != nil {
		return HoldResolutionOutcome{}, err
	}
	st, err = readHoldExecStatus(ctx, tx, id)
	if err != nil {
		return HoldResolutionOutcome{}, err
	}
	out.Counted, out.Required = st.Counted, st.Required
	if !st.RequesterValid || st.Counted < st.Required {
		out.Resolution = res
		return out, recordHoldResolutionAudit(ctx, tx, call, "approved", res, nil, nil, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
			"counted": st.Counted, "required": st.Required, "requester_valid": st.RequesterValid,
		})
	}

	// The executor's own re-check of the ADR 0111 6.4 preconditions (the database re-runs
	// the same checks at -> executing).
	refusal := ""
	if !st.Enabled {
		refusal = holdRefusedPolicy
	} else {
		refusal = holdPreconditionRefusal(ctx, tx, wr, tenantStatus, brandStatus)
	}
	if refusal != "" {
		code := refusal
		after, err := setHoldResolutionState(ctx, tx, id, ResolutionRefusedAtExecute, &code)
		if err != nil {
			return HoldResolutionOutcome{}, err
		}
		out.Resolution = after
		out.Refused = true
		before := res.State
		return out, recordHoldResolutionAudit(ctx, tx, call, "refused", after, &before, st.CountedIDs, map[string]any{
			"approval_id": approvalID.String(), "withdrawal_state": string(wr.State),
			"tenant_status_now": derefStr(tenantStatus), "brand_status_now": derefStr(brandStatus),
		})
	}

	// Step 7: executing; executed_txid = txid_current() is forced by the guard, which
	// re-verifies the count and every precondition.
	if _, err := setHoldResolutionState(ctx, tx, id, ResolutionExecuting, nil); err != nil {
		return HoldResolutionOutcome{}, err
	}
	// Step 8: the posting and the approved -> rejected CAS.
	if err := withdrawal.ReleaseForGovernedResolution(ctx, tx, wr.ID); err != nil {
		return HoldResolutionOutcome{}, fmt.Errorf("payments: governed hold release: %w", err)
	}
	wrAfter, err := withdrawal.GetByID(ctx, tx, wr.ID)
	if err != nil {
		return HoldResolutionOutcome{}, err
	}
	if wrAfter.ReleaseLedgerTransactionID == nil || wrAfter.State != withdrawal.StateRejected {
		return HoldResolutionOutcome{}, fmt.Errorf("%w: withdrawal %s has no release transaction after the governed release", ErrHoldResolutionIntegrity, wr.ID)
	}
	// Step 9: executed + link (the deferred trigger verifies the two-leg shape).
	var after HoldResolution
	if err := scanHoldResolution(tx.QueryRow(ctx, `UPDATE withdrawal_hold_resolutions SET state = 'executed', ledger_transaction_id = $2
		WHERE id = $1 RETURNING `+holdResolutionColumns, id, wrAfter.ReleaseLedgerTransactionID), &after); err != nil {
		return HoldResolutionOutcome{}, err
	}
	out.Resolution = after
	out.Executed = true
	before := res.State
	return out, recordHoldResolutionAudit(ctx, tx, call, "executed", after, &before, st.CountedIDs, map[string]any{
		"approval_id": approvalID.String(), "required_at_execution": st.Required,
		"contributing_policy_ids_at_execution": resolutionUUIDStrings(st.Contributing),
		"withdrawal_state_before":              string(wr.State), "withdrawal_state_after": string(wrAfter.State),
		"hold_ledger_transaction_id": uuidPtrString(wr.HoldLedgerTransactionID),
	})
}

// holdPreconditionRefusal is the executor's Go-side restatement of the ADR 0111 6.4
// preconditions: "" when they all hold, else the closed refusal code the resolution
// ends refused_at_execution with. The withdrawal row is the one locked in step 1;
// the statuses are the ones read under the step 2 locks.
func holdPreconditionRefusal(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest, tenantStatus, brandStatus *string) string {
	if wr.State != withdrawal.StateApproved {
		return holdRefusedNotApproved
	}
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1 AND tenant_id = $2`,
		wr.ID, wr.TenantID).Scan(&attempts); err != nil || attempts != 0 {
		return holdRefusedAttemptExists
	}
	// A-19: a release is refused when the tenant AND the brand are active again. An
	// unreadable status is treated as active (it cannot prove the precondition).
	if tenantStatus == nil || brandStatus == nil || (*tenantStatus == "active" && *brandStatus == "active") {
		return holdRefusedActiveAgain
	}
	return ""
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func uuidPtrString(u *uuid.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// lockHoldResolutionStaffAndGrants takes the A8 L1 FOR SHARE locks: staff_users rows of
// the requester and every candidate approver (ascending id), then their grants in
// force at now() (ascending id). Only id, tenant_id, role, status, person_id are ever
// selected from staff_users (A-19 column discipline).
func lockHoldResolutionStaffAndGrants(ctx context.Context, tx pgx.Tx, r HoldResolution) error {
	var approvers []uuid.UUID
	rows, err := tx.Query(ctx, `SELECT decided_by FROM withdrawal_hold_resolution_approvals
		WHERE resolution_id = $1 AND decision = 'approve' AND payload_hash = $2 ORDER BY decided_by`, r.ID, r.PayloadHash)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		approvers = append(approvers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	staff := append([]uuid.UUID{r.RequestedBy}, approvers...)
	if err := drainRows(tx.Query(ctx, `SELECT id, tenant_id, role, status, person_id FROM staff_users
		WHERE id = ANY($1) ORDER BY id FOR SHARE`, staff)); err != nil {
		return fmt.Errorf("payments: lock staff rows: %w", err)
	}
	if err := drainRows(tx.Query(ctx, `SELECT id FROM staff_capability_grants
		WHERE tenant_id = $1 AND revoked_at IS NULL AND valid_from <= now() AND (valid_until IS NULL OR now() < valid_until)
		  AND ((grantee_staff_id = $2 AND capability = 'withdrawal_hold_resolution:request')
		    OR (grantee_staff_id = ANY($3) AND capability = 'withdrawal_hold_resolution:approve'))
		ORDER BY id FOR SHARE`, r.TenantID, r.RequestedBy, approvers)); err != nil {
		return fmt.Errorf("payments: lock grants: %w", err)
	}
	return nil
}

// recordHoldResolutionAudit writes one audit_log row for a resolution transition in the
// same transaction: actor, approvers, withdrawal, reason, resulting state and the
// pinned statuses. Under an acting session migration 0112's audit_log_acting_actor
// trigger re-forces the actor and tenant anyway. The evidence HASH is recorded, never
// a free-text reference. Action: withdrawal.hold_resolution_<event>.
func recordHoldResolutionAudit(ctx context.Context, tx pgx.Tx, call HoldResolutionCall, event string, r HoldResolution, before *ResolutionState, approvers []uuid.UUID, extra map[string]any) error {
	md := map[string]any{
		"actor_scope":                actorproof.ScopePlatformActing,
		"resolution_id":              r.ID.String(),
		"withdrawal_request_id":      r.WithdrawalRequestID.String(),
		"kind":                       string(r.Kind),
		"asset_code":                 r.AssetCode,
		"amount_minor_units":         fmt.Sprint(r.Amount),
		"reason_code":                r.ReasonCode,
		"evidence_ref_hash":          r.EvidenceRefHash,
		"payload_hash":               r.PayloadHash,
		"after_state":                string(r.State),
		"requested_by":               r.RequestedBy.String(),
		"acting_user":                call.ActorID.String(),
		"withdrawal_state_at_submit": r.WithdrawalStateAtSubmit,
		"tenant_status_at_submit":    r.TenantStatusAtSubmission,
		"brand_status_at_submit":     r.BrandStatusAtSubmission,
		"required_at_submission":     r.RequiredAtSubmission,
		"expires_at":                 r.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	if before != nil {
		md["before_state"] = string(*before)
	}
	if len(approvers) > 0 {
		// The counted approvals; the approver principals are read back from them.
		ids := resolutionUUIDStrings(approvers)
		md["counted_approval_ids"] = ids
		var deciders []string
		rows, err := tx.Query(ctx, `SELECT decided_by::text FROM withdrawal_hold_resolution_approvals WHERE id = ANY($1) ORDER BY decided_at, id`, approvers)
		if err == nil {
			for rows.Next() {
				var d string
				if rows.Scan(&d) == nil {
					deciders = append(deciders, d)
				}
			}
			rows.Close()
		}
		md["approvers"] = deciders
	}
	if r.TenantStatusAtExecution != nil {
		md["tenant_status_at_execution"] = *r.TenantStatusAtExecution
	}
	if r.BrandStatusAtExecution != nil {
		md["brand_status_at_execution"] = *r.BrandStatusAtExecution
	}
	if r.PayoutInstrumentID != nil {
		md["payout_instrument_id"] = r.PayoutInstrumentID.String()
	}
	if r.LedgerTransactionID != nil {
		md["ledger_transaction_id"] = r.LedgerTransactionID.String()
	}
	if r.RefusalCode != nil {
		md["refusal_code"] = *r.RefusalCode
	}
	for k, v := range extra {
		md[k] = v
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   call.TenantID,
		ActorType:  audit.ActorStaff,
		ActorID:    call.ActorID,
		Action:     "withdrawal.hold_resolution_" + event,
		TargetType: "withdrawal_hold_resolution",
		TargetID:   r.ID.String(),
		Outcome:    audit.OutcomeSuccess,
		IPAddress:  call.Meta.IPAddress,
		UserAgent:  call.Meta.UserAgent,
		RequestID:  call.Meta.RequestID,
		Metadata:   md,
	})
}

// --- error classification --------------------------------------------------------

// ClassifyHoldResolutionError maps an error to a class, by sentinel or SQLSTATE only -
// never by message text.
func ClassifyHoldResolutionError(err error) ResolutionErrClass {
	switch {
	case err == nil:
		return ResolutionErrNone
	case errors.Is(err, ErrHoldResolutionNotFound), errors.Is(err, pgx.ErrNoRows):
		return ResolutionErrNotFound
	case errors.Is(err, ErrHoldResolutionNotPermitted):
		return ResolutionErrForbidden
	case errors.Is(err, ErrHoldResolutionNotPending):
		return ResolutionErrConflict
	case errors.Is(err, ErrHoldResolutionExpired):
		return ResolutionErrExpired
	case errors.Is(err, ErrHoldResolutionInvalid):
		return ResolutionErrInvalid
	case errors.Is(err, withdrawal.ErrStateConflict):
		return ResolutionErrConflict
	}
	code := ResolutionSQLState(err)
	switch {
	case code == "HR014":
		return ResolutionErrDisabled
	case code == "HR010", code == "HR041":
		return ResolutionErrPrecondition
	case code == "40001", code == "40P01":
		return ResolutionErrRetryable
	case code == "HR001", code == "HR003", code == "HR011", code == "HR032", code == "42501":
		return ResolutionErrForbidden
	case len(code) == 5 && code[:2] == "AP":
		// SIGNED-ACTOR-PROOF refusal (migration 0120): fail closed.
		return ResolutionErrForbidden
	case code == "CG001", code == "CG002", code == "CG020", code == "HR002":
		return ResolutionErrSession
	case len(code) == 5 && code[:2] == "HR", code == "CG030", code == "CG031", code == "23505":
		return ResolutionErrConflict
	case code == "23514", code == "22003", code == "23503":
		return ResolutionErrInvalid
	}
	return ResolutionErrOther
}

// The CLOSED token set: the only error strings a client ever sees. No SQL text and
// no Person ids.
const (
	TokenHoldResolveDisabled     = "hold_resolution_disabled"
	TokenHoldResolveNotPermitted = "hold_resolution_not_permitted"
	TokenHoldResolvePrecondition = "hold_resolution_precondition_failed"
	TokenHoldResolveConflict     = "hold_resolution_conflict"
	TokenHoldResolveExpired      = "hold_resolution_expired"
	TokenHoldResolveNotFound     = "hold_resolution_not_found"
)

// HoldResolutionToken returns the closed token for a class, or "" for a class that maps
// to a validation (400) or an internal (500) response.
func HoldResolutionToken(class ResolutionErrClass) string {
	switch class {
	case ResolutionErrDisabled:
		return TokenHoldResolveDisabled
	case ResolutionErrForbidden, ResolutionErrSession:
		return TokenHoldResolveNotPermitted
	case ResolutionErrPrecondition, ResolutionErrNotResolvable:
		return TokenHoldResolvePrecondition
	case ResolutionErrConflict, ResolutionErrRetryable:
		return TokenHoldResolveConflict
	case ResolutionErrExpired:
		return TokenHoldResolveExpired
	case ResolutionErrNotFound:
		return TokenHoldResolveNotFound
	}
	return ""
}
