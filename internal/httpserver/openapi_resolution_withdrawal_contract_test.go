package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/payments"
)

// R36: every mounted route of the payment force-resolution (M1/M2/M4), the withdrawal
// hold-resolution (HSEC) and the core withdrawal lifecycle families is documented under
// the same path and method; the closed error tokens and the generic error codes those
// routes actually return are documented and present in the Error.code enum. DB-free: it
// reads the route sources and the spec only.
func TestOpenAPI_ResolutionAndWithdrawalFamilies_Documented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)

	re := regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|DELETE) (/v1/[^"]*)"`)
	var routes [][]string
	for file, filter := range map[string]func(string) bool{
		"payment_force_resolution_routes.go":   func(p string) bool { return strings.Contains(p, "/payment-force-resolutions") },
		"withdrawal_hold_resolution_routes.go": func(p string) bool { return strings.Contains(p, "/withdrawal-hold-resolutions") },
		"financial_routes.go": func(p string) bool {
			return strings.Contains(p, "/withdrawals") || strings.Contains(p, "/withdrawal-policies")
		},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if filter(m[2]) {
				routes = append(routes, m)
				n++
			}
		}
		want := map[string]int{"payment_force_resolution_routes.go": 6, "withdrawal_hold_resolution_routes.go": 6, "financial_routes.go": 15}[file]
		if n != want {
			t.Fatalf("%s: expected %d mounted operations in the family, found %d (update the spec and this test together)", file, want, n)
		}
	}
	for _, r := range routes {
		block := openAPIPathBlock(t, spec, r[2])
		if !strings.Contains(block, "    "+strings.ToLower(r[1])+":\n") {
			t.Errorf("%s %s is mounted but not documented", r[1], r[2])
		}
	}

	// The closed tokens are carried in Error.message; each must be documented.
	for _, tok := range []string{
		payments.TokenForceResolveDisabled, payments.TokenForceResolveNotPermitted, payments.TokenForceResolvePreconditionFail,
		payments.TokenForceResolveReasonNotResolved, payments.TokenForceResolveConflict, payments.TokenForceResolveExpired,
		payments.TokenForceResolveNotFound, payments.TokenForceResolveEvidenceInsufficient, payments.TokenForceResolveEvidenceMismatch,
		payments.TokenForceResolveEvidenceOverflow, payments.TokenForceResolveEvidenceUnsealed,
		payments.TokenHoldResolveDisabled, payments.TokenHoldResolveNotPermitted, payments.TokenHoldResolvePrecondition,
		payments.TokenHoldResolveConflict, payments.TokenHoldResolveExpired, payments.TokenHoldResolveNotFound,
	} {
		if !strings.Contains(spec, tok) {
			t.Errorf("closed error token %q is not documented", tok)
		}
	}

	// The generic envelope codes these routes return must exist in the Error.code enum.
	start := strings.Index(spec, "    Error:\n")
	if start < 0 {
		t.Fatal("schema Error missing")
	}
	enum := spec[start:]
	if i := strings.Index(enum, "        message:"); i > 0 {
		enum = enum[:i]
	}
	for _, code := range []string{"validation_error", "invalid_request", "unauthorized", "forbidden", "not_found", "conflict", "internal_error", "service_unavailable"} {
		if !strings.Contains(enum, "- "+code+"\n") {
			t.Errorf("Error.code enum lacks %q", code)
		}
	}

	// Specific contract points.
	resolve := openAPIPathBlock(t, spec, "/v1/admin/withdrawals/{id}/resolve")
	for _, want := range []string{"dispatch is still in progress", "amount/asset does not", `"409"`, `"503"`} {
		if !strings.Contains(resolve, want) {
			t.Errorf("resolve: OpenAPI entry lacks %q", want)
		}
	}
	for _, p := range []string{
		"/v1/admin/tenants/{tenantID}/payment-force-resolutions",
		"/v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions",
	} {
		b := openAPIPathBlock(t, spec, p)
		for _, want := range []string{"MOCK", "a body tenant_id is accepted and ignored", "signed actor proof"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: OpenAPI entry lacks %q", p, want)
			}
		}
	}
	if !strings.Contains(openAPIPathBlock(t, spec, "/v1/admin/tenants/{tenantID}/payment-force-resolutions"), "non-MOCK M4") {
		t.Error("payment force-resolution entry must state that non-MOCK M4 is blocked")
	}
}
