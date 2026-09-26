// KYC-WH-1 (Stage 10.2, ADR 0091, design §H K14): modeled exactly on
// openapi_paymentswebhook_contract_test.go's own structural-check pattern
// (see that file's header comment for why this is a text-structural check,
// not a full JSON-Schema conformance check).
package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPI_KYCWebhook_ContractMatchesHandler(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatalf("read openapi spec: %v", err)
	}
	content := string(raw)

	const pathKey = "  /v1/webhooks/kyc/{tenantSlug}/{providerID}:"
	start := strings.Index(content, pathKey)
	if start < 0 {
		t.Fatalf("expected the OpenAPI spec to document %q", pathKey)
	}
	rest := content[start+len(pathKey):]

	nextTopLevel := regexp.MustCompile(`(?m)^  \S`)
	loc := nextTopLevel.FindStringIndex(rest)
	block := rest
	if loc != nil {
		block = rest[:loc[0]]
	}

	mustContain := []string{
		"security: []",
		"X-KYC-Signature",
		`pattern: "^v1=[0-9a-f]{64}$"`,
		"X-KYC-Key-Id",
		"KYCMockCallback",
		"test support",
	}
	for _, want := range mustContain {
		if !strings.Contains(block, want) {
			t.Errorf("KYC webhook OpenAPI entry missing %q", want)
		}
	}

	// Response codes: the handler's actually-reachable outcomes per
	// kyc_admin_handlers.go's newKYCWebhookHandler - 204/400/401/404/500.
	// Deliberately NOT 200 (the pre-fix leak this stage closes) and NOT
	// 409/503 (payments-only).
	for _, code := range []string{`"204":`, `"400":`, `"401":`, `"404":`, `"500":`} {
		if !strings.Contains(block, code) {
			t.Errorf("KYC webhook OpenAPI entry missing response code %s", code)
		}
	}
	if strings.Contains(block, `"200":`) {
		t.Error(`KYC webhook OpenAPI entry must not document a "200" response - success is 204 with no body (the pre-fix leak this stage closes)`)
	}

	// The 400/401 split must document the actual post-fix behavior: 400
	// is reachable only after verification succeeds, and an oversized
	// body is part of the uniform 401 family.
	idx400 := strings.Index(block, `"400":`)
	idx401 := strings.Index(block, `"401":`)
	if idx400 < 0 || idx401 < 0 {
		t.Fatalf("expected both %q and %q response blocks to be present", `"400":`, `"401":`)
	}
	block400 := block[idx400:idx401]
	if !strings.Contains(block400, "verification succeeds") {
		t.Error(`the "400" response must document that it is reachable ONLY after signature verification succeeds`)
	}
	if !strings.Contains(block, "oversized") {
		t.Error(`the "401" response must document that an oversized (>256 KiB) body is part of the uniform pre-verification 401 family`)
	}

	// The KYCMockCallback schema must exist and declare the load-bearing
	// fields the mock adapter's HandleCallback actually parses.
	schemaKey := "    KYCMockCallback:"
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
	for _, field := range []string{"provider_reference", "outcome", "reason"} {
		if !strings.Contains(schemaBlock, field) {
			t.Errorf("KYCMockCallback schema missing field %q", field)
		}
	}

	// PlayerVerification must exist and must NOT declare provider_reference
	// (K10/design §B5) - the player-facing schema the two /v1/me/kyc/
	// verifications operations now reference.
	pvKey := "    PlayerVerification:"
	pvStart := strings.Index(content, pvKey)
	if pvStart < 0 {
		t.Fatalf("expected the OpenAPI spec to define the %q schema", strings.TrimSpace(pvKey))
	}
	pvRest := content[pvStart+len(pvKey):]
	pvLoc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(pvRest)
	pvBlock := pvRest
	if pvLoc != nil {
		pvBlock = pvRest[:pvLoc[0]]
	}
	if strings.Contains(pvBlock, "provider_reference:") {
		t.Error("PlayerVerification schema must never declare a provider_reference property (K10/design §B5)")
	}
	for _, field := range []string{"id", "player_account_id", "status", "provider_id"} {
		if !strings.Contains(pvBlock, field) {
			t.Errorf("PlayerVerification schema missing field %q", field)
		}
	}

	// The player-facing paths must reference PlayerVerification, not the
	// staff-facing Verification schema.
	meKYCKey := "  /v1/me/kyc/verifications:"
	meStart := strings.Index(content, meKYCKey)
	if meStart < 0 {
		t.Fatalf("expected the OpenAPI spec to document %q", meKYCKey)
	}
	meRest := content[meStart+len(meKYCKey):]
	meLoc := nextTopLevel.FindStringIndex(meRest)
	meBlock := meRest
	if meLoc != nil {
		meBlock = meRest[:meLoc[0]]
	}
	if !strings.Contains(meBlock, "PlayerVerification") {
		t.Error("/v1/me/kyc/verifications must reference the PlayerVerification schema")
	}
	if strings.Contains(meBlock, "$ref: \"#/components/schemas/Verification\"") {
		t.Error("/v1/me/kyc/verifications must not reference the staff-facing Verification schema")
	}
}
