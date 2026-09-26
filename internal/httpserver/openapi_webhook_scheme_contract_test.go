// Stage 10.3 W1a (WH-VENDOR-SCHEME-1; 01-provider-trust-analysis.md §1.3
// "API / OpenAPI"): each webhook path's authentication headers are
// re-described as provider-defined, with the X-{Domain}-* headers
// documented as the platform MOCK scheme only. Structural substring check,
// exactly like the per-domain contract tests (see
// openapi_paymentswebhook_contract_test.go's rationale); those tests are
// left unedited and still pin the MOCK header names, pattern and codes.
package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPI_Webhooks_AuthHeadersDocumentedAsProviderDefined(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatalf("read openapi spec: %v", err)
	}
	content := string(raw)
	nextTopLevel := regexp.MustCompile(`(?m)^  \S`)
	for domain, pathKey := range map[string]string{
		"Payments": "  /v1/webhooks/payments/{tenantSlug}/{providerID}:",
		"KYC":      "  /v1/webhooks/kyc/{tenantSlug}/{providerID}:",
		"Casino":   "  /v1/webhooks/casino/{tenantSlug}/{providerID}:",
	} {
		start := strings.Index(content, pathKey)
		if start < 0 {
			t.Fatalf("expected the OpenAPI spec to document %q", pathKey)
		}
		rest := content[start+len(pathKey):]
		block := rest
		if loc := nextTopLevel.FindStringIndex(rest); loc != nil {
			block = rest[:loc[0]]
		}
		for _, want := range []string{
			"- name: X-" + domain + "-Signature",
			"Authentication headers are provider-defined",
			"platform MOCK scheme only (never a vendor protocol)",
			"- name: X-" + domain + "-Key-Id",
			"Platform MOCK scheme key id",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("%s webhook OpenAPI entry missing %q", domain, want)
			}
		}
	}
}
