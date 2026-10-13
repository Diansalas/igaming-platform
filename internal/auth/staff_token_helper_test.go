package auth

import (
	"time"

	"github.com/google/uuid"
)

// testStaffParams are the claims of an aal2 staff token for the unit tests of this package (ADR 0119 S3: staff
// tokens are minted only by IssueStaff). Integration tests elsewhere use internal/auth/authtest.
func testStaffParams(staffID, tenantID uuid.UUID, role Role) StaffTokenParams {
	now := time.Now()
	return StaffTokenParams{StaffID: staffID, TenantID: tenantID, Role: role, SessionID: uuid.New(),
		AAL: StaffAAL, AMR: []string{"pwd", "otp"}, AuthTime: now, MFAAt: now, StaffVersion: 0}
}
