//go:build integration

// Package authtest (integration builds ONLY: it is behind the integration build tag, so no production binary can
// contain it) mints MFA-level (aal2) staff access tokens for tests (ADR 0119 section 4.1, slice S3). Since S3 a staff
// token without session assurance claims is refused by auth.Issuer.Verify and auth.Issuer.Issue refuses
// PrincipalStaff, so every staff-token fixture goes through this one helper. When S4a adds the per-request liveness
// check (RequireLiveStaffSession), only this helper changes: it will then also write the sealed session row the
// token's sid names. A static test pins that only _test.go files import this package (the prooftest pin).
//
// ADR 0119 S4a: every staff route now runs the per-request liveness check, which needs a live sealed sessions row for
// the token's sid, an active staff row at the token's role and a usable second factor. StaffToken / Token still mint
// a bare token (for code paths that never reach a staff route: unit tests of the auth package, refusals that happen
// before liveness); LiveStaffToken mints a token whose session really exists.
package authtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// StaffParams returns the claims of an aal2 staff token for staffID, as issued right after a second-factor login
// (amr pwd+otp, auth_time = mfa_at = now, a fresh sid, staff version 0 until migration 0139 adds staff_version).
func StaffParams(staffID, tenantID uuid.UUID, role auth.Role) auth.StaffTokenParams {
	now := time.Now()
	return auth.StaffTokenParams{
		StaffID: staffID, TenantID: tenantID, Role: role, SessionID: uuid.New(),
		AAL: auth.StaffAAL, AMR: []string{staffmfa.AMRPassword, staffmfa.AMROTP},
		AuthTime: now, MFAAt: now, StaffVersion: 0,
	}
}

// StaffToken mints an aal2 staff access token (TTL one hour) with issuer.
func StaffToken(tb testing.TB, issuer *auth.Issuer, staffID, tenantID uuid.UUID, role auth.Role) string {
	tb.Helper()
	tok, err := issuer.IssueStaff(StaffParams(staffID, tenantID, role), time.Hour)
	if err != nil {
		tb.Fatalf("authtest: issue staff token: %v", err)
	}
	return tok
}

// Token mints a token of any principal type: staff tokens go through StaffToken, every other principal type
// through issuer.Issue. It is the drop-in for the pre-S3 test helpers that took a principal type parameter.
func Token(tb testing.TB, issuer *auth.Issuer, subject, tenantID uuid.UUID, role auth.Role, principal auth.PrincipalType) string {
	tb.Helper()
	if principal == auth.PrincipalStaff {
		return StaffToken(tb, issuer, subject, tenantID, role)
	}
	tok, err := issuer.Issue(subject.String(), tenantID, role, principal, time.Hour)
	if err != nil {
		tb.Fatalf("authtest: issue token: %v", err)
	}
	return tok
}

// LiveStaffToken mints an aal2 staff access token (TTL one hour) whose sid names a REAL, sealed sessions row (issued
// through auth.IssueStaffSession with keys, the same keys the server under test verifies seals with), so the token
// passes the S4a liveness check as long as the staff row is active at role. The staff member must already exist
// (staffID in tenantID; uuid.Nil = platform). If the staff member has no active second factor, a PLACEHOLDER factor
// row is planted: its ciphertext is junk, so it can never verify a code (a real login of that staff member would fail
// integrity); it exists only so that the liveness "usable factor" rule holds for fixture staff that never enrol. pool
// is the pool the test writes through (the runtime role in the harnesses).
func LiveStaffToken(tb testing.TB, pool *db.Pool, keys *staffmfa.Keys, issuer *auth.Issuer, staffID, tenantID uuid.UUID, role auth.Role) string {
	tb.Helper()
	sess := LiveStaffSession(tb, pool, keys, staffID, tenantID)
	p := StaffParams(staffID, tenantID, role)
	p.SessionID, p.AuthTime, p.MFAAt = sess.ID, sess.AuthTime, sess.MFAAt
	tok, err := issuer.IssueStaff(p, time.Hour)
	if err != nil {
		tb.Fatalf("authtest: issue live staff token: %v", err)
	}
	return tok
}

// ClaimedStaffToken is the harness form of LiveStaffToken for tests that mint tokens with ARBITRARY claims: when a
// staff row staffID is visible in tenantID's scope it writes a live session for it (the token's role claim is kept
// as given, so a role that differs from the row is refused by liveness as a role mismatch); when no such row exists
// in that scope (an unknown subject, or a real subject presented under another tenant claim) it mints a bare token,
// which liveness refuses as an unknown session. Either way the token is genuinely signed: what the S4a chain judges
// is whether the claims match a live session, exactly as in production.
func ClaimedStaffToken(tb testing.TB, pool *db.Pool, keys *staffmfa.Keys, issuer *auth.Issuer, staffID, tenantID uuid.UUID, role auth.Role) string {
	tb.Helper()
	visible := false
	fn := func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM staff_users WHERE id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid)`,
			staffID, nullableTenant(tenantID)).Scan(&visible)
	}
	var err error
	if tenantID != uuid.Nil {
		err = pool.WithTenant(context.Background(), tenantID, fn)
	} else {
		err = pool.WithoutTenant(context.Background(), fn)
	}
	if err != nil {
		tb.Fatalf("authtest: staff visibility for %s: %v", staffID, err)
	}
	if !visible {
		return StaffToken(tb, issuer, staffID, tenantID, role)
	}
	return LiveStaffToken(tb, pool, keys, issuer, staffID, tenantID, role)
}

func nullableTenant(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// LiveStaffTokenAged is LiveStaffToken for a session whose second factor was last verified mfaAge ago (database
// clock): auth_time = mfa_at = now - mfaAge (ADR 0119 S4b: a stale step-up). The token's mfa_at claim equals the
// row's (whole seconds).
func LiveStaffTokenAged(tb testing.TB, pool *db.Pool, keys *staffmfa.Keys, issuer *auth.Issuer, staffID, tenantID uuid.UUID, role auth.Role, mfaAge time.Duration) string {
	tb.Helper()
	sess := liveStaffSessionAged(tb, pool, keys, staffID, tenantID, mfaAge)
	p := StaffParams(staffID, tenantID, role)
	p.SessionID, p.AuthTime, p.MFAAt = sess.ID, sess.AuthTime, sess.MFAAt
	tok, err := issuer.IssueStaff(p, time.Hour)
	if err != nil {
		tb.Fatalf("authtest: issue aged staff token: %v", err)
	}
	return tok
}

// LiveStaffSession writes a sealed aal2 staff session row (and the placeholder factor of LiveStaffToken when the
// staff member has no active factor) and returns it.
func LiveStaffSession(tb testing.TB, pool *db.Pool, keys *staffmfa.Keys, staffID, tenantID uuid.UUID) auth.Session {
	tb.Helper()
	return liveStaffSessionAged(tb, pool, keys, staffID, tenantID, 0)
}

func liveStaffSessionAged(tb testing.TB, pool *db.Pool, keys *staffmfa.Keys, staffID, tenantID uuid.UUID, mfaAge time.Duration) auth.Session {
	tb.Helper()
	var sess auth.Session
	fn := func(ctx context.Context, tx pgx.Tx) error {
		if err := EnsurePlaceholderFactor(ctx, tx, staffID); err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		now = now.Add(-mfaAge)
		var err error
		sess, err = auth.IssueStaffSession(ctx, tx, keys, auth.StaffSessionParams{
			StaffID: staffID, TenantID: tenantID, AMR: []string{staffmfa.AMRPassword, staffmfa.AMROTP},
			Now: now, AbsoluteTTL: 12 * time.Hour, IdleTTL: 30 * time.Minute,
		})
		return err
	}
	var err error
	if tenantID != uuid.Nil {
		err = pool.WithTenant(context.Background(), tenantID, fn)
	} else {
		err = pool.WithoutTenant(context.Background(), fn)
	}
	if err != nil {
		tb.Fatalf("authtest: live staff session for %s: %v", staffID, err)
	}
	return sess
}

// EnsurePlaceholderFactor plants the LiveStaffToken placeholder factor (pending, then activated: the SF002 status
// graph) for staffID inside tx (scoped to the staff member) unless an active factor already exists.
func EnsurePlaceholderFactor(ctx context.Context, tx pgx.Tx, staffID uuid.UUID) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`INSERT INTO staff_mfa_factors (staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, pending_expires_at)
		 SELECT $1, 'totp', 'pending', 'authtest-placeholder', '\x000000000000000000000000'::bytea, '\x00'::bytea, now() + interval '10 minutes'
		 WHERE NOT EXISTS (SELECT 1 FROM staff_mfa_factors WHERE staff_user_id = $1 AND status = 'active')
		 RETURNING id`, staffID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'active', activated_at = now(), pending_expires_at = NULL WHERE id = $1`, id)
	return err
}
