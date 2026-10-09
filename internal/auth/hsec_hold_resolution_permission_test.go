package auth

import (
	"os"
	"regexp"
	"testing"
)

// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 6.3, security H-4): the three static
// permissions are held by RolePlatformAdmin ONLY. Every tenant role - tenant_admin,
// finance, compliance and every other - and the player are denied. (No platform finance
// role exists.) The static gate is only the first layer; the in-force platform grant, the
// acting session and the proof are the database's.
var hsecHoldResolutionPermissions = []Permission{
	PermWithdrawalHoldResolutionRequest, PermWithdrawalHoldResolutionApprove, PermWithdrawalHoldResolutionRead,
}

func TestHSEC_HoldResolutionPermissions_PlatformAdminOnly(t *testing.T) {
	for _, perm := range hsecHoldResolutionPermissions {
		if !RoleHasPermission(RolePlatformAdmin, perm) {
			t.Errorf("platform_admin must hold %s", perm)
		}
		for _, r := range allRoles {
			if r == RolePlatformAdmin {
				continue
			}
			if RoleHasPermission(r, perm) {
				t.Errorf("role %q must NOT hold %s (ADR 0111 H-4: platform_admin only)", r, perm)
			}
		}
		for _, r := range []Role{"", "unknown", "platform_finance", "PLATFORM_ADMIN"} {
			if RoleHasPermission(r, perm) {
				t.Errorf("role %q must NOT hold %s", r, perm)
			}
		}
	}
}

// The role set in allRoles is hand-maintained; this scans jwt.go's Role
// constants so a newly added tenant role cannot silently escape the denial.
func TestHSEC_HoldResolutionPermissions_EveryDeclaredRoleDenied(t *testing.T) {
	src, err := os.ReadFile("jwt.go")
	if err != nil {
		t.Fatal(err)
	}
	roles := regexp.MustCompile(`(?m)^\s*Role\w+\s+Role\s*=\s*"([a-z_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(roles) < 8 {
		t.Fatalf("found only %d declared roles; the scan is broken", len(roles))
	}
	for _, m := range roles {
		role := Role(m[1])
		for _, perm := range hsecHoldResolutionPermissions {
			if got := RoleHasPermission(role, perm); got != (role == RolePlatformAdmin) {
				t.Errorf("role %q: %s = %v", role, perm, got)
			}
		}
	}
}

// Request and approve are distinct permissions from the K3 force-resolution pair (a
// force-resolve holder gets no hold-resolution authority by role).
func TestHSEC_HoldResolutionPermissions_NotTheForceResolvePair(t *testing.T) {
	for _, r := range allRoles {
		if r == RolePlatformAdmin {
			continue
		}
		if RoleHasPermission(r, PermPaymentForceResolveRequest) && (RoleHasPermission(r, PermWithdrawalHoldResolutionRequest) || RoleHasPermission(r, PermWithdrawalHoldResolutionApprove)) {
			t.Errorf("role %q: payment_force_resolve must not imply withdrawal_hold_resolution", r)
		}
	}
}
