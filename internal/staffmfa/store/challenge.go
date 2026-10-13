package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// ErrInvalidChallenge is the single answer for a challenge that is unknown,
// expired, consumed, exhausted, of the wrong purpose, or presented for a
// different staff member: it never distinguishes them (ADR 0119 section 14.2).
var ErrInvalidChallenge = errors.New("staffmfa/store: invalid challenge")

// ChallengePurpose is the closed vocabulary of the purpose column.
type ChallengePurpose string

// Challenge purposes: 'mfa' is issued after a correct password; 'enrol' is
// issued by an invite / reset / bootstrap redemption or a recovery code.
const (
	ChallengeMFA   ChallengePurpose = "mfa"
	ChallengeEnrol ChallengePurpose = "enrol"
)

// ChallengeOrigin is the closed vocabulary of the via column.
type ChallengeOrigin string

// Challenge origins.
const (
	OriginLogin        ChallengeOrigin = "login"
	OriginRecoveryCode ChallengeOrigin = "recovery_code"
	OriginInvite       ChallengeOrigin = "invite"
	OriginMFAReset     ChallengeOrigin = "mfa_reset"
	OriginBootstrap    ChallengeOrigin = "bootstrap"
)

func (p ChallengePurpose) kind() (staffmfa.MACKind, bool) {
	switch p {
	case ChallengeMFA:
		return staffmfa.MACChallengeMFA, true
	case ChallengeEnrol:
		return staffmfa.MACChallengeEnrol, true
	}
	return 0, false
}

func (o ChallengeOrigin) valid() bool {
	switch o {
	case OriginLogin, OriginRecoveryCode, OriginInvite, OriginMFAReset, OriginBootstrap:
		return true
	}
	return false
}

// Challenge is a locked, validated challenge row.
type Challenge struct {
	ID                  uuid.UUID
	Staff               staffmfa.StaffRef
	Purpose             ChallengePurpose
	Origin              ChallengeOrigin
	StaffVersionAtIssue int64
	AttemptsLeft        int
	ExpiresAt           time.Time
}

// ChallengeRef is what the pre-tenant lookup reveals: which challenge row and
// which staff scope. It is NOT proof that the challenge is still usable; that
// is decided by LockChallenge under the row lock.
type ChallengeRef struct {
	ID    uuid.UUID
	Staff staffmfa.StaffRef
}

// IssueChallenge inserts a challenge (5 minutes, 5 attempts) and returns the
// token, to be shown once. Only the keyed MAC is stored. staffVersion is the
// staff_version the staff member had when the challenge was issued (0 until
// migration 0139 adds the column).
func (s *Store) IssueChallenge(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, purpose ChallengePurpose, via ChallengeOrigin, staffVersion int64, now time.Time) (staffmfa.Token, error) {
	kind, ok := purpose.kind()
	if !ok || !via.valid() || !staff.Valid() || staffVersion < 0 || now.IsZero() {
		return staffmfa.Token{}, ErrInvalidInput
	}
	tok, err := staffmfa.NewToken(staff.StaffID)
	if err != nil {
		return staffmfa.Token{}, err
	}
	h, err := s.keys.HashToken(kind, tok)
	if err != nil {
		return staffmfa.Token{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO staff_auth_challenges
			(tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, created_at, via)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		tenantArg(staff.TenantID), staff.StaffID, string(purpose), h.KID, h.MAC, staffVersion,
		staffmfa.ChallengeAttempts, now.Add(staffmfa.ChallengeTTL).UTC(), now.UTC(), string(via)); err != nil {
		return staffmfa.Token{}, fmt.Errorf("staffmfa/store: insert challenge: %w", err)
	}
	return tok, nil
}

// ResolveChallenge is phase 1 of the ADR 0016 two-phase pattern: it maps a
// presented challenge token to the row's id and the staff member's scope, in a
// pre-tenant lookup transaction that can see at most the row whose keyed MAC
// equals a MAC computed from the presented token. Every failure, including a
// malformed token, is ErrInvalidChallenge. The caller then opens the resolved
// scope (WithScope) and calls LockChallenge.
func (s *Store) ResolveChallenge(ctx context.Context, pool *db.Pool, purpose ChallengePurpose, raw string) (ChallengeRef, error) {
	kind, ok := purpose.kind()
	if !ok {
		return ChallengeRef{}, ErrInvalidInput
	}
	tok, err := staffmfa.ParseToken(raw)
	if err != nil {
		return ChallengeRef{}, ErrInvalidChallenge
	}
	hashes, err := s.keys.TokenLookupHashes(kind, tok)
	if err != nil {
		return ChallengeRef{}, ErrInvalidChallenge
	}
	for _, h := range hashes {
		var ref ChallengeRef
		var found bool
		err := pool.WithStaffChallengeLookup(ctx, hex.EncodeToString(h.MAC), func(ctx context.Context, tx pgx.Tx) error {
			var tenant *uuid.UUID
			var rowKID string
			var rowMAC []byte
			var rowPurpose string
			err := tx.QueryRow(ctx, `SELECT id, tenant_id, staff_user_id, token_kid, token_mac, purpose
				FROM staff_auth_challenges WHERE token_mac = $1`, h.MAC).
				Scan(&ref.ID, &tenant, &ref.Staff.StaffID, &rowKID, &rowMAC, &rowPurpose)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("staffmfa/store: look up challenge: %w", err)
			}
			if rowPurpose != string(purpose) ||
				!s.keys.VerifyTokenHash(kind, tok, ref.Staff.StaffID, staffmfa.KeyedHash{KID: rowKID, MAC: rowMAC}) {
				return nil
			}
			if tenant != nil {
				ref.Staff.TenantID = *tenant
			}
			found = true
			return nil
		})
		if err != nil {
			return ChallengeRef{}, err
		}
		if found {
			return ref, nil
		}
	}
	return ChallengeRef{}, ErrInvalidChallenge
}

// WithScope runs fn in the transaction scope of staff: WithTenant for a tenant
// staff member, a plain platform-scoped transaction for platform staff. The
// scope comes from the StaffRef a lookup resolved, never from a request body.
func WithScope(ctx context.Context, pool *db.Pool, staff staffmfa.StaffRef, fn db.TxFunc) error {
	if !staff.Valid() {
		return ErrInvalidInput
	}
	if staff.TenantID != uuid.Nil {
		return pool.WithTenant(ctx, staff.TenantID, fn)
	}
	return pool.WithoutTenant(ctx, fn)
}

// LockChallenge is phase 2: inside the resolved scope it re-finds the row by
// keyed MAC, locks it FOR UPDATE and validates it (unconsumed, unexpired,
// attempts left, the right purpose, the token's staff member). Anything else
// is ErrInvalidChallenge, with no distinction.
func (s *Store) LockChallenge(ctx context.Context, tx pgx.Tx, purpose ChallengePurpose, tok staffmfa.Token, now time.Time) (Challenge, error) {
	kind, ok := purpose.kind()
	if !ok || now.IsZero() {
		return Challenge{}, ErrInvalidInput
	}
	hashes, err := s.keys.TokenLookupHashes(kind, tok)
	if err != nil {
		return Challenge{}, ErrInvalidChallenge
	}
	macs := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		macs = append(macs, h.MAC)
	}
	var (
		c      Challenge
		tenant *uuid.UUID
		kid    string
		mac    []byte
		pur    string
		via    string
		cons   *time.Time
	)
	err = tx.QueryRow(ctx, `SELECT id, tenant_id, staff_user_id, purpose, via, token_kid, token_mac, staff_version_at_issue,
			attempts_left, expires_at, consumed_at
		FROM staff_auth_challenges
		WHERE staff_user_id = $1 AND purpose = $2 AND token_mac = ANY($3)
		FOR UPDATE`, tok.StaffID(), string(purpose), macs).
		Scan(&c.ID, &tenant, &c.Staff.StaffID, &pur, &via, &kid, &mac, &c.StaffVersionAtIssue, &c.AttemptsLeft, &c.ExpiresAt, &cons)
	if errors.Is(err, pgx.ErrNoRows) {
		return Challenge{}, ErrInvalidChallenge
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("staffmfa/store: lock challenge: %w", err)
	}
	if !s.keys.VerifyTokenHash(kind, tok, c.Staff.StaffID, staffmfa.KeyedHash{KID: kid, MAC: mac}) ||
		cons != nil || c.AttemptsLeft <= 0 || !now.Before(c.ExpiresAt) {
		return Challenge{}, ErrInvalidChallenge
	}
	if tenant != nil {
		c.Staff.TenantID = *tenant
	}
	c.Purpose, c.Origin = ChallengePurpose(pur), ChallengeOrigin(via)
	return c, nil
}

// DecrementChallengeAttempts spends one attempt of a locked challenge and
// consumes it when none are left. The conditions (unconsumed, attempts left)
// make a double decrement impossible even without the row lock. It reports the
// attempts left afterwards.
func (s *Store) DecrementChallengeAttempts(ctx context.Context, tx pgx.Tx, id uuid.UUID, now time.Time) (int, error) {
	var left int
	err := tx.QueryRow(ctx, `UPDATE staff_auth_challenges
		SET attempts_left = attempts_left - 1,
		    consumed_at = CASE WHEN attempts_left - 1 = 0 THEN $2::timestamptz ELSE NULL END
		WHERE id = $1 AND consumed_at IS NULL AND attempts_left > 0
		RETURNING attempts_left`, id, now.UTC()).Scan(&left)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvalidChallenge
	}
	if err != nil {
		return 0, fmt.Errorf("staffmfa/store: decrement challenge attempts: %w", err)
	}
	return left, nil
}

// ConsumeChallenge consumes a challenge (single use). The condition is what
// makes a challenge unusable after success; ErrInvalidChallenge when it was
// already consumed.
func (s *Store) ConsumeChallenge(ctx context.Context, tx pgx.Tx, id uuid.UUID, now time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE staff_auth_challenges SET consumed_at = $2 WHERE id = $1 AND consumed_at IS NULL`, id, now.UTC())
	if err != nil {
		return fmt.Errorf("staffmfa/store: consume challenge: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrInvalidChallenge
	}
	return nil
}
