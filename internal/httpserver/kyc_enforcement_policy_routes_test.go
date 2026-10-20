package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// lic10 (L-20): static pins of the KYC enforcement policy authoring routes (no database).

var kycPolicyRoutes = map[string]string{
	"GET /v1/admin/kyc/enforcement-policies":                       "none",
	"POST /v1/admin/kyc/enforcement-policies":                      "critical",
	"POST /v1/admin/kyc/enforcement-policies/{policyID}/activate":  "critical",
	"POST /v1/admin/kyc/enforcement-policies/{policyID}/withdraw":  "critical",
	"POST /v1/admin/kyc/enforcement-policies/{policyID}/supersede": "critical",
}

func TestKYCEnforcementPolicyRoutes_GoldenClassAndRegistrationAreExact(t *testing.T) {
	n := 0
	for pat, class := range staffRouteGolden {
		if !strings.Contains(pat, "/kyc/enforcement-policies") {
			continue
		}
		n++
		if want, ok := kycPolicyRoutes[pat]; !ok || want != class {
			t.Errorf("%s: class %q, want %q (known %v)", pat, class, want, ok)
		}
	}
	if n != len(kycPolicyRoutes) {
		t.Errorf("golden table has %d enforcement-policy routes, want %d", n, len(kycPolicyRoutes))
	}
	src, err := os.ReadFile("kyc_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func registerKYCEnforcementPolicyRoutes")
	if i < 0 {
		t.Fatal("registerKYCEnforcementPolicyRoutes missing")
	}
	reg := body[i:]
	if got := regexp.MustCompile(`mux\.Handle\(`).FindAllString(reg, -1); len(got) != len(kycPolicyRoutes) {
		t.Fatalf("%d registrations, want %d", len(got), len(kycPolicyRoutes))
	}
	// Platform-only: never tenant-scope wrapped, never a tenant permission; writes use Manage, the read uses Read.
	if strings.Contains(reg, "RequireTenantScope") {
		t.Error("enforcement policy routes must not be tenant-scope wrapped (a platform token carries no tenant)")
	}
	if c := strings.Count(reg, "PermKYCEnforcementPolicyRead"); c != 1 {
		t.Errorf("PermKYCEnforcementPolicyRead on %d routes, want 1 (the read)", c)
	}
	if c := strings.Count(reg, "PermKYCEnforcementPolicyManage"); c != 4 {
		t.Errorf("PermKYCEnforcementPolicyManage on %d routes, want 4 (every write)", c)
	}
	if c := strings.Count(reg, "stepUpCritical"); c != 4 {
		t.Errorf("stepUpCritical on %d routes, want 4 (every write)", c)
	}
}

// The handlers ship no value: no numeric threshold literal, no asset code, no tier, no jurisdiction and no policy is created
// by the code; every field comes from the request.
func TestKYCEnforcementPolicyHandlers_ShipNoValues(t *testing.T) {
	src, err := os.ReadFile("kyc_enforcement_policy_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(src), "")
	for _, bad := range []string{`"EUR"`, `"USD"`, `"basic"`, `"full"`, `ThresholdMinorUnits: sp(`, `INSERT INTO kyc_enforcement_policies`} {
		if strings.Contains(code, bad) {
			t.Errorf("handlers contain %s: the platform ships no policy value", bad)
		}
	}
	// A write never sets a status: creation is the library's draft-only insert; activation is a separate, four-eyes call.
	if regexp.MustCompile(`(?i)status\s*:\s*"active"`).MatchString(code) {
		t.Error("a handler must never create an active policy")
	}
}

func TestOpenAPI_KYCEnforcementPolicyRoutesDocumented(t *testing.T) {
	spec := readSpec(t)
	for pat := range kycPolicyRoutes {
		parts := strings.SplitN(pat, " ", 2)
		block := openAPIPathBlock(t, spec, parts[1])
		if !strings.Contains(block, "    "+strings.ToLower(parts[0])+":\n") {
			t.Errorf("%s is mounted but not documented", pat)
		}
		if strings.HasPrefix(pat, "POST") && !strings.Contains(block, "step-up `critical`") {
			t.Errorf("%s: the description must state the critical step-up", pat)
		}
	}
	// Reverse: nothing documented under the prefix is unmounted.
	for _, op := range documentedOps(spec, "/v1/admin/kyc/enforcement-policies") {
		if _, ok := kycPolicyRoutes[op]; !ok {
			t.Errorf("%s is documented but not mounted", op)
		}
	}
	if n := len(documentedOps(spec, "/v1/admin/kyc/enforcement-policies")); n != len(kycPolicyRoutes) {
		t.Errorf("%d documented operations, want %d", n, len(kycPolicyRoutes))
	}
}
