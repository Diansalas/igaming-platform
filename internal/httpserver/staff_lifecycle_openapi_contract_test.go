package httpserver

// ADR 0119 S5 + S6: DB-free OpenAPI drift for the staff lifecycle routes (docs/plans/adr0119-s5s6-api-contract.md):
// mounted == documented in both directions, every lifecycle DTO == its schema, and no response DTO or response
// schema can carry a secret (the invitation / MFA-reset token travels ONLY in the email body, never in an HTTP
// response; the acceptance request that carries it is x-sensitive).

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/stafflifecycle"
)

var lifecyclePrefixes = []string{
	"/v1/admin/tenants/{tenantID}/staff/invitations",
	"/v1/admin/platform-staff",
	"/v1/admin/platform/staff-lifecycle-requests",
	"/v1/staff/invitations/accept",
}

// lifecycleTenantStaffOps are the tenant-staff paths the lifecycle routes own exactly (the shared
// /v1/admin/tenants/{tenantID}/staff/{staffID}/ prefix also carries the display-name and person-link routes).
var lifecycleTenantStaffPaths = []string{
	"/v1/admin/tenants/{tenantID}/staff",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/lifecycle-events",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/invitation/resend",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/invitation/revoke",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/suspend",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/reactivate",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/offboard",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/role",
	"/v1/admin/tenants/{tenantID}/staff/{staffID}/mfa-reset",
}

func isLifecyclePath(p string) bool {
	for _, x := range lifecyclePrefixes {
		if strings.HasPrefix(p, x) {
			return true
		}
	}
	for _, x := range lifecycleTenantStaffPaths {
		if p == x {
			return true
		}
	}
	return false
}

func TestOpenAPI_StaffLifecycle_MountedEqualsDocumented(t *testing.T) {
	spec := readSpec(t)
	var mounted []string
	for _, r := range mountedRoutes(t, "staff_lifecycle_routes.go", "routes.go") {
		if isLifecyclePath(strings.SplitN(r, " ", 2)[1]) {
			mounted = append(mounted, r)
		}
	}
	var documented []string
	for _, op := range documentedOps(spec, "/v1/") {
		if isLifecyclePath(strings.SplitN(op, " ", 2)[1]) {
			documented = append(documented, op)
		}
	}
	sort.Strings(mounted)
	sort.Strings(documented)
	// 25 lifecycle routes plus the pre-existing platform person-link route under the platform-staff prefix.
	if len(mounted) != 26 {
		t.Fatalf("expected 26 routes (the 25 lifecycle routes + platform person-link), mounted %d: %v", len(mounted), mounted)
	}
	if !reflect.DeepEqual(mounted, documented) {
		t.Fatalf("lifecycle route drift:\n mounted    %v\n documented %v", mounted, documented)
	}
	// The legacy admin-chosen-password create route is gone from both (ADR 0119 S5, section 10.2).
	for _, r := range append(mounted, documented...) {
		if r == "POST /v1/admin/tenants/{tenantID}/staff" {
			t.Fatal("the legacy POST /v1/admin/tenants/{tenantID}/staff route must not exist")
		}
	}
	for _, r := range mountedRoutes(t, "routes.go", "admin_routes.go") {
		if r == "POST /v1/admin/tenants/{tenantID}/staff" {
			t.Fatal("the legacy create route is still mounted")
		}
	}
}

func TestOpenAPI_StaffLifecycle_DTOsMatchSchemas(t *testing.T) {
	spec := readSpec(t)
	for _, c := range []struct {
		schema string
		typ    reflect.Type
	}{
		{"StaffMfaSummary", reflect.TypeOf(staffMfaSummaryDTO{})},
		{"StaffDirectoryEntry", reflect.TypeOf(staffDirectoryEntryDTO{})},
		{"StaffDirectoryList", reflect.TypeOf(staffDirectoryListDTO{})},
		{"StaffLifecycleResult", reflect.TypeOf(staffLifecycleResultDTO{})},
		{"StaffLifecycleState", reflect.TypeOf(stafflifecycle.State{})},
		{"StaffLifecycleEvent", reflect.TypeOf(staffLifecycleEventDTO{})},
		{"StaffLifecycleEventList", reflect.TypeOf(staffLifecycleEventListDTO{})},
		{"PlatformStaffLifecycleRequest", reflect.TypeOf(platformStaffLifecycleRequestDTO{})},
		{"PlatformStaffLifecycleRequestList", reflect.TypeOf(platformStaffLifecycleRequestListDTO{})},
		{"PlatformStaffLifecycleRequestCreate", reflect.TypeOf(platformStaffLifecycleRequestCreate{})},
		{"PlatformStaffLifecycleDecision", reflect.TypeOf(platformStaffLifecycleDecision{})},
		{"StaffInvitationRequest", reflect.TypeOf(staffInvitationRequest{})},
		// One Go body for every action: the role-change schema documents all of it; the other actions' schema is
		// the same minus `role` (the handler refuses a role on any other action).
		{"StaffRoleChangeRequest", reflect.TypeOf(staffLifecycleActionRequest{})},
		{"InvitationAcceptRequest", reflect.TypeOf(invitationAcceptRequest{})},
	} {
		got, want := govSchemaProps(t, spec, c.schema), jsonFieldNames(c.typ)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema properties %v != %s JSON fields %v", c.schema, got, c.typ.Name(), want)
		}
	}
	var noRole []string
	for _, f := range jsonFieldNames(reflect.TypeOf(staffLifecycleActionRequest{})) {
		if f != "role" {
			noRole = append(noRole, f)
		}
	}
	if got := govSchemaProps(t, spec, "StaffLifecycleActionRequest"); !reflect.DeepEqual(got, noRole) {
		t.Errorf("StaffLifecycleActionRequest: schema properties %v != %v", got, noRole)
	}
}

// secretShaped reports a JSON field / schema property name that could carry a secret.
func secretShaped(name string) bool {
	for _, seg := range strings.Split(name, "_") {
		switch seg {
		case "token", "secret", "password", "otpauth", "recovery", "uri", "code":
			return true
		}
	}
	return false
}

// jsonNamesDeep returns every JSON field name reachable from typ (structs, slices, pointers).
func jsonNamesDeep(typ reflect.Type, seen map[reflect.Type]bool) []string {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return nil
	}
	seen[typ] = true
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			out = append(out, tag)
		}
		out = append(out, jsonNamesDeep(f.Type, seen)...)
	}
	return out
}

// The invitation / MFA-reset token is never in an HTTP response (OQ-1 (b) and the API contract): no response DTO and
// no response schema has a secret-shaped field, and the one request that carries the token is x-sensitive.
func TestOpenAPI_StaffLifecycle_NoSecretInAnyResponse(t *testing.T) {
	spec := readSpec(t)
	for _, typ := range []reflect.Type{
		reflect.TypeOf(staffDirectoryEntryDTO{}), reflect.TypeOf(staffDirectoryListDTO{}), reflect.TypeOf(staffLifecycleResultDTO{}),
		reflect.TypeOf(staffLifecycleEventListDTO{}), reflect.TypeOf(platformStaffLifecycleRequestListDTO{}),
		reflect.TypeOf(platformStaffLifecycleRequestDTO{}),
	} {
		for _, n := range jsonNamesDeep(typ, map[reflect.Type]bool{}) {
			if secretShaped(n) && n != "reason_code" {
				t.Errorf("response DTO %s has a secret-shaped field %q", typ.Name(), n)
			}
		}
	}
	for _, s := range []string{"StaffMfaSummary", "StaffDirectoryEntry", "StaffDirectoryList", "StaffLifecycleResult", "StaffLifecycleState",
		"StaffLifecycleEvent", "StaffLifecycleEventList", "PlatformStaffLifecycleRequest", "PlatformStaffLifecycleRequestList"} {
		for _, p := range govSchemaProps(t, spec, s) {
			if secretShaped(p) && p != "reason_code" {
				t.Errorf("response schema %s has a secret-shaped property %q", s, p)
			}
		}
	}
	if b := govSchemaBlock(t, spec, "InvitationAcceptRequest"); !strings.Contains(b, "x-sensitive") {
		t.Error("InvitationAcceptRequest carries the token and must be x-sensitive")
	}
	// Non-vacuity: the scanner catches a planted token field.
	type planted struct {
		Staff struct {
			InviteToken string `json:"invite_token"`
		} `json:"staff"`
	}
	found := false
	for _, n := range jsonNamesDeep(reflect.TypeOf(planted{}), map[reflect.Type]bool{}) {
		found = found || secretShaped(n)
	}
	if !found {
		t.Fatal("the scanner must catch a nested token field")
	}
}
