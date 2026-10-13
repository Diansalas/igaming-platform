package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// Store holds the keys the non-factor stores hash with. It is stateless
// otherwise; every method takes the caller's already-scoped pgx.Tx.
type Store struct {
	keys *staffmfa.Keys
}

// New returns a Store over keys.
func New(keys *staffmfa.Keys) (*Store, error) {
	if keys == nil {
		return nil, ErrInvalidInput
	}
	return &Store{keys: keys}, nil
}

// IssueRecoveryCodes revokes every live recovery code of the staff member and
// inserts a fresh batch of staffmfa.RecoveryCodesPerBatch codes, stored ONLY
// as keyed HMACs. It returns the batch (to be shown once, no-store). The
// staff member's ACTIVE factor row is locked FOR UPDATE first: that is the
// serialisation point shared with Verify and ConsumeRecoveryCode, so two
// concurrent regenerations cannot leave two live batches. A staff member with
// no active factor has no recovery codes (ErrNoActiveFactor).
func (s *Store) IssueRecoveryCodes(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, now time.Time) (staffmfa.RecoveryCodeBatch, error) {
	if !staff.Valid() || now.IsZero() {
		return staffmfa.RecoveryCodeBatch{}, ErrInvalidInput
	}
	if _, err := lockActive(ctx, tx, staff); err != nil {
		return staffmfa.RecoveryCodeBatch{}, err
	}
	batch, err := staffmfa.GenerateRecoveryCodes()
	if err != nil {
		return staffmfa.RecoveryCodeBatch{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE staff_mfa_recovery_codes SET revoked_at = $2
		WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, staff.StaffID, now.UTC()); err != nil {
		return staffmfa.RecoveryCodeBatch{}, fmt.Errorf("staffmfa/store: revoke old recovery codes: %w", err)
	}
	batchID := uuid.New()
	for _, c := range batch.Codes() {
		h, err := s.keys.HashRecoveryCode(staff.StaffID, c)
		if err != nil {
			return staffmfa.RecoveryCodeBatch{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO staff_mfa_recovery_codes (tenant_id, staff_user_id, batch_id, code_kid, code_mac, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantArg(staff.TenantID), staff.StaffID, batchID, h.KID, h.MAC, now.UTC()); err != nil {
			return staffmfa.RecoveryCodeBatch{}, fmt.Errorf("staffmfa/store: insert recovery code: %w", err)
		}
	}
	return batch, nil
}

// RecoveryCodesRemaining counts the staff member's live (unconsumed,
// unrevoked) recovery codes.
func (s *Store) RecoveryCodesRemaining(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef) (int, error) {
	if !staff.Valid() {
		return 0, ErrInvalidInput
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_mfa_recovery_codes
		WHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid AND consumed_at IS NULL AND revoked_at IS NULL`,
		staff.StaffID, tenantArg(staff.TenantID)).Scan(&n); err != nil {
		return 0, fmt.Errorf("staffmfa/store: count recovery codes: %w", err)
	}
	return n, nil
}

// ConsumeRecoveryCode verifies a presented recovery code against the staff
// member's live codes and consumes it (single use). The ACTIVE factor row is
// locked FOR UPDATE first and its lock state is evaluated BEFORE any code is
// looked up or compared (a locked factor answers locked, whatever the code):
// recovery-code failures count against the same factor counters as TOTP
// failures (ADR 0119 section 6.5), one budget for every second-factor guess.
//
// Like VerifyDetailed, a refusal is reported through the result with a nil
// error and the caller commits. On OutcomeAccepted the code is consumed; the
// caller issues a purpose 'enrol' challenge, never a session (section 6.1.3).
func (s *Store) ConsumeRecoveryCode(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, a staffmfa.Assertion, now time.Time) (VerifyResult, error) {
	if !staff.Valid() || now.IsZero() {
		return VerifyResult{}, ErrInvalidInput
	}
	row, err := lockActive(ctx, tx, staff)
	if err != nil {
		return VerifyResult{}, err
	}
	switch row.counters.LockState(now) {
	case staffmfa.LockOpen:
	case staffmfa.LockHard:
		return VerifyResult{Result: staffmfa.Result{Outcome: staffmfa.OutcomeHardLocked}}, nil
	default:
		return VerifyResult{Result: staffmfa.Result{Outcome: staffmfa.OutcomeLocked}}, nil
	}

	res := staffmfa.Result{Outcome: staffmfa.OutcomeRejected}
	if code, err := staffmfa.RecoveryCodeFromAssertion(a); err == nil {
		consumed, err := s.consumeMatching(ctx, tx, staff, code, now)
		if err != nil {
			return VerifyResult{}, err
		}
		if consumed {
			res = staffmfa.Result{Outcome: staffmfa.OutcomeAccepted}
		}
	}
	if res.Outcome == staffmfa.OutcomeAccepted {
		if err := persistCounters(ctx, tx, row.id, row.counters.RecordSuccess()); err != nil {
			return VerifyResult{}, err
		}
		return VerifyResult{Result: res}, nil
	}
	hard, err := recordOutcome(ctx, tx, row, res, now)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{Result: res, HardLockSet: hard}, nil
}

// consumeMatching finds the staff member's live code whose keyed MAC matches
// (under any retained MAC kid) and consumes it.
func (s *Store) consumeMatching(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, code staffmfa.RecoveryCode, now time.Time) (bool, error) {
	hashes, err := s.keys.RecoveryCodeLookupHashes(staff.StaffID, code)
	if err != nil {
		return false, nil // malformed: an ordinary refusal
	}
	macs := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		macs = append(macs, h.MAC)
	}
	rows, err := tx.Query(ctx, `SELECT id, code_kid, code_mac FROM staff_mfa_recovery_codes
		WHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid AND code_mac = ANY($3)
		  AND consumed_at IS NULL AND revoked_at IS NULL`, staff.StaffID, tenantArg(staff.TenantID), macs)
	if err != nil {
		return false, fmt.Errorf("staffmfa/store: look up recovery code: %w", err)
	}
	type cand struct {
		id uuid.UUID
		h  staffmfa.KeyedHash
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.h.KID, &c.h.MAC); err != nil {
			rows.Close()
			return false, fmt.Errorf("staffmfa/store: scan recovery code: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("staffmfa/store: look up recovery code: %w", err)
	}
	for _, c := range cands {
		if s.keys.VerifyRecoveryCodeHash(staff.StaffID, code, c.h) {
			return consumeCode(ctx, tx, c.id, now)
		}
	}
	return false, nil
}

// consumeCode marks one recovery code consumed. The condition (still unconsumed
// and unrevoked) is what makes the code single-use even without the factor
// lock; it reports whether THIS call consumed it.
func consumeCode(ctx context.Context, tx pgx.Tx, id uuid.UUID, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE staff_mfa_recovery_codes SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, id, now.UTC())
	if err != nil {
		return false, fmt.Errorf("staffmfa/store: consume recovery code: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
