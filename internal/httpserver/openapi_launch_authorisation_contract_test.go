package httpserver

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// ADR 0112 slice 4: every mounted launch authorisation route is documented under the same path and
// method; the closed LAUNCH_* codes in internal/apierror and in the Error.code enum are the same
// set; the fixed disclaimer is the spec's only value; the response DTO fields match the schema
// properties. DB-free: it reads the route sources and the spec only.
func launchSpec(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestOpenAPI_LaunchAuthorisation_RoutesDocumented(t *testing.T) {
	spec := launchSpec(t)
	src, err := os.ReadFile("launch_authorisation_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|DELETE) (/v1/[^"]*)"`)
	routes := re.FindAllStringSubmatch(string(src), -1)
	if len(routes) != 11 {
		t.Fatalf("expected 11 mounted launch operations, found %d (update the spec and this test together)", len(routes))
	}
	mounted := map[string]bool{}
	for _, r := range routes {
		mounted[strings.ToLower(r[1])+" "+r[2]] = true
		block := openAPIPathBlock(t, spec, r[2])
		if !strings.Contains(block, "    "+strings.ToLower(r[1])+":\n") {
			t.Errorf("%s %s is mounted but not documented", r[1], r[2])
		}
		if !strings.Contains(block, "launch_authorisation:") {
			t.Errorf("%s %s: the entry must name its static permission", r[1], r[2])
		}
		if !strings.Contains(block, "bearerAuth") || !strings.Contains(block, `"401"`) {
			t.Errorf("%s %s: security / 401 missing", r[1], r[2])
		}
	}
	// The reverse direction: nothing documented under the launch names is unmounted.
	pathRe := regexp.MustCompile(`(?m)^  (/v1/admin/(?:tenants/\{tenantID\}/(?:launch-[a-z-]+|suspend|brands/\{brandID\}/suspend)|launch-authorisations)[^:\s]*):\n((?:    .*\n|\n)*)`)
	opRe := regexp.MustCompile(`(?m)^    (get|post|put|delete):\n`)
	documented := 0
	for _, m := range pathRe.FindAllStringSubmatch(spec, -1) {
		for _, op := range opRe.FindAllStringSubmatch(m[2], -1) {
			documented++
			if !mounted[op[1]+" "+m[1]] {
				t.Errorf("documented but not mounted: %s %s", op[1], m[1])
			}
		}
	}
	if documented != len(routes) {
		t.Errorf("documented %d launch operations, mounted %d", documented, len(routes))
	}

	// Specific contract points per ADR 0112 section 9 and the slice-4 brief.
	for _, p := range []string{
		"/v1/admin/tenants/{tenantID}/launch-authorisations",
		"/v1/admin/tenants/{tenantID}/brands/{brandID}/suspend",
		"/v1/admin/tenants/{tenantID}/suspend",
	} {
		b := openAPIPathBlock(t, spec, p)
		for _, want := range []string{"signed actor proof", "ignored", "not a legal or regulatory authorisation"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: OpenAPI entry lacks %q", p, want)
			}
		}
	}
	approve := openAPIPathBlock(t, spec, "/v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/approve")
	for _, want := range []string{"platform_admin ONLY", "seen_readiness_hash", "LAUNCH_REFUSED_AT_EXECUTION", "LAUNCH_ALREADY_DECIDED"} {
		if !strings.Contains(approve, want) {
			t.Errorf("approve: OpenAPI entry lacks %q", want)
		}
	}
	for _, p := range []string{"/v1/admin/tenants/{tenantID}/launch-readiness", "/v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/cancel"} {
		if b := openAPIPathBlock(t, spec, p); !strings.Contains(b, `"404"`) && !strings.Contains(b, "LaunchNotFound") {
			t.Errorf("%s: 404 must be documented", p)
		}
	}
}

// The Error.code enum and the apierror LAUNCH_* constants are one set (no code can be returned
// that the spec does not list, and none is listed that the code cannot return).
func TestOpenAPI_LaunchAuthorisation_ErrorCodesMatch(t *testing.T) {
	spec := launchSpec(t)
	src, err := os.ReadFile("../apierror/apierror.go")
	if err != nil {
		t.Fatal(err)
	}
	codeRe := regexp.MustCompile(`Code = "(LAUNCH_[A-Z_]+)"`)
	inCode := map[string]bool{}
	for _, m := range codeRe.FindAllStringSubmatch(string(src), -1) {
		inCode[m[1]] = true
	}
	start := strings.Index(spec, "    Error:\n")
	if start < 0 {
		t.Fatal("schema Error missing")
	}
	enum := spec[start:]
	if i := strings.Index(enum, "        message:"); i > 0 {
		enum = enum[:i]
	}
	inSpec := map[string]bool{}
	for _, m := range regexp.MustCompile(`- (LAUNCH_[A-Z_]+)\n`).FindAllStringSubmatch(enum, -1) {
		inSpec[m[1]] = true
	}
	if len(inCode) < 19 {
		t.Fatalf("expected the closed LAUNCH_* set in apierror, found %d", len(inCode))
	}
	for c := range inCode {
		if !inSpec[c] {
			t.Errorf("%s is returned by the API but not in the Error.code enum", c)
		}
	}
	for c := range inSpec {
		if !inCode[c] {
			t.Errorf("%s is in the Error.code enum but not defined in apierror", c)
		}
	}
	// The existing code the closure refusal reuses, and the check_codes property.
	if !strings.Contains(enum, "- TENANT_CLOSE_BLOCKED_OPEN_ROUNDS\n") || !strings.Contains(spec[start:], "check_codes:") {
		t.Error("Error must list TENANT_CLOSE_BLOCKED_OPEN_ROUNDS and document check_codes")
	}
	// Each mapped code is documented in the conflict / forbidden responses.
	conflict := spec[strings.Index(spec, "    LaunchConflict:\n"):]
	for c := range inCode {
		switch c {
		case "LAUNCH_AUTHORISATION_UNAVAILABLE", "LAUNCH_PROOF_REFUSED", "LAUNCH_NO_PERSON":
			continue
		}
		if !strings.Contains(conflict, c) {
			t.Errorf("%s is not described in the LaunchConflict response", c)
		}
	}
	forbidden := spec[strings.Index(spec, "    LaunchForbidden:\n"):]
	for _, c := range []string{"LAUNCH_PROOF_REFUSED", "LAUNCH_NO_PERSON"} {
		if !strings.Contains(forbidden, c) {
			t.Errorf("%s must be described in LaunchForbidden", c)
		}
	}
}

// The disclaimer constant of the code is the spec's only allowed value; no schema describes a legal
// conclusion.
func TestOpenAPI_LaunchAuthorisation_DisclaimerIsTheFixedText(t *testing.T) {
	spec := launchSpec(t)
	block := spec[strings.Index(spec, "    LaunchDisclaimer:\n"):]
	block = block[:strings.Index(block, "    LaunchCheck:\n")]
	if !strings.Contains(block, `- "`+launchgov.Disclaimer+`"`) {
		t.Fatalf("the LaunchDisclaimer enum must be exactly launchgov.Disclaimer:\n%s", block)
	}
	// Every launch envelope schema requires it.
	for _, name := range []string{"LaunchReadinessResponse", "LaunchRequestEnvelope", "LaunchRequestDetail", "LaunchRequestList", "LaunchQueueList", "LaunchResult", "LaunchDecisionResult", "LaunchTransitionList"} {
		b := schemaBlock(t, spec, name)
		if !regexp.MustCompile(`required: \[disclaimer`).MatchString(b) {
			t.Errorf("%s must require the disclaimer", name)
		}
	}
}

// schemaBlock returns the text of one components/schemas entry.
func schemaBlock(t *testing.T, spec, name string) string {
	t.Helper()
	key := "\n    " + name + ":\n"
	i := strings.Index(spec, key)
	if i < 0 {
		t.Fatalf("schema %s missing", name)
	}
	rest := spec[i+len(key):]
	if loc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

func launchSchemaProps(block string) map[string]bool {
	out := map[string]bool{}
	in := false
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "      properties:") {
			in = true
			continue
		}
		if in && strings.HasPrefix(line, "      ") && !strings.HasPrefix(line, "        ") {
			in = false
		}
		if in {
			if m := regexp.MustCompile(`^        ([a-z_]+):`).FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
	}
	return out
}

func jsonFields(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Anonymous {
			out = append(out, jsonFields(f.Type)...)
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, tag)
	}
	return out
}

// The wire DTOs and the schemas describe the same properties, in both directions.
func TestOpenAPI_LaunchAuthorisation_DTOsMatchSchemas(t *testing.T) {
	spec := launchSpec(t)
	cases := []struct {
		dto    any
		schema string
	}{
		{launchRequestDTO{}, "LaunchRequest"},
		{launchApprovalDTO{}, "LaunchApproval"},
		{launchPriorRejectDTO{}, "LaunchPriorReject"},
		{launchReadinessDTO{}, "LaunchReadiness"},
		{launchCheckDTO{}, "LaunchCheck"},
		{launchTransitionDTO{}, "LaunchTransition"},
		{launchCreateBody{}, "LaunchCreateRequest"},
		{launchLicensingBody{}, "LaunchLicensing"},
		{launchDecideBody{}, "LaunchDecideRequest"},
		{launchCancelBody{}, "LaunchCancelRequest"},
		{launchSuspendBody{}, "LaunchSuspendRequest"},
		{launchResultResponse{}, "LaunchResult"},
		{launchDecisionResponse{}, "LaunchDecisionResult"},
		// Review condition (g): the envelopes too (they were inline structs before).
		{launchReadinessResponse{}, "LaunchReadinessResponse"},
		{launchListResponse{}, "LaunchRequestList"},
		{launchQueueResponse{}, "LaunchQueueList"},
		{launchRequestEnvelope{}, "LaunchRequestEnvelope"},
		{launchDetailResponse{}, "LaunchRequestDetail"},
		{launchHistoryResponse{}, "LaunchTransitionList"},
	}
	for _, c := range cases {
		props := launchSchemaProps(schemaBlock(t, spec, c.schema))
		fields := jsonFields(reflect.TypeOf(c.dto))
		sort.Strings(fields)
		seen := map[string]bool{}
		for _, f := range fields {
			seen[f] = true
			if !props[f] {
				t.Errorf("%s: the field %q is on the wire but not in the schema", c.schema, f)
			}
		}
		for p := range props {
			if !seen[p] {
				t.Errorf("%s: the schema property %q is not produced by the DTO", c.schema, p)
			}
		}
	}
}

// Every $ref in the spec resolves (a dangling ref in the new section would otherwise go unnoticed).
func TestOpenAPI_LaunchAuthorisation_RefsResolve(t *testing.T) {
	spec := launchSpec(t)
	for _, m := range regexp.MustCompile(`\$ref: "#/components/(schemas|responses|parameters)/(\w+)"`).FindAllStringSubmatch(spec, -1) {
		var key string
		switch m[1] {
		case "schemas":
			key = "\n    " + m[2] + ":\n"
		case "responses":
			key = "\n    " + m[2] + ":\n"
		case "parameters":
			key = "\n    " + m[2] + ":\n"
		}
		if !strings.Contains(spec, key) {
			t.Errorf("dangling reference #/components/%s/%s", m[1], m[2])
		}
	}
}
