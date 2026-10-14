package httpserver

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ADR 0114 slice S0: DB-free contract/drift tests for the governed routes that were mounted but
// undocumented (K1 capability grants, K2 manual adjustments, financial-policy, platform-actions
// audit, KYC enforcement decisions, staff display-name and person-link), plus drift between the
// response DTOs and their schemas for the schemas S0 changed. Structural checks over the YAML text,
// in the style of openapi_payments_killswitch_contract_test.go (no YAML library in this module).

func readSpec(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// mountedRoutes returns "METHOD /path" for every mux.Handle("METHOD /path" literal in the files.
func mountedRoutes(t *testing.T, files ...string) []string {
	t.Helper()
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) (/v1/[^"]*)"`)
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out = append(out, m[1]+" "+m[2])
		}
	}
	return out
}

// documentedOps returns "METHOD /path" for every operation documented under paths whose path has
// one of the prefixes (the reverse drift direction: documented but not mounted).
func documentedOps(spec string, prefixes ...string) []string {
	var out []string
	pathRe := regexp.MustCompile(`(?m)^  (/v1/\S*):\n`)
	locs := pathRe.FindAllStringSubmatchIndex(spec, -1)
	for i, loc := range locs {
		path := spec[loc[2]:loc[3]]
		match := false
		for _, p := range prefixes {
			if strings.HasPrefix(path, p) {
				match = true
			}
		}
		if !match {
			continue
		}
		end := len(spec)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := spec[loc[1]:end]
		// stop at the first top-level key after paths
		if idx := regexp.MustCompile(`(?m)^\S`).FindStringIndex(block); idx != nil {
			block = block[:idx[0]]
		}
		for _, m := range regexp.MustCompile(`(?m)^    (get|post|put|patch|delete):\n`).FindAllStringSubmatch(block, -1) {
			out = append(out, strings.ToUpper(m[1])+" "+path)
		}
	}
	return out
}

func TestOpenAPI_GovernedRoutes_MountedRoutesDocumented(t *testing.T) {
	spec := readSpec(t)
	groups := []struct {
		name  string
		files []string
		keep  func(route string) bool
		want  int
	}{
		{"K1 capability grants", []string{"capability_routes.go"}, func(string) bool { return true }, 7},
		{"K2 manual adjustments and financial policy", []string{"manual_adjustment_routes.go"}, func(string) bool { return true }, 12},
		{"platform actions audit, display names, person links", []string{"routes.go"}, func(r string) bool {
			return strings.Contains(r, "/audit-log/platform-actions") || strings.Contains(r, "display-name") || strings.Contains(r, "person-link")
		}, 5},
		{"KYC enforcement decisions", []string{"kyc_routes.go"}, func(r string) bool { return strings.Contains(r, "enforcement-decisions") }, 1},
	}
	for _, g := range groups {
		var routes []string
		for _, r := range mountedRoutes(t, g.files...) {
			if g.keep(r) {
				routes = append(routes, r)
			}
		}
		if len(routes) != g.want {
			t.Fatalf("%s: expected %d mounted operations, found %d: %v", g.name, g.want, len(routes), routes)
		}
		for _, r := range routes {
			parts := strings.SplitN(r, " ", 2)
			block := openAPIPathBlock(t, spec, parts[1])
			if !strings.Contains(block, "    "+strings.ToLower(parts[0])+":\n") {
				t.Errorf("%s: %s is mounted but not documented", g.name, r)
			}
		}
	}
}

func TestOpenAPI_GovernedRoutes_DocumentedOperationsAreMounted(t *testing.T) {
	spec := readSpec(t)
	mounted := map[string]bool{}
	for _, r := range mountedRoutes(t, "capability_routes.go", "manual_adjustment_routes.go", "routes.go", "kyc_routes.go") {
		mounted[r] = true
	}
	prefixes := []string{
		"/v1/admin/tenants/{tenantID}/capability-grants", "/v1/admin/tenants/{tenantID}/manual-adjustments",
		"/v1/admin/financial-polic", "/v1/admin/tenants/{tenantID}/financial-policy-changes",
		"/v1/admin/audit-log/platform-actions", "/v1/admin/kyc/enforcement-decisions",
		"/v1/admin/staff/me/display-name", "/v1/admin/platform-staff/", "/v1/admin/tenants/{tenantID}/staff/{staffID}/",
	}
	ops := documentedOps(spec, prefixes...)
	if len(ops) != 25 {
		t.Fatalf("expected exactly 25 documented operations in the governed families, found %d: %v", len(ops), ops)
	}
	for _, op := range ops {
		if !mounted[op] {
			t.Errorf("%s is documented but no such route is mounted", op)
		}
	}
}

// schemaBlock returns the text of components.schemas.<name>.
func govSchemaBlock(t *testing.T, spec, name string) string {
	t.Helper()
	key := "\n    " + name + ":\n"
	start := strings.Index(spec, key)
	if start < 0 {
		t.Fatalf("schema %s missing", name)
	}
	rest := spec[start+len(key):]
	if loc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

// schemaProps returns the property names declared directly under the schema's `properties:`.
func govSchemaProps(t *testing.T, spec, name string) []string {
	t.Helper()
	block := govSchemaBlock(t, spec, name)
	i := strings.Index(block, "\n      properties:\n")
	if i < 0 {
		t.Fatalf("schema %s has no properties", name)
	}
	rest := block[i+len("\n      properties:\n"):]
	if loc := regexp.MustCompile(`(?m)^      \S`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	var props []string
	for _, m := range regexp.MustCompile(`(?m)^        ([A-Za-z_]+):`).FindAllStringSubmatch(rest, -1) {
		props = append(props, m[1])
	}
	sort.Strings(props)
	return props
}

func jsonFieldNames(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}

// DTO <-> schema drift: the schema documents exactly the fields the DTO emits.
func TestOpenAPI_GovernedRoutes_DTOsMatchSchemas(t *testing.T) {
	spec := readSpec(t)
	cases := []struct {
		schema string
		typ    reflect.Type
	}{
		{"CapabilityGrantRequest", reflect.TypeOf(capabilityGrantRequestDTO{})},
		{"CapabilityGrant", reflect.TypeOf(capabilityGrantDTO{})},
		{"CapabilityGrantRequestBody", reflect.TypeOf(createCapabilityGrantRequestBody{})},
		{"CapabilityGrantDecisionBody", reflect.TypeOf(decideCapabilityGrantRequestBody{})},
		{"CapabilityGrantRevokeBody", reflect.TypeOf(revokeCapabilityGrantBody{})},
		{"ManualAdjustment", reflect.TypeOf(adjustmentDTO{})},
		{"ManualAdjustmentSubmitBody", reflect.TypeOf(submitAdjustmentBody{})},
		{"ManualAdjustmentDecisionBody", reflect.TypeOf(decideAdjustmentBody{})},
		{"ManualAdjustmentDecision", reflect.TypeOf(decisionResponse{})},
		{"FinancialPolicyChangeProposal", reflect.TypeOf(proposePolicyChangeBody{})},
		{"FinancialPolicyChangeDecisionBody", reflect.TypeOf(decidePolicyChangeBody{})},
		{"FinancialPolicyChange", reflect.TypeOf(policyChangeDTO{})},
		{"FinancialPolicy", reflect.TypeOf(policyDTO{})},
		{"PlatformActionAuditActor", reflect.TypeOf(platformActionAuditActor{})},
		{"PlatformActionAuditChainEntry", reflect.TypeOf(platformActionAuditChainEntry{})},
		{"PlatformActionAuditEntry", reflect.TypeOf(platformActionAuditEntry{})},
		{"KYCEnforcementDecision", reflect.TypeOf(enforcementDecisionResponse{})},
		{"KYCEnforcementDecisionPage", reflect.TypeOf(listEnforcementDecisionsResponse{})},
		{"StaffDisplayNameRequest", reflect.TypeOf(updateStaffDisplayNameRequest{})},
		{"StaffPersonLinkRequest", reflect.TypeOf(linkStaffPersonRequest{})},
		// schemas changed by S0 (staff-id redaction, evidence reference):
		{"PaymentsKillSwitch", reflect.TypeOf(killSwitchDTO{})},
		{"PaymentsKillSwitchReleaseRequest", reflect.TypeOf(killSwitchReleaseRequestDTO{})},
		{"PaymentForceResolution", reflect.TypeOf(resolutionDTO{})},
		{"WithdrawalHoldResolution", reflect.TypeOf(holdResolutionDTO{})},
		{"ProviderCredentialHandle", reflect.TypeOf(providerCredentialHandleDTO{})},
		// ADR 0114 S1b: the alert list/get item (acted-by ids, timestamps, delivery fields).
		{"PlatformAlert", reflect.TypeOf(alertListItemDTO{})},
	}
	for _, c := range cases {
		got, want := govSchemaProps(t, spec, c.schema), jsonFieldNames(c.typ)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema properties %v != %s JSON fields %v", c.schema, got, c.typ.Name(), want)
		}
	}
}

func TestOpenAPI_GovernedRoutes_RequestSchemasStrictAndTenantFree(t *testing.T) {
	spec := readSpec(t)
	for _, name := range []string{
		"CapabilityGrantRequestBody", "CapabilityGrantDecisionBody", "CapabilityGrantRevokeBody",
		"ManualAdjustmentSubmitBody", "ManualAdjustmentDecisionBody", "FinancialPolicyChangeProposal",
		"FinancialPolicyChangeDecisionBody", "StaffDisplayNameRequest", "StaffPersonLinkRequest",
	} {
		block := govSchemaBlock(t, spec, name)
		if !strings.Contains(block, "additionalProperties: false") {
			t.Errorf("%s must refuse unknown properties (the handler uses DisallowUnknownFields)", name)
		}
		if regexp.MustCompile(`(?m)^        tenant_id:`).MatchString(block) {
			t.Errorf("%s must carry no tenant field - the target tenant comes from the path only", name)
		}
	}
}

// ADR 0114 S0 contract: the staff-id fields a tenant caller can read are nullable in the spec, the
// K3 schema carries only a presence flag for the evidence reference, and no schema exposes its text.
func TestOpenAPI_GovernedRoutes_RedactionContractDocumented(t *testing.T) {
	spec := readSpec(t)
	for schema, fields := range map[string][]string{
		"PaymentsKillSwitch":               {"changed_by"},
		"PaymentsKillSwitchReleaseRequest": {"requested_by", "approved_by"},
		"CapabilityGrantRequest":           {"grantee_staff_id", "requested_by"},
		"CapabilityGrant":                  {"grantee_staff_id"},
		"ManualAdjustment":                 {"initiated_by"},
		"PaymentForceResolution":           {"requested_by"},
		"WithdrawalHoldResolution":         {"requested_by"},
		"ProviderCredentialHandle":         {"revoked_by"},
		"AuditEntry":                       {"actor_id"},
		"PlatformAlert":                    {"acked_by", "resolved_by"}, // ADR 0114 S1b
	} {
		block := govSchemaBlock(t, spec, schema)
		for _, f := range fields {
			re := regexp.MustCompile(`(?m)^        ` + f + `:.*nullable: true`)
			if !re.MatchString(block) {
				t.Errorf("%s.%s must be documented nullable (null for a tenant caller when the actor is platform staff)", schema, f)
			}
		}
	}
	k3 := govSchemaBlock(t, spec, "PaymentForceResolution")
	if regexp.MustCompile(`(?m)^        evidence_reference:`).MatchString(k3) || !strings.Contains(k3, "evidence_reference_present:") {
		t.Error("PaymentForceResolution must document evidence_reference_present and never evidence_reference")
	}
	if regexp.MustCompile(`(?m)^\s+evidence_reference:`).MatchString(spec) {
		t.Error("no schema may document an evidence_reference text field")
	}
}
