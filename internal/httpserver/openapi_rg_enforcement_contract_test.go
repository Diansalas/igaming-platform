package httpserver

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// ADR 0115 S4-min OpenAPI drift pins (DB-free): the player RG routes the B2C RG screen calls
// (/v1/me/rg/self-exclusion, /status, /options and the S2 limits / time-out routes) and the staff enforcement
// switch are documented in docs/api/openapi/platform-api.yaml both ways, and the response DTOs the new routes
// serialise match their schemas field for field. The B2C contract test (b2c/src/api/rg.contract.test.ts) pins
// that every path the B2C RG api module calls exists in the same file.

func rgPlayerEnforcementRoutes(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	for _, f := range []string{"rg_routes.go", "rg_enforcement_handlers.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|DELETE) (/v1/(?:me/rg/(?:self-exclusion|status|options)|admin/rg/enforcement))"`).FindAllStringSubmatch(string(src), -1)...)
	}
	return out
}

func TestOpenAPI_RGEnforcement_RoutesDocumentedBothWays(t *testing.T) {
	spec := launchSpec(t)
	routes := rgPlayerEnforcementRoutes(t)
	if len(routes) != 5 {
		t.Fatalf("expected 5 mounted operations (self-exclusion, status, options, enforcement GET+POST), found %d (update the spec and this test together)", len(routes))
	}
	mounted := map[string]bool{}
	for _, r := range routes {
		mounted[strings.ToLower(r[1])+" "+r[2]] = true
		block := openAPIPathBlock(t, spec, r[2])
		if !strings.Contains(block, "\n    "+strings.ToLower(r[1])+":\n") && !strings.HasPrefix(block, "    "+strings.ToLower(r[1])+":\n") {
			t.Errorf("%s %s is mounted but not documented", r[1], r[2])
			continue
		}
		if !strings.Contains(block, "bearerAuth") || !strings.Contains(block, `"401"`) {
			t.Errorf("%s %s: security / 401 missing", r[1], r[2])
		}
	}
	pathRe := regexp.MustCompile(`(?m)^  (/v1/(?:me/rg/(?:self-exclusion|status|options)|admin/rg/enforcement)):\n((?:    .*\n|\n)*)`)
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
		t.Errorf("documented %d, mounted %d", documented, len(routes))
	}
}

// schemaBlock returns the YAML of one components/schemas entry.
func rgSchemaBlock(t *testing.T, spec, name string) string {
	t.Helper()
	key := "\n    " + name + ":\n"
	i := strings.Index(spec, key)
	if i < 0 {
		t.Fatalf("schema %s is not in the spec", name)
	}
	rest := spec[i+len(key):]
	if loc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

func rgJSONTags(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	return out
}

func TestOpenAPI_RGEnforcement_DTOFieldsMatchSchemas(t *testing.T) {
	spec := launchSpec(t)
	for _, c := range []struct {
		schema string
		dto    any
	}{
		{"RGOptions", rgOptionsResponse{}},
		{"RGLimitOption", rgLimitOptionResponse{}},
		{"RGEnforcement", rgEnforcementResponse{}},
		{"RGEnforcementRequest", activateRGEnforcementRequest{}},
		{"RGSelfExclusionRequest", createSelfExclusionRequest{}},
		{"RGRestriction", restrictionResponse{}},
	} {
		block := rgSchemaBlock(t, spec, c.schema)
		props := regexp.MustCompile(`(?m)^        ([a-z_]+):`).FindAllStringSubmatch(block, -1)
		inSpec := map[string]bool{}
		for _, p := range props {
			inSpec[p[1]] = true
		}
		inDTO := map[string]bool{}
		for _, tag := range rgJSONTags(c.dto) {
			inDTO[tag] = true
			if !inSpec[tag] {
				t.Errorf("%s: DTO field %q is not in the schema", c.schema, tag)
			}
		}
		for p := range inSpec {
			if !inDTO[p] {
				t.Errorf("%s: schema property %q is not in the DTO", c.schema, p)
			}
		}
	}
}

// The list envelope of the staff read is exactly {items, enforcement_enabled}.
func TestOpenAPI_RGEnforcement_ListEnvelope(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"items": []rgEnforcementResponse{}, "enforcement_enabled": false})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	block := rgSchemaBlock(t, launchSpec(t), "RGEnforcementList")
	for k := range m {
		if !strings.Contains(block, "        "+k+":") {
			t.Errorf("RGEnforcementList lacks %q", k)
		}
	}
}
