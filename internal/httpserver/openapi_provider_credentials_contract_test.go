// Stage 10.3 W2a: contract test for the provider-credential admin API
// (security review §6). Every route registerProviderCredentialRoutes mounts
// is documented under the same path and method, with its error codes; the
// request schemas carry no tenant field and refuse unknown properties; and
// the Error enum lists the new closed codes. Structural substring checks,
// like the other OpenAPI contract tests in this package.
package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func openAPIPathBlock(t *testing.T, spec, path string) string {
	t.Helper()
	key := "  " + path + ":\n"
	start := strings.Index(spec, key)
	if start < 0 {
		t.Fatalf("OpenAPI spec does not document %s", path)
	}
	rest := spec[start+len(key):]
	if loc := regexp.MustCompile(`(?m)^  \S`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

func TestOpenAPI_ProviderCredentials_RoutesDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	src, err := os.ReadFile("provider_credential_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle\("(GET|POST) (/v1/admin/tenants/\{tenantID\}/provider-credential[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) != 7 {
		t.Fatalf("expected 7 mounted operations (6 routes), found %d", len(routes))
	}
	for _, r := range routes {
		method, path := strings.ToLower(r[1]), r[2]
		block := openAPIPathBlock(t, spec, path)
		if !strings.Contains(block, "    "+method+":\n") {
			t.Fatalf("%s %s is mounted but not documented", r[1], path)
		}
	}
	for path, codes := range map[string][]string{
		"/v1/admin/tenants/{tenantID}/provider-credential-requests":                       {"credential_registration_rejected", `"409"`, `"403"`, `"401"`},
		"/v1/admin/tenants/{tenantID}/provider-credential-requests/{requestID}/decisions": {"approval_rejected", `"409"`},
		"/v1/admin/tenants/{tenantID}/provider-credential-requests/{requestID}/apply":     {"credential_activation_rejected", `"409"`},
		"/v1/admin/tenants/{tenantID}/provider-credentials/{handleID}/transitions":        {"credential_transition_rejected", `"409"`, `"400"`},
		"/v1/admin/tenants/{tenantID}/provider-credentials":                               {"provider_credential:read", `"403"`},
	} {
		block := openAPIPathBlock(t, spec, path)
		for _, want := range codes {
			if !strings.Contains(block, want) {
				t.Errorf("%s: OpenAPI entry lacks %q", path, want)
			}
		}
	}
	for _, schema := range []string{"ProviderCredentialRegistrationRequest", "ProviderCredentialDecisionRequest", "ProviderCredentialTransitionRequest"} {
		start := strings.Index(spec, "    "+schema+":\n")
		if start < 0 {
			t.Fatalf("schema %s missing", schema)
		}
		block := spec[start:]
		if loc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(block[5:]); loc != nil {
			block = block[:loc[0]+5]
		}
		if !strings.Contains(block, "additionalProperties: false") {
			t.Errorf("%s must refuse unknown properties", schema)
		}
		if regexp.MustCompile(`(?m)^        (tenant_id|target_tenant_id|secret|secret_value):`).MatchString(block) {
			t.Errorf("%s must carry no tenant or secret field", schema)
		}
	}
	for _, code := range []string{"invalid_request", "credential_registration_rejected", "approval_rejected",
		"credential_activation_rejected", "credential_transition_rejected"} {
		if !strings.Contains(spec, "            - "+code+"\n") {
			t.Errorf("the Error code enum lacks %s", code)
		}
	}
}
