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
	// - 200/400/401/409/500/503. PRH-payments-callback-cutover (ADR 0095
	// §6.2/S95-C4): 404 is NO LONGER part of this route's contract - an
	// unresolved deposit callback now defers (200, durable receipt) instead
	// of erroring; ErrDepositIntentNotFound is unreachable from
	// ReceiveVerifiedCallback's deposit/reversal branches.
	for _, code := range []string{`"200":`, `"400":`, `"401":`, `"409":`, `"500":`, `"503":`} {
		if !strings.Contains(block, code) {
			t.Errorf("payments webhook OpenAPI entry missing response code %s", code)
		}
	}
	if strings.Contains(block, `"404":`) {
		t.Error(`the payments webhook OpenAPI entry must NOT document a 404 response (ADR 0095 §6.2/S95-C4: an unresolved callback now defers instead of erroring)`)
	}

	// S95-C4: the 200 body must be the uniform, disposition-free shape
	// (request_id + received) - never a disposition- or resource-revealing
	// field such as deposit_intent_id/status/tombstoned.
	const responses200Marker = `"200":`
	idx200 := strings.Index(block, responses200Marker)
	idx400ForBlock := strings.Index(block, `"400":`)
	if idx200 < 0 || idx400ForBlock < 0 || idx400ForBlock < idx200 {
		t.Fatalf("expected the %q response block to precede %q", responses200Marker, `"400":`)
	}
	block200 := block[idx200:idx400ForBlock]
	for _, want := range []string{"request_id", "received"} {
		if !strings.Contains(block200, want) {
			t.Errorf("payments webhook 200 response schema missing uniform field %q", want)
		}
	}
	for _, mustNotAppear := range []string{"deposit_intent_id", "tombstoned"} {
		if strings.Contains(block200, mustNotAppear) {
			t.Errorf("payments webhook 200 response schema must not reveal disposition-specific field %q (S95-C4)", mustNotAppear)
		}
	}

	// Stage 10.1 security review PW-2/P3-5: the 400/401 split must
	// document the ACTUAL post-fix behavior - an oversized/unparseable
	// body is part of the uniform 401 family (pre-verification), never a
	// distinguishable 400, and 400 is reachable only after verification
	// succeeds. This is a structural substring check (no YAML/JSON-Schema
	// library - see this file's own top-of-file rationale), so it proves
	// the WORDING was updated alongside the code, not full semantic
	// conformance.
	const (
		responses400Marker = `"400":`
		responses401Marker = `"401":`
	)
	idx400 := strings.Index(block, responses400Marker)
	idx401 := strings.Index(block, responses401Marker)
	if idx400 < 0 || idx401 < 0 {
		t.Fatalf("expected both %q and %q response blocks to be present", responses400Marker, responses401Marker)
	}
	// The 400 block runs from its own marker up to the 401 marker (they
	// are adjacent, in ascending numeric order, in this spec).
	block400 := block[idx400:idx401]
	if !strings.Contains(block400, "verification succeeds") {
		t.Error(`the "400" response must document that it is reachable ONLY after signature verification succeeds`)
	}
	if strings.Contains(block400, "1 MiB") {
		t.Error(`the "400" response must NOT claim an oversized body - that is part of the uniform 401 family, not a distinguishable 400 (PW-2)`)
	}
	if !strings.Contains(block, `oversized`) {
		t.Error(`the "401" response must document that an oversized (>1 MiB) body is part of the uniform pre-verification 401 family`)
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
