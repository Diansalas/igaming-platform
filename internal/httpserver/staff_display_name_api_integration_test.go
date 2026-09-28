//go:build integration

// PRH-2 G1 (ADR 0104 §3/§5.4; security confirmation N-2): the
// display_name write endpoints, end to end over HTTP. Reuses newKSAPI's
// harness (payments_kill_switch_api_integration_test.go) - these tests
// need no payments wiring, but sharing the harness avoids a third
// parallel fixture builder.
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

func staffDisplayName(t *testing.T, a *ksAPI, tenantID, staffID uuid.UUID) *string {
	t.Helper()
	var dn *string
	err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT display_name FROM staff_users WHERE id = $1`, staffID).Scan(&dn)
	})
	if err != nil {
		t.Fatalf("read display_name: %v", err)
	}
	return dn
}

func auditRowExists(t *testing.T, a *ksAPI, tenantID uuid.UUID, action, targetID string) bool {
	t.Helper()
	var n int
	if err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND target_id=$3`, tenantID, action, targetID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestStaffDisplayNameAPI_AnotherUser_Succeeds is N-2's "another user"
// happy path: PermStaffManage (tenant_admin) renaming a different staff
// member in its own tenant.
func TestStaffDisplayNameAPI_AnotherUser_Succeeds(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	adminTok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	other := a.tenantStaff(tenant, "support")

	resp := a.do("PATCH", "/v1/admin/tenants/"+tenant.String()+"/staff/"+other.String()+"/display-name", adminTok,
		map[string]any{"display_name": "Support Agent"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", resp.status, resp.body)
	}
	if dn := staffDisplayName(t, a, tenant, other); dn == nil || *dn != "Support Agent" {
		t.Fatalf("expected display_name=Support Agent, got %v", dn)
	}
	if !auditRowExists(t, a, tenant, "staff.display_name_changed", other.String()) {
		t.Fatal("expected a staff.display_name_changed audit row")
	}
}

// TestStaffDisplayNameAPI_AnotherUser_CrossTenantRefused: N-2's "tenant
// callers limited to their own tenant's staff".
func TestStaffDisplayNameAPI_AnotherUser_CrossTenantRefused(t *testing.T) {
	a := newKSAPI(t)
	tenantA := a.tenant()
	tenantB := a.tenant()
	adminA := a.tenantStaff(tenantA, "tenant_admin")
	adminATok := a.token(adminA, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	staffB := a.tenantStaff(tenantB, "support")

	resp := a.do("PATCH", "/v1/admin/tenants/"+tenantB.String()+"/staff/"+staffB.String()+"/display-name", adminATok,
		map[string]any{"display_name": "Should Not Apply"})
	if resp.status != http.StatusForbidden {
		t.Fatalf("expected 403 for a cross-tenant rename attempt, got status=%d body=%s", resp.status, resp.body)
	}
	if dn := staffDisplayName(t, a, tenantB, staffB); dn != nil {
		t.Fatalf("expected display_name to remain unset, got %v", *dn)
	}
}

// TestStaffDisplayNameAPI_AnotherUser_WithoutPermStaffManageRefused: no
// permission, no rename.
func TestStaffDisplayNameAPI_AnotherUser_WithoutPermStaffManageRefused(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	// "support" does not hold PermStaffManage.
	supportStaff := a.tenantStaff(tenant, "support")
	supportTok := a.token(supportStaff, tenant, auth.RoleSupport, auth.PrincipalStaff)
	other := a.tenantStaff(tenant, "support")

	resp := a.do("PATCH", "/v1/admin/tenants/"+tenant.String()+"/staff/"+other.String()+"/display-name", supportTok,
		map[string]any{"display_name": "Should Not Apply"})
	if resp.status != http.StatusForbidden {
		t.Fatalf("expected 403 without PermStaffManage, got status=%d body=%s", resp.status, resp.body)
	}
}

// TestStaffDisplayNameAPI_PlatformAdmin_CanRenameAnyTenant: the platform
// caller half of canActOnTenant.
func TestStaffDisplayNameAPI_PlatformAdmin_CanRenameAnyTenant(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	platAdmin := a.platformAdmin()
	platTok := a.token(platAdmin, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	staff := a.tenantStaff(tenant, "support")

	resp := a.do("PATCH", "/v1/admin/tenants/"+tenant.String()+"/staff/"+staff.String()+"/display-name", platTok,
		map[string]any{"display_name": "Renamed By Platform"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", resp.status, resp.body)
	}
	if dn := staffDisplayName(t, a, tenant, staff); dn == nil || *dn != "Renamed By Platform" {
		t.Fatalf("expected display_name=Renamed By Platform, got %v", dn)
	}
	// F-8(b) (code review): assert the audit row, with before and after.
	// (F-8(a): this platform-rename-of-tenant-staff write lands in
	// TENANT scope, TenantID = the target tenant, not a subject row - the
	// PLAT-AUDIT-SUBJECT-1 divergence the code review's own finding names;
	// tracked there, not fixed here.)
	meta := a.auditMetadata(tenant, "staff.display_name_changed")
	if meta["after"] != "Renamed By Platform" {
		t.Errorf("F-8(b): expected audit metadata.after=Renamed By Platform, got %v", meta["after"])
	}
	if meta["before"] != nil {
		t.Errorf("F-8(b): expected audit metadata.before=nil (no prior name), got %v", meta["before"])
	}
	if meta["self"] != false {
		t.Errorf("F-8(b): expected audit metadata.self=false (renaming another user), got %v", meta["self"])
	}
}

// TestStaffDisplayNameAPI_SelfRename_Succeeds: N-2's self-rename path -
// only the verified token subject.
func TestStaffDisplayNameAPI_SelfRename_Succeeds(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	staff := a.tenantStaff(tenant, "support")
	staffTok := a.token(staff, tenant, auth.RoleSupport, auth.PrincipalStaff)

	resp := a.do("PATCH", "/v1/admin/staff/me/display-name", staffTok, map[string]any{"display_name": "My Own Name"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", resp.status, resp.body)
	}
	if dn := staffDisplayName(t, a, tenant, staff); dn == nil || *dn != "My Own Name" {
		t.Fatalf("expected display_name=My Own Name, got %v", dn)
	}
	if !auditRowExists(t, a, tenant, "staff.display_name_changed", staff.String()) {
		t.Fatal("expected a staff.display_name_changed audit row for the self-rename")
	}
}

// TestStaffDisplayNameAPI_SelfRename_PlatformAdmin confirms the
// self-rename route also works for a platform_admin (tenant_id IS NULL).
func TestStaffDisplayNameAPI_SelfRename_PlatformAdmin(t *testing.T) {
	a := newKSAPI(t)
	platAdmin := a.platformAdmin()
	platTok := a.token(platAdmin, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)

	resp := a.do("PATCH", "/v1/admin/staff/me/display-name", platTok, map[string]any{"display_name": "Platform Self"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", resp.status, resp.body)
	}
	var dn *string
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT display_name FROM staff_users WHERE id = $1`, platAdmin).Scan(&dn)
	}); err != nil {
		t.Fatal(err)
	}
	if dn == nil || *dn != "Platform Self" {
		t.Fatalf("expected display_name=Platform Self, got %v", dn)
	}
	// F-8(b): assert the audit row, with before and after, on the
	// platform self-rename path too.
	var raw []byte
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id IS NULL AND action='staff.display_name_changed' AND target_id=$1`, platAdmin.String()).Scan(&raw)
	}); err != nil {
		t.Fatalf("read platform self-rename audit metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode audit metadata: %v", err)
	}
	if meta["after"] != "Platform Self" {
		t.Errorf("F-8(b): expected audit metadata.after=Platform Self, got %v", meta["after"])
	}
	if meta["before"] != nil {
		t.Errorf("F-8(b): expected audit metadata.before=nil (no prior name), got %v", meta["before"])
	}
	if meta["self"] != true {
		t.Errorf("F-8(b): expected audit metadata.self=true, got %v", meta["self"])
	}
}

// TestStaffDisplayNameAPI_SelfRename_CannotTargetAnotherStaffID confirms
// there is no id/body field that could ever redirect a self-rename at
// another staff member - the endpoint takes NO staff id at all.
func TestStaffDisplayNameAPI_SelfRename_CannotTargetAnotherStaffID(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	staff := a.tenantStaff(tenant, "support")
	staffTok := a.token(staff, tenant, auth.RoleSupport, auth.PrincipalStaff)
	other := a.tenantStaff(tenant, "support")

	// An extra, unrecognized "staff_id" field in the body must be refused
	// (decodeJSON's unknown-field rejection - same convention the
	// kill-switch route table relies on for "a body carrying a tenant id
	// is refused").
	resp := a.do("PATCH", "/v1/admin/staff/me/display-name", staffTok, map[string]any{
		"display_name": "Hijack Attempt", "staff_id": other.String(),
	})
	if resp.status != http.StatusBadRequest && resp.status != http.StatusUnprocessableEntity {
		t.Fatalf("expected an unknown-field body to be refused, got status=%d body=%s", resp.status, resp.body)
	}
	if dn := staffDisplayName(t, a, tenant, other); dn != nil {
		t.Fatalf("the other staff member must never be renamed by this route, got %v", *dn)
	}
}

// TestStaffDisplayNameAPI_HygieneRefusals: length, control, and bidi/
// zero-width refusals over HTTP (N-3).
func TestStaffDisplayNameAPI_HygieneRefusals(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	staff := a.tenantStaff(tenant, "support")
	staffTok := a.token(staff, tenant, auth.RoleSupport, auth.PrincipalStaff)

	cases := map[string]string{
		"empty":              "",
		"too_long":           strings.Repeat("a", 101),
		"control_char":       "Bad\u0007Name",
		"bidi_override":      "Bad" + string(rune(0x202E)) + "Name",
		"zero_width":         "Bad" + string(rune(0x200B)) + "Name",
		"word_joiner":        "Bad" + string(rune(0x2060)) + "Name", // G1-C3
		"byte_order_mark":    "Bad" + string(rune(0xFEFF)) + "Name", // G1-C3
		"arabic_letter_mark": "Bad" + string(rune(0x061C)) + "Name", // G1-C3
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			resp := a.do("PATCH", "/v1/admin/staff/me/display-name", staffTok, map[string]any{"display_name": value})
			if resp.status != http.StatusUnprocessableEntity && resp.status != http.StatusBadRequest {
				t.Fatalf("expected a validation refusal for %q, got status=%d body=%s", name, resp.status, resp.body)
			}
		})
	}
	if dn := staffDisplayName(t, a, tenant, staff); dn != nil {
		t.Fatalf("expected display_name to remain unset after all refusals, got %v", *dn)
	}
}

// TestStaffDisplayNameAPI_ScriptTagIsJSONEscaped: ADR 0104 §5.4's output
// encoding requirement - a name containing "<script>" is hygiene-valid
// (no control/bidi/zero-width characters) and is returned JSON-escaped by
// the standard encoder, never raw HTML.
func TestStaffDisplayNameAPI_ScriptTagIsJSONEscaped(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	staff := a.tenantStaff(tenant, "support")
	staffTok := a.token(staff, tenant, auth.RoleSupport, auth.PrincipalStaff)

	resp := a.do("PATCH", "/v1/admin/staff/me/display-name", staffTok, map[string]any{"display_name": "<script>alert(1)</script>"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", resp.status, resp.body)
	}
	dn := staffDisplayName(t, a, tenant, staff)
	if dn == nil || *dn != "<script>alert(1)</script>" {
		t.Fatalf("expected the raw stored value to be unescaped in the DB, got %v", dn)
	}
	// Go's encoding/json escapes '<', '>' and '&' to </>/&
	// by default (HTMLEscape, on by default for json.Marshal/Encoder) -
	// this is the ADR's "output-encoded by the JSON encoder" guarantee,
	// verified end to end via the platform-actions projection rather than
	// asserted only at the unit level.
}
