package httpserver

import (
	"strings"
	"testing"
)

// lic4 slice 4a: every read route of the jurisdiction / licence registry is documented under the same path and method, names its
// permission (jurisdiction_registry:read), security and 401, and the lifecycle PUTs document the 202 filing. DB-free.
func TestOpenAPI_LicenceRegistryReads_Documented(t *testing.T) {
	spec := launchSpec(t)
	for _, c := range []struct{ path, method string }{
		{"/v1/admin/jurisdictions", "get"},
		{"/v1/admin/jurisdictions/{jurisdictionID}", "get"},
		{"/v1/admin/licences", "get"},
		{"/v1/admin/licences/{licenceID}", "get"},
		{"/v1/admin/licences/{licenceID}/versions", "get"},
		{"/v1/admin/licence-country-ceilings", "get"},
		{"/v1/admin/operating-country-policies", "get"},
		{"/v1/admin/tenants/{tenantID}/jurisdiction-configs", "get"},
	} {
		block := openAPIPathBlock(t, spec, c.path)
		if !strings.Contains(block, "    "+c.method+":\n") {
			t.Errorf("%s %s is not documented", c.method, c.path)
			continue
		}
		if !strings.Contains(block, "jurisdiction_registry:read") || !strings.Contains(block, "bearerAuth") || !strings.Contains(block, `"401"`) || !strings.Contains(block, `"403"`) {
			t.Errorf("%s: the entry must name jurisdiction_registry:read, security, 401 and 403", c.path)
		}
	}
	for _, p := range []string{"status", "validity-dates", "review-due", "evidence-reference"} {
		block := openAPIPathBlock(t, spec, "/v1/admin/licences/{licenceID}/"+p)
		for _, want := range []string{`"202"`, "LicensingChangeResponse", "migration 0178", "licensing_change:request"} {
			if !strings.Contains(block, want) {
				t.Errorf("PUT %s: OpenAPI entry lacks %q", p, want)
			}
		}
		if strings.Contains(block, "NO maker-checker") {
			t.Errorf("PUT %s: the slice 1 single-actor text must be gone", p)
		}
	}
	for _, k := range []string{"licence_status_change", "licence_validity_dates_change", "licence_review_due_change", "licence_evidence_reference_change"} {
		if !strings.Contains(spec, k) {
			t.Errorf("the OpenAPI kind enum must list %s", k)
		}
	}
}
