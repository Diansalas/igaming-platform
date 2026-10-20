package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// lic6 I-4: every platform registry READ route is gated by jurisdiction_registry:read (held by platform_admin only, pinned in
// internal/auth) and none is wrapped in RequireTenantScope or a tenant permission; the read handlers take the platform
// transaction (WithPlatformAdmin) and never a tenant one. DB-free; the behavioural refusal of non-platform principals is
// TestReadAPIs_NonPlatformPrincipalsRefused (integration).
func TestRegistryReadRoutes_AreGatedByTheReadPermissionOnly(t *testing.T) {
	b, err := os.ReadFile("jurisdiction_admin_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	re := regexp.MustCompile(`mux\.Handle\("GET (/v1/admin/(?:jurisdictions|licences|licence-country-ceilings|operating-country-policies|operating-bases|tenants/\{tenantID\}/jurisdiction-configs)[^"]*)",\s*\n\s*(staffRoute\([^\n]*)`)
	ms := re.FindAllStringSubmatch(src, -1)
	want := map[string]bool{
		"/v1/admin/jurisdictions": true, "/v1/admin/jurisdictions/{jurisdictionID}": true,
		"/v1/admin/licences": true, "/v1/admin/licences/{licenceID}": true, "/v1/admin/licences/{licenceID}/versions": true,
		"/v1/admin/licence-country-ceilings": true, "/v1/admin/operating-country-policies": true,
		"/v1/admin/operating-bases": true, "/v1/admin/operating-bases/{basisID}": true,
		"/v1/admin/tenants/{tenantID}/jurisdiction-configs": true,
	}
	for _, m := range ms {
		delete(want, m[1])
		if !strings.Contains(m[2], "auth.RequirePermission(auth.PermJurisdictionRegistryRead)") {
			t.Errorf("GET %s must be gated by auth.PermJurisdictionRegistryRead: %s", m[1], m[2])
		}
		if strings.Contains(m[2], "RequireTenantScope") || strings.Contains(m[2], "PermJurisdictionRegistryManage") {
			t.Errorf("GET %s must not be tenant-scoped or use the manage permission: %s", m[1], m[2])
		}
		if !strings.Contains(m[2], "stepUpNone") {
			t.Errorf("GET %s must be step-up none", m[1])
		}
	}
	for p := range want {
		t.Errorf("registry read route GET %s is not mounted in jurisdiction_admin_routes.go", p)
	}
	h, err := os.ReadFile("licence_registry_read_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(h), "WithTenant(") || strings.Contains(string(h), "WithPrincipalScope(") {
		t.Error("the registry read handlers must use the platform transaction only")
	}
	if n := strings.Count(string(h), "deps.DB.WithPlatformAdmin("); n < 5 {
		t.Errorf("expected the read handlers to open platform transactions, found %d", n)
	}
}
