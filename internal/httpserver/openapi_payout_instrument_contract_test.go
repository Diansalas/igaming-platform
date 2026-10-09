package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// B13 / B13-B: every payout-instrument route, the staff submit and the player
// withdrawal request are documented under the same path and method, with the
// error codes the handlers actually emit; the staff detail schema carries the
// masked bound instrument (and only those three fields).
func TestOpenAPI_PayoutInstrument_RoutesAndCodesDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	src, err := os.ReadFile("payout_instrument_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle\("(GET|POST) (/v1/[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) != 5 {
		t.Fatalf("expected 5 mounted payout-instrument operations, found %d", len(routes))
	}
	for _, r := range routes {
		block := openAPIPathBlock(t, spec, r[2])
		if !strings.Contains(block, "    "+strings.ToLower(r[1])+":\n") {
			t.Errorf("%s %s is mounted but not documented", r[1], r[2])
		}
	}
	for path, wants := range map[string][]string{
		"/v1/me/withdrawals": {"post:", "payout_instrument_id", "PAYOUT_INSTRUMENT_REQUIRED", "PAYOUT_INSTRUMENT_NOT_USABLE",
			"TENANT_OR_BRAND_NOT_ACTIVE", `"400"`, `"409"`, `"503"`},
		"/v1/admin/withdrawals/{id}/submit":     {"post:", "PAYMENT_METHOD_MISMATCH", "PAYOUT_DESTINATION_NOT_USABLE", `"400"`, `"409"`},
		"/v1/me/payout-instruments":             {"get:", "post:", "PAYOUT_INSTRUMENT_NOT_ACCEPTED", `"429"`},
		"/v1/me/payout-instruments/{id}/revoke": {"PAYOUT_INSTRUMENT_IN_USE", `"404"`},
	} {
		block := openAPIPathBlock(t, spec, path)
		for _, want := range wants {
			if !strings.Contains(block, want) {
				t.Errorf("%s: OpenAPI entry lacks %q", path, want)
			}
		}
	}
	start := strings.Index(spec, "    WithdrawalBoundInstrument:\n")
	if start < 0 {
		t.Fatal("schema WithdrawalBoundInstrument missing")
	}
	block := spec[start:]
	if loc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(block[5:]); loc != nil {
		block = block[:loc[0]+5]
	}
	for _, leak := range []string{"fingerprint", "ciphertext", "seal", "detail:"} {
		if strings.Contains(block, leak) {
			t.Errorf("the staff bound-instrument schema must not declare %q", leak)
		}
	}
	for _, want := range []string{"id:", "rail:", "display_mask:"} {
		if !strings.Contains(block, want) {
			t.Errorf("the staff bound-instrument schema lacks %q", want)
		}
	}
}
