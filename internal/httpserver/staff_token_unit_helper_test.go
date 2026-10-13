package httpserver

import (
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// unitStaffParams are the claims of an aal2 staff token for the DB-free unit tests of this package (ADR 0119 S3:
// auth.Issuer.Issue refuses staff tokens). Integration tests use internal/auth/authtest.
func unitStaffParams(staffID, tenantID uuid.UUID, role auth.Role) auth.StaffTokenParams {
	now := time.Now()
	return auth.StaffTokenParams{StaffID: staffID, TenantID: tenantID, Role: role, SessionID: uuid.New(),
		AAL: auth.StaffAAL, AMR: []string{"pwd", "otp"}, AuthTime: now, MFAAt: now, StaffVersion: 0}
}
