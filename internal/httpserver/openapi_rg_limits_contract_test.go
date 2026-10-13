package httpserver

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/rg"
)

// ADR 0115 slice S2 OpenAPI drift pins. DB-free: it reads the route source and the spec only.

func rgRoutes(t *testing.T) [][]string {
	t.Helper()
	src, err := os.ReadFile("rg_limits_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|DELETE) (/v1/[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	return routes
}

func TestOpenAPI_RGLimits_RoutesDocumentedBothWays(t *testing.T) {
	spec := launchSpec(t)
	routes := rgRoutes(t)
	if len(routes) != 15 {
		t.Fatalf("expected 15 mounted RG limit operations, found %d (update the spec and this test together)", len(routes))
	}
	mounted := map[string]bool{}
	for _, r := range routes {
		mounted[strings.ToLower(r[1])+" "+r[2]] = true
		block := openAPIPathBlock(t, spec, r[2])
		op := block
		if i := strings.Index(block, "\n    "+strings.ToLower(r[1])+":\n"); i >= 0 {
			op = block[i:]
		} else if !strings.HasPrefix(block, "    "+strings.ToLower(r[1])+":\n") {
			t.Errorf("%s %s is mounted but not documented", r[1], r[2])
			continue
		}
		// cut the operation at the next sibling operation
		if m := regexp.MustCompile(`\n    (get|post|put|delete):\n`).FindStringIndex(op[1:]); m != nil {
			op = op[:m[0]+1]
		}
		if !strings.Contains(op, "bearerAuth") || !strings.Contains(op, `"401"`) {
			t.Errorf("%s %s: security / 401 missing", r[1], r[2])
		}
		if r[1] == "POST" && strings.HasPrefix(r[2], "/v1/me/rg/") || r[1] == "POST" && r[2] == "/v1/admin/players/{id}/rg/limits" {
			if !strings.Contains(op, "#/components/parameters/IdempotencyKey") {
				t.Errorf("%s %s: the Idempotency-Key parameter must be documented", r[1], r[2])
			}
		}
	}
	// the reverse direction: nothing documented under the RG paths is unmounted
	pathRe := regexp.MustCompile(`(?m)^  (/v1/(?:me/rg/(?:limits|time-out)[^:\s]*|admin/players/\{id\}/rg/[^:\s]*|admin/rg/limit-configs|admin/platform/rg/[^:\s]*)):\n((?:    .*\n|\n)*)`)
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
		t.Errorf("documented %d RG limit operations, mounted %d", documented, len(routes))
	}
}

func TestOpenAPI_RGLimits_NamedPermissionsAndStatuses(t *testing.T) {
	spec := launchSpec(t)
	perm := map[string]string{
		"/v1/admin/players/{id}/rg/limits":                                "rg_limit:",
		"/v1/admin/players/{id}/rg/history":                               "rg_limit:read",
		"/v1/admin/rg/limit-configs":                                      "rg_config:",
		"/v1/admin/platform/rg/limit-config-change-requests":              "rg_floor:",
		"/v1/admin/platform/rg/limit-config-change-requests/{id}/approve": "rg_floor:decide",
		"/v1/admin/platform/rg/limit-config-change-requests/{id}/reject":  "rg_floor:decide",
		"/v1/admin/platform/rg/limit-config-change-requests/{id}/cancel":  "rg_floor:request",
	}
	for p, want := range perm {
		if b := openAPIPathBlock(t, spec, p); !strings.Contains(b, want) {
			t.Errorf("%s: the entry must name its static permission %q", p, want)
		}
	}
	setLimit := openAPIPathBlock(t, spec, "/v1/me/rg/limits")
	for _, want := range []string{`"201"`, `"202"`, `"422"`, `"429"`, "RG_LIMIT_ABOVE_CEILING", "RG_PERIOD_NOT_ALLOWED", "Idempotency"} {
		if !strings.Contains(setLimit, want) {
			t.Errorf("POST /v1/me/rg/limits: the entry lacks %q", want)
		}
	}
	for _, p := range []string{"/v1/me/rg/limits", "/v1/admin/platform/rg/limit-config-change-requests/{id}/approve"} {
		if b := openAPIPathBlock(t, spec, p); !strings.Contains(b, "NOT IMPLEMENTED") && !strings.Contains(b, "signed actor proof") {
			t.Errorf("%s: the status / proof caveat is missing", p)
		}
	}
	if b := openAPIPathBlock(t, spec, "/v1/admin/players/{id}/rg/limits"); !strings.Contains(b, "RG_STEP_UP_REQUIRED") || !strings.Contains(b, "ADR 0017") {
		t.Error("the staff limit write must document the step-up 403 and ADR 0017")
	}
}

// The Error.code enum and the apierror RG_* constants are one set.
func TestOpenAPI_RGLimits_ErrorCodesMatch(t *testing.T) {
	spec := launchSpec(t)
	src, err := os.ReadFile("../apierror/apierror.go")
	if err != nil {
		t.Fatal(err)
	}
	inCode := map[string]bool{}
	for _, m := range regexp.MustCompile(`Code = "(RG_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1) {
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
	for _, m := range regexp.MustCompile(`- (RG_[A-Z_]+)\n`).FindAllStringSubmatch(enum, -1) {
		inSpec[m[1]] = true
	}
	if len(inCode) != 22 {
		t.Fatalf("expected the closed set of 22 RG_* codes in apierror, found %d", len(inCode))
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
	// every code is described in one of the RG response objects
	described := spec[strings.Index(spec, "    RGForbidden:\n"):]
	for c := range inCode {
		if !strings.Contains(described, c) && c != "RG_UNAVAILABLE" {
			t.Errorf("%s is not described in an RG response", c)
		}
	}
	// every refusal the service can produce maps onto a documented code
	for _, code := range []rg.RefusalCode{rg.RefusedAboveCeiling, rg.RefusedPeriodNotAllowed, rg.RefusedDurationNotAllowed, rg.RefusedNotOffered,
		rg.RefusedBackdated, rg.RefusedSyntheticRefused, rg.RefusedNoChange, rg.RefusedChangeNotPending, rg.RefusedCoolingOff,
		rg.RefusedConfigMissing, rg.RefusedPendingCap, rg.RefusedIdempotencyMismatch, rg.RefusedOperatorLoosens, rg.RefusedNoFloor,
		rg.RefusedSelfApproval, rg.RefusedRequestNotPending, rg.RefusedGuard, rg.RefusedStepUpRequired, rg.RefusedSelfTarget,
		rg.RefusedIdentityReview, rg.RefusedProof, rg.RefusedUnavailable} {
		c, _ := rgErrorCode(&rg.Refusal{Code: code})
		if !inCode[string(c)] {
			t.Errorf("refusal %s maps to %q, which is not an RG_* code", code, c)
		}
	}
}

// The wire DTOs and the schemas describe the same properties, in both directions.
func TestOpenAPI_RGLimits_DTOsMatchSchemas(t *testing.T) {
	spec := launchSpec(t)
	cases := []struct {
		dto    any
		schema string
	}{
		{rgLimitChangeResponse{}, "RGLimitChange"},
		{rgLimitStateResponse{}, "RGLimitState"},
		{rgMyLimitsResponse{}, "RGMyLimits"},
		{setLimitRequest{}, "RGSetLimitRequest"},
		{staffSetLimitRequest{}, "RGStaffSetLimitRequest"},
		{rgStaffLimitChangeResponse{}, "RGStaffLimitChange"},
		{rgStaffLimitsResponse{}, "RGStaffLimits"},
		{rgStaffHistoryResponse{}, "RGStaffHistory"},
		{createTimeOutRequest{}, "RGTimeOutRequest"},
		{rg.ConfigContent{}, "RGConfigContent"},
		{writeRGOperatorConfigRequest{}, "RGOperatorConfigRequest"},
		{rgConfigResponse{}, "RGConfig"},
		{requestRGFloorRequest{}, "RGFloorRequestBody"},
		{rgFloorRequestResponse{}, "RGFloorRequest"},
	}
	for _, c := range cases {
		props := launchSchemaProps(schemaBlock(t, spec, c.schema))
		seen := map[string]bool{}
		for _, f := range jsonFields(reflect.TypeOf(c.dto)) {
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
	// the three attested shapes
	for _, n := range []string{"RGAttestedString", "RGAttestedInt", "RGAttestedBool"} {
		props := launchSchemaProps(schemaBlock(t, spec, n))
		if !props["none"] || !props["value"] || len(props) != 2 {
			t.Errorf("%s must describe exactly none and value: %v", n, props)
		}
	}
}

// The restriction schema now admits the second restriction type the time-out route returns.
func TestOpenAPI_RGLimits_RestrictionTypeIncludesTimeOut(t *testing.T) {
	if b := schemaBlock(t, launchSpec(t), "RGRestriction"); !strings.Contains(b, "enum: [self_exclusion, time_out]") {
		t.Error("RGRestriction.restriction_type must list time_out")
	}
}

// Every $ref in the spec still resolves (the new section included).
func TestOpenAPI_RGLimits_RefsResolve(t *testing.T) {
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
			t.Errorf("dangling $ref %s", m[0])
		}
	}
}
