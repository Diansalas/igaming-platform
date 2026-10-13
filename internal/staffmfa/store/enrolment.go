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

// ErrInvalidEnrolmentToken is the single answer for an enrolment token that is
// unknown, expired, consumed, revoked, of the wrong purpose, or presented for
// a different staff member (ADR 0119 section 14.2).
var ErrInvalidEnrolmentToken = errors.New("staffmfa/store: invalid enrolment token")

// EnrolmentPurpose is the closed vocabulary of the purpose column.
type EnrolmentPurpose string

// Enrolment token purposes (section 6.3, 6.7).
const (
	EnrolInvite        EnrolmentPurpose = "invite"
	EnrolMFAReset      EnrolmentPurpose = "mfa_reset"
	EnrolPasswordReset EnrolmentPurpose = "password_reset"
	EnrolBootstrap     EnrolmentPurpose = "bootstrap"
)

func (p EnrolmentPurpose) kind() (staffmfa.MACKind, bool) {
	switch p {
	case EnrolInvite:
		return staffmfa.MACEnrolInvite, true
	case EnrolMFAReset:
		return staffmfa.MACEnrolMFAReset, true
	case EnrolPasswordReset:
		return staffmfa.MACEnrolPasswordReset, true
	case EnrolBootstrap:
		return staffmfa.MACEnrolBootstrap, true
	}
	return 0, false
}

// EnrolmentToken is a locked, validated enrolment token row.
type EnrolmentToken struct {
	ID        uuid.UUID
	Staff     staffmfa.StaffRef
	Purpose   EnrolmentPurpose
	ExpiresAt time.Time
}

// EnrolmentRef is what the pre-tenant lookup reveals.
type EnrolmentRef struct {
	ID      uuid.UUID
	Staff   staffmfa.StaffRef
	Purpose EnrolmentPurpose
}

// IssueEnrolmentToken issues a token of purpose for the staff member (TTL by
// purpose, section 6.3) after revoking the live token of the same
// (staff, purpose). issuedBy is the issuing staff member (nil for a system or
// bootstrap issue); lifecycleRequestID is the governing four-eyes request if
// any. It returns the token for delivery (email or the protected bootstrap
// channel): the API never returns it to the issuer. Only the keyed MAC is stored.
func (s *Store) IssueEnrolmentToken(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, purpose EnrolmentPurpose, issuedBy, lifecycleRequestID *uuid.UUID, now time.Time) (staffmfa.Token, error) {
	kind, ok := purpose.kind()
	if !ok || !staff.Valid() || now.IsZero() {
		return staffmfa.Token{}, ErrInvalidInput
	}
	ttl, err := staffmfa.EnrolmentTokenTTL(kind)
	if err != nil {
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
	if _, err := tx.Exec(ctx, `UPDATE staff_enrolment_tokens SET revoked_at = $3
		WHERE staff_user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND revoked_at IS NULL`,
		staff.StaffID, string(purpose), now.UTC()); err != nil {
		return staffmfa.Token{}, fmt.Errorf("staffmfa/store: revoke live enrolment token: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO staff_enrolment_tokens
			(tenant_id, staff_user_id, purpose, token_kid, token_mac, expires_at, issued_by, lifecycle_request_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tenantArg(staff.TenantID), staff.StaffID, string(purpose), h.KID, h.MAC, now.Add(ttl).UTC(),
		issuedBy, lifecycleRequestID, now.UTC()); err != nil {
		if isUniqueViolation(err) {
			return staffmfa.Token{}, ErrConflict
		}
		return staffmfa.Token{}, fmt.Errorf("staffmfa/store: insert enrolment token: %w", err)
	}
	return tok, nil
}

// ResolveEnrolmentToken is phase 1 of the two-phase pattern for enrolment
// tokens. An endpoint that accepts more than one purpose (/enrol/redeem takes
// mfa_reset or bootstrap) passes them all: one lookup per purpose, because the
// purpose is inside the keyed MAC (ADR 0119 section 21.2 item 1). Every
// failure is ErrInvalidEnrolmentToken.
func (s *Store) ResolveEnrolmentToken(ctx context.Context, pool *db.Pool, purposes []EnrolmentPurpose, raw string) (EnrolmentRef, error) {
	if len(purposes) == 0 {
		return EnrolmentRef{}, ErrInvalidInput
	}
	tok, err := staffmfa.ParseToken(raw)
	if err != nil {
		return EnrolmentRef{}, ErrInvalidEnrolmentToken
	}
	for _, purpose := range purposes {
		kind, ok := purpose.kind()
		if !ok {
			return EnrolmentRef{}, ErrInvalidInput
		}
		hashes, err := s.keys.TokenLookupHashes(kind, tok)
		if err != nil {
			return EnrolmentRef{}, ErrInvalidEnrolmentToken
		}
		for _, h := range hashes {
			var ref EnrolmentRef
			var found bool
			err := pool.WithStaffEnrolmentLookup(ctx, hex.EncodeToString(h.MAC), func(ctx context.Context, tx pgx.Tx) error {
				var tenant *uuid.UUID
				var rowKID, rowPurpose string
				var rowMAC []byte
				err := tx.QueryRow(ctx, `SELECT id, tenant_id, staff_user_id, token_kid, token_mac, purpose
					FROM staff_enrolment_tokens WHERE token_mac = $1`, h.MAC).
					Scan(&ref.ID, &tenant, &ref.Staff.StaffID, &rowKID, &rowMAC, &rowPurpose)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("staffmfa/store: look up enrolment token: %w", err)
				}
				if rowPurpose != string(purpose) ||
					!s.keys.VerifyTokenHash(kind, tok, ref.Staff.StaffID, staffmfa.KeyedHash{KID: rowKID, MAC: rowMAC}) {
					return nil
				}
				if tenant != nil {
					ref.Staff.TenantID = *tenant
				}
				ref.Purpose = purpose
				found = true
				return nil
			})
			if err != nil {
				return EnrolmentRef{}, err
			}
			if found {
				return ref, nil
			}
		}
	}
	return EnrolmentRef{}, ErrInvalidEnrolmentToken
}

// LockEnrolmentToken is phase 2: inside the resolved scope it re-finds the row
// by keyed MAC, locks it FOR UPDATE and validates it (unconsumed, unrevoked,
// unexpired, the right purpose and staff member). Anything else is
// ErrInvalidEnrolmentToken.
func (s *Store) LockEnrolmentToken(ctx context.Context, tx pgx.Tx, purpose EnrolmentPurpose, tok staffmfa.Token, now time.Time) (EnrolmentToken, error) {
	kind, ok := purpose.kind()
	if !ok || now.IsZero() {
		return EnrolmentToken{}, ErrInvalidInput
	}
	hashes, err := s.keys.TokenLookupHashes(kind, tok)
	if err != nil {
		return EnrolmentToken{}, ErrInvalidEnrolmentToken
	}
	macs := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		macs = append(macs, h.MAC)
	}
	var (
		t            EnrolmentToken
		tenant       *uuid.UUID
		kid, pur     string
		mac          []byte
		consumed, rv *time.Time
	)
	err = tx.QueryRow(ctx, `SELECT id, tenant_id, staff_user_id, purpose, token_kid, token_mac, expires_at, consumed_at, revoked_at
		FROM staff_enrolment_tokens
		WHERE staff_user_id = $1 AND purpose = $2 AND token_mac = ANY($3)
		FOR UPDATE`, tok.StaffID(), string(purpose), macs).
		Scan(&t.ID, &tenant, &t.Staff.StaffID, &pur, &kid, &mac, &t.ExpiresAt, &consumed, &rv)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrolmentToken{}, ErrInvalidEnrolmentToken
	}
	if err != nil {
		return EnrolmentToken{}, fmt.Errorf("staffmfa/store: lock enrolment token: %w", err)
	}
	if !s.keys.VerifyTokenHash(kind, tok, t.Staff.StaffID, staffmfa.KeyedHash{KID: kid, MAC: mac}) ||
		consumed != nil || rv != nil || !now.Before(t.ExpiresAt) {
		return EnrolmentToken{}, ErrInvalidEnrolmentToken
	}
	if tenant != nil {
		t.Staff.TenantID = *tenant
	}
	t.Purpose = EnrolmentPurpose(pur)
	return t, nil
}

// ConsumeEnrolmentToken consumes a token (single use). The condition (still
// live) is what makes it single-use even without the row lock.
func (s *Store) ConsumeEnrolmentToken(ctx context.Context, tx pgx.Tx, id uuid.UUID, now time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE staff_enrolment_tokens SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, id, now.UTC())
	if err != nil {
		return fmt.Errorf("staffmfa/store: consume enrolment token: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrInvalidEnrolmentToken
	}
	return nil
}
