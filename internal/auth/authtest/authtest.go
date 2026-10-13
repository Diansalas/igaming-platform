//go:build integration

// Package authtest (integration builds ONLY: it is behind the integration build tag, so no production binary can
// contain it) mints MFA-level (aal2) staff access tokens for tests (ADR 0119 section 4.1, slice S3). Since S3 a staff
// token without session assurance claims is refused by auth.Issuer.Verify and auth.Issuer.Issue refuses
// PrincipalStaff, so every staff-token fixture goes through this one helper. When S4a adds the per-request liveness
// check (RequireLiveStaffSession), only this helper changes: it will then also write the sealed session row the
// token's sid names. A static test pins that only _test.go files import this package (the prooftest pin).
package authtest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
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
