// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091, design §G/§H C14): modeled on
// openapi_paymentswebhook_contract_test.go exactly - see that file's own
// top-of-file rationale for why this is a structural substring check
// against the plain-text spec, not a full JSON-Schema conformance check.
package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPI_CasinoWebhook_ContractMatchesHandler(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatalf("read openapi spec: %v", err)
	}
	content := string(raw)

	const pathKey = "  /v1/webhooks/casino/{tenantSlug}/{providerID}:"
	start := strings.Index(content, pathKey)
	if start < 0 {
		t.Fatalf("expected the OpenAPI spec to document %q", pathKey)
	}
	rest := content[start+len(pathKey):]

	// The path's own block ends at the next line indented at the SAME
	// level (exactly 2 spaces then a non-space) - i.e. the next top-level
	// path entry.
	nextTopLevel := regexp.MustCompile(`(?m)^  \S`)
	loc := nextTopLevel.FindStringIndex(rest)
	block := rest
	if loc != nil {
		block = rest[:loc[0]]
	}

	mustContain := []string{
		"security: []",
		"X-Casino-Signature",
		`pattern: "^v1=[0-9a-f]{64}$"`,
		"X-Casino-Key-Id",
		"CasinoMockCallback",
	}
	for _, want := range mustContain {
		if !strings.Contains(block, want) {
			t.Errorf("casino webhook OpenAPI entry missing %q", want)
		}
	}

	// Response codes: the handler's actually-reachable outcomes per
	// casino_handlers.go's newCasinoWebhookHandler - 200/400/401/409/500/503.
	for _, code := range []string{`"200":`, `"400":`, `"401":`, `"409":`, `"500":`, `"503":`} {
		if !strings.Contains(block, code) {
			t.Errorf("casino webhook OpenAPI entry missing response code %s", code)
		}
	}

	// Stage 10.2 design §C2/§C6: the 400/401 split must document the
	// ACTUAL post-fix behavior - an oversized/unparseable body is part of
	// the uniform 401 family (pre-verification), never a distinguishable
	// 400, and 400 is reachable only after verification succeeds.
	const (
		responses400Marker = `"400":`
		responses401Marker = `"401":`
	)
	idx400 := strings.Index(block, responses400Marker)
	idx401 := strings.Index(block, responses401Marker)
	if idx400 < 0 || idx401 < 0 {
		t.Fatalf("expected both %q and %q response blocks to be present", responses400Marker, responses401Marker)
	}
	block400 := block[idx400:idx401]
	if !strings.Contains(block400, "verification succeeds") {
		t.Error(`the "400" response must document that it is reachable ONLY after signature verification succeeds`)
	}
	if strings.Contains(block400, "1 MiB") {
		t.Error(`the "400" response must NOT claim an oversized body - that is part of the uniform 401 family, not a distinguishable 400`)
	}
	// Stage 10.2 final review (K8/L3): scope the "oversized" check to the
	// "401" response block SPECIFICALLY, not the whole path entry - the
	// top-of-path description also happens to mention "oversized" (in
	// prose explaining the design), so a bare block-wide substring check
	// would stay green even if the "401" response object's OWN
	// description dropped the word entirely. This is proven by
	// construction below: idx409 bounds block401 to end exactly where the
	// "401" response object ends and the "409" one begins, so text living
	// only in the shared top-of-path description (before idx401) can never
	// satisfy this assertion.
	idx409 := strings.Index(block[idx401:], `"409":`)
	if idx409 < 0 {
		t.Fatalf(`expected a %q response block after %q`, `"409":`, responses401Marker)
	}
	block401 := block[idx401 : idx401+idx409]
	if !strings.Contains(block401, `oversized`) {
		t.Error(`the "401" response block ITSELF must document that an oversized (>1 MiB) body is part of the uniform pre-verification 401 family`)
	}
	// The 503 branch is post-verification (the casino capability kill
	// switch), unlike payments' unconditional 503 - the spec text must say
	// so, so a reader cannot mistake it for a pre-verification oracle.
	idx503 := strings.Index(block, `"503":`)
	if idx503 < 0 {
		t.Fatal(`expected a "503" response block`)
	}
	block503 := block[idx503:]
	if loc := nextTopLevel.FindStringIndex(block503); loc != nil {
		block503 = block503[:loc[0]]
	}
	if !strings.Contains(block503, "verification succeeds") {
		t.Error(`the "503" response must document that it is reachable ONLY after signature verification succeeds (the capability kill switch)`)
	}

	// The schema referenced by the requestBody must itself exist and
	// declare the mock's own event_type/outcome enums, provider_tx_id and
	// amount - the load-bearing fields HandleCallback actually parses.
	schemaKey := "    CasinoMockCallback:"
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
	for _, field := range []string{"event_type", "provider_tx_id", "outcome", "amount", "asset_code", "session_id"} {
		if !strings.Contains(schemaBlock, field) {
			t.Errorf("CasinoMockCallback schema missing field %q", field)
		}
	}
}
