package auth

// Staff session assurance (ADR 0119 sections 7.1 and 7.2, slice S3). A staff
// `sessions` row carries aal, amr, auth_time, mfa_at, absolute_expires_at and
// a keyed seal over them (staffmfa.Keys.SealSession); the SF040 trigger of
// migration 0138 refuses a staff INSERT without them and freezes the
// immutable ones. On every staff refresh the seal is verified before anything
// else; a missing or bad seal is refused, and - unless the row predates the
// 0138 cutover - recorded as tampering (audit row here, P1 alert raised by the
// HTTP layer). A staff session never outlives absolute_expires_at, and each
// rotation's refresh token lives at most the staff idle TTL.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

var (
	// ErrStaffSessionUnavailable: a staff session operation in a process that
	// holds no staff MFA keys. It fails closed (the HTTP layer answers 503);
	// it is never treated as "no assurance needed".
	ErrStaffSessionUnavailable = errors.New("auth: staff session assurance is not available in this process")
)

// Causes of a SessionIntegrityError (closed vocabulary; also the alert's
// `cause` attribute).
const (
	IntegrityCauseBadSeal          = "bad_seal"
	IntegrityCauseNullSealPostCut  = "null_seal_after_cutover"
	integrityCausePreCutoverLegacy = "pre_cutover"
	// integrityCauseLegacySealV1: a genuine S3-format (v1) seal (ADR 0119 S4b,
	// condition S3-R10): the session predates seal format v2. Retired quietly
	// like a pre-cutover row (plain not-found, no audit, no alert): only a holder
	// of the MAC key could have produced it.
	integrityCauseLegacySealV1 = "legacy_seal_v1"
)

// SessionIntegrityError reports a staff session whose assurance seal is
// missing (after the 0138 cutover) or does not verify: a row written or
// edited by SQL. The refresh is refused like a not-found; RotateSession has
// already written the auth.session_assurance_invalid audit row (AuditErr is
// set if that failed); the caller raises the staff session integrity alert.
type SessionIntegrityError struct {
	SessionID   uuid.UUID
	PrincipalID uuid.UUID
	TenantID    uuid.UUID // uuid.Nil for platform staff
	Cause       string
	AuditErr    error
}

func (e *SessionIntegrityError) Error() string {
	return "auth: staff session assurance invalid (" + e.Cause + ")"
}

// StaffSessionPolicy carries what staff session issuance and rotation need.
type StaffSessionPolicy struct {
	Keys    *staffmfa.Keys
	IdleTTL time.Duration // STAFF_REFRESH_IDLE_TTL_SECONDS (default 30 min), capped at absolute expiry
}

// RotationPolicy selects the rotation rules per principal type: players keep
// PlayerTTL and no seal; staff sessions follow Staff (nil fails closed with
// ErrStaffSessionUnavailable).
type RotationPolicy struct {
	PlayerTTL time.Duration
	Staff     *StaffSessionPolicy
}

// StaffSessionParams describes a NEW staff session, issued right after a
// verified second factor.
type StaffSessionParams struct {
	StaffID  uuid.UUID
	TenantID uuid.UUID // uuid.Nil for platform staff
	AMR      []string
	// Now is the second-factor verification instant (the transaction's
	// database clock in the S3 flows): auth_time and mfa_at.
	Now         time.Time
	AbsoluteTTL time.Duration // STAFF_SESSION_ABSOLUTE_TTL_SECONDS (default 12 h)
	IdleTTL     time.Duration
	UserAgent   string
	IPAddress   string
}

// IssueStaffSession inserts a new aal2 staff session (sealed) in tx, which the
// caller has scoped to the staff member's tenant (or platform scope).
func IssueStaffSession(ctx context.Context, tx pgx.Tx, keys *staffmfa.Keys, p StaffSessionParams) (Session, error) {
	if keys == nil {
		return Session{}, ErrStaffSessionUnavailable
	}
	if p.StaffID == uuid.Nil || len(p.AMR) == 0 || !HasSecondFactorAMR(p.AMR) || p.Now.IsZero() || p.AbsoluteTTL <= 0 || p.IdleTTL <= 0 {
		return Session{}, fmt.Errorf("auth: incomplete staff session parameters")
	}
	now := staffmfa.SessionTime(p.Now)
	a := staffmfa.SessionAssurance{
		SessionID: uuid.New(), PrincipalType: string(PrincipalStaff), PrincipalID: p.StaffID, TenantID: p.TenantID,
		AAL: StaffAAL, AMR: append([]string(nil), p.AMR...), AuthTime: now, MFAAt: now,
		AbsoluteExpiresAt: staffmfa.SessionTime(now.Add(p.AbsoluteTTL)),
	}
	return insertStaffSession(ctx, tx, keys, a, idleExpiry(now, p.IdleTTL, a.AbsoluteExpiresAt), p.UserAgent, p.IPAddress)
}

// idleExpiry is the refresh token's expiry: now + idle, never past the
// session's absolute expiry.
func idleExpiry(now time.Time, idle time.Duration, absolute time.Time) time.Time {
	e := staffmfa.SessionTime(now.Add(idle))
	if e.After(absolute) {
		return absolute
	}
	return e
}

func insertStaffSession(ctx context.Context, tx pgx.Tx, keys *staffmfa.Keys, a staffmfa.SessionAssurance, expiresAt time.Time, userAgent, ipAddress string) (Session, error) {
	token, err := generateRefreshToken()
	if err != nil {
		return Session{}, err
	}
	// Seal format v2 (S3-R10): the refresh token hash and the idle expiry are bound.
	expiresAt = staffmfa.SessionTime(expiresAt)
	a.RefreshTokenHash = hashRefreshToken(token)
	a.ExpiresAt = expiresAt
	seal, err := keys.SealSession(a)
	if err != nil {
		return Session{}, fmt.Errorf("auth: seal staff session: %w", err)
	}
	var tenantIDArg *uuid.UUID
	if a.TenantID != uuid.Nil {
		t := a.TenantID
		tenantIDArg = &t
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, user_agent, ip_address, expires_at,
		                       aal, amr, auth_time, mfa_at, absolute_expires_at, assurance_kid, assurance_seal)
		 VALUES ($1, 'staff', $2, $3, $4, NULLIF($5, ''), NULLIF($6, '')::inet, $7, $8, $9, $10, $11, $12, $13, $14)`,
		a.SessionID, a.PrincipalID, tenantIDArg, a.RefreshTokenHash, userAgent, ipAddress, expiresAt,
		a.AAL, a.AMR, a.AuthTime, a.MFAAt, a.AbsoluteExpiresAt, seal.KID, seal.MAC,
	); err != nil {
		return Session{}, fmt.Errorf("auth: insert staff session: %w", err)
	}
	return Session{
		ID: a.SessionID, PrincipalType: PrincipalStaff, PrincipalID: a.PrincipalID, TenantID: a.TenantID,
		RefreshToken: token, ExpiresAt: expiresAt,
		AAL: a.AAL, AMR: append([]string(nil), a.AMR...), AuthTime: a.AuthTime, MFAAt: a.MFAAt, AbsoluteExpiresAt: a.AbsoluteExpiresAt,
	}, nil
}

// staffAssuranceRow is the assurance part of a staff sessions row, read in the
// same pre-tenant lookup transaction as the row itself.
type staffAssuranceRow struct {
	aal                             *int16
	amr                             []string
	authTime, mfaAt, absoluteExpiry *time.Time
	kid                             *string
	seal                            []byte
	createdAt                       time.Time
	cutoverAt                       *time.Time
}

// readStaffAssurance reads the assurance columns of the row with hash, inside
// the caller's WithSessionLookup transaction. It is a separate statement from
// the base lookup so that player refresh never references a 0138 column.
func readStaffAssurance(ctx context.Context, tx pgx.Tx, hash string) (staffAssuranceRow, error) {
	var r staffAssuranceRow
	err := tx.QueryRow(ctx,
		`SELECT s.aal, s.amr, s.auth_time, s.mfa_at, s.absolute_expires_at, s.assurance_kid, s.assurance_seal, s.created_at,
		        (SELECT c.at FROM staff_session_cutover c)
		 FROM sessions s WHERE s.refresh_token_hash = $1`, hash,
	).Scan(&r.aal, &r.amr, &r.authTime, &r.mfaAt, &r.absoluteExpiry, &r.kid, &r.seal, &r.createdAt, &r.cutoverAt)
	return r, err
}

// assurance rebuilds the sealed values of row; ok is false when any column is NULL.
func (row sessionRow) assurance() (staffmfa.SessionAssurance, staffmfa.KeyedHash, bool) {
	s := row.staff
	if s == nil || s.aal == nil || s.amr == nil || s.authTime == nil || s.mfaAt == nil || s.absoluteExpiry == nil || s.kid == nil || s.seal == nil {
		return staffmfa.SessionAssurance{}, staffmfa.KeyedHash{}, false
	}
	tenant := uuid.Nil
	if row.tenantID != nil {
		tenant = *row.tenantID
	}
	return staffmfa.SessionAssurance{
		SessionID: row.id, PrincipalType: string(row.principalType), PrincipalID: row.principalID, TenantID: tenant,
		AAL: int(*s.aal), AMR: s.amr, AuthTime: *s.authTime, MFAAt: *s.mfaAt, AbsoluteExpiresAt: *s.absoluteExpiry,
		RefreshTokenHash: row.refreshHash, ExpiresAt: row.expiresAt,
	}, staffmfa.KeyedHash{KID: *s.kid, MAC: s.seal}, true
}

// staffSealVerdict applies ADR 0119 section 7.2 and the review C1 cutover
// rule: "" (sealed and valid), integrityCausePreCutoverLegacy (a NULL seal on a
// session created before the 0138 cutover: plain not-found, no audit, no
// alert), or an IntegrityCause* (tampering).
func staffSealVerdict(row sessionRow, keys *staffmfa.Keys) string {
	s := row.staff
	if s == nil {
		return IntegrityCauseBadSeal
	}
	if s.seal == nil {
		if s.cutoverAt != nil && s.createdAt.Before(*s.cutoverAt) {
			return integrityCausePreCutoverLegacy
		}
		return IntegrityCauseNullSealPostCut
	}
	a, h, ok := row.assurance()
	if !ok {
		return IntegrityCauseBadSeal
	}
	if !keys.VerifySessionSeal(a, h) {
		if keys.IsLegacySessionSeal(a, h) {
			return integrityCauseLegacySealV1
		}
		return IntegrityCauseBadSeal
	}
	// S3-R13: a sealed row must also be internally coherent (a second factor named, auth_time <= mfa_at).
	if a.AAL != StaffAAL || !HasSecondFactorAMR(a.AMR) || a.MFAAt.Before(a.AuthTime) {
		return IntegrityCauseBadSeal
	}
	return ""
}

// recordSessionIntegrity writes the auth.session_assurance_invalid audit row in
// the row's own scope and returns the SessionIntegrityError.
func recordSessionIntegrity(ctx context.Context, pool *db.Pool, row sessionRow, cause, userAgent, ipAddress string) error {
	tenantID := uuid.Nil
	if row.tenantID != nil {
		tenantID = *row.tenantID
	}
	auditErr := withResolvedScope(ctx, pool, row, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorType(row.principalType), ActorID: row.principalID,
			Action: "auth.session_assurance_invalid", TargetType: "session", TargetID: row.id.String(),
			Outcome: audit.OutcomeDenied, IPAddress: ipAddress, UserAgent: userAgent,
			Metadata: map[string]any{"cause": cause},
		})
	})
	return &SessionIntegrityError{SessionID: row.id, PrincipalID: row.principalID, TenantID: tenantID, Cause: cause, AuditErr: auditErr}
}

// rotateStaff inserts the successor of a verified staff session row: the same
// aal, amr, auth_time, mfa_at and absolute expiry, a new id and seal, and an
// idle expiry capped at the absolute expiry.
func rotateStaff(ctx context.Context, tx pgx.Tx, p *StaffSessionPolicy, row sessionRow, now time.Time, userAgent, ipAddress string) (Session, error) {
	a, _, ok := row.assurance()
	if !ok {
		return Session{}, fmt.Errorf("auth: rotate staff session: incomplete assurance")
	}
	a.SessionID = uuid.New()
	a.AMR = append([]string(nil), a.AMR...)
	// insertStaffSession binds the successor's own refresh token hash and idle expiry.
	a.RefreshTokenHash, a.ExpiresAt = "", time.Time{}
	return insertStaffSession(ctx, tx, p.Keys, a, idleExpiry(now, p.IdleTTL, a.AbsoluteExpiresAt), userAgent, ipAddress)
}
