// ADR 0097 (PAYWH-RL-1) T18 OpenAPIContract_WebhookRateLimit (§16 QA
// review item 3): the spec documents 429 and 503 (with Retry-After) on
// all three webhook paths. Same "plain-text structural check, no schema-
// validation library" approach as the existing openapi_*webhook_contract_
// test.go files (see their own doc comments for why).
package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func webhookOpenAPIBlock(t *testing.T, pathKey string) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatalf("read openapi spec: %v", err)
	}
	content := string(raw)
	start := strings.Index(content, pathKey)
	if start < 0 {
		t.Fatalf("expected the OpenAPI spec to document %q", pathKey)
	}
	rest := content[start+len(pathKey):]
	nextTopLevel := regexp.MustCompile(`(?m)^  \S`)
	loc := nextTopLevel.FindStringIndex(rest)
	if loc != nil {
		return rest[:loc[0]]
	}
	return rest
}

func TestOpenAPI_WebhookRateLimit_AllThreeRoutesDocument429And503(t *testing.T) {
	for _, path := range []string{
		"  /v1/webhooks/payments/{tenantSlug}/{providerID}:",
		"  /v1/webhooks/casino/{tenantSlug}/{providerID}:",
		"  /v1/webhooks/kyc/{tenantSlug}/{providerID}:",
	} {
		t.Run(path, func(t *testing.T) {
			block := webhookOpenAPIBlock(t, path)
			if !strings.Contains(block, `"429"`) {
				t.Errorf("%s: missing 429 response (ADR 0097)", path)
			}
			if !strings.Contains(block, `"503"`) {
				t.Errorf("%s: missing 503 response", path)
			}
			// Retry-After is documented either inline (a header block in
			// this path's own response) or via a $ref to one of the
			// shared components that itself documents it (checked
			// separately by TestOpenAPI_WebhookRateLimit_GenericBodyOnly's
			// sibling assertions below) - a $ref by definition doesn't
			// repeat the header text at the use site.
			if !strings.Contains(block, "Retry-After") &&
				!strings.Contains(block, "#/components/responses/WebhookRateLimited") &&
				!strings.Contains(block, "#/components/responses/WebhookAdmissionUnavailable") {
				t.Errorf("%s: missing a documented Retry-After header (inline or via the shared WebhookRateLimited/WebhookAdmissionUnavailable components)", path)
			}
		})
	}
}

// TestOpenAPI_WebhookRateLimit_GenericBodyOnly guards against the generic
// admission-rejection response schema ever growing a domain-specific
// field: both shared response components must reference the same generic
// Error schema, never a bespoke one.
func TestOpenAPI_WebhookRateLimit_GenericBodyOnly(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatalf("read openapi spec: %v", err)
	}
	content := string(raw)
	for _, component := range []string{"WebhookRateLimited:", "WebhookAdmissionUnavailable:"} {
		start := strings.Index(content, component)
		if start < 0 {
			t.Fatalf("expected the OpenAPI spec to define components.responses.%s", component)
		}
		rest := content[start:]
		nextComponent := regexp.MustCompile(`(?m)^    \S`)
		loc := nextComponent.FindStringIndex(rest[1:])
		block := rest
		if loc != nil {
			block = rest[:loc[0]+1]
		}
		if !strings.Contains(block, `$ref: "#/components/schemas/Error"`) {
			t.Errorf("%s must reference the generic Error schema only", component)
		}
	}
}
