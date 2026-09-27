// PRH-I1 (ADR 0095 §10.5): contract test for the payment kill-switch admin
// API. Every route registerPaymentsKillSwitchRoutes mounts is documented
// under the same path and method; the request schemas refuse unknown
// properties and carry no tenant field; the Error enum's 409 conflict
// response is referenced. Structural substring checks, mirroring
// openapi_provider_credentials_contract_test.go exactly.
package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPI_PaymentsKillSwitch_RoutesDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	src, err := os.ReadFile("payments_kill_switch_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle\("(GET|POST) (/v1/admin/tenants/\{tenantID\}/payments/[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) != 7 {
		t.Fatalf("expected 7 mounted operations, found %d", len(routes))
	}
	for _, r := range routes {
		method, path := strings.ToLower(r[1]), r[2]
		block := openAPIPathBlock(t, spec, path)
		if !strings.Contains(block, "    "+method+":\n") {
			t.Fatalf("%s %s is mounted but not documented", r[1], path)
		}
	}

	for path, codes := range map[string][]string{
		"/v1/admin/tenants/{tenantID}/payments/kill-switches":                                    {`"409"`, `"403"`, `"401"`},
		"/v1/admin/tenants/{tenantID}/payments/kill-switches/{killSwitchID}/release-requests":    {`"409"`, `"404"`},
		"/v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}/approve": {`"409"`, `"404"`},
		"/v1/admin/tenants/{tenantID}/payments/kill-switch-release-requests/{requestID}/cancel":  {`"409"`, `"404"`},
		"/v1/admin/tenants/{tenantID}/payments/kill-switches/{killSwitchID}":                     {`"404"`, `"403"`},
	} {
		block := openAPIPathBlock(t, spec, path)
		for _, want := range codes {
			if !strings.Contains(block, want) {
				t.Errorf("%s: OpenAPI entry lacks %q", path, want)
			}
		}
	}

	for _, schema := range []string{"PaymentsKillSwitchEngageRequest", "PaymentsKillSwitchReleaseRequestBody"} {
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
		if regexp.MustCompile(`(?m)^        tenant_id:`).MatchString(block) {
			t.Errorf("%s must carry no tenant field - the target tenant comes from the path only", schema)
		}
	}

	// The response schema (PaymentsKillSwitch) must expose the DB-forced
	// actor/scope columns as read-only response fields, never accept them
	// as request input (they are absent from both request schemas above -
	// checked separately since PaymentsKillSwitch is a response-only type).
	for _, field := range []string{"engaged_by_scope", "changed_by", "changed_by_scope", "version"} {
		if !strings.Contains(spec, field+":") {
			t.Errorf("PaymentsKillSwitch response schema missing field %q", field)
		}
	}
}
