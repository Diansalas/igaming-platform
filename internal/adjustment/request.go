package adjustment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
)

// Request is one ledger_adjustment_requests row.
type Request struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	WalletID               uuid.UUID
	PlayerAccountID        uuid.UUID
	BrandID                uuid.UUID
	AccountType            string
	AssetCode              string
	Direction              Direction
	Amount                 int64
	ReasonCode             ReasonCode
	CausationTransactionID *uuid.UUID
	EvidenceRefHash        *string
	NoteHash               string
	PayloadHash            string
	InitiatedBy            uuid.UUID
	InitiatedByScope       string
	InitiatedByPersonID    uuid.UUID
	TenantStatusAtSubmit   string
	RequiredAtSubmission   int
	ContributingPolicyIDs  []uuid.UUID
	State                  State
	ExpiresAt              time.Time
	LedgerTransactionID    *uuid.UUID
	RefusalCode            *string
	CreatedAt              time.Time
	ClosedAt               *time.Time
	IdempotencyKey         string
	RequiredAtExecution    *int
	TenantStatusAtExecute  *string
	ContributingAtExecute  []uuid.UUID
}

// requestColumns is the single SELECT list for ledger_adjustment_requests.
// amount is NUMERIC(38,0) bounded by CHECK to int64 max, read as text and
// parsed exactly (never through a float).
const requestColumns = `id, tenant_id, wallet_id, player_account_id, brand_id, account_type, asset_code, direction,
	amount::text, reason_code, causation_transaction_id, evidence_ref_hash, note_hash, payload_hash,
	initiated_by, initiated_by_scope, initiated_by_person_id, tenant_status_at_submission,
	required_at_submission, contributing_policy_ids, state, expires_at, ledger_transaction_id,
	refusal_code, created_at, closed_at, idempotency_key, required_at_execution,
	tenant_status_at_execution, contributing_policy_ids_at_execution`

func scanRequest(row pgx.Row, r *Request) error {
	var amount string
	if err := row.Scan(&r.ID, &r.TenantID, &r.WalletID, &r.PlayerAccountID, &r.BrandID, &r.AccountType, &r.AssetCode,
		&r.Direction, &amount, &r.ReasonCode, &r.CausationTransactionID, &r.EvidenceRefHash, &r.NoteHash, &r.PayloadHash,
		&r.InitiatedBy, &r.InitiatedByScope, &r.InitiatedByPersonID, &r.TenantStatusAtSubmit,
		&r.RequiredAtSubmission, &r.ContributingPolicyIDs, &r.State, &r.ExpiresAt, &r.LedgerTransactionID,
		&r.RefusalCode, &r.CreatedAt, &r.ClosedAt, &r.IdempotencyKey, &r.RequiredAtExecution,
		&r.TenantStatusAtExecute, &r.ContributingAtExecute); err != nil {
		return err
	}
	n, ok := new(big.Int).SetString(amount, 10)
	if !ok || !n.IsInt64() {
		return fmt.Errorf("%w: amount %q is not an int64", ErrIntegrity, amount)
	}
	r.Amount = n.Int64()
	return nil
}

// SubmitInput is everything a caller supplies. Every actor, derived,
// pinned and hash column is forced by migration 0113 - there is no field
// for any of them.
type SubmitInput struct {
	WalletID               uuid.UUID
	AssetCode              string
	Direction              Direction
	Amount                 int64
	ReasonCode             ReasonCode
	CausationTransactionID *uuid.UUID
	// EvidenceRefHash is a lowercase hex SHA-256 of an external evidence
	// reference (no PII ever reaches the platform) - required by
	// compensating_entry and external_instruction.
	EvidenceRefHash *string
	// Note is bounded free text (1-1000 bytes, no control characters but
	// \n). Only its hash is stored on the request; the note itself goes
	// to the audit metadata.
	Note string
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateNote mirrors migration 0113's ledger_adjustment_note_hash
// check (C-100-5), so a bad note is refused before any SQL.
func ValidateNote(note string) error {
	if len(note) < 1 || len(note) > 1000 {
		return fmt.Errorf("%w: note must be 1-1000 bytes", ErrInvalidInput)
	}
	if !utf8.ValidString(note) {
		return fmt.Errorf("%w: note must be valid UTF-8", ErrInvalidInput)
	}
	for _, r := range note {
		if r == '\n' {
			continue
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return fmt.Errorf("%w: note may not contain control characters other than newline", ErrInvalidInput)
		}
	}
	return nil
}

func (in SubmitInput) validate() error {
	if in.WalletID == uuid.Nil {
		return fmt.Errorf("%w: wallet_id is required", ErrInvalidInput)
	}
	if in.AssetCode == "" || len(in.AssetCode) > 16 {
		return fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if in.Direction != DirectionCreditPlayer && in.Direction != DirectionDebitPlayer {
		return fmt.Errorf("%w: direction must be credit_player or debit_player", ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return fmt.Errorf("%w: amount must be a positive integer in minor units", ErrInvalidInput)
	}
	switch in.ReasonCode {
	case ReasonOperationalErrorCorrection, ReasonCompensatingEntry, ReasonGoodwillCredit, ReasonExternalInstruction:
	default:
		return fmt.Errorf("%w: unknown reason_code", ErrInvalidInput)
	}
	if in.EvidenceRefHash != nil && !hex64.MatchString(*in.EvidenceRefHash) {
		return fmt.Errorf("%w: evidence_ref_hash must be 64 lowercase hex characters", ErrInvalidInput)
	}
	return ValidateNote(in.Note)
}

// SubmitInTx inserts a request in tx (a tenant or acting session). The id
// is chosen here so the acting-session open audit row can name it.
func SubmitInTx(ctx context.Context, tx pgx.Tx, call Call, id uuid.UUID, in SubmitInput) (Request, error) {
	if err := in.validate(); err != nil {
		return Request{}, err
	}
	// SIGNED-ACTOR-PROOF (migration 0120): bind the proof to this actor, this
	// request id and the caller-supplied payload (the same digest the
	// zz_actor_proof_guard trigger recomputes from the row).
	noteSum := sha256.Sum256([]byte(in.Note)) // = ledger_adjustment_note_hash(note)
	if err := call.attachProof(ctx, tx, actorproof.OpAdjustmentInitiate, id.String(), actorproof.Digest(
		actorproof.S(call.TenantID.String()), actorproof.S(in.WalletID.String()), actorproof.S(in.AssetCode),
		actorproof.S(string(in.Direction)), actorproof.S(fmt.Sprint(in.Amount)), actorproof.S(string(in.ReasonCode)),
		actorproof.UUIDP(in.CausationTransactionID), in.EvidenceRefHash, actorproof.S(hex.EncodeToString(noteSum[:])))); err != nil {
		return Request{}, err
	}
	var r Request
	row := tx.QueryRow(ctx, `
		INSERT INTO ledger_adjustment_requests
			(id, tenant_id, wallet_id, player_account_id, brand_id, asset_code, direction, amount, reason_code,
			 causation_transaction_id, evidence_ref_hash, note_hash, payload_hash, initiated_by, initiated_by_scope,
			 initiated_by_person_id, tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7::numeric, $8, $9, $10, ledger_adjustment_note_hash($11),
			'-', $4, 'tenant', $4, '-', 1, '{}', now())
		RETURNING `+requestColumns,
		id, call.TenantID, in.WalletID, uuid.Nil, in.AssetCode, string(in.Direction), fmt.Sprint(in.Amount), string(in.ReasonCode),
		in.CausationTransactionID, in.EvidenceRefHash, in.Note)
	if err := scanRequest(row, &r); err != nil {
		return Request{}, err
	}
	if err := recordAudit(ctx, tx, call, "ledger_adjustment.submitted", r, nil, map[string]any{
		"note":                    in.Note,
		"required_at_submission":  r.RequiredAtSubmission,
		"contributing_policy_ids": uuidStrings(r.ContributingPolicyIDs),
		"tenant_status":           r.TenantStatusAtSubmit,
	}); err != nil {
		return Request{}, err
	}
	return r, nil
}

// GetInTx reads one request visible to tx's session.
func GetInTx(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID) (Request, error) {
	var r Request
	err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM ledger_adjustment_requests WHERE id = $1 AND tenant_id = $2`,
		requestID, tenantID), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

// ListInTx lists a tenant's requests, newest first (bounded).
func ListInTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Request, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM ledger_adjustment_requests WHERE tenant_id = $1 ORDER BY created_at DESC, id LIMIT $2`,
		tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		if err := scanRequest(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CancelInTx cancels a pending request (initiator only, trigger-enforced).
func CancelInTx(ctx context.Context, tx pgx.Tx, call Call, requestID uuid.UUID) (Request, error) {
	before, err := lockRequest(ctx, tx, call.TenantID, requestID)
	if err != nil {
		return Request{}, err
	}
	if before.State != StatePending {
		return Request{}, ErrNotPending
	}
	if err := call.attachProof(ctx, tx, actorproof.OpAdjustmentCancel, requestID.String(), before.PayloadHash); err != nil {
		return Request{}, err
	}
	after, err := setState(ctx, tx, requestID, StateCancelled, nil)
	if err != nil {
		return Request{}, err
	}
	return after, recordAudit(ctx, tx, call, "ledger_adjustment.cancelled", after, &before.State, nil)
}

// lockRequest is ADR 0082 A8's class-L1 request lock (FOR UPDATE).
func lockRequest(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID) (Request, error) {
	var r Request
	err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM ledger_adjustment_requests WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		requestID, tenantID), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

// setState moves the request's state; the migration 0113 guard decides
// whether the transition is legal (and forces executed_txid itself).
func setState(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, to State, refusalCode *string) (Request, error) {
	var r Request
	err := scanRequest(tx.QueryRow(ctx, `UPDATE ledger_adjustment_requests SET state = $2, refusal_code = $3 WHERE id = $1 RETURNING `+requestColumns,
		requestID, string(to), refusalCode), &r)
	return r, err
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// recordAudit writes one audit_log row for a request transition in the
// same transaction (ADR 0100 §9). Under an acting session migration 0112's
// audit_log_acting_actor trigger re-forces the actor and tenant anyway.
func recordAudit(ctx context.Context, tx pgx.Tx, call Call, action string, r Request, before *State, extra map[string]any) error {
	md := map[string]any{
		"actor_scope":             call.Scope,
		"request_id":              r.ID.String(),
		"wallet_id":               r.WalletID.String(),
		"player_account_id":       r.PlayerAccountID.String(),
		"asset_code":              r.AssetCode,
		"direction":               string(r.Direction),
		"amount_minor_units":      fmt.Sprint(r.Amount),
		"reason_code":             string(r.ReasonCode),
		"payload_hash":            r.PayloadHash,
		"after_state":             string(r.State),
		"contributing_policy_ids": uuidStrings(r.ContributingPolicyIDs),
		"tenant_status":           r.TenantStatusAtSubmit,
	}
	if before != nil {
		md["before_state"] = string(*before)
	}
	if r.LedgerTransactionID != nil {
		md["ledger_transaction_id"] = r.LedgerTransactionID.String()
	}
	if r.RefusalCode != nil {
		md["refusal_code"] = *r.RefusalCode
	}
	if r.CausationTransactionID != nil {
		md["causation_transaction_id"] = r.CausationTransactionID.String()
	}
	for k, v := range extra {
		md[k] = v
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   call.TenantID,
		ActorType:  audit.ActorStaff,
		ActorID:    call.ActorID,
		Action:     action,
		TargetType: "ledger_adjustment_request",
		TargetID:   r.ID.String(),
		Outcome:    audit.OutcomeSuccess,
		IPAddress:  call.Meta.IPAddress,
		UserAgent:  call.Meta.UserAgent,
		RequestID:  call.Meta.RequestID,
		Metadata:   md,
	})
}

// Submit opens the caller's session and submits.
func (s *Service) Submit(ctx context.Context, target Target, in SubmitInput, meta Meta) (Request, error) {
	id := uuid.New()
	var out Request
	err := s.runSession(ctx, target, id, meta, func(ctx context.Context, tx pgx.Tx, call Call) error {
		var err error
		out, err = SubmitInTx(ctx, tx, call, id, in)
		return err
	})
	return out, err
}

// Cancel opens the caller's session and cancels.
func (s *Service) Cancel(ctx context.Context, target Target, requestID uuid.UUID, meta Meta) (Request, error) {
	var out Request
	err := s.runSession(ctx, target, requestID, meta, func(ctx context.Context, tx pgx.Tx, call Call) error {
		var err error
		out, err = CancelInTx(ctx, tx, call, requestID)
		return err
	})
	return out, err
}

// Get opens the caller's session and reads one request.
func (s *Service) Get(ctx context.Context, target Target, requestID uuid.UUID, meta Meta) (Request, error) {
	var out Request
	err := s.runSession(ctx, target, requestID, meta, func(ctx context.Context, tx pgx.Tx, call Call) error {
		var err error
		out, err = GetInTx(ctx, tx, call.TenantID, requestID)
		return err
	})
	return out, err
}

// List opens the caller's session and lists requests.
func (s *Service) List(ctx context.Context, target Target, limit int, meta Meta) ([]Request, error) {
	var out []Request
	err := s.runSession(ctx, target, uuid.Nil, meta, func(ctx context.Context, tx pgx.Tx, call Call) error {
		var err error
		out, err = ListInTx(ctx, tx, call.TenantID, limit)
		return err
	})
	return out, err
}
