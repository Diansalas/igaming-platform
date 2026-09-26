// PAY-WH-TENANT-1 / API-DOC-PAYWH (docs/plans/stage-10.1-planning/
// 16-pay-wh-review-qa-test-plan.md §4): no OpenAPI schema-validation
// library is verified in go.sum for this module (gopkg.in/yaml.v3 and
// go.yaml.in/yaml/v3 appear only as transitive, go.mod-only entries with
// no package-content hash recorded - importing either would require
// fetching a new dependency, which this task must not do). Per the QA
// plan's own fallback ("if none available, use a minimal structural check
// with the standard library"), this test parses
// docs/api/openapi/platform-api.yaml as plain text and asserts the
// payments webhook path, its headers, and its response codes are present
// - not a full JSON-Schema conformance check. A live-request/response
// schema-conformance check (the QA plan's other suggestion) is NOT
// implemented here; that gap is recorded rather than silently skipped
// (docs/testing/testing-strategy.md is the right home for a platform-wide
// OpenAPI-lint decision, which is a devops/architect call, not this
// package's).
package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPI_PaymentsWebhook_ContractMatchesHandler(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatalf("read openapi spec: %v", err)
	}
	content := string(raw)

	const pathKey = "  /v1/webhooks/payments/{tenantSlug}/{providerID}:"
	start := strings.Index(content, pathKey)
	if start < 0 {
		t.Fatalf("expected the OpenAPI spec to document %q", pathKey)
	}
	rest := content[start+len(pathKey):]

	// The path's own block ends at the next line indented at the SAME
	// level (exactly 2 spaces then a non-space, non-"post:" key) - i.e.
	// the next top-level path entry.
	nextTopLevel := regexp.MustCompile(`(?m)^  \S`)
	loc := nextTopLevel.FindStringIndex(rest)
	block := rest
	if loc != nil {
		block = rest[:loc[0]]
	}

	mustContain := []string{
		"security: []",
		"X-Payments-Signature",
		`pattern: "^v1=[0-9a-f]{64}$"`,
		"X-Payments-Key-Id",
		"PaymentsMockCallback",
	}
	for _, want := range mustContain {
		if !strings.Contains(block, want) {
			t.Errorf("payments webhook OpenAPI entry missing %q", want)
		}
	}

	// Response codes: the handler's actually-reachable outcomes per
	// deposit_handlers.go/payment_callback_errors.go (mapReceiveCallbackError)
	// - 200/400/401/404/409/500/503.
	for _, code := range []string{`"200":`, `"400":`, `"401":`, `"404":`, `"409":`, `"500":`, `"503":`} {
		if !strings.Contains(block, code) {
			t.Errorf("payments webhook OpenAPI entry missing response code %s", code)
		}
	}

	// The schema referenced by the requestBody must itself exist and
	// declare the mock's own event_type/outcome enums, provider_reference
	// and amount - the load-bearing fields HandleCallback actually parses.
	schemaKey := "    PaymentsMockCallback:"
	schemaStart := strings.Index(content, schemaKey)
	if schemaStart < 0 {
		t.Fatalf("expected the OpenAPI spec to define the %q schema", strings.TrimSpace(schemaKey))
	}
	schemaRest := content[schemaStart+len(schemaKey):]
	schemaLoc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(schemaRest)
	schemaBlock := schemaRest
	if schemaLoc != nil {
		schemaBlock = schemaRest[:schemaLoc[0]]
	}
	for _, field := range []string{"event_type", "provider_reference", "outcome", "amount", "asset_code"} {
		if !strings.Contains(schemaBlock, field) {
			t.Errorf("PaymentsMockCallback schema missing field %q", field)
		}
	}
}
