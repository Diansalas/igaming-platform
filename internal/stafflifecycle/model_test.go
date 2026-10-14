package stafflifecycle

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ADR 0119 s10.1 / s17.1 role-grant authority (replaces the removed legacy create route's allow-list unit tests,
// internal/httpserver/admin_routes_test.go): a tenant admin may grant tenant_admin, support and compliance only; the
// platform-only roles (finance, risk_manager, promotions_manager, bonus_operations) are platform-admin only;
// platform_admin is never granted through the tenant path; no other role may grant anything.
func TestGrantable_RoleAuthorityMatrix(t *testing.T) {
	tenant := uuid.New()
	platform := Actor{StaffID: uuid.New(), Role: RolePlatformAdmin}
	tadmin := Actor{StaffID: uuid.New(), TenantID: tenant, Role: RoleTenantAdmin}
	support := Actor{StaffID: uuid.New(), TenantID: tenant, Role: RoleSupport}
	compliance := Actor{StaffID: uuid.New(), TenantID: tenant, Role: RoleCompliance}
	for _, role := range TenantRoles {
		if !Grantable(platform, role) {
			t.Errorf("platform_admin must grant %s", role)
		}
		want := role == RoleTenantAdmin || role == RoleSupport || role == RoleCompliance
		if got := Grantable(tadmin, role); got != want {
			t.Errorf("tenant_admin grant %s = %v, want %v", role, got, want)
		}
		if Grantable(support, role) || Grantable(compliance, role) {
			t.Errorf("only admins grant (%s)", role)
		}
	}
	for _, a := range []Actor{platform, tadmin} {
		if Grantable(a, RolePlatformAdmin) || Grantable(a, "made_up") || Grantable(a, "") {
			t.Errorf("%s granted a non-tenant role", a.Role)
		}
	}
}

func TestPersonRequired_ApproveCapableRoles(t *testing.T) {
	want := map[string]bool{RolePlatformAdmin: true, RoleTenantAdmin: true, RoleFinance: true, RoleCompliance: true}
	for _, r := range append([]string{RolePlatformAdmin}, TenantRoles...) {
		if PersonRequired(r) != want[r] {
			t.Errorf("PersonRequired(%s) = %v", r, PersonRequired(r))
		}
	}
}

func TestValidateReason_ClosedListsAndNote(t *testing.T) {
	for action, codes := range ReasonCodes {
		for _, c := range codes {
			r := Reason{Code: c}
			if c == "other" {
				if err := ValidateReason(action, r); !errors.Is(err, ErrInvalidInput) {
					t.Errorf("%s/other without note accepted", action)
				}
				r.Note = "a perfectly fine note"
			}
			if err := ValidateReason(action, r); err != nil {
				t.Errorf("%s/%s refused: %v", action, c, err)
			}
		}
		if err := ValidateReason(action, Reason{Code: "made_up"}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s accepted an unknown code", action)
		}
	}
	// A code valid for another action is refused (closed per action).
	if ValidateReason(ActionSuspend, Reason{Code: "employment_ended"}) == nil {
		t.Error("an offboard code was accepted for suspend")
	}
	for name, note := range map[string]string{
		"short": "too short", "long": strings.Repeat("x", 501), "control": "line one\nline two!",
		"bidi": "abc\u202Edefghijk", "zero width": "abc\u200Bdefghijk", "bom": "abc\uFEFFdefghijk", "alm": "abc\u061Cdefghijk",
	} {
		if err := ValidateReason(ActionSuspend, Reason{Code: "other", Note: note}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("note %s accepted", name)
		}
	}
}

func TestRequestDigest_IsContentBound(t *testing.T) {
	// k2_canonical: len:value, '~' for NULL, comma-joined; sha256 hex. This unit test checks only that the digest is
	// content-bound; equality with the database's staff_lifecycle_request_digest (including non-ASCII notes) is
	// TestFixRound_F11_RequestDigestEqualsTheDatabase (review fix F11: the earlier name over-claimed).
	target := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	d := RequestDigest(KindPlatformMFAReset, &target, "", nil, "device_lost", "")
	if len(d) != 64 || d == RequestDigest(KindPlatformMFAReset, &target, "", nil, "device_replaced", "") {
		t.Fatalf("digest %q not content-bound", d)
	}
}
